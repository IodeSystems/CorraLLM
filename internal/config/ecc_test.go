package config

import (
	"strings"
	"testing"
	"time"
)

func TestNonECCMaxAge(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"": DefaultNonECCMaxAge, "0": 0, "12h": 12 * time.Hour, " 90m ": 90 * time.Minute,
	} {
		got, err := ResidencyConfig{NonECCMaxAge: in}.NonECCMaxAgeDuration()
		if err != nil || got != want {
			t.Errorf("%q: got %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"two days", "-1h"} {
		if _, err := (ResidencyConfig{NonECCMaxAge: bad}).NonECCMaxAgeDuration(); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
	if DefaultNonECCMaxAge != 48*time.Hour {
		t.Errorf("default = %v, want 48h (owner rule)", DefaultNonECCMaxAge)
	}
}

func TestValidateECCAndResidency(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, err string }{
		"ok": {`
residency: { nonEccMaxAge: 24h }
servers:
  box1:
    pools: { gpu0: 24GB, system: 64GB }
    ecc: { gpu0: true, system: false }
`, ""},
		"ecc names an undeclared pool": {`
servers:
  box1:
    pools: { gpu0: 24GB }
    ecc: { gpu1: true }
`, `ecc key "gpu1" is not one of its pools`},
		"bad max age": {`
residency: { nonEccMaxAge: soon }
servers:
  box1:
    pools: { gpu0: 24GB }
`, "residency.nonEccMaxAge"},
	} {
		_, err := LoadBytesForTest([]byte(tc.yaml))
		switch {
		case tc.err == "" && err != nil:
			t.Errorf("%s: unexpected error %v", name, err)
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("%s: got %v, want an error containing %q", name, err, tc.err)
		}
	}
}
