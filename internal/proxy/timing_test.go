package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// The headers are the contract a benchmark relies on to subtract corrallm's own
// overhead from its wall clock. If they stop arriving, a probe's timing quietly
// starts describing queue depth instead of the model.
func TestTimingHeadersReachTheClient(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","choices":[{"message":{"content":"hi"}}]}`))
	}))
	defer upstream.Close()

	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	cfg := mkConfig(t, "mock", upstream.URL)
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mock","messages":[{"role":"user","content":"x"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// Present even at zero: absence has to mean "corrallm too old to report",
	// which it cannot if a zero queue also omits the header.
	for _, h := range []string{HeaderQueuedMS, HeaderLoadMS, HeaderTTFBMS} {
		v := rec.Header().Get(h)
		if v == "" {
			t.Errorf("%s missing", h)
			continue
		}
		if _, err := strconv.ParseInt(v, 10, 64); err != nil {
			t.Errorf("%s = %q, not an integer: %v", h, v, err)
		}
	}
	// An idle proxy admits immediately and the backend is a live httptest
	// server, so neither overhead should register.
	if got := rec.Header().Get(HeaderQueuedMS); got != "0" {
		t.Errorf("%s = %q on an idle proxy, want 0", HeaderQueuedMS, got)
	}
}

func TestSetTimingHeadersFormatsIntegers(t *testing.T) {
	h := http.Header{}
	setTimingHeaders(h, 1200, 34000, 87)
	for header, want := range map[string]string{
		HeaderQueuedMS: "1200",
		HeaderLoadMS:   "34000",
		HeaderTTFBMS:   "87",
	} {
		if got := h.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}
