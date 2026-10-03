package config

import "testing"

// TestThermalMode: above the cooling target the room is cooled, below the
// heating target it is heated, at or between the targets neither runs.
func TestThermalMode(t *testing.T) {
	var none *Thermal
	if none.Mode() != "" {
		t.Error("nil thermal must have no mode")
	}
	for _, c := range []struct {
		ambient float64
		want    string
	}{{75, "cooling"}, {72, "neutral"}, {70, "neutral"}, {68, "neutral"}, {60, "heating"}} {
		th := &Thermal{AmbientF: c.ambient, CoolTargetF: 72, HeatTargetF: 68}
		if got := th.Mode(); got != c.want {
			t.Errorf("ambient %v: mode %q, want %q", c.ambient, got, c.want)
		}
	}
}
