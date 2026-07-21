package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func modelWithProxy(t *testing.T, y string) Model {
	t.Helper()
	var b Model
	if err := yaml.Unmarshal([]byte("proxy: "+y), &b); err != nil {
		t.Fatalf("unmarshal %q: %v", y, err)
	}
	return b
}

func TestProxyTargetForms(t *testing.T) {
	cases := []struct {
		name, yaml, want string
	}{
		{"port", "8081", "http://127.0.0.1:8081"},
		{"hostport", `"box1:8082"`, "http://box1:8082"},
		{"fullurl", `"https://api.example.com/v1"`, "https://api.example.com/v1"},
		{"object", "{ host: box2, port: 9000 }", "http://box2:9000"},
		{"object443https", "{ host: api.anthropic.com, port: 443 }", "https://api.anthropic.com:443"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tgt, err := modelWithProxy(t, c.yaml).ProxyTarget()
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got := tgt.URL.String(); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestProxyTargetHeaderEnvExpansion(t *testing.T) {
	t.Setenv("TEST_API_KEY", "sekret")
	b := modelWithProxy(t, `{ host: api.example.com, port: 443, headers: { authorization: "Bearer ${TEST_API_KEY}" } }`)
	tgt, err := b.ProxyTarget()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := tgt.Headers["authorization"]; got != "Bearer sekret" {
		t.Errorf("header expansion: got %q", got)
	}
}

func TestProxyTargetMissing(t *testing.T) {
	var b Model
	if _, err := b.ProxyTarget(); err == nil {
		t.Fatal("expected error for missing proxy target")
	}
}

// A remote that mounts its OpenAI surface below root (Groq /openai, OpenRouter
// /api) needs a base-path prefix; the client always sends the standard /v1/...
func TestProxyTargetBasePath(t *testing.T) {
	cases := []struct {
		name, yaml, wantBase string
	}{
		{"groq", "{ host: api.groq.com, port: 443, basePath: /openai }", "/openai"},
		{"openrouter", "{ host: openrouter.ai, port: 443, basePath: /api/v1 }", "/api/v1"},
		{"trims slashes", "{ host: h, port: 1, basePath: /openai/ }", "/openai"},
		{"bare word", "{ host: h, port: 1, basePath: api }", "/api"},
		{"empty is noop", "{ host: h, port: 1 }", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tgt, err := modelWithProxy(t, c.yaml).ProxyTarget()
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if tgt.BasePath != c.wantBase {
				t.Errorf("BasePath = %q, want %q", tgt.BasePath, c.wantBase)
			}
		})
	}
}
