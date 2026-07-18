package run

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunModeModes(t *testing.T) {
	cases := []struct {
		in   RunMode
		want []RunMode
	}{
		{ModeAny, []RunMode{ModeAny}},
		{ModeCold, []RunMode{ModeCold}},
		{ModeWarm, []RunMode{ModeWarm}},
		// `both` must expand COLD FIRST: running warm first would leave the
		// model resident and make the "cold" pass a lie.
		{ModeBoth, []RunMode{ModeCold, ModeWarm}},
	}
	for _, tc := range cases {
		got := tc.in.Modes()
		if len(got) != len(tc.want) {
			t.Errorf("%q -> %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%q -> %v, want %v", tc.in, got, tc.want)
				break
			}
		}
	}
}

// stubCorrallm serves the admin load/unload endpoints.
func stubCorrallm(t *testing.T, ok bool, msg string, calls *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("admin token not sent: %q", got)
		}
		*calls = append(*calls, r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": ok, "evicted": 1, "message": msg})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func cfgFor(base string) Config {
	c := Config{}
	c.LLM.BaseURL = base
	c.LLM.AdminTokenEnv = "TEST_ADMIN_TOKEN"
	return c
}

func TestPrepareResidency_ColdEvicts(t *testing.T) {
	var calls []string
	srv := stubCorrallm(t, true, "evicted 1 backend(s)", &calls)
	t.Setenv("TEST_ADMIN_TOKEN", "tok")
	c := newResidencyClient(cfgFor(srv.URL))
	if c == nil {
		t.Fatal("client should exist when a token is configured")
	}
	note := prepareResidency(context.Background(), c, ModeCold, "m")
	if len(calls) != 1 || calls[0] != "/api/v1/models/unload" {
		t.Errorf("cold should call unload, got %v", calls)
	}
	if strings.Contains(note, "WARNING") {
		t.Errorf("successful eviction should not warn: %q", note)
	}
}

func TestPrepareResidency_WarmLoads(t *testing.T) {
	var calls []string
	srv := stubCorrallm(t, true, "loaded", &calls)
	t.Setenv("TEST_ADMIN_TOKEN", "tok")
	c := newResidencyClient(cfgFor(srv.URL))
	prepareResidency(context.Background(), c, ModeWarm, "m")
	if len(calls) != 1 || calls[0] != "/api/v1/models/load" {
		t.Errorf("warm should call load, got %v", calls)
	}
}

// corrallm refuses to evict pinned / in-flight models, so "cold" is a REQUEST,
// not a guarantee. A pass that silently ran warm while claiming cold would be
// evidence for a path it never tested — the exact failure that let the bonsai
// vision bug hide behind a "verified end-to-end" comment.
func TestPrepareResidency_RefusedEvictionWarnsLoudly(t *testing.T) {
	var calls []string
	srv := stubCorrallm(t, false, "model is persistent", &calls)
	t.Setenv("TEST_ADMIN_TOKEN", "tok")
	c := newResidencyClient(cfgFor(srv.URL))
	note := prepareResidency(context.Background(), c, ModeCold, "m")
	if !strings.Contains(note, "WARNING") || !strings.Contains(note, "NOT evicted") {
		t.Errorf("a refused eviction must warn loudly, got %q", note)
	}
	if !strings.Contains(note, "persistent") {
		t.Errorf("note should carry corrallm's reason, got %q", note)
	}
}

// No admin token: cold/warm cannot be honored. Warn rather than pretend.
func TestPrepareResidency_NoTokenWarns(t *testing.T) {
	t.Setenv("TEST_ADMIN_TOKEN", "")
	if c := newResidencyClient(cfgFor("http://x")); c != nil {
		t.Fatal("no token should yield a nil client")
	}
	note := prepareResidency(context.Background(), nil, ModeCold, "m")
	if !strings.Contains(note, "WARNING") || !strings.Contains(note, "does not prove") {
		t.Errorf("missing token must warn that the result proves nothing: %q", note)
	}
}

// ModeAny must not touch residency at all — it is the default, and every
// existing probe relies on it being a no-op.
func TestPrepareResidency_AnyIsNoOp(t *testing.T) {
	var calls []string
	srv := stubCorrallm(t, true, "", &calls)
	t.Setenv("TEST_ADMIN_TOKEN", "tok")
	c := newResidencyClient(cfgFor(srv.URL))
	if note := prepareResidency(context.Background(), c, ModeAny, "m"); note != "" {
		t.Errorf("ModeAny should produce no note, got %q", note)
	}
	if len(calls) != 0 {
		t.Errorf("ModeAny must not call the admin API, got %v", calls)
	}
}
