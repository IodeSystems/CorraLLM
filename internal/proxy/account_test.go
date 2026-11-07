package proxy

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/store"
)

// fireworksRig is the live Fireworks shape beside the Claude model: the provider
// $5/calendar-day hard, its credential $100/calendar-month hard.
func fireworksRig(t *testing.T) (*Proxy, *store.Store, config.Candidate) {
	t.Helper()
	p, st, _ := spendRig(t, "http://127.0.0.1:1", func(c *config.Config) {
		fw := claudeModel(t, "http://127.0.0.1:1", "")
		fw.Proxy.Content[5].Value = "openai" // api: anthropic → openai
		fw.Extension, fw.ProviderName = "fx", "fireworks"
		c.Models["fireworks-glm"] = fw
		c.Extensions = map[string]config.Extension{"fx": {Providers: map[string]config.Provider{"fireworks": {
			Limits: config.LimitSet{{USD: 5, Per: "calendar-day", Hard: true}},
			Credentials: []config.Credential{{Name: "default",
				Limits: config.LimitSet{{USD: 100, Per: "calendar-month", Hard: true}}}},
		}}}}
	})
	cands, ok := p.config().ResolveServed("fireworks-glm")
	if !ok || len(cands) != 1 || cands[0].Credential == nil {
		t.Fatalf("candidates = %+v", cands)
	}
	return p, st, cands[0]
}

// spendAs charges usd the way a served request does: past the full day, the
// timed budget pays; every scope counts it.
func spendAs(p *Proxy, cand config.Candidate, usd float64) {
	for _, sc := range cand.QuotaScopes(p.config()) {
		if config.IsProviderScope(sc) && p.quota.LimitFull(sc) {
			p.grants.spend(p.store, sc, cand.Name, usd)
		}
		p.quota.Charge(sc, config.DimUSD, usd)
	}
}

func fireworks(t *testing.T, p *Proxy, now time.Time) SpendProvider {
	t.Helper()
	rep, err := p.Spend(now)
	if err != nil {
		t.Fatal(err)
	}
	for _, pr := range rep.Providers {
		if pr.Provider == "fireworks" {
			return pr
		}
	}
	t.Fatalf("no fireworks in %+v", rep.Providers)
	return SpendProvider{}
}

func usdNear(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestFireworksAccountChunksCeilingsAndDrain(t *testing.T) {
	p, _, cand := fireworksRig(t)
	now := time.Now().UTC()
	exp := now.Add(48 * time.Hour)
	if err := p.AddBudget("fireworks", 10, "", exp.Format(time.RFC3339), "bank", now); err != nil {
		t.Fatal(err)
	}
	spendAs(p, cand, 3)
	pr := fireworks(t, p, now)
	a := pr.Account
	if a == nil {
		t.Fatal("no account")
	}
	raw, _ := json.MarshalIndent(a, "", "  ")
	t.Logf("account:\n%s", raw)
	// $2 of the day, then the $10 budget; the credential's $97 does not bind.
	if a.AvailableUSD == nil || !usdNear(*a.AvailableUSD, 12) || a.Overdrawn {
		t.Fatalf("available = %v, overdrawn %v", a.AvailableUSD, a.Overdrawn)
	}
	if len(a.Chunks) != 2 {
		t.Fatalf("chunks = %+v", a.Chunks)
	}
	day, bud := a.Chunks[0], a.Chunks[1]
	midnight := config.PeriodStart("day", 0, now).AddDate(0, 0, 1).Format(time.RFC3339)
	if day.Kind != "day" || day.USD != 5 || !usdNear(day.LeftUSD, 2) || day.Banked || day.RefillsAt != midnight || day.Expires != "" {
		t.Errorf("day chunk = %+v", day)
	}
	if bud.Kind != "budget" || bud.USD != 10 || bud.LeftUSD != 10 || !bud.Banked || bud.Expires != exp.Format(time.RFC3339) || bud.Note != "bank" {
		t.Errorf("budget chunk = %+v", bud)
	}
	if len(a.Ceilings) != 1 {
		t.Fatalf("ceilings = %+v", a.Ceilings)
	}
	c := a.Ceilings[0]
	monthEnd := config.PeriodStart("month", 0, now).AddDate(0, 1, 0).Format(time.RFC3339)
	if c.Scope != "cred:fireworks/default" || c.Window != "calendar-month" || c.USD != 100 || !usdNear(c.UsedUSD, 3) || c.ResetsAt != monthEnd {
		t.Errorf("ceiling = %+v", c)
	}
	if a.Drain != "soonest-expiring first" || strings.Join(a.DrainOrder, ",") != day.Name+","+bud.Name || a.DrainNote != "" {
		t.Errorf("drain = %q %v %q", a.Drain, a.DrainOrder, a.DrainNote)
	}
	// The legacy fields come from the credential's month.
	if pr.LimitUSD == nil || *pr.LimitUSD != 100 || len(pr.Budgets) != 1 {
		t.Errorf("provider = %+v", pr)
	}

	// Past the day: the budget pays, and the day is spent, not overdrawn.
	spendAs(p, cand, 2)
	spendAs(p, cand, 3)
	a = fireworks(t, p, now).Account
	if !usdNear(a.Chunks[0].LeftUSD, 0) || !usdNear(a.Chunks[1].LeftUSD, 7) || a.Overdrawn {
		t.Fatalf("after $8: chunks = %+v", a.Chunks)
	}
	if !usdNear(*a.AvailableUSD, 7) {
		t.Errorf("after $8: available = %v, want the budget's $7", *a.AvailableUSD)
	}
	if !usdNear(a.Ceilings[0].UsedUSD, 8) {
		t.Errorf("after $8: the month counts all of it: %+v", a.Ceilings[0])
	}
}

func TestAccountOverdrawn(t *testing.T) {
	p, _, cand := fireworksRig(t)
	now := time.Now().UTC()
	spendAs(p, cand, 4)
	spendAs(p, cand, 1.5) // the request that crosses the line is charged in full
	a := fireworks(t, p, now).Account
	if !usdNear(a.Chunks[0].LeftUSD, -0.5) || !a.Overdrawn || a.OverdrawnUSD == nil || !usdNear(*a.OverdrawnUSD, 0.5) {
		t.Fatalf("chunks = %+v, overdrawn %v %v", a.Chunks, a.Overdrawn, a.OverdrawnUSD)
	}
	if !usdNear(*a.AvailableUSD, -0.5) {
		t.Errorf("available = %v", *a.AvailableUSD)
	}
}

func TestDrainNoteSaysWhereTheLedgerIsNotSoonestExpiring(t *testing.T) {
	limits := []AccountChunk{{Name: "day", Kind: "day", RefillsAt: "2026-11-08T00:00:00Z"}}
	soon := []AccountChunk{{Name: "budget 1", Kind: "budget", Expires: "2026-11-07T18:00:00Z"}}
	order, note := drainOrder(limits, soon)
	if strings.Join(order, ",") != "day,budget 1" || !strings.Contains(note, "budget 1 expires 2026-11-07T18:00:00Z, before day refills") {
		t.Errorf("order %v, note %q", order, note)
	}
	if _, note := drainOrder(limits, []AccountChunk{{Name: "b", Expires: "2026-11-09T00:00:00Z"}}); note != "" {
		t.Errorf("a budget outliving the day: note %q", note)
	}
	once := []AccountChunk{{Name: "once", Kind: "once", Banked: true}}
	if _, note := drainOrder(once, soon); !strings.Contains(note, "once (banked, no expiry) is spent before budget 1") {
		t.Errorf("banked limit first: note %q", note)
	}
}

func TestPacedDayAccountTimedBudgetWaits(t *testing.T) {
	p, _, cand := limitRig(t, config.LimitSet{
		{USD: 10, Per: "calendar-month", Hard: true, Renews: 15},
		{Pace: true, Per: "calendar-day", Hard: true},
	})
	now := time.Now().UTC()
	if err := p.AddBudget("anthropic", 3, "48h", "", "", now); err != nil {
		t.Fatal(err)
	}
	p.quota.Charge(config.ProviderScopeKey("anthropic"), config.DimUSD, 0.1)
	_ = cand
	rep, err := p.Spend(now)
	if err != nil {
		t.Fatal(err)
	}
	a := rep.Providers[0].Account
	allow := config.PacedAllowance(10, 0.1, 0.1, 15, now)
	// The paced day binds and a timed budget does not pay past it.
	if a == nil || a.AvailableUSD == nil || !usdNear(*a.AvailableUSD, allow-0.1) {
		t.Fatalf("account = %+v, want today's share less its spend", a)
	}
	if a.Chunks[0].Kind != "paced-day" || !usdNear(a.Chunks[0].USD, allow) || len(a.Ceilings) != 1 {
		t.Errorf("chunks %+v ceilings %+v", a.Chunks, a.Ceilings)
	}
}

func TestClaudeStaysFirstAndOtherProvidersFollow(t *testing.T) {
	p, st, _ := fireworksRig(t)
	now := time.Now()
	spendRow(t, st, "claude", 1.5, now.Add(-time.Second))
	spendRow(t, st, "fireworks-glm", 0.25, now.Add(-time.Second))
	rep, err := p.Spend(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Providers) != 2 || rep.Providers[0].Provider != "anthropic" || rep.Providers[1].Provider != "fireworks" {
		t.Fatalf("providers = %+v", rep.Providers)
	}
	c, f := rep.Providers[0], rep.Providers[1]
	if c.EstimatedUSD != 1.5 || c.LimitUSD != nil || c.RemainingUSD != nil || c.Source != "estimate" {
		t.Errorf("claude = %+v", c)
	}
	if f.EstimatedUSD != 0.25 || f.Source != "estimate" || f.BilledUSD != nil {
		t.Errorf("fireworks = %+v", f)
	}
}
