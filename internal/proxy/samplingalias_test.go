package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

// The sampling profile belongs to the MODEL, however the caller names it: by
// its canonical name, an alias, or a lane it serves.
func TestSamplingProfileFollowsAliasesAndLanes(t *testing.T) {
	f := newFakeOpenAIChat(t)
	temp := 0.123
	m := modelTo(t, f.srv.URL, "chat")
	m.Aliases = []string{"m-alias"}
	m.Sampling = &config.SamplingConfig{Thinking: config.SamplingProfile{Temperature: &temp}, Instruct: config.SamplingProfile{Temperature: &temp}}
	h, _ := refusalRig(t, map[string]config.Model{"m": m}, map[string]config.Lane{"m-lane": {Members: []config.LaneMember{{Model: "m"}}}})
	for _, name := range []string{"m", "m-alias", "m-lane"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"`+name+`","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("X-Corrallm-Key", "k")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		f.mu.Lock()
		got := f.body["temperature"]
		f.mu.Unlock()
		if rec.Code != 200 || got != temp {
			t.Errorf("%s: status %d, backend temperature %v, want %v", name, rec.Code, got, temp)
		}
	}
	// And on /v1/messages, after the translation.
	rec := postMessages(h, `{"model":"m-lane","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`, map[string]string{"x-api-key": "k"})
	f.mu.Lock()
	defer f.mu.Unlock()
	if rec.Code != 200 || f.body["temperature"] != temp {
		t.Errorf("/v1/messages: status %d, backend temperature %v", rec.Code, f.body["temperature"])
	}
}
