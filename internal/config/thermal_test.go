package config

import (
	"math"
	"testing"
)

// TestThermalMode: the ROOM decides the mode — above the cooling target it is
// cooled, below the heating target heated, at or between the targets neither.
func TestThermalMode(t *testing.T) {
	var none *Thermal
	if none.Mode() != "" {
		t.Error("nil thermal must have no mode")
	}
	for _, c := range []struct {
		room float64
		want string
	}{{77, "cooling"}, {72, "neutral"}, {70, "neutral"}, {68, "neutral"}, {60, "heating"}} {
		th := &Thermal{RoomF: c.room, OutdoorF: 50, CoolTargetF: 72, HeatTargetF: 68}
		if got := th.Mode(); got != c.want {
			t.Errorf("room %v: mode %q, want %q", c.room, got, c.want)
		}
	}
}

// TestCoolFactor: a fan's cost per therm comes from the room-to-outdoor gap —
// halving when the gap doubles, impossible when outside is warmer — while a
// fixed factor is used as given.
func TestCoolFactor(t *testing.T) {
	fan := func(room, out float64) *Thermal {
		return &Thermal{RoomF: room, OutdoorF: out, CoolFanCFM: 2000, CoolFanWatts: 75}
	}
	f5, ok := fan(77, 72).CoolFactor(0.14)
	if !ok || math.Abs(f5-0.0972) > 0.0001 {
		t.Errorf("5°F gap: %v %v, want ≈0.0972", f5, ok)
	}
	f10, _ := fan(77, 67).CoolFactor(0.14)
	if math.Abs(f10-f5/2) > 1e-9 {
		t.Errorf("10°F gap = %v, want half of %v", f10, f5)
	}
	if _, ok := fan(77, 80).CoolFactor(0.14); ok {
		t.Error("a fan cannot cool when outside is warmer than the room")
	}
	if f, ok := (&Thermal{CoolCostPerTherm: 1.5}).CoolFactor(0.14); !ok || f != 1.5 {
		t.Errorf("fixed factor = %v %v, want 1.5", f, ok)
	}
}
