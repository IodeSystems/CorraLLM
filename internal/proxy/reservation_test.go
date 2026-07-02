package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

func newReservationRouter(t *testing.T) chi.Router {
	t.Helper()
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := mkConfig(t, "mock", "http://127.0.0.1:1")
	mgr := proc.NewManager(cfg)
	t.Cleanup(mgr.Shutdown)
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)
	return r
}

func doJSON(t *testing.T, r chi.Router, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestReservationCreateListRelease(t *testing.T) {
	r := newReservationRouter(t)

	code, out := doJSON(t, r, http.MethodPost, "/v1/reservations", `{"model":"mock"}`)
	if code != http.StatusOK {
		t.Fatalf("create status=%d body=%v", code, out)
	}
	if out["lane"] != "default" || out["model"] != "mock" {
		t.Errorf("unexpected create body: %v", out)
	}
	if _, ok := out["expires_at"].(string); !ok {
		t.Errorf("missing expires_at: %v", out)
	}

	code, out = doJSON(t, r, http.MethodGet, "/v1/reservations", "")
	if code != http.StatusOK {
		t.Fatalf("list status=%d", code)
	}
	list, _ := out["reservations"].([]any)
	if len(list) != 1 {
		t.Fatalf("want 1 reservation, got %v", out)
	}
	if m := list[0].(map[string]any); m["model"] != "mock" || m["lane"] != "default" {
		t.Errorf("unexpected list entry: %v", m)
	}

	code, out = doJSON(t, r, http.MethodDelete, "/v1/reservations?model=mock", "")
	if code != http.StatusOK || out["released"] != true {
		t.Fatalf("release status=%d body=%v", code, out)
	}

	_, out = doJSON(t, r, http.MethodGet, "/v1/reservations", "")
	if list, _ := out["reservations"].([]any); len(list) != 0 {
		t.Errorf("reservation should be gone after release: %v", out)
	}
}

func TestReservationUnknownModel404(t *testing.T) {
	r := newReservationRouter(t)
	code, _ := doJSON(t, r, http.MethodPost, "/v1/reservations", `{"model":"nope"}`)
	if code != http.StatusNotFound {
		t.Fatalf("want 404 for unknown model, got %d", code)
	}
}

func TestReservationSlotsExceedCapacity(t *testing.T) {
	r := newReservationRouter(t)
	// mkConfig backend has no MaxConcurrent → capacity 1; asking for 2 is invalid.
	code, out := doJSON(t, r, http.MethodPost, "/v1/reservations", `{"model":"mock","slots":2}`)
	if code != http.StatusBadRequest {
		t.Fatalf("want 400 for slots>capacity, got %d body=%v", code, out)
	}
}

func TestReservationDeleteRequiresModel(t *testing.T) {
	r := newReservationRouter(t)
	code, _ := doJSON(t, r, http.MethodDelete, "/v1/reservations", "")
	if code != http.StatusBadRequest {
		t.Fatalf("want 400 without ?model=, got %d", code)
	}
}
