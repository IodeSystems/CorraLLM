package configdb

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/iodesystems/corrallm/internal/config"
)

// Source is the configuration, wherever it lives.
//
// The point of the type is that callers stop knowing. `config.Load(path)` and
// `config.SaveValidated(path, c)` were reasonable while a file was the only
// answer; a Source lets the daemon, the CLI and the API each ask for "the
// config" without repeating the decision about where that is.
type Source struct{ DB *sql.DB }

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

// Save validates BEFORE writing.
//
// A file could be written and then found invalid at the next start, which is
// bad enough. A database that the running daemon reloads from would serve the
// broken config immediately, so the check has to happen first — and on a COPY,
// because Finalize resolves extensions into models and storing the resolved
// form would duplicate every provided model as if it had been declared.
func (s *Source) Save(ctx context.Context, c *config.Config) error {
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
	return Write(ctx, s.DB, authored)
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
func (s *Source) ImportFile(ctx context.Context, path string) (*config.Config, error) {
	c, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if err := s.Save(ctx, c); err != nil {
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
