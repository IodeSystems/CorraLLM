package config

import (
	"testing"
	"time"
)

func TestParseRate(t *testing.T) {
	cases := []struct {
		dim, spec  string
		wantAmount float64
		wantWindow time.Duration
	}{
		{"cost", "$20/hr", 20, time.Hour},
		{"dwell", "600s/min", 600, time.Minute},
		{"requests", "100/min", 100, time.Minute},
		{"cost", "5/day", 5, 24 * time.Hour},
		{"dwell", "30s/s", 30, time.Second},
	}
	for _, c := range cases {
		r, err := ParseRate(c.dim, c.spec)
		if err != nil {
			t.Errorf("ParseRate(%q,%q): %v", c.dim, c.spec, err)
			continue
		}
		if r.Amount != c.wantAmount || r.Window != c.wantWindow {
			t.Errorf("ParseRate(%q,%q) = %+v, want {%v %v}", c.dim, c.spec, r, c.wantAmount, c.wantWindow)
		}
	}
}

func TestParseRateErrors(t *testing.T) {
	bad := []struct{ dim, spec string }{
		{"cost", "20"},        // no window
		{"cost", "$20/year"},  // unknown window
		{"requests", "x/min"}, // non-numeric
		{"bogus", "1/min"},    // unknown dimension
	}
	for _, c := range bad {
		if _, err := ParseRate(c.dim, c.spec); err == nil {
			t.Errorf("ParseRate(%q,%q): want error", c.dim, c.spec)
		}
	}
}
