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

// counts reads the two tables the in-flight design touches: finished rows and
// standing markers.
func counts(t *testing.T, st *store.Store) (rows, markers int) {
	t.Helper()
	all, err := st.RecentActivity(store.Window{}, 100, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.InflightCount()
	if err != nil {
		t.Fatal(err)
	}
	return len(all), m
}

// A request that is being served is a marker and NOT an activity row, so no
// reader of activity sees it half-written; when it ends there is one row and
// no marker.
func TestInflight_SuccessLeavesOneRowAndNoMarker(t *testing.T) {
	treatAsRemote(t)
	var st *store.Store
	var rowsDuring, markersDuring int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rowsDuring, markersDuring = counts(t, st)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(up.Close)
	var h http.Handler
	h, st = refusalRig(t, map[string]config.Model{"m": modelTo(t, up.URL, "chat")}, nil)

	if rec := post(h, "m"); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if rowsDuring != 0 || markersDuring != 1 {
		t.Errorf("during the request: %d rows, %d markers; want 0 and 1", rowsDuring, markersDuring)
	}
	if rows, markers := counts(t, st); rows != 1 || markers != 0 {
		t.Errorf("after: %d rows, %d markers; want 1 and 0", rows, markers)
	}
}

// A spill to the next candidate: the refusing candidate's row plus the
// winner's, and no marker left behind by either.
func TestInflight_UpstreamSpillLeavesTwoRows(t *testing.T) {
	treatAsRemote(t)
	shed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(shed.Close)
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(ok.Close)
	a, b := modelTo(t, shed.URL, "chat"), modelTo(t, ok.URL, "chat")
	a.Quality, b.Quality = 1, 1
	h, st := refusalRig(t, map[string]config.Model{"a": a, "b": b},
		map[string]config.Lane{"chat": {Members: []config.LaneMember{{Model: "a"}, {Model: "b"}}}})

	if rec := post(h, "chat"); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if rows, markers := counts(t, st); rows != 2 || markers != 0 {
		t.Errorf("%d rows, %d markers; want 2 and 0", rows, markers)
	}
}

// A free-tier hard failure (401/402/403) spills WITHOUT a row of its own, as it
// always did; the marker it took must not outlive it.
func TestInflight_HardFailSpillLeavesOneRow(t *testing.T) {
	treatAsRemote(t)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(bad.Close)
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(ok.Close)
	a, b := modelTo(t, bad.URL, "chat"), modelTo(t, ok.URL, "chat")
	a.Quality, b.Quality = 1, 1
	a.FreeTier = &config.FreeTier{}
	h, st := refusalRig(t, map[string]config.Model{"a": a, "b": b},
		map[string]config.Lane{"chat": {Members: []config.LaneMember{{Model: "a"}, {Model: "b"}}}})

	if rec := post(h, "chat"); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if rows, markers := counts(t, st); rows != 1 || markers != 0 {
		t.Errorf("%d rows, %d markers; want 1 and 0", rows, markers)
	}
}

// Our own 429 is decided before the backend is reached: one row, and it never
// takes a marker. The request holding the slot is a marker until it ends.
func TestInflight_OwnRejectionLeavesOneRow(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		close(started)
		<-release
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()

	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()
	r := chi.NewRouter()
	New(mkConfig(t, "mock", up.URL), mgr, sched.New(), st).Mount(r)

	done := make(chan struct{})
	go func() {
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"mock"}`)))
		close(done)
	}()
	<-started
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mock"}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d", rec.Code)
	}
	if rows, markers := counts(t, st); rows != 1 || markers != 1 {
		t.Errorf("while the first is served: %d rows, %d markers; want the 429 row and the first's marker", rows, markers)
	}
	close(release)
	<-done
	if rows, markers := counts(t, st); rows != 2 || markers != 0 {
		t.Errorf("after: %d rows, %d markers; want 2 and 0", rows, markers)
	}
}
