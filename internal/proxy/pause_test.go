package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
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
		Keys:           config.GroupKeys(map[string]string{"k": "g"}),
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

// TestAllMembersPaused429: a lane with nothing unpaused left is a 429 telling
// the caller to come back in five minutes — the pause's standing answer.
func TestAllMembersPaused429(t *testing.T) {
	r, mgr := pauseLaneFixture(t)
	for _, m := range []string{"m-1", "m-2"} {
		if _, err := mgr.PauseModel(m, "", time.Time{}); err != nil {
			t.Fatalf("Pause %s: %v", m, err)
		}
	}
	rec := sendToLane(t, r, "m")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d (%s)", rec.Code, rec.Body.String())
	}
	if ra := rec.Header().Get("Retry-After"); ra != "300" {
		t.Errorf("Retry-After = %q, want 300", ra)
	}
	if !strings.Contains(rec.Body.String(), `"type":"paused"`) || !strings.Contains(rec.Body.String(), `"retry_after":300`) {
		t.Errorf("body = %s, want a paused error carrying retry_after", rec.Body.String())
	}
}

// TestPausedModelDirect429: naming a paused model directly (no lane to fall
// through to) is answered immediately rather than queueing behind it.
func TestPausedModelDirect429(t *testing.T) {
	r, mgr := pauseLaneFixture(t)
	if _, err := mgr.PauseModel("m-1", "", time.Time{}); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	rec := sendToLane(t, r, "m-1")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d (%s)", rec.Code, rec.Body.String())
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

// postChat drives one inference request through the mounted router.
func postChat(t *testing.T, r *chi.Mux, model string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer k")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestPausedIndefiniteHasNoRetryAfter: the original behavior, and the case the
// "no retry interval would be honest" comment was written for. Nothing but a
// TestPausedIndefiniteRetriesInFiveMinutes: nothing says when an indefinite
// pause lifts, so the caller is told five minutes — and told it again on every
// request until it does.
func TestPausedIndefiniteRetriesInFiveMinutes(t *testing.T) {
	r, mgr := pauseLaneFixture(t)
	for _, m := range []string{"m-1", "m-2"} {
		if _, err := mgr.PauseModel(m, "", time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		w := postChat(t, r, "m")
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("request %d: status = %d, want 429", i, w.Code)
		}
		if ra := w.Header().Get("Retry-After"); ra != "300" {
			t.Errorf("request %d: Retry-After = %q, want 300", i, ra)
		}
	}
}

// TestPausedTimedCapsAtFiveMinutes: a timed pause lifting an hour from now
// still says five minutes — the caller keeps checking in rather than going
// quiet for the whole hour.
func TestPausedTimedCapsAtFiveMinutes(t *testing.T) {
	r, mgr := pauseLaneFixture(t)
	resume := time.Now().Add(time.Hour)
	for _, m := range []string{"m-1", "m-2"} {
		if _, err := mgr.PauseModel(m, "gpu needed elsewhere", resume); err != nil {
			t.Fatal(err)
		}
	}
	w := postChat(t, r, "m")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	secs, err := strconv.Atoi(w.Header().Get("Retry-After"))
	if err != nil {
		t.Fatalf("Retry-After = %q, want an integer second count: %v", w.Header().Get("Retry-After"), err)
	}
	if secs != 300 {
		t.Errorf("Retry-After = %ds, want 300 (capped; the pause has an hour left)", secs)
	}
}

// TestPausedLaneUsesSoonestResume: service returns when ANY member does, so a
// lane answers with the soonest resume, not the last one looked at.
func TestPausedLaneUsesSoonestResume(t *testing.T) {
	r, mgr := pauseLaneFixture(t)
	if _, err := mgr.PauseModel("m-1", "", time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.PauseModel("m-2", "", time.Now().Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	w := postChat(t, r, "m")
	secs, err := strconv.Atoi(w.Header().Get("Retry-After"))
	if err != nil {
		t.Fatalf("Retry-After = %q: %v", w.Header().Get("Retry-After"), err)
	}
	if secs < 170 || secs > 180 {
		t.Errorf("Retry-After = %ds, want ~180 — the SOONEST member, under the 5 minute cap", secs)
	}
}

// TestPausedMixedIndefiniteAndTimed: an indefinitely-paused member has no
// resume time to contribute, but it must not veto a sibling that does — the
// lane still comes back when the timed one lifts.
func TestPausedMixedIndefiniteAndTimed(t *testing.T) {
	r, mgr := pauseLaneFixture(t)
	if _, err := mgr.PauseModel("m-1", "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.PauseModel("m-2", "", time.Now().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	w := postChat(t, r, "m")
	secs, err := strconv.Atoi(w.Header().Get("Retry-After"))
	if err != nil {
		t.Fatalf("Retry-After = %q, want the timed sibling's interval: %v", w.Header().Get("Retry-After"), err)
	}
	if secs < 110 || secs > 120 {
		t.Errorf("Retry-After = %ds, want ~120", secs)
	}
}
