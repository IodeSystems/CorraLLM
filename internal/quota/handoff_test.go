package quota

import (
	"net/http"
	"sync"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

// Blue-green: the draining predecessor and its successor charge the same
// windows at the same time through one store. Neither process may lose the
// other's charges or count one twice — the store ends at the sum, and each
// ledger reads the sum after a Resync.
func TestHandoff_TwoLedgersOneStore(t *testing.T) {
	st := &memCounterStore{}
	limits := []config.Limit{{USD: 50, Per: "calendar-month", Hard: true}, lim("req", 1000, "day")}
	old, nu := New(), New()
	for _, l := range []*Ledger{old, nu} {
		l.UseStore(st)
		l.SetLimits("fw", limits)
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			old.Charge("fw", config.DimUSD, 0.01)
			old.ObserveResponse("fw", 200, http.Header{})
		}()
		go func() {
			defer wg.Done()
			nu.Charge("fw", config.DimUSD, 0.02)
			nu.ObserveResponse("fw", 200, http.Header{})
		}()
	}
	wg.Wait()
	rows, _ := st.LoadQuotaCounters()
	got := map[string]float64{}
	for _, r := range rows {
		got[r.Label] = r.Used
	}
	if u := got["usd/calendar-month"]; u < 1.4999 || u > 1.5001 {
		t.Errorf("stored spend = %v, want 1.50 (50×0.01 + 50×0.02)", u)
	}
	if r := got["req/day"]; r < 99.9 || r > 100.0001 {
		t.Errorf("stored requests = %v, want ~100", r)
	}
	for name, l := range map[string]*Ledger{"old": old, "new": nu} {
		l.Resync()
		for _, w := range l.Snapshot()[0].Windows {
			if w.Label == "usd/calendar-month" && (w.Used < 1.4999 || w.Used > 1.5001) {
				t.Errorf("%s ledger after resync = %v, want 1.50", name, w.Used)
			}
		}
	}
}

// Another process's charges fold in on our own next charge, without waiting
// for a Resync, and our charge is not counted twice.
func TestHandoff_ChargeFoldsInOtherProcess(t *testing.T) {
	st := &memCounterStore{}
	a, b := New(), New()
	for _, l := range []*Ledger{a, b} {
		l.UseStore(st)
		l.SetLimits("fw", []config.Limit{{USD: 5, Per: "calendar-day", Hard: true}})
	}
	a.Charge("fw", config.DimUSD, 3)
	b.Charge("fw", config.DimUSD, 1)
	if u := b.Snapshot()[0].Windows[0].Used; u != 4 {
		t.Errorf("b after its own charge = %v, want 4 (a's 3 + its 1)", u)
	}
	b.Charge("fw", config.DimUSD, 1.5)
	if u := b.Snapshot()[0].Windows[0].Used; u != 5.5 || b.Available("fw") {
		t.Errorf("b = %v (available %v), want 5.5 and blocked", u, b.Available("fw"))
	}
	// a has not charged since: a Resync is what shows it b's spend.
	a.Resync()
	if u := a.Snapshot()[0].Windows[0].Used; u != 5.5 {
		t.Errorf("a after resync = %v, want 5.5", u)
	}
}
