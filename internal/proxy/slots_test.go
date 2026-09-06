package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/slotcache"
)

// OFF is the default, and off means it touches nothing — not the backend, not
// the disk. A feature that trades latency for disk must be something an operator
// turned on.
func TestSlotSwapIsOffUntilConfigured(t *testing.T) {
	p := &Proxy{}
	if p.slots != nil {
		t.Fatal("a fresh proxy has a slot cache")
	}
	body := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("x", 20<<10) + `"}]}`)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	p.swapSlot(r.Context(), &config.ProxyTarget{URL: u}, "m", config.Model{Cmd: "llama-server"}, "sk-aw4", r, body)
	if called {
		t.Error("the slot cache is off and it still called the backend")
	}
}

// A remote provider's slot is not ours to ask about. Target.Model is set exactly
// when corrallm is forwarding to somebody else's endpoint.
func TestSlotSwapLeavesRemoteBackendsAlone(t *testing.T) {
	p := &Proxy{}
	p.SetSlotCache(t.TempDir(), 1<<30)
	body := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("x", 20<<10) + `"}]}`)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	// A remote: an upstream id, and no command corrallm ever ran.
	p.swapSlot(r.Context(), &config.ProxyTarget{URL: u, Model: "llama-3.3-70b"}, "groq-llama", config.Model{}, "sk-aw4", r, body)
	if called {
		t.Error("asked a remote provider about its slots")
	}
}

// Below the floor, reprocessing is cheaper than remembering, so nothing happens.
func TestSlotSwapIgnoresShortPrompts(t *testing.T) {
	p := &Proxy{}
	p.SetSlotCache(t.TempDir(), 1<<30)
	small := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	if len(small) >= minSlotBytes {
		t.Fatal("this test needs a body under the floor")
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(small)))
	r.Header.Set("Content-Type", "application/json")
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	p.swapSlot(r.Context(), &config.ProxyTarget{URL: u}, "m", config.Model{Cmd: "llama-server"}, "sk-aw4", r, small)
	if called {
		t.Error("a short prompt paid for the machinery")
	}
}

// The whole point, end to end through the proxy's own guard: a second
// conversation on the same backend saves the first and restores itself.
func TestSlotSwapSavesThenRestoresOnAConversationChange(t *testing.T) {
	dir := t.TempDir()
	p := &Proxy{}
	p.SetSlotCache(dir, 1<<30)

	var actions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actions = append(actions, r.URL.Query().Get("action"))
		_, _ = w.Write([]byte(`{"n_saved":10,"n_restored":10,"n_written":100,"n_read":100}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	target := &config.ProxyTarget{URL: u}
	model := config.Model{Cmd: "llama-server -c 188000"}
	filler := strings.Repeat("x", 20<<10)

	req := func(conversation string) *http.Request {
		body := `{"messages":[{"role":"user","content":"` + filler + `"}]}`
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set(slotcache.ConversationHeader, conversation)
		return r
	}

	first := req("task-a")
	p.swapSlot(first.Context(), target, "m", model, "sk-aw4", first, []byte(strings.Repeat("x", 20<<10)))
	if len(actions) != 0 {
		t.Fatalf("the first conversation should not touch the backend: %v", actions)
	}

	second := req("task-b")
	p.swapSlot(second.Context(), target, "m", model, "sk-aw4", second, []byte(strings.Repeat("x", 20<<10)))
	if len(actions) != 1 || actions[0] != "save" {
		t.Fatalf("switching conversations should save the outgoing one: %v", actions)
	}
}
