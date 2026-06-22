package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func backendWithProxy(t *testing.T, y string) Backend {
	t.Helper()
	var b Backend
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
			tgt, err := backendWithProxy(t, c.yaml).ProxyTarget()
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
	b := backendWithProxy(t, `{ host: api.example.com, port: 443, headers: { authorization: "Bearer ${TEST_API_KEY}" } }`)
	tgt, err := b.ProxyTarget()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := tgt.Headers["authorization"]; got != "Bearer sekret" {
		t.Errorf("header expansion: got %q", got)
	}
}

func TestProxyTargetMissing(t *testing.T) {
	var b Backend
	if _, err := b.ProxyTarget(); err == nil {
		t.Fatal("expected error for missing proxy target")
	}
}
