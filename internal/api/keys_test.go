package api

import (
	"context"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/store"
)

func boolp(b bool) *bool { return &b }

// The roster exists because an UNASSIGNED key was invisible: ResolveGroup falls
// back silently, so a key nobody had thought about looked exactly like one
// deliberately placed in the default lane. You could only manage keys you
// already knew about.
func TestKeysDistinguishesAssignedFromFellThrough(t *testing.T) {
	h := &Handlers{}
	h.SetConfig(&config.Config{
		Keys: map[string]string{"aw3": "interactive"},
		PriorityGroups: map[string]config.PriorityGroup{
			"interactive": {Weight: 10},
			"default":     {Weight: 1},
		},
	})
	out, err := h.Keys(context.Background(), &KeysInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Body.Keys) != 1 {
		t.Fatalf("keys = %+v, want the one configured key", out.Body.Keys)
	}
	k := out.Body.Keys[0]
	if !k.Recognized {
		t.Error("a key present in the config is recognized")
	}
	if k.Group != "interactive" || k.Weight != 10 {
		t.Errorf("got %s/%d, want interactive/10 — the WEIGHT is what schedules the caller", k.Group, k.Weight)
	}
	if k.Hash == "" || k.Hash == k.Key {
		t.Errorf("hash %q must be a stable identifier distinct from the key", k.Hash)
	}
	if k.Key != "aw3" {
		t.Errorf("key = %q; keys are not secret here and hiding them makes the roster useless", k.Key)
	}
}

// The policy for strangers was implicit and therefore unstateable: accept
// anyone at weight 1, expressed as a silent fallback rather than a decision.
func TestUnknownKeyPolicyIsExplicitAndDefaultsToAccepting(t *testing.T) {
	var zero config.UnknownKeyPolicy
	if !zero.Allowed() {
		t.Error("an omitted policy must keep accepting keys; flipping it by omission " +
			"would lock out every caller on an upgrade")
	}
	if zero.FallbackGroup() != "default" {
		t.Errorf("fallback = %q, want default", zero.FallbackGroup())
	}

	denied := config.UnknownKeyPolicy{Allow: boolp(false), Group: "quarantine"}
	if denied.Allowed() {
		t.Error("an explicit false must refuse")
	}
	if denied.FallbackGroup() != "quarantine" {
		t.Errorf("fallback = %q, want quarantine", denied.FallbackGroup())
	}

	// And the resolver reports recognition, which is what enrolment keys on.
	cfg := &config.Config{
		Keys:           map[string]string{"known": "batch"},
		PriorityGroups: map[string]config.PriorityGroup{"batch": {Weight: 1}},
		UnknownKeys:    config.UnknownKeyPolicy{Group: "quarantine"},
	}
	if _, _, ok := cfg.ResolveGroupRecognized("known"); !ok {
		t.Error("a configured key is recognized")
	}
	name, _, ok := cfg.ResolveGroupRecognized("stranger")
	if ok {
		t.Error("a key nobody assigned is NOT recognized")
	}
	if name != "quarantine" {
		t.Errorf("stranger landed in %q, want the configured fallback", name)
	}
}

// The roster surfaces what needs a decision: a stranger doing real traffic is
// the most urgent row on the page.
func TestKeysSortsUnrecognizedFirst(t *testing.T) {
	rows := []KeyRow{
		{Key: "assigned-busy", Recognized: true, Requests: 900},
		{Key: "stranger-quiet", Recognized: false, Requests: 1},
		{Key: "stranger-busy", Recognized: false, Requests: 500},
	}
	sortKeyRows(rows)
	if rows[0].Key != "stranger-busy" || rows[1].Key != "stranger-quiet" {
		t.Errorf("order = %v; unrecognized first, then busiest",
			[]string{rows[0].Key, rows[1].Key, rows[2].Key})
	}
}

// The roster's usage half must arrive INTACT. It shipped once with only the
// request count copied out of the rollup — cost, dwell and last-seen were on the
// wire as zeros, which reads as "this caller costs nothing and has never
// called" rather than as a missing field. A count-only assertion passed the
// whole time.
func TestKeysCopiesEveryRollupField(t *testing.T) {
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	for _, a := range []store.Activity{
		{TS: 1_700_000_000_000, Served: "m", Key: "aw3", Status: 200, CostUSD: 0.25, DwellMS: 40},
		{TS: 1_700_000_060_000, Served: "m", Key: "aw3", Status: 200, CostUSD: 0.25, DwellMS: 60},
	} {
		if err := st.InsertActivity(a); err != nil {
			t.Fatal(err)
		}
	}

	h := &Handlers{Store: st}
	h.SetConfig(&config.Config{
		PriorityGroups: map[string]config.PriorityGroup{"default": {Weight: 1}},
	})
	out, err := h.Keys(context.Background(), &KeysInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Body.Keys) != 1 {
		t.Fatalf("keys = %+v, want the one key seen in traffic", out.Body.Keys)
	}
	k := out.Body.Keys[0]
	if k.Requests != 2 || k.CostUSD != 0.5 || k.DwellMS != 100 {
		t.Errorf("usage = %d req / $%v / %dms, want 2 / 0.5 / 100", k.Requests, k.CostUSD, k.DwellMS)
	}
	// The NEWEST row, in RFC3339 — the question is "is this caller still
	// around", which a first-seen or a sum cannot answer.
	if k.LastSeen != "2023-11-14T22:14:20Z" {
		t.Errorf("lastSeen = %q, want the newest row's timestamp", k.LastSeen)
	}
}
