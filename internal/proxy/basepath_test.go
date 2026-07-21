package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

// The client speaks the standard /v1/chat/completions; a Groq-style upstream
// must receive /openai/v1/chat/completions. Proves the base-path prefix is
// actually applied by the reverse-proxy Director, not just parsed.
func TestReverseProxy_PrependsBasePath(t *testing.T) {
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(200)
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)

	rp := newReverseProxy(&config.ProxyTarget{URL: u, BasePath: "/openai"})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rp.ServeHTTP(httptest.NewRecorder(), req)

	if gotPath != "/openai/v1/chat/completions" {
		t.Errorf("upstream got path %q, want /openai/v1/chat/completions", gotPath)
	}
}

// Empty BasePath (every local backend) must forward the path untouched.
func TestReverseProxy_NoBasePathIsUnchanged(t *testing.T) {
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(200)
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)

	rp := newReverseProxy(&config.ProxyTarget{URL: u})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rp.ServeHTTP(httptest.NewRecorder(), req)

	if gotPath != "/v1/chat/completions" {
		t.Errorf("upstream got path %q, want /v1/chat/completions (unchanged)", gotPath)
	}
}
