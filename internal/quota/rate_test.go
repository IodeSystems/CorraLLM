package quota

import (
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

// A window's spend rate is its charges over the trailing hour (or since start),
// and it projects where that rate takes the level: the calendar boundary's
// level, and the moment the limit is reached when that comes first (P56).
func TestWindowSpendRateAndProjection(t *testing.T) {
	t0 := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	now := t0
	l := New()
	l.now, l.started = func() time.Time { return now }, t0
	l.SetLimits("key:k", []config.Limit{
		{USD: 10, Per: "calendar-day", Hard: true},
		{USD: 1, Per: "hour"},
	})
	for _, m := range []int{10, 20} {
		now = t0.Add(time.Duration(m) * time.Minute)
		l.Charge("key:k", config.DimUSD, 1)
	}
	now = t0.Add(30 * time.Minute)
	ws := l.Snapshot()[0].Windows
	day, hour := ws[0], ws[1]
	if day.RatePerHour != 4 || day.RateSpan != 30*time.Minute {
		t.Fatalf("rate = $%v/h over %v, want $4/h over 30m", day.RatePerHour, day.RateSpan)
	}
	// $8 of room at $4/h: two hours from now, well before midnight.
	if want := now.Add(2 * time.Hour); !day.ExhaustsAt.Equal(want) {
		t.Errorf("day exhausts at %v, want %v", day.ExhaustsAt, want)
	}
	// $2 now + $4/h × 23.5 h.
	if day.ProjectedAtReset != 96 {
		t.Errorf("projected at reset = %v, want 96", day.ProjectedAtReset)
	}
	// The hourly falloff window is already over its $1 and fills faster than
	// it leaks, but it has no room left to count down: no exhaust time.
	if !hour.ExhaustsAt.IsZero() || hour.ProjectedAtReset != 0 {
		t.Errorf("hour window = %+v, want no projection", hour)
	}

	// An hour later with no new charges, the rate falls to 0.
	now = t0.Add(90 * time.Minute)
	if r := l.Snapshot()[0].Windows[0].RatePerHour; r != 0 {
		t.Errorf("rate after an idle hour = %v, want 0", r)
	}
}
