package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// pauseLaneFixture builds a two-member lane "m" over two live upstreams, with
// the manager sharing the proxy's config (so it can resolve a model by name).
func pauseLaneFixture(t *testing.T) (*chi.Mux, *proc.Manager) {
	t.Helper()
	reply := func(body string) *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" {
				return
			}
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(s.Close)
		return s
	}
	up1, up2 := reply(`{"served_by":"up1"}`), reply(`{"served_by":"up2"}`)

	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := &config.Config{
		Models: map[string]config.Model{
			"m-1": modelTo(t, up1.URL, "local"),
			"m-2": modelTo(t, up2.URL, "cloud"),
		},
		Lanes: map[string]config.Lane{
			"m": {Members: []config.LaneMember{{Model: "m-1"}, {Model: "m-2"}}},
		},
		PriorityGroups: map[string]config.PriorityGroup{"g": {Weight: 1}},
		Keys:           map[string]string{"k": "g"},
	}
	mgr := proc.NewManager(cfg)
	t.Cleanup(mgr.Shutdown)

	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)
	return r, mgr
}

func sendToLane(t *testing.T, r *chi.Mux, model string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`"}`))
	req.Header.Set("X-Corrallm-Key", "k")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestPausedMemberFallsThrough: pausing a lane's first member routes to the
// second rather than failing — a pause is a per-model decision, and the lane's
// job is to keep serving from what is left.
func TestPausedMemberFallsThrough(t *testing.T) {
	r, mgr := pauseLaneFixture(t)

	if rec := sendToLane(t, r, "m"); !strings.Contains(rec.Body.String(), `"served_by":"up1"`) {
		t.Fatalf("precondition: lane should start on m-1, got %d (%s)", rec.Code, rec.Body.String())
	}

	if _, err := mgr.PauseModel("m-1", "test", time.Time{}); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	rec := sendToLane(t, r, "m")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"served_by":"up2"`) {
		t.Errorf("expected fall-through to m-2, got: %s", rec.Body.String())
	}
}

// TestAllMembersPaused503: a lane with nothing unpaused left is a 503, not a
// 429. There is no retry interval that would be honest about an operator
// decision, so telling the client to come back in N seconds would be a lie.
func TestAllMembersPaused503(t *testing.T) {
	r, mgr := pauseLaneFixture(t)
	for _, m := range []string{"m-1", "m-2"} {
		if _, err := mgr.PauseModel(m, "", time.Time{}); err != nil {
			t.Fatalf("Pause %s: %v", m, err)
		}
	}
	rec := sendToLane(t, r, "m")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d (%s)", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") != "" {
		t.Error("a pause must not advertise a Retry-After")
	}
}

// TestPausedModelDirect503: naming a paused model directly (no lane to fall
// through to) fails immediately rather than queueing behind it.
func TestPausedModelDirect503(t *testing.T) {
	r, mgr := pauseLaneFixture(t)
	if _, err := mgr.PauseModel("m-1", "", time.Time{}); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	rec := sendToLane(t, r, "m-1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestCatalogReportsPaused: /v1/models still LISTS a paused model (it is not
// deleted) but reports it as paused rather than absent, which would say a
// request could load it.
func TestCatalogReportsPaused(t *testing.T) {
	r, mgr := pauseLaneFixture(t)
	if _, err := mgr.PauseModel("m-1", "", time.Time{}); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var got struct {
		Data []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, e := range got.Data {
		if e.ID != "m-1" {
			continue
		}
		seen = true
		if e.State != "paused" {
			t.Errorf("m-1 state = %q, want paused", e.State)
		}
	}
	if !seen {
		t.Error("a paused model must stay listed in the catalog")
	}
}

// TestResumeRestoresRouting: the lane goes back to its top member once the
// pause lifts.
func TestResumeRestoresRouting(t *testing.T) {
	r, mgr := pauseLaneFixture(t)
	if _, err := mgr.PauseModel("m-1", "", time.Time{}); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if _, err := mgr.UnpauseModel(context.Background(), "m-1"); err != nil {
		t.Fatalf("Unpause: %v", err)
	}
	rec := sendToLane(t, r, "m")
	if !strings.Contains(rec.Body.String(), `"served_by":"up1"`) {
		t.Errorf("expected m-1 back in service, got %d (%s)", rec.Code, rec.Body.String())
	}
}
