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

// laneRig serves one model, reachable both by its own name and through a lane.
func laneRig(t *testing.T) (*chi.Mux, *store.Store) {
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

	// `pub` is a free-tier backend that is NOT private, so a request marked
	// sensitive filters it out and leaves the `pub` lane with no candidate —
	// the cheapest way to reach a logged, unserved request.
	pub := modelTo(t, up.URL, "local")
	pub.FreeTier = &config.FreeTier{Provider: "somebody", Private: false}

	cfg := &config.Config{
		Models: map[string]config.Model{"m": modelTo(t, up.URL, "local"), "pub": pub},
		Lanes: map[string]config.Lane{
			"chat": {Members: []config.LaneMember{{Model: "m"}}},
			"pub":  {Members: []config.LaneMember{{Model: "pub"}}},
		},
		PriorityGroups: map[string]config.PriorityGroup{"default": {Weight: 1}},
		Keys:           map[string]config.KeyPolicy{"yscr": {Group: "default"}},
	}
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)
	return r, st
}

func askFor(t *testing.T, r *chi.Mux, model string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`"}`))
	req.Header.Set("X-Corrallm-Key", "yscr")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func onlyRow(t *testing.T, st *store.Store) store.Activity {
	t.Helper()
	rows, err := st.RecentActivity(store.Window{}, 10, "", "", "")
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 activity row, got %d", len(rows))
	}
	return rows[0]
}

// A lane-addressed request must record the model that ANSWERED, not the lane.
//
// It used to record the lane for both, which cost the one caller using lanes
// its whole history: 4,249 rows on the live box said the model was `chat` and
// carried 19.2M prompt tokens belonging to no model, while Prometheus had the
// real name all along. Two accounts of one request that disagreed.
func TestLaneRequestIsAttributedToTheModelThatAnswered(t *testing.T) {
	r, st := laneRig(t)
	if rec := askFor(t, r, "chat"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	row := onlyRow(t, st)
	if row.Served != "m" {
		t.Errorf("served = %q, want the model that answered (m)", row.Served)
	}
	if row.Requested != "chat" {
		t.Errorf("requested = %q, want the lane the caller wrote (chat)", row.Requested)
	}
}

// And the pinned case keeps both, equal — so "did anyone use the lane" is a
// question the column can answer without parsing it back out of the payload,
// which is absent on any row whose capture was off or truncated.
func TestPinnedRequestRecordsTheSameNameTwice(t *testing.T) {
	r, st := laneRig(t)
	if rec := askFor(t, r, "m"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	row := onlyRow(t, st)
	if row.Served != "m" || row.Requested != "m" {
		t.Errorf("served/requested = %q/%q, want m/m", row.Served, row.Requested)
	}
}

// A request nobody served has no serving model. Served repeats the requested
// name rather than naming a candidate that refused: a backend that rejected a
// request did not serve it, and putting it here would credit a model with work
// it declined to do.
func TestUnservedRequestNamesNoModel(t *testing.T) {
	r, st := laneRig(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"pub"}`))
	req.Header.Set("X-Corrallm-Key", "yscr")
	req.Header.Set("X-Corrallm-Sensitive", "true")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 once the only candidate is filtered out", rec.Code)
	}
	row := onlyRow(t, st)
	if row.Served != "pub" || row.Requested != "pub" {
		t.Errorf("unserved row = served %q / requested %q, want pub/pub", row.Served, row.Requested)
	}
}
