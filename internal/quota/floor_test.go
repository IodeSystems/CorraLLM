package quota

import (
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

type memCounters struct{ saved map[string]float64 }

func (m *memCounters) LoadQuotaCounters() ([]PersistedCounter, error) { return nil, nil }
func (m *memCounters) AddQuotaCounter(b, label string, amt, _ float64, _ time.Duration, _, _ time.Time) (float64, error) {
	m.saved[b+"/"+label] += amt
	return m.saved[b+"/"+label], nil
}
func (m *memCounters) RaiseQuotaCounter(b, label string, floor float64, _, _ time.Time) (float64, error) {
	m.saved[b+"/"+label] = max(m.saved[b+"/"+label], floor)
	return m.saved[b+"/"+label], nil
}

func TestRaiseCalendarFloorOnlyRaisesCalendarWindows(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	l := New()
	l.now = func() time.Time { return now }
	st := &memCounters{saved: map[string]float64{}}
	l.UseStore(st)
	l.SetLimits("c", []config.Limit{
		{USD: 5, Per: "calendar-day", Hard: true},
		{USD: 50, Per: "calendar-month", Hard: true},
		{USD: 100, Per: "month"}, // falloff: must stay untouched
	})
	l.Charge("c", config.DimUSD, 0.22)

	if got := l.RaiseCalendarFloor("c", config.DimUSD, "month", 0.72); got < 0.49 || got > 0.51 {
		t.Errorf("raised = %v, want ~0.50", got)
	}
	used := map[string]float64{}
	for _, w := range l.Snapshot()[0].Windows {
		used[w.Label] = w.Used
	}
	if used["usd/calendar-month"] != 0.72 {
		t.Errorf("month = %v, want 0.72", used["usd/calendar-month"])
	}
	if used["usd/calendar-day"] != 0.22 {
		t.Errorf("day = %v, want 0.22 (a month floor must not touch the day window)", used["usd/calendar-day"])
	}
	if used["usd/month"] != 0.22 {
		t.Errorf("falloff = %v, want 0.22 (untouched)", used["usd/month"])
	}
	if st.saved["c/usd/calendar-month"] != 0.72 {
		t.Errorf("persisted = %v, want 0.72", st.saved["c/usd/calendar-month"])
	}
}

func TestRaiseCalendarFloorNeverLowers(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	l := New()
	l.now = func() time.Time { return now }
	l.SetLimits("c", []config.Limit{{USD: 5, Per: "calendar-day", Hard: true}})
	l.Charge("c", config.DimUSD, 3)
	if got := l.RaiseCalendarFloor("c", config.DimUSD, "day", 1); got != 0 {
		t.Errorf("raised = %v, want 0", got)
	}
	if got := l.RaiseCalendarFloor("c", config.DimUSD, "day", 3); got != 0 {
		t.Errorf("raised at equal = %v, want 0", got)
	}
	if u := l.Snapshot()[0].Windows[0].Used; u != 3 {
		t.Errorf("used = %v, want 3", u)
	}
	// A floor can fill the cap and block, which is the point.
	l.RaiseCalendarFloor("c", config.DimUSD, "day", 5)
	if blocked, _ := l.HardBlocked("c"); !blocked {
		t.Error("floor at the limit: want blocked")
	}
	// A new period starts from zero: a stale floor does not carry over.
	now = now.Add(24 * time.Hour)
	if u := l.Snapshot()[0].Windows[0].Used; u != 0 {
		t.Errorf("next day used = %v, want 0", u)
	}
	if got := l.RaiseCalendarFloor("missing", config.DimUSD, "day", 9); got != 0 {
		t.Errorf("unknown scope raised = %v", got)
	}
}
