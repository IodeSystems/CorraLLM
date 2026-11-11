package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

func TestSessionAffinityValueIsStableAndHidesTheCaller(t *testing.T) {
	a := sessionAffinityValue("conv-1")
	if a != sessionAffinityValue("conv-1") {
		t.Fatal("not stable")
	}
	if a == sessionAffinityValue("conv-2") {
		t.Fatal("two conversations share a value")
	}
	if len(a) != 32 || strings.Contains(a, "conv-1") {
		t.Fatalf("bad value %q", a)
	}
}

// seen records the affinity header each fake upstream received.
type seen struct {
	mu   sync.Mutex
	vals []string
}

func (s *seen) add(v string) { s.mu.Lock(); s.vals = append(s.vals, v); s.mu.Unlock() }
func (s *seen) get() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.vals...)
}

func affinityUpstream(t *testing.T, s *seen) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		s.add(r.Header.Get("X-Session-Id"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return u.Port()
}

func TestSessionAffinityHeaderOnlyOnTheConfiguredProvider(t *testing.T) {
	var with, without seen
	pw, po := affinityUpstream(t, &with), affinityUpstream(t, &without)
	y := fmt.Sprintf(`
extensions:
  corp:
    providers:
      aff:
        proxy: %s
        sessionAffinityHeader: X-Session-Id
        provides: {m: {type: chat}}
      plain:
        proxy: %s
        provides: {m: {type: chat}}
`, pw, po)
	cfg, err := config.LoadBytesForTest([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	cfg.PriorityGroups = map[string]config.PriorityGroup{"default": {Weight: 1}}
	cfg.Keys = map[string]config.KeyPolicy{"k": {Group: "default"}}
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr := proc.NewManager(&config.Config{})
	t.Cleanup(mgr.Shutdown)
	p := New(cfg, mgr, sched.NewWithConfig(cfg), st)
	r := chi.NewRouter()
	p.Mount(r)

	post := func(path, body string, hdr map[string]string) {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("X-Corrallm-Key", "k")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
	chatBody := func(model, msg string) string {
		return `{"model":"` + model + `","messages":[{"role":"user","content":"` + msg + `"}]}`
	}
	post("/v1/chat/completions", chatBody("aff-m", "hello"), nil)
	post("/v1/chat/completions", chatBody("aff-m", "hello"), nil)
	post("/v1/chat/completions", chatBody("aff-m", "other"), nil)
	post("/v1/chat/completions", chatBody("aff-m", "hello"), map[string]string{"X-Session-Id": "client-own"})
	post("/v1/chat/completions", chatBody("plain-m", "hello"), nil)

	got := with.get()
	if len(got) != 4 || got[0] == "" || got[0] != got[1] || got[0] == got[2] || got[3] != "client-own" {
		t.Fatalf("configured provider saw %q", got)
	}
	if len(got[0]) != 32 {
		t.Fatalf("value %q", got[0])
	}
	if o := without.get(); len(o) != 1 || o[0] != "" {
		t.Fatalf("unconfigured provider saw %q", o)
	}

	// /v1/messages, translated for this OpenAI upstream, carries one too.
	post("/v1/messages", `{"model":"aff-m","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`, nil)
	if got := with.get(); len(got) != 5 || got[4] == "" {
		t.Fatalf("/v1/messages: %q", got)
	}
}
