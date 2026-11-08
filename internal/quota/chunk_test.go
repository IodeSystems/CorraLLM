package quota

import (
	"math"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

func levels(l *Ledger) map[string]float64 {
	out := map[string]float64{}
	for _, w := range l.Snapshot()[0].Windows {
		out[w.Label] = w.Used
	}
	return out
}

// P74 S4: a ceiling is charged the full cost, a chunk window only what it paid.
func TestChargeSplitChargesCeilingsInFullAndChunksTheirShare(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	l := New()
	l.now = func() time.Time { return now }
	l.SetLimits("prov:x", []config.Limit{
		{USD: 5, Per: "calendar-day", Hard: true},
		{USD: 20, Per: "total", Hard: true},
		{USD: 100, Per: "calendar-month", Hard: true},
	})
	l.ChargeSplit("prov:x", 3, map[string]float64{"usd/calendar-day": 3}, 0)
	l.ChargeSplit("prov:x", 4, map[string]float64{"usd/calendar-day": 2, "usd/total": 2}, 0)
	l.ChargeSplit("prov:x", 1, nil, 1) // a timed budget paid it all
	got := levels(l)
	if got["usd/calendar-day"] != 5 || got["usd/total"] != 2 || got["usd/calendar-month"] != 8 {
		t.Fatalf("levels = %v, want day 5, total 2, month 8", got)
	}
	if b, _ := l.HardBlocked("prov:x"); !b {
		t.Error("HardBlocked counts the full day")
	}
	if b, _ := l.CeilingBlocked("prov:x"); b {
		t.Error("CeilingBlocked must leave the day chunk to the chunk walk")
	}
	if !l.CeilingAvailable("prov:x") || l.Available("prov:x") {
		t.Error("CeilingAvailable leaves out chunks; Available does not")
	}
	ch := l.Chunks("prov:x")
	if len(ch) != 2 || ch[0].Label != "usd/calendar-day" || ch[0].Room != 0 || ch[0].RefillIn != 12*time.Hour ||
		ch[1].Label != "usd/total" || ch[1].Room != 18 || ch[1].RefillIn != 0 {
		t.Fatalf("chunks = %+v", ch)
	}
	// The month ceiling full blocks whatever chunk has room.
	l.ChargeSplit("prov:x", 92, map[string]float64{"usd/total": 2}, 90)
	if b, wait := l.CeilingBlocked("prov:x"); !b || wait != 23*24*time.Hour-12*time.Hour {
		t.Errorf("full month: blocked %v, wait %v", b, wait)
	}
}

// The paced day spreads the month's own credit: spend that timed budgets paid
// counts in the month ceiling, not in the month's spend the pacing divides.
func TestPacedDayLeavesTimedBudgetSpendOutOfTheMonth(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC) // 11 days to the 15th
	st := &memCounterStore{}
	limits := []config.Limit{
		{USD: 200, Per: "calendar-month", Hard: true, Renews: 15},
		{Pace: true, Per: "calendar-day", Hard: true},
	}
	l := New()
	l.now = func() time.Time { return now }
	l.UseStore(st)
	l.SetLimits("prov:a", limits)
	l.ChargeSplit("prov:a", 50, nil, 50)
	l.ChargeSplit("prov:a", 2, map[string]float64{"usd/calendar-day": 2}, 0)
	paced := func(l *Ledger) Window {
		for _, w := range l.Snapshot()[0].Windows {
			if w.Paced {
				return w
			}
		}
		t.Fatal("no paced window")
		return Window{}
	}
	if w := paced(l); math.Abs(w.Limit-200.0/11) > 1e-9 || w.Used != 2 {
		t.Fatalf("paced = %+v, want 200/11 with $2 used", w)
	}
	if got := levels(l)["usd/calendar-month"]; got != 52 {
		t.Errorf("month = %v, want 52: the ceiling sees all of it", got)
	}
	// Survives a restart: a new ledger on the same store.
	l2 := New()
	l2.now = func() time.Time { return now }
	l2.UseStore(st)
	l2.SetLimits("prov:a", limits)
	if w := paced(l2); math.Abs(w.Limit-200.0/11) > 1e-9 {
		t.Errorf("after restart paced = %+v, want 200/11", w)
	}
	// Next day: the $2 the day paid is the month's spend; (200-2)/10.
	now = now.Add(24 * time.Hour)
	if w := paced(l2); math.Abs(w.Limit-198.0/10) > 1e-9 {
		t.Errorf("next day paced = %v, want 19.8", w.Limit)
	}
}
