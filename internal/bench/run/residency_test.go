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
		{ModeWarm, []RunMode{ModeWarm}},
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

// The bench must never EVICT. Eviction is a cost other callers pay, and it was
// removed along with the exclusive lease; corrallm still exposes the endpoints
// for operators. This is a guard, not a behavior test: the only way it fails is
// if someone reintroduces an unload call inside prepareResidency.
func TestPrepareResidency_NeverEvicts(t *testing.T) {
	var calls []string
	srv := stubCorrallm(t, true, "", &calls)
	t.Setenv("TEST_ADMIN_TOKEN", "tok")
	c := newResidencyClient(cfgFor(srv.URL))
	for _, m := range ValidRunModes {
		prepareResidency(context.Background(), c, m, "m")
	}
	for _, path := range calls {
		if strings.Contains(path, "unload") {
			t.Errorf("the bench called %s — eviction was removed with the exclusive lease", path)
		}
	}
}

// No admin token: warm cannot be honored. Warn rather than pretend, so a load
// latency nobody arranged is not silently attributed to the model.
func TestPrepareResidency_NoTokenWarns(t *testing.T) {
	t.Setenv("TEST_ADMIN_TOKEN", "")
	if c := newResidencyClient(cfgFor("http://x")); c != nil {
		t.Fatal("no token should yield a nil client")
	}
	note := prepareResidency(context.Background(), nil, ModeWarm, "m")
	if !strings.Contains(note, "WARNING") || !strings.Contains(note, "cold load") {
		t.Errorf("missing token must warn the first request may pay a load: %q", note)
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
