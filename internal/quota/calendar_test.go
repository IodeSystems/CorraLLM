package quota

import (
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

func TestCalendarMonthHardCapEmptiesOnTheFirst(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	l := New()
	l.now = func() time.Time { return now }
	l.SetLimits("b", []config.Limit{{USD: 50, Per: "calendar-month", Hard: true}})

	l.Charge("b", config.DimUSD, 50)
	// A falloff window would have leaked ~$1.67/day; a calendar one holds.
	now = time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC)
	blocked, wait := l.HardBlocked("b")
	if !blocked {
		t.Fatal("full calendar month: want blocked")
	}
	if wait != time.Hour {
		t.Errorf("wait = %v, want 1h (until 11-01 00:00 UTC)", wait)
	}

	now = time.Date(2026, 11, 1, 0, 0, 1, 0, time.UTC)
	if blocked, _ := l.HardBlocked("b"); blocked {
		t.Fatal("new month: want unblocked")
	}
	if u := l.Snapshot()[0].Windows[0].Used; u != 0 {
		t.Errorf("used in new month = %v, want 0", u)
	}
}

func TestCalendarDayKeepsChargesWithinTheDay(t *testing.T) {
	now := time.Date(2026, 10, 20, 1, 0, 0, 0, time.UTC)
	l := New()
	l.now = func() time.Time { return now }
	l.SetLimits("b", []config.Limit{{USD: 5, Per: "calendar-day", Hard: true}})
	l.Charge("b", config.DimUSD, 2)
	now = now.Add(20 * time.Hour)
	l.Charge("b", config.DimUSD, 2)
	if u := l.Snapshot()[0].Windows[0].Used; u != 4 {
		t.Errorf("used = %v, want 4 (no leak within the day)", u)
	}
	now = now.Add(4 * time.Hour) // 10-21 01:00
	if u := l.Snapshot()[0].Windows[0].Used; u != 0 {
		t.Errorf("next day used = %v, want 0", u)
	}
}

func TestParseRetryAfterForms(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	for v, want := range map[string]time.Duration{
		"3":                             3 * time.Second,
		"1.5":                           1500 * time.Millisecond,
		"2s":                            2 * time.Second,
		"Tue, 20 Oct 2026 12:00:30 GMT": 30 * time.Second,
	} {
		got, ok := ParseRetryAfter(v, now)
		if !ok || got.Sub(now) != want {
			t.Errorf("ParseRetryAfter(%q) = %v, %v; want +%v", v, got.Sub(now), ok, want)
		}
	}
	if _, ok := ParseRetryAfter("soon", now); ok {
		t.Error(`"soon" parsed`)
	}
}
