package configdb

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/iodesystems/corrallm/internal/config"
)

// Source is the configuration, wherever it lives.
//
// The point of the type is that callers stop knowing. `config.Load(path)` and
// `config.SaveValidated(path, c)` were reasonable while a file was the only
// answer; a Source lets the daemon, the CLI and the API each ask for "the
// config" without repeating the decision about where that is.
type Source struct {
	DB *sql.DB
	// Note labels the revision the next Save records. See WithNote.
	Note string
}

// Load reads the stored config and finalizes it exactly as the file path does.
//
// Resolution and validation come from config.Finalize rather than being redone
// here: two copies of that sequence is how a config valid from one source
// becomes invalid from the other.
func (s *Source) Load(ctx context.Context) (*config.Config, error) {
	c, err := Read(ctx, s.DB)
	if err != nil {
		return nil, err
	}
	if err := c.Finalize(); err != nil {
		return nil, fmt.Errorf("stored config: %w", err)
	}
	return c, nil
}

// Update is THE way to change configuration.
//
// Read, modify, validate, write, record — all inside one transaction, holding
// one lock. Everything else that writes config goes through it.
//
// The read has to be inside, and that is the whole point. Editing was
// read-modify-write with the read done separately from the write: the API took
// the in-memory config, copied it, applied the change and saved. Two concurrent
// edits therefore both started from the same base and the second silently
// discarded the first — a model added in one browser tab disappearing when
// another tab renamed a lane. Reading inside the transaction makes the second
// edit see the first.
//
// The mutex is process-wide because there is one configuration per process, and
// it serialises writers before they reach SQLite rather than letting them
// collide there. It is NOT what makes this safe across processes — the
// transaction is. A `corrallm config load` running against the daemon's
// database takes SQLite's write lock, and whichever transaction commits second
// still read inside its own transaction.
func (s *Source) Update(ctx context.Context, fn func(*config.Config) error) (*config.Config, error) {
	writeMu.Lock()
	defer writeMu.Unlock()

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	cur, err := Read(ctx, tx)
	if err != nil {
		return nil, err
	}
	// Callers edit the RESOLVED config — the API's handlers look up models by
	// served name, which only exist after extensions are expanded — so resolve
	// before handing it over, exactly as a load would.
	if err := cur.Finalize(); err != nil {
		return nil, fmt.Errorf("the stored config no longer finalizes: %w", err)
	}
	// Handed to the caller EDIT-READY: the maps a handler will assign into must
	// exist, because a config read from an empty store has none and the first
	// "add a model" on a fresh install panics on a nil map. The file path got
	// this for free from its copy-for-edit step; reading from the store does
	// not, and a caller should not have to remember which maps to allocate.
	if cur.Models == nil {
		cur.Models = map[string]config.Model{}
	}
	if cur.Servers == nil {
		cur.Servers = map[string]config.Server{}
	}
	if cur.Lanes == nil {
		cur.Lanes = map[string]config.Lane{}
	}
	if cur.PriorityGroups == nil {
		cur.PriorityGroups = map[string]config.PriorityGroup{}
	}
	if cur.Keys == nil {
		cur.Keys = map[string]string{}
	}
	if cur.Extensions == nil {
		cur.Extensions = map[string]config.Extension{}
	}
	if cur.Providers == nil {
		cur.Providers = map[string]config.LocalProvider{}
	}
	if cur.Tools == nil {
		cur.Tools = map[string]config.Tool{}
	}

	if err := fn(cur); err != nil {
		return nil, err
	}

	authored := config.ForWriting(cur)
	check, err := clone(authored)
	if err != nil {
		return nil, err
	}
	if err := check.Finalize(); err != nil {
		return nil, fmt.Errorf("refusing to store an invalid config: %w", err)
	}
	if err := writeTx(ctx, tx, authored); err != nil {
		return nil, err
	}
	if err := recordTx(ctx, tx, authored, s.Note); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return check, nil
}

// writeMu serialises config writers in this process. See Update.
var writeMu sync.Mutex

// Save replaces the whole configuration.
//
// Update is preferred for an EDIT, because it reads and writes atomically.
// This is for the cases that genuinely replace everything and have nothing to
// merge with: an import, a restore, a CLI load.
//
// Validates BEFORE writing.
//
// A file could be written and then found invalid at the next start, which is
// bad enough. A database that the running daemon reloads from would serve the
// broken config immediately, so the check has to happen first — and on a COPY,
// because Finalize resolves extensions into models and storing the resolved
// form would duplicate every provided model as if it had been declared.
func (s *Source) Save(ctx context.Context, c *config.Config) error {
	writeMu.Lock()
	defer writeMu.Unlock()

	// Reduce to the AUTHORED config first, then validate that. Validating the
	// caller's copy instead checks a config that may already have been through
	// resolution, and Finalize resolving it a second time reports every
	// extension-provided model as colliding with itself.
	authored := config.ForWriting(c)
	check, err := clone(authored)
	if err != nil {
		return err
	}
	if err := check.Finalize(); err != nil {
		return fmt.Errorf("refusing to store an invalid config: %w", err)
	}
	if err := Write(ctx, s.DB, authored); err != nil {
		return err
	}
	// Recorded AFTER the write succeeds: a revision is a state the system
	// actually reached. Recording before would fill the history with configs
	// that failed validation and never ran.
	//
	// A failure to record is logged by the caller, not returned: losing an
	// audit entry is bad, and refusing a config change that has already been
	// committed is worse.
	if err := Record(ctx, s.DB, authored, s.Note); err != nil {
		return fmt.Errorf("config saved, but recording the revision failed: %w", err)
	}
	return nil
}

// WithNote returns a Source that labels its next save.
//
// The note is what makes history readable a month later — "ui: upsert model
// qwen3.8" beats a timestamp and a blob. A copy rather than a mutation so
// concurrent callers cannot relabel each other's writes.
func (s *Source) WithNote(note string) *Source {
	return &Source{DB: s.DB, Note: note}
}

// clone deep-copies through YAML, which is the only representation that
// respects the custom marshallers these types carry.
func clone(c *config.Config) (*config.Config, error) {
	b, err := yaml.Marshal(c)
	if err != nil {
		return nil, err
	}
	var out config.Config
	if err := yaml.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ExportYAML renders the stored config as YAML.
//
// This is the escape hatch that makes deleting config.yml survivable: the
// database stops being the only readable copy the moment anyone runs it. It
// exports what was STORED, not what was resolved — an export you cannot import
// again would be a backup of nothing.
func (s *Source) ExportYAML(ctx context.Context) ([]byte, error) {
	c, err := Read(ctx, s.DB)
	if err != nil {
		return nil, err
	}
	return yaml.Marshal(c)
}

// ImportFile parses a YAML config and stores it, replacing what was there.
//
// Labels the revision with where it came from when the caller has not said
// otherwise: an unlabelled entry in the history is a date and a byte count,
// which is exactly as useful as no history.
func (s *Source) ImportFile(ctx context.Context, path string) (*config.Config, error) {
	c, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	src := s
	if src.Note == "" {
		src = s.WithNote("loaded from " + path)
	}
	if err := src.Save(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

// VerifyAgainstFile reports whether the stored config matches the file.
//
// This is what "a verified import" means, and it is the gate on deleting
// anything: the config read back OUT of the tables must be semantically equal
// to the config parsed from the file. Compared as re-marshalled YAML rather
// than as the original bytes, because key order, comments and formatting are
// not meaning — and the file's comments provably do not survive a rewrite even
// today.
func (s *Source) VerifyAgainstFile(ctx context.Context, path string) error {
	fromFile, err := config.Load(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	stored, err := Read(ctx, s.DB)
	if err != nil {
		return err
	}
	// The file side is finalized by config.Load; finalize the stored side too,
	// or the comparison is between a resolved config and an unresolved one.
	if err := stored.Finalize(); err != nil {
		return fmt.Errorf("stored config does not finalize: %w", err)
	}
	a, err := yaml.Marshal(fromFile)
	if err != nil {
		return err
	}
	b, err := yaml.Marshal(stored)
	if err != nil {
		return err
	}
	var am, bm map[string]any
	if err := yaml.Unmarshal(a, &am); err != nil {
		return err
	}
	if err := yaml.Unmarshal(b, &bm); err != nil {
		return err
	}
	var differing []string
	for _, k := range unionKeys(am, bm) {
		if !yamlEqual(am[k], bm[k]) {
			differing = append(differing, k)
		}
	}
	if len(differing) > 0 {
		return fmt.Errorf("the stored config differs from %s in: %v", path, differing)
	}
	return nil
}

func unionKeys(a, b map[string]any) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range []map[string]any{a, b} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	return out
}

func yamlEqual(a, b any) bool {
	ab, err1 := yaml.Marshal(a)
	bb, err2 := yaml.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ab) == string(bb)
}

// FileExists is a small helper the boot path uses to decide whether there is
// anything to import.
func FileExists(path string) bool {
	if path == "" {
		return false
	}
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}
