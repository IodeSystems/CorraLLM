package configdb

import (
	"context"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

// The field parsed and marshalled correctly and still vanished, because the
// key table had no column for it — the same shape of bug as a write landing in
// a derived view. A config setting that reverts itself at the next save is
// worse than one that was never accepted, so the round trip is the test.
func TestKeyThinkingSurvivesTheDatabase(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	no, yes := false, true
	in := &config.Config{
		PriorityGroups: map[string]config.PriorityGroup{"batch": {Weight: 1}, "default": {Weight: 5}},
		Keys: map[string]config.KeyPolicy{
			"agent":  {Group: "batch", Thinking: &no},
			"person": {Group: "default", Thinking: &yes},
			"plain":  {Group: "default"},
		},
	}
	if err := Write(ctx, db, in); err != nil {
		t.Fatal(err)
	}
	out, err := Read(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if th, set := out.Keys["agent"].ThinkingPreference(); !set || th {
		t.Errorf("agent = %v/%v, want an explicit false", th, set)
	}
	if th, set := out.Keys["person"].ThinkingPreference(); !set || !th {
		t.Errorf("person = %v/%v, want an explicit true", th, set)
	}
	// Unset must come back UNSET, not false — otherwise every key that never
	// had an opinion acquires one on the first save.
	if _, set := out.Keys["plain"].ThinkingPreference(); set {
		t.Error("a key with no preference gained one through the database")
	}
}
