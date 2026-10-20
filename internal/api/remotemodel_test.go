package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

// One call yields a servable, locked-down, priced remote model: the provider
// reached at the base URL's host and path, the key referenced (never stored in
// config), the allowlist and hard budget on the credential, and the price under
// the model's cost class.
func TestAddRemoteModel(t *testing.T) {
	cfg, err := config.Load(managedConfig(t, remoteProviderYAML))
	if err != nil {
		t.Fatal(err)
	}
	h := storeBackedHandlers(t, cfg)
	in := &AddRemoteModelInput{Body: RemoteModelSpec{
		Provider: "fireworks", BaseURL: "https://api.fireworks.ai/inference/v1",
		ModelID:   "accounts/fireworks/models/deepseek-v4p1-flash-1017",
		SecretRef: "FIREWORKS_API_KEY", MaxTokens: 32000,
		Allow:   []string{"sk-aw4"},
		Limits:  []Limit{{USD: 5, Per: "day", Hard: true}},
		Pricing: &RemotePricing{InputPerM: 0.30, CachedPerM: 0.006, OutputPerM: 1.20},
	}}
	out, err := h.AddRemoteModel(context.Background(), in)
	if err != nil || !out.Body.OK {
		t.Fatalf("add: err=%v out=%+v", err, out.Body)
	}
	saved := reloadStored(t, h, "after add")

	pv := saved.Extensions["fireworks"].Providers["fireworks"]
	m, ok := saved.Models["fireworks-deepseek-v4p1-flash-1017"]
	if !ok {
		t.Fatalf("not served under the derived name; provides=%v", pv.Provides)
	}
	tgt, err := m.ProxyTarget()
	if err != nil {
		t.Fatal(err)
	}
	if got := tgt.URL.Hostname() + tgt.BasePath; tgt.URL.Scheme != "https" || got != "api.fireworks.ai/inference" {
		t.Errorf("target = %s, want the base URL without its /v1", got)
	}
	if tgt.Model != "accounts/fireworks/models/deepseek-v4p1-flash-1017" {
		t.Errorf("upstream = %q", tgt.Model)
	}
	if m.Cmd != "" || m.Persistent || m.Server != "" {
		t.Errorf("remote model carries local-process fields: %+v", m)
	}

	if len(pv.Credentials) != 1 {
		t.Fatalf("credentials = %+v", pv.Credentials)
	}
	cr := pv.Credentials[0]
	if cr.Headers["authorization"] != "Bearer ${FIREWORKS_API_KEY}" {
		t.Errorf("auth header = %q, want a reference", cr.Headers["authorization"])
	}
	if !cr.AllowsKey("sk-aw4") || cr.AllowsKey("stranger") {
		t.Errorf("allowlist not applied: %v", cr.Allow)
	}
	if len(cr.Limits) != 1 || !cr.Limits[0].Hard || cr.Limits[0].USD != 5 {
		t.Errorf("limits = %+v, want one hard $5/day", cr.Limits)
	}

	cost, ok := saved.CommandCosts[m.Type]["extract"].(map[string]any)
	if m.Type != "fireworks-deepseek-v4p1-flash-1017" || !ok {
		t.Fatalf("type = %q, costs = %v", m.Type, saved.CommandCosts[m.Type])
	}
	if cost["cachedCostFactor"].(float64) != 0.006/1e6 || cost["generateCostFactor"].(float64) != 1.20/1e6 {
		t.Errorf("prices = %v", cost)
	}
	if config.ModelCapability(m) != config.Capability("chat") {
		t.Errorf("capability = %q: a priced cost class must still serve chat", config.ModelCapability(m))
	}

	// A second model on the same provider keeps the first.
	in.Body.ModelID, in.Body.Pricing = "accounts/fireworks/models/glm-5p2", nil
	if out, err := h.AddRemoteModel(context.Background(), in); err != nil || !out.Body.OK {
		t.Fatalf("second add: %v %+v", err, out.Body)
	}
	saved = reloadStored(t, h, "after second add")
	if _, ok := saved.Models["fireworks-deepseek-v4p1-flash-1017"]; !ok {
		t.Error("adding a second model dropped the first")
	}
	if saved.Models["fireworks-glm-5p2"].Type != "chat" {
		t.Errorf("unpriced model type = %q, want chat", saved.Models["fireworks-glm-5p2"].Type)
	}
}

func TestAddRemoteModelRefusesBadInput(t *testing.T) {
	cfg, err := config.Load(managedConfig(t, remoteProviderYAML))
	if err != nil {
		t.Fatal(err)
	}
	h := storeBackedHandlers(t, cfg)
	for _, b := range []RemoteModelSpec{
		{Provider: "Fire Works", BaseURL: "https://x.ai/v1", ModelID: "m"},
		{Provider: "x", BaseURL: "ftp://x.ai", ModelID: "m"},
		{Provider: "x", BaseURL: "https://x.ai/v1", ModelID: " "},
	} {
		out, err := h.AddRemoteModel(context.Background(), &AddRemoteModelInput{Body: b})
		if err != nil || out.Body.OK {
			t.Errorf("%+v: want a refusal message, got err=%v ok=%v", b, err, out.Body.OK)
		}
	}
}

func TestSplitBaseURL(t *testing.T) {
	for _, c := range []struct {
		in, scheme, host, path string
		port                   int
	}{
		{"https://api.fireworks.ai/inference/v1", "https", "api.fireworks.ai", "/inference", 443},
		{"https://api.deepseek.com", "https", "api.deepseek.com", "", 443},
		{"api.together.xyz/v1/", "https", "api.together.xyz", "", 443},
		{"http://127.0.0.1:8080/v1", "http", "127.0.0.1", "", 8080},
	} {
		s, h, p, bp, err := splitBaseURL(c.in)
		if err != nil || s != c.scheme || h != c.host || p != c.port || bp != c.path {
			t.Errorf("%s → %s %s %d %q %v", c.in, s, h, p, bp, err)
		}
	}
}

// The Anthropic preset's shape: native Messages API through the shim, key in
// x-api-key without a Bearer prefix.
func TestAddRemoteModelAnthropic(t *testing.T) {
	cfg, err := config.Load(managedConfig(t, remoteProviderYAML))
	if err != nil {
		t.Fatal(err)
	}
	h := storeBackedHandlers(t, cfg)
	out, err := h.AddRemoteModel(context.Background(), &AddRemoteModelInput{Body: RemoteModelSpec{
		Provider: "anthropic", BaseURL: "https://api.anthropic.com", API: "anthropic",
		ModelID: "claude-sonnet-5-5", SecretRef: "ANTHROPIC_API_KEY", HeaderName: "x-api-key",
	}})
	if err != nil || !out.Body.OK {
		t.Fatalf("add: %v %+v", err, out.Body)
	}
	saved := reloadStored(t, h, "after add")
	m := saved.Models["anthropic-claude-sonnet-5-5"]
	tgt, err := m.ProxyTarget()
	if err != nil {
		t.Fatal(err)
	}
	if tgt.API != config.APIAnthropic {
		t.Errorf("api = %q, want anthropic", tgt.API)
	}
	cr := saved.Extensions["anthropic"].Providers["anthropic"].Credentials[0]
	if cr.Headers["x-api-key"] != "${ANTHROPIC_API_KEY}" {
		t.Errorf("headers = %v, want a bare x-api-key reference", cr.Headers)
	}
}

// The preview reads an unconfigured endpoint's list with the given key, and
// converts per-token prices to per-1M.
func TestPreviewRemoteModels(t *testing.T) {
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/inference/v1/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"b-model","context_length":131072,"pricing":{"prompt":"0.0000003","completion":"0.0000012"}},{"id":"a-model"}]}`))
	}))
	defer up.Close()
	h := &Handlers{}
	in := &RemoteModelsPreviewInput{}
	in.Body.BaseURL, in.Body.SecretValue = up.URL+"/inference/v1", "k"
	out, err := h.PreviewRemoteModels(context.Background(), in)
	if err != nil || out.Body.Error != "" {
		t.Fatalf("%v %q", err, out.Body.Error)
	}
	if gotAuth != "Bearer k" {
		t.Errorf("auth = %q", gotAuth)
	}
	m := out.Body.Models
	if len(m) != 2 || m[0].ID != "a-model" || m[1].ContextLength != 131072 {
		t.Fatalf("models = %+v", m)
	}
	if d := m[1].InputPerM - 0.30; d > 1e-9 || d < -1e-9 {
		t.Errorf("input $/1M = %v, want 0.30", m[1].InputPerM)
	}
}
