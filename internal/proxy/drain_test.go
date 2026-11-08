package proxy

import (
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

// P74 S4 drain order: (1) unbanked timed budgets, soonest expiry first; (2) the
// calendar day; (3) banked timed budgets, then total limits. Ceilings always.

// clockAt points the grant book and the ledger at *at.
func clockAt(p *Proxy, at *time.Time) {
	f := func() time.Time { return *at }
	p.grants.now = f
	p.quota.SetClock(f)
}

func grantSpent(t *testing.T, p *Proxy, scope string, id int64) float64 {
	t.Helper()
	for _, g := range p.grants.of(scope) {
		if g.ID == id {
			return g.Spent
		}
	}
	t.Fatalf("no grant %d", id)
	return 0
}

func scopeLevel(p *Proxy, scope, label string) float64 {
	for _, e := range p.quota.Snapshot() {
		if e.Backend != scope {
			continue
		}
		for _, w := range e.Windows {
			if w.Label == label {
				return w.Used
			}
		}
	}
	return -1
}

func addBudget(t *testing.T, p *Proxy, provider string, usd float64, expires time.Time, banked bool, now time.Time) int64 {
	t.Helper()
	if err := p.AddBudget(provider, usd, "", expires.Format(time.RFC3339), "", banked, now); err != nil {
		t.Fatal(err)
	}
	var id int64
	for _, g := range p.grants.of(config.ProviderScopeKey(provider)) {
		id = max(id, g.ID)
	}
	return id
}

// Fireworks: prov day $5 hard, banked $10 budget, cred month $100 ceiling. The
// day pays first, the budget only once the day is full, and the next day the
// day pays again before the budget. A cost past the day spills into the budget.
func TestFireworksDayFirstThenBankedBudgetThenDayAgain(t *testing.T) {
	p, _, cand := fireworksRig(t)
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	clockAt(p, &now)
	scope := config.ProviderScopeKey("fireworks")
	id := addBudget(t, p, "fireworks", 10, now.Add(72*time.Hour), true, now)

	spendAs(p, cand, 3)
	if d, b := scopeLevel(p, scope, "usd/calendar-day"), grantSpent(t, p, scope, id); d != 3 || b != 0 {
		t.Fatalf("after $3: day %v budget %v, want 3 and 0", d, b)
	}
	spendAs(p, cand, 4) // $2 fills the day, $2 spills into the budget
	if d, b := scopeLevel(p, scope, "usd/calendar-day"), grantSpent(t, p, scope, id); d != 5 || b != 2 {
		t.Fatalf("after $7: day %v budget %v, want 5 and 2", d, b)
	}
	if !admits(p, cand) {
		t.Fatal("the budget has $8 left: want admitted")
	}
	if m := scopeLevel(p, "cred:fireworks/default", "usd/calendar-month"); m != 7 {
		t.Errorf("month ceiling = %v, want 7: it sees every dollar", m)
	}

	now = now.Add(24 * time.Hour) // the next UTC day
	spendAs(p, cand, 2)
	if d, b := scopeLevel(p, scope, "usd/calendar-day"), grantSpent(t, p, scope, id); d != 2 || b != 2 {
		t.Fatalf("next day: day %v budget %v, want 2 and 2 (the day pays before the budget)", d, b)
	}
	a := fireworks(t, p, now).Account
	if strings.Join(a.DrainOrder, ",") != "prov:fireworks calendar-day,budget 1" || a.DrainNote != "" || a.Drain != drainRule {
		t.Errorf("drain = %q %v %q", a.Drain, a.DrainOrder, a.DrainNote)
	}
	spendAs(p, cand, 3)
	spendAs(p, cand, 8)
	if admits(p, cand) {
		t.Fatal("day and budget spent: want refused")
	}
	if _, wait := p.filterByQuota([]config.Candidate{cand}); wait != 14*time.Hour {
		t.Errorf("wait = %v, want 14h (the day refills at 00:00 UTC)", wait)
	}
}

// Anthropic: month $200 ceiling, day $0.01 hard, unbanked $104.54 budget. The
// budget pays first, and the day's counter stays untouched while it does.
func TestAnthropicUnbankedBudgetPaysBeforeTheDay(t *testing.T) {
	p, _, cand := limitRig(t, config.LimitSet{
		{USD: 200, Per: "calendar-month", Hard: true, Renews: 15},
		{USD: 0.01, Per: "calendar-day", Hard: true},
	})
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	clockAt(p, &now)
	scope := config.ProviderScopeKey("anthropic")
	id := addBudget(t, p, "anthropic", 104.54, now.Add(30*24*time.Hour), false, now)

	spendAs(p, cand, 50)
	if d, b := scopeLevel(p, scope, "usd/calendar-day"), grantSpent(t, p, scope, id); d != 0 || b != 50 {
		t.Fatalf("after $50: day %v budget %v, want 0 and 50", d, b)
	}
	spendAs(p, cand, 54.54)
	if d := scopeLevel(p, scope, "usd/calendar-day"); d != 0 {
		t.Fatalf("day = %v while the budget paid, want 0", d)
	}
	if !admits(p, cand) {
		t.Fatal("the budget is spent, the day is not: want admitted")
	}
	spendAs(p, cand, 0.004)
	if d := scopeLevel(p, scope, "usd/calendar-day"); !usdNear(d, 0.004) {
		t.Fatalf("day = %v, want 0.004 once the budget is spent", d)
	}
	if m := scopeLevel(p, scope, "usd/calendar-month"); !usdNear(m, 104.544) {
		t.Errorf("month ceiling = %v, want 104.544", m)
	}
	spendAs(p, cand, 0.01)
	if admits(p, cand) {
		t.Fatal("day spent too: want refused")
	}
}

// Two unbanked budgets: the one expiring soonest pays first, whatever was made
// first; a cost past its remainder spills into the next, then the day.
func TestUnbankedBudgetsSoonestFirstAndSpill(t *testing.T) {
	p, _, cand := dayRig(t)
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	clockAt(p, &now)
	scope := config.ProviderScopeKey("anthropic")
	late := addBudget(t, p, "anthropic", 2, now.Add(48*time.Hour), false, now)
	soon := addBudget(t, p, "anthropic", 1, now.Add(12*time.Hour), false, now)

	spendAs(p, cand, 0.5)
	if s, l := grantSpent(t, p, scope, soon), grantSpent(t, p, scope, late); s != 0.5 || l != 0 {
		t.Fatalf("soon %v late %v, want 0.5 and 0", s, l)
	}
	spendAs(p, cand, 3) // 0.5 to soon, 2 to late, 0.5 to the day
	if s, l, d := grantSpent(t, p, scope, soon), grantSpent(t, p, scope, late), scopeLevel(p, scope, "usd/calendar-day"); s != 1 || l != 2 || d != 0.5 {
		t.Fatalf("soon %v late %v day %v, want 1, 2, 0.5", s, l, d)
	}
	if m := scopeLevel(p, scope, "usd/calendar-month"); m != 3.5 {
		t.Errorf("month ceiling = %v, want 3.5", m)
	}
}

// The month ceiling full refuses, with the budget and the day still holding money.
func TestMonthCeilingFullBlocksWithChunkMoneyLeft(t *testing.T) {
	p, _, cand := limitRig(t, config.LimitSet{
		{USD: 10, Per: "calendar-month", Hard: true, Renews: 15},
		{USD: 5, Per: "calendar-day", Hard: true},
	})
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	clockAt(p, &now)
	addBudget(t, p, "anthropic", 20, now.Add(48*time.Hour), false, now)
	spendAs(p, cand, 10)
	if admits(p, cand) {
		t.Fatal("a full month ceiling must refuse whatever chunk has room")
	}
	if _, wait := p.filterByQuota([]config.Candidate{cand}); wait != 7*24*time.Hour-10*time.Hour {
		t.Errorf("wait = %v, want until the 15th", wait)
	}
}

// A total limit is a banked chunk spent last: after the day and after a banked
// budget.
func TestTotalLimitIsSpentLast(t *testing.T) {
	p, _, cand := limitRig(t, config.LimitSet{
		{USD: 1, Per: "calendar-day", Hard: true},
		{USD: 20, Per: "total", Hard: true},
	})
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	clockAt(p, &now)
	scope := config.ProviderScopeKey("anthropic")
	id := addBudget(t, p, "anthropic", 2, now.Add(48*time.Hour), true, now)
	spendAs(p, cand, 1)
	spendAs(p, cand, 1)
	if d, b, tot := scopeLevel(p, scope, "usd/calendar-day"), grantSpent(t, p, scope, id), scopeLevel(p, scope, "usd/total"); d != 1 || b != 1 || tot != 0 {
		t.Fatalf("day %v budget %v total %v, want 1, 1, 0", d, b, tot)
	}
	spendAs(p, cand, 1.5)
	if b, tot := grantSpent(t, p, scope, id), scopeLevel(p, scope, "usd/total"); b != 2 || tot != 0.5 {
		t.Fatalf("budget %v total %v, want 2 and 0.5", b, tot)
	}
	rep, err := p.Spend(now)
	if err != nil {
		t.Fatal(err)
	}
	a := rep.Providers[0].Account
	if strings.Join(a.DrainOrder, ",") != "prov:anthropic calendar-day,prov:anthropic total" || a.DrainNote != "" {
		t.Errorf("drain = %v %q (the spent budget is not listed)", a.DrainOrder, a.DrainNote)
	}
	if a.AvailableUSD == nil || *a.AvailableUSD != 19.5 {
		t.Errorf("available = %v, want 19.5", a.AvailableUSD)
	}
}
