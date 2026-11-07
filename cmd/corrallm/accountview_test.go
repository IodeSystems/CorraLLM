package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/proxy"
)

func f64(v float64) *float64 { return &v }

func TestSpendTextPrintsTheAccount(t *testing.T) {
	rep := proxy.SpendReport{Providers: []proxy.SpendProvider{{Provider: "fireworks", Period: "2026-11", Source: "estimate",
		Account: &proxy.Account{AvailableUSD: f64(12), Drain: "soonest-expiring first",
			DrainOrder: []string{"prov:fireworks calendar-day", "budget 1"},
			Chunks: []proxy.AccountChunk{
				{Name: "prov:fireworks calendar-day", Kind: "day", USD: 5, LeftUSD: 2, RefillsAt: "2026-11-08T00:00:00Z"},
				{Name: "budget 1", Kind: "budget", USD: 10, LeftUSD: 10, Banked: true, Expires: "2026-11-09T00:00:00Z", Note: "bank"},
			},
			Ceilings: []proxy.AccountCeiling{{Scope: "cred:fireworks/default", Window: "calendar-month", USD: 100, UsedUSD: 3, ResetsAt: "2026-12-01T00:00:00Z"}},
		}}}}
	got := spendText(rep)
	for _, want := range []string{
		"    available: $12.00\n",
		"    chunk prov:fireworks calendar-day (day): $2.00 left of $5.00, refills 2026-11-08T00:00:00Z\n",
		"    chunk budget 1 (budget, banked): $10.00 left of $10.00, expires 2026-11-09T00:00:00Z (bank)\n",
		"    ceiling cred:fireworks/default calendar-month: $3.00 used of $100.00, resets 2026-12-01T00:00:00Z\n",
		"    drain: soonest-expiring first (order: prov:fireworks calendar-day, budget 1)\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestKeysListPrintsEffectiveAndWhoseLimit(t *testing.T) {
	raw := []byte(`{"self":{"account":{"budgets":[{"selector":"provider:fireworks","available_usd":19,"overdrawn":false,
		"chunks":[],"ceilings":[],"drain":"soonest-expiring first","effective_usd":4,"limited_by":"provider fireworks"}],
		"subkeys":[{"key":"ck-1","label":"tbench","spent_today_usd":1,"budgets":[{"selector":"provider:fireworks",
		"available_usd":-0.5,"overdrawn":true,"overdrawn_usd":0.5,"chunks":[],"ceilings":[],"drain":"soonest-expiring first",
		"effective_usd":-0.5,"limited_by":"subkey tbench"}]}]}},"keys":[]}`)
	var b bytes.Buffer
	printKeyAccount(&b, raw)
	got := b.String()
	for _, want := range []string{
		"    provider:fireworks: effective $4.00 (limited by provider fireworks)\n",
		"      available: $19.00\n",
		"  subkey tbench: $1.00 spent today (UTC)\n",
		"    provider:fireworks: effective -$0.50 (limited by subkey tbench)\n",
		"      OVERDRAWN by $0.50",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	b.Reset()
	printKeyAccount(&b, []byte(`{"self":{},"keys":[]}`))
	if b.Len() != 0 {
		t.Errorf("an older server's reply: %q", b.String())
	}
}
