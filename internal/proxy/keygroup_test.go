package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// escalationRig serves one model and records what each request resolved to.
func escalationRig(t *testing.T) (*chi.Mux, *store.Store) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(up.Close)

	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr := proc.NewManager(&config.Config{})
	t.Cleanup(mgr.Shutdown)

	cfg := &config.Config{
		Models: map[string]config.Model{"m": modelTo(t, up.URL, "local")},
		PriorityGroups: map[string]config.PriorityGroup{
			"interactive": {Weight: 10},
			"batch":       {Weight: 1},
			"default":     {Weight: 1},
		},
		Keys: map[string]config.KeyPolicy{
			"yscr":   {Group: "default"},
			"sk-aw4": {Group: "batch", Allow: map[string]bool{"interactive": true}},
		},
	}
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)
	return r, st
}

func sendAs(t *testing.T, r *chi.Mux, cred string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m"}`))
	req.Header.Set("X-Corrallm-Key", cred)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// A permitted key gets the weighting it asks for, and the activity row still
// attributes to the base identity — otherwise one tenant's spend splits across
// a row per group and "what did aw4 cost" answers with a fraction of it.
func TestEscalatedRequestIsServedAndAttributedToTheBaseKey(t *testing.T) {
	r, st := escalationRig(t)
	if rec := sendAs(t, r, "sk-aw4:interactive"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec := sendAs(t, r, "sk-aw4"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	rows, err := st.RollupByKey(0)
	if err != nil {
		t.Fatalf("rollup: %v", err)
	}
	for _, row := range rows {
		if strings.Contains(row.Key, config.GroupSeparator) {
			t.Errorf("activity key %q carries the group suffix; cost would split per group", row.Key)
		}
	}
	if len(rows) != 1 || rows[0].Key != "sk-aw4" {
		t.Errorf("rollup = %+v, want one row for sk-aw4", rows)
	}
}

// A key that was never granted the group is served anyway — refusing a request
// over a weighting is worse than serving it at the weight it is entitled to —
// but the refusal is announced rather than silent.
func TestUnpermittedEscalationIsServedAndAnnounced(t *testing.T) {
	r, _ := escalationRig(t)
	rec := sendAs(t, r, "yscr:interactive")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a denied escalation still serves", rec.Code)
	}
	if got := rec.Header().Get(HeaderGroupDenied); got != "interactive" {
		t.Errorf("%s = %q, want interactive — a silent downgrade is the failure this prevents",
			HeaderGroupDenied, got)
	}
}

// The header must be ABSENT when nothing was refused, or its presence means
// nothing.
func TestNoDeniedHeaderOnAnOrdinaryRequest(t *testing.T) {
	r, _ := escalationRig(t)
	for _, cred := range []string{"sk-aw4", "sk-aw4:interactive", "yscr"} {
		rec := sendAs(t, r, cred)
		if got := rec.Header().Get(HeaderGroupDenied); got != "" {
			t.Errorf("%s = %q for %q, want absent", HeaderGroupDenied, got, cred)
		}
	}
}
