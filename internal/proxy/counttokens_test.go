package proxy

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/iodesystems/corrallm/internal/config"
)

// proxyNode builds the `proxy:` yaml.Node a Model carries, pointing at a test
// upstream. Config parses this node lazily, so a test has to hand it the same
// shape the file would.
func proxyNode(t *testing.T, host string, port int) yaml.Node {
	t.Helper()
	var n yaml.Node
	src := "host: " + host + "\nport: " + itoa(port) + "\n"
	if err := yaml.Unmarshal([]byte(src), &n); err != nil {
		t.Fatalf("proxy node: %v", err)
	}
	// yaml.Unmarshal wraps in a document node; the mapping is its first child.
	return *n.Content[0]
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// remoteTestServer starts an upstream on a NON-LOOPBACK local address.
//
// It has to: the handler gates on Model.Remote(), which is false for anything on
// 127.0.0.0/8, and httptest.NewServer binds loopback by default. A test server on
// 127.0.0.1 is therefore indistinguishable from the llama.cpp process corrallm
// spawned — which is precisely the distinction under test, so the fixture cannot
// paper over it.
func remoteTestServer(t *testing.T, h http.Handler) (srv *httptest.Server, host string, port int) {
	t.Helper()
	ip := nonLoopbackIPv4(t)
	ln, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Skipf("cannot listen on a non-loopback address (%s): %v", ip, err)
	}
	srv = &httptest.Server{Listener: ln, Config: &http.Server{Handler: h}}
	srv.Start()
	return srv, ip, ln.Addr().(*net.TCPAddr).Port
}

func nonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("no interface addresses: %v", err)
	}
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok || n.IP.IsLoopback() {
			continue
		}
		if v4 := n.IP.To4(); v4 != nil {
			return v4.String()
		}
	}
	t.Skip("host has no non-loopback IPv4; cannot represent a remote provider")
	return ""
}

func countTokensProxy(t *testing.T, models map[string]config.Model) *Proxy {
	t.Helper()
	p := &Proxy{}
	p.SetConfig(&config.Config{Models: models})
	return p
}

// TestCountTokensMatchesAGlobTemplate is the whole reason this route exists:
// Anthropic models are registered as glob templates (`claude-haiku-*`), so an
// exact-map lookup — which is what /upstream does — answers "unknown model" for
// a concrete dated id a caller legitimately asks for.
func TestCountTokensMatchesAGlobTemplate(t *testing.T) {
	var gotPath, gotBody string
	up, host, port := remoteTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":9}`))
	}))
	defer up.Close()

	p := countTokensProxy(t, map[string]config.Model{
		"claude-haiku-*": {Proxy: proxyNode(t, host, port)},
	})

	rec := httptest.NewRecorder()
	body := `{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"hello world"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	req.ContentLength = int64(len(body))
	p.handleCountTokens(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/messages/count_tokens" {
		t.Errorf("upstream path %q, want /v1/messages/count_tokens — the provider's own route must forward unchanged", gotPath)
	}
	// The dated id must reach the provider untouched: `upstream` is unset on the
	// Anthropic passthrough precisely so its own matrix validates the variant.
	if !strings.Contains(gotBody, `"claude-haiku-4-5"`) {
		t.Errorf("upstream body lost the requested model id: %s", gotBody)
	}
	var out struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.InputTokens != 9 {
		t.Errorf("response did not pass through: %s (%v)", rec.Body.String(), err)
	}
}

// TestCountTokensEstimatesWhatAnthropicDoesNotServe — a local backend, a
// proxy pointing at this box, and a lane have no count_tokens route (a lane's
// members do not even share a tokenizer). Claude Code calls it anyway, so each
// gets an estimate, marked as one so it cannot pass for a tokenizer's count.
func TestCountTokensEstimatesWhatAnthropicDoesNotServe(t *testing.T) {
	p := countTokensProxy(t, map[string]config.Model{
		// A locally-spawned backend DOES have a proxy target — a loopback port
		// is how corrallm reaches the llama.cpp process it started.
		"Qwen3-6-27B-MPT": {Proxy: proxyNode(t, "127.0.0.1", 5800)},
		"local-alias":     {Proxy: proxyNode(t, "localhost", 5801)},
	})
	cfg := *p.config()
	cfg.Lanes = map[string]config.Lane{"chat": {Members: []config.LaneMember{{Model: "Qwen3-6-27B-MPT"}, {Model: "local-alias"}}}}
	p.SetConfig(&cfg)
	for _, model := range []string{"Qwen3-6-27B-MPT", "local-alias", "chat"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
			strings.NewReader(`{"model":"`+model+`","system":"12345678","messages":[{"role":"user","content":"hi"}]}`))
		p.handleCountTokens(rec, req)
		var out struct {
			InputTokens int  `json:"input_tokens"`
			Estimated   bool `json:"estimated"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.InputTokens <= 0 || !out.Estimated {
			t.Errorf("%s: %d %s, want 200 with an estimate marked estimated", model, rec.Code, rec.Body)
		}
	}
}

// TestCountTokensRejectsABodyWithNoModel — the model is the only thing that
// decides where this forwards, so an absent one is a 400, not a nil-deref.
func TestCountTokensRejectsABodyWithNoModel(t *testing.T) {
	p := countTokensProxy(t, map[string]config.Model{})
	for _, body := range []string{`{}`, `{"model":"  "}`, `not json`} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
		p.handleCountTokens(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: status %d, want 400", body, rec.Code)
		}
	}
}
