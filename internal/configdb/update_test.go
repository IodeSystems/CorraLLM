package configdb

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

// Concurrent edits must not lose one another.
//
// This is the bug Update exists for. Editing was read-modify-write with the
// read done separately from the write: the API took the in-memory config,
// copied it, applied the change and saved. Two edits therefore both started
// from the same base, and the second silently discarded the first — a model
// added in one browser tab vanishing when another tab renamed a lane.
//
// Twenty concurrent edits, each adding a distinct key. All twenty must survive.
func TestConcurrentUpdatesDoNotLoseEachOther(t *testing.T) {
	ctx := context.Background()
	src := &Source{DB: openDB(t)}
	if err := src.WithNote("base").Save(ctx, &config.Config{
		PriorityGroups: map[string]config.PriorityGroup{"default": {Weight: 1}},
	}); err != nil {
		t.Fatal(err)
	}

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("k%02d", i)
			_, errs[i] = src.WithNote("add "+key).Update(ctx, func(c *config.Config) error {
				if c.Keys == nil {
					c.Keys = map[string]string{}
				}
				c.Keys[key] = "default"
				return nil
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("edit %d failed: %v", i, err)
		}
	}

	got, err := src.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var missing []string
	for i := range n {
		k := fmt.Sprintf("k%02d", i)
		if _, ok := got.Keys[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d of %d concurrent edits were lost: %v", len(missing), n, missing)
	}
}

// An edit that fails validation must leave the stored config untouched. The
// write and the revision are in one transaction, so a rejected edit rolls both
// back rather than recording a state that never existed.
func TestRejectedUpdateChangesNothing(t *testing.T) {
	ctx := context.Background()
	src := &Source{DB: openDB(t)}
	if err := src.WithNote("base").Save(ctx, &config.Config{
		PriorityGroups: map[string]config.PriorityGroup{"default": {Weight: 1}},
		Keys:           map[string]string{"good": "default"},
	}); err != nil {
		t.Fatal(err)
	}
	revsBefore, _ := Revisions(ctx, src.DB, 50)

	_, err := src.WithNote("bad").Update(ctx, func(c *config.Config) error {
		c.Keys["bad"] = "no-such-group" // fails validation
		return nil
	})
	if err == nil {
		t.Fatal("an invalid edit was accepted")
	}

	got, err := src.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Keys["bad"]; ok {
		t.Error("the rejected edit was stored anyway")
	}
	revsAfter, _ := Revisions(ctx, src.DB, 50)
	if len(revsAfter) != len(revsBefore) {
		t.Errorf("a rejected edit recorded a revision (%d -> %d); history would describe a state that never existed",
			len(revsBefore), len(revsAfter))
	}
}

// An error from the caller's function aborts cleanly — it is how a handler
// rejects a request ("no such model"), not a storage failure.
func TestUpdatePropagatesCallerErrors(t *testing.T) {
	ctx := context.Background()
	src := &Source{DB: openDB(t)}
	if err := src.Save(ctx, &config.Config{Keys: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	want := fmt.Errorf("no such model")
	_, err := src.Update(ctx, func(*config.Config) error { return want })
	if err == nil || err.Error() != want.Error() {
		t.Errorf("got %v, want the caller's error unchanged", err)
	}
}
