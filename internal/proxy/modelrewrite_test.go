package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

func TestRewriteModelField(t *testing.T) {
	// Swaps model, preserves every other field.
	in := `{"model":"groq-llama-70b","messages":[{"role":"user","content":"hi"}],"temperature":0}`
	out := rewriteModelField([]byte(in), "llama-3.3-70b-versatile")
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("result not valid JSON: %v", err)
	}
	if m["model"] != "llama-3.3-70b-versatile" {
		t.Errorf("model = %v, want llama-3.3-70b-versatile", m["model"])
	}
	if m["temperature"].(float64) != 0 || m["messages"] == nil {
		t.Errorf("other fields not preserved: %v", m)
	}

	// No-op on a body with no model field or invalid JSON — forwards unchanged.
	noModel := []byte(`{"messages":[]}`)
	if string(rewriteModelField(noModel, "x")) != string(noModel) {
		t.Error("body without model should be unchanged")
	}
	bad := []byte(`not json`)
	if string(rewriteModelField(bad, "x")) != string(bad) {
		t.Error("non-JSON body should be unchanged")
	}
}

// End-to-end: the upstream must receive the substituted id, not the served name.
func TestReverseProxy_ModelRewriteReachesUpstream(t *testing.T) {
	var gotModel string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		gotModel, _ = m["model"].(string)
		w.WriteHeader(200)
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)

	// newReverseProxy does not itself rewrite the body — the handler does before
	// dispatch — so exercise the helper + proxy together as the handler does.
	body := rewriteModelField([]byte(`{"model":"groq-llama-70b","messages":[]}`), "llama-3.3-70b-versatile")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	newReverseProxy(&config.ProxyTarget{URL: u, Model: "llama-3.3-70b-versatile"}).
		ServeHTTP(httptest.NewRecorder(), req)

	if gotModel != "llama-3.3-70b-versatile" {
		t.Errorf("upstream saw model %q, want llama-3.3-70b-versatile", gotModel)
	}
}
