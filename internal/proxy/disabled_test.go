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

// disabledLaneFixture is pauseLaneFixture's shape with the lane's TOP rung
// disabled in config — which is how the operator's case actually looks.
func disabledLaneFixture(t *testing.T) *chi.Mux {
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

	dead := modelTo(t, up1.URL, "local")
	dead.Disabled = true
	cfg := &config.Config{
		Models: map[string]config.Model{
			"m-1": dead,
			"m-2": modelTo(t, up2.URL, "cloud"),
		},
		Lanes: map[string]config.Lane{
			"m": {Members: []config.LaneMember{{Model: "m-1"}, {Model: "m-2"}}},
		},
		PriorityGroups: map[string]config.PriorityGroup{"g": {Weight: 1}},
		Keys:           config.GroupKeys(map[string]string{"k": "g"}),
	}
	mgr := proc.NewManager(cfg)
	t.Cleanup(mgr.Shutdown)

	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)
	return r
}

// A lane falls through a disabled rung exactly as if it had never been written
// down — no admission slot taken on it, no spill to explain, no 503.
func TestDisabledMemberIsNotEvenTried(t *testing.T) {
	r := disabledLaneFixture(t)
	rec := sendToLane(t, r, "m")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"served_by":"up2"`) {
		t.Errorf("lane did not fall past its disabled top rung: %s", rec.Body.String())
	}
}

// Named directly, it is a 404 that says WHICH kind of nothing this is.
//
// 404 and not the pause's 503: a 503 says the thing exists and may come back,
// which invites a retry, and a disabled model comes back when somebody edits
// the config and not before. The word "disabled" is the load-bearing part —
// "unknown model m-1" would send an operator hunting for a typo in a name that
// is spelled correctly and sitting in their config.
func TestDisabledModelNamedDirectlyIs404AndSaysSo(t *testing.T) {
	r := disabledLaneFixture(t)
	rec := sendToLane(t, r, "m-1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "disabled") {
		t.Errorf("body = %q, want it to say the model is disabled", body)
	}
	if strings.Contains(body, "unknown model") {
		t.Errorf("body = %q — a name in the config reported as unknown", body)
	}

	// A name that really is unknown still says so.
	rec = sendToLane(t, r, "no-such-thing")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "unknown model") {
		t.Errorf("unknown model = %d %q", rec.Code, rec.Body.String())
	}
}

// /v1/models is a catalogue clients pick from. A disabled model listed there
// would hand out a name that 404s on the next call — the opposite of a pause,
// which stays listed in state "paused" because it is coming back.
func TestDisabledModelIsNotListed(t *testing.T) {
	r := disabledLaneFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("X-Corrallm-Key", "k")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, `"m-1"`) {
		t.Errorf("/v1/models advertised a disabled model: %s", body)
	}
	if !strings.Contains(body, `"m-2"`) {
		t.Errorf("/v1/models lost a live model: %s", body)
	}
}

// /upstream bypasses resolution entirely, so it needs its own guard — without
// one the manager's refusal surfaces as "backend unavailable", a 503 about
// capacity for something that is a configuration decision.
func TestUpstreamRefusesADisabledModel(t *testing.T) {
	r := disabledLaneFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/upstream/m-1/props", nil)
	req.Header.Set("X-Corrallm-Key", "k")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "disabled") {
		t.Errorf("body = %q, want it to say the model is disabled", rec.Body.String())
	}
}
