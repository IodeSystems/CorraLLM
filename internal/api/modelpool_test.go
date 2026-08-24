package api

import (
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

// Saving a model from the form must not drop its `pool:`.
//
// applySpec is a PATCH by design — it overlays only the fields the form models,
// so everything else survives. That property is what makes this safe, and it is
// worth pinning rather than assuming: this codebase has already shipped three
// separate handlers that wrote to the wrong copy of a model, and the delete one
// reported success while changing nothing.
//
// The consequence if it regressed is quiet. `pool:` is the only statement of
// which card a model is on; losing it on an unrelated form save would silently
// move the model's accounting to the server-wide default pool, charging a model
// running on gpu1 to gpu0.
func TestApplySpecKeepsPool(t *testing.T) {
	prev := config.Model{
		Cmd: "llama-server -ngl 99", Server: "box1",
		Pool:     "gpu1",
		RAMUsage: map[string]string{"gpu1": "8GB"},
	}
	// What the form sends back: it models neither pool nor placements.
	next := config.Model{
		Cmd: "llama-server -ngl 99 --parallel 2", Server: "box1",
		RAMUsage: map[string]string{"gpu1": "8GB"},
	}
	got := applySpec(prev, next)
	if got.Pool != "gpu1" {
		t.Fatalf("pool = %q, want gpu1 — a form save must not move a model to another card", got.Pool)
	}
	if got.Cmd != next.Cmd {
		t.Errorf("cmd = %q, want the edited value", got.Cmd)
	}
}

// The form edits ramUsage (size) and not pool (placement). If it did not SAY so,
// a form showing a ramUsage row per pool would read as covering placement too —
// which is precisely the conflation the two fields were split to end.
func TestAdvancedFieldsNamesPool(t *testing.T) {
	got := advancedFields(config.Model{Server: "box1", Pool: "gpu1"})
	if !contains(got, "pool") {
		t.Errorf("advanced = %v, want it to name pool", got)
	}
}

// A model with no pool is the single-GPU case, and listing "pool" there would
// send an operator looking for a field that is not set.
func TestAdvancedFieldsOmitsAnUnsetPool(t *testing.T) {
	got := advancedFields(config.Model{Server: "box1"})
	if contains(got, "pool") {
		t.Errorf("advanced = %v, want no pool entry when none is declared", got)
	}
}

// A model served two ways is the case the form is least able to represent, so
// staying silent about it is the worst available answer.
func TestAdvancedFieldsNamesPlacements(t *testing.T) {
	got := advancedFields(config.Model{Placements: []config.Placement{
		{Name: "a", Server: "box1", Cmd: "x"},
	}})
	if !contains(got, "placements") {
		t.Errorf("advanced = %v, want it to name placements", got)
	}
}

func contains(all []string, want string) bool {
	for _, s := range all {
		if s == want {
			return true
		}
	}
	return false
}
