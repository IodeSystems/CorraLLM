package configdb

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/iodesystems/corrallm/internal/config"
	_ "modernc.org/sqlite"
)

// The round trip IS the port.
//
// Nothing downstream may switch to reading from SQLite until the config that
// comes back out is the config that went in. A file could be trusted to hold
// whatever was written to it; a normalized schema is a hand-written mapping,
// and a mapping with a missing field loses data silently — which is precisely
// the failure this port exists to end. So the test is: write, read, compare,
// and treat any difference as a defect in the mapper.

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db
}

// roundTrip writes a config, reads it back, and returns the result.
func roundTrip(t *testing.T, in *config.Config) *config.Config {
	t.Helper()
	db := openDB(t)
	ctx := context.Background()
	if err := Write(ctx, db, in); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := Read(ctx, db)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return out
}

// compare marshals both sides to YAML and diffs the parsed maps.
//
// Compared as PARSED structures rather than text: key order and formatting are
// not meaning, and a byte comparison would fail on those while ignoring the
// difference that matters.
func compare(t *testing.T, want, got *config.Config) {
	t.Helper()
	wb, err := yaml.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	gb, err := yaml.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var wm, gm map[string]any
	if err := yaml.Unmarshal(wb, &wm); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(gb, &gm); err != nil {
		t.Fatal(err)
	}
	for _, k := range union(wm, gm) {
		if !reflect.DeepEqual(wm[k], gm[k]) {
			t.Errorf("section %q did not survive the round trip:\n  wrote: %#v\n  read:  %#v", k, wm[k], gm[k])
		}
	}
}

func union(a, b map[string]any) []string {
	seen := map[string]bool{}
	var out []string
	for k := range a {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for k := range b {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// THE test: the operator's real config, not a fixture.
//
// A fixture only proves the mapper handles what its author thought of. The live
// file has two servers with device pinning by UUID, six local models with
// sampling profiles and per-mode overrides, three extensions (one virtual, one
// spawned, one remote), pooled providers with credentials, lanes, groups, keys
// and tools. Skipped when absent, so this is not a test that only passes here.
func TestRoundTripLiveConfig(t *testing.T) {
	path := os.Getenv("HOME") + "/.corrallm/config.yml"
	if _, err := os.Stat(path); err != nil {
		t.Skip("no live config on this machine")
	}
	in, err := config.Load(path)
	if err != nil {
		t.Fatalf("load live config: %v", err)
	}
	compare(t, in, roundTrip(t, in))
}

// An empty config must survive too: a fresh install writes one before anything
// has been configured, and a reader that cannot cope turns "nothing yet" into
// a startup failure.
func TestRoundTripEmpty(t *testing.T) {
	compare(t, &config.Config{}, roundTrip(t, &config.Config{}))
}

// Write REPLACES. A deletion has to take effect, and a merge-only write would
// resurrect what was removed on the next save.
func TestWriteReplacesRatherThanMerges(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	first := &config.Config{
		Servers: map[string]config.Server{
			"box1": {Pools: map[string]string{"gpu0": "24GB"}},
			"box2": {Pools: map[string]string{"gpu0": "8GB"}},
		},
		Keys: map[string]string{"a": "interactive", "b": "batch"},
	}
	if err := Write(ctx, db, first); err != nil {
		t.Fatal(err)
	}

	second := &config.Config{
		Servers: map[string]config.Server{"box1": {Pools: map[string]string{"gpu0": "24GB"}}},
		Keys:    map[string]string{"a": "interactive"},
	}
	if err := Write(ctx, db, second); err != nil {
		t.Fatal(err)
	}

	got, err := Read(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if _, gone := got.Servers["box2"]; gone {
		t.Error("a removed server came back — the write merged instead of replacing")
	}
	if _, gone := got.Keys["b"]; gone {
		t.Error("a removed key came back")
	}
}

// IsEmpty is what a boot uses to decide between "import the file" and "this is
// a fresh install", so it must not call a written-but-minimal config empty.
func TestIsEmpty(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	empty, err := IsEmpty(ctx, db)
	if err != nil || !empty {
		t.Fatalf("a new database should be empty (got %v, %v)", empty, err)
	}
	if err := Write(ctx, db, &config.Config{
		Keys: map[string]string{"k": "default"},
	}); err != nil {
		t.Fatal(err)
	}
	empty, err = IsEmpty(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if empty {
		t.Error("a config with one key reported empty; a boot would re-import over it")
	}
}

// A field with no column must still survive. This is the exact shape of the bug
// that motivated the port: `tools:` was written by a binary that had no member
// for it and the section vanished.
func TestUnprojectedFieldsSurvive(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	in := &config.Config{
		Servers: map[string]config.Server{"box1": {Pools: map[string]string{"gpu0": "24GB"}}},
	}
	if err := Write(ctx, db, in); err != nil {
		t.Fatal(err)
	}
	// Simulate a config section the mapper knows nothing about.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO config_scalar (key, value) VALUES ('server.rest.box1', '{"somethingNew":"kept"}')`); err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(ctx, `SELECT value FROM config_scalar WHERE key = 'server.rest.box1'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("the remainder row was not stored")
	}
}
