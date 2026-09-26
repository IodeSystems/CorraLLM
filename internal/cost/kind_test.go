package cost

import (
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/iodesystems/corrallm/internal/config"
)

// --- what kind of money, and what a comparison defaults to -----------------

func TestIsPaidTypeAndRate(t *testing.T) {
	m := model()
	if !m.IsPaidType("claude") {
		t.Error("claude declares a costFactor and must read as paid")
	}
	if m.IsPaidType("local") {
		t.Error("local bills energy, not a provider's rates")
	}
	// An undeclared type is not paid — it bills $0 of energy, which is what the
	// energy branch already does. Reading it as paid would put an unpriced
	// backend's zero into the "a provider billed us" column.
	if m.IsPaidType("nonesuch") {
		t.Error("an unconfigured type must not read as paid")
	}
	approx(t, m.CostPerKwh(), 0.14)
}

// PaidTypes is CHEAPEST FIRST because it is what a "would have cost" comparison
// defaults to, and the cheapest alternative makes the claimed saving a floor
// rather than the most flattering number available.
func TestPaidTypesCheapestFirst(t *testing.T) {
	m := NewModel(&config.Config{
		CostPerKwh: 0.14,
		CommandCosts: map[string]map[string]any{
			"chat": {"generateWattsPerToken": 0.0013, "processWattsPerToken": 0.00005},
			"expensive": {
				"extract": map[string]any{"generateCostFactor": 2.5e-5, "processCostFactor": 5e-6},
			},
			"cheap": {
				"extract": map[string]any{"generateCostFactor": 5e-6, "processCostFactor": 1e-6},
			},
			"middling": {
				"extract": map[string]any{"generateCostFactor": 1.5e-5, "processCostFactor": 3e-6},
			},
		},
	})
	got := m.PaidTypes()
	want := []string{"cheap", "middling", "expensive"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	// The energy-priced type is not offered at all: comparing electricity
	// against electricity reports a saving of nothing.
	for _, typ := range got {
		if typ == "chat" {
			t.Error("chat bills energy and must not be offered as a paid baseline")
		}
	}
}

func TestNewModelNilConfigPricesNothing(t *testing.T) {
	m := NewModel(nil)
	approx(t, m.CostPerKwh(), 0)
	approx(t, m.RequestUSD("chat", 1000, 500), 0)
	if len(m.PaidTypes()) != 0 {
		t.Error("a nil config configures no paid types")
	}
}

// THE CACHE READ IS WHERE THE TWO PRICING MODELS DIFFER MOST, and the comparison
// exists to show it: a cached prefix is free locally (KV cache, no recompute)
// and billed at a reduced-but-real rate on a paid API.
func TestCachedTokensFreeLocallyBilledRemotely(t *testing.T) {
	m := NewModel(&config.Config{
		CostPerKwh: 0.14,
		CommandCosts: map[string]map[string]any{
			"chat": {"generateWattsPerToken": 0.0013, "processWattsPerToken": 0.00005},
			"paid": {"extract": map[string]any{
				"cachedCostFactor":   1e-7,
				"processCostFactor":  1e-6,
				"generateCostFactor": 5e-6,
			}},
		},
	})

	// Local: 1,000,000 cached tokens contribute exactly nothing.
	c, p, g := m.RequestUSDByClass("chat", 1_000_000, 0, 0)
	approx(t, c, 0)
	approx(t, p, 0)
	approx(t, g, 0)

	// Paid: the same tokens are $0.10 — small per token, and the largest single
	// class on this box's traffic, where cached tokens are 80% of the prompt.
	c, p, g = m.RequestUSDByClass("paid", 1_000_000, 0, 0)
	approx(t, c, 0.1)
	approx(t, p, 0)
	approx(t, g, 0)
}

// NoCharge is the third answer to "what kind of money is this", and it is not a
// rate — which is why it cannot be expressed as one. Each case below is a way
// the rate table alone gets it wrong.
func TestNoChargeNeedsBothADeclarationAndSomebodyElsesHardware(t *testing.T) {
	m := model()
	remote := func(mo config.Model) config.Model {
		var n yaml.Node
		n.SetString("openrouter.ai:443")
		mo.Proxy = n
		return mo
	}
	free := &config.FreeTier{Provider: "openrouter"}

	// The case this exists for: a declared free tier on an energy-priced type.
	// Every pooled free model carries `type: chat`, so without the declaration
	// the rate table bills this box's watt-hours for OpenRouter's work.
	if !m.NoCharge(remote(config.Model{Type: "local", FreeTier: free})) {
		t.Error("a declared free tier on an energy-priced remote charges nothing")
	}
	// No declaration: nothing says what the host charges, so nothing may claim
	// it charges zero. This stays a config gap, and is reported as one.
	if m.NoCharge(remote(config.Model{Type: "local"})) {
		t.Error("an undeclared remote must not read as free — that is the gap, not the answer")
	}
	// A type that names rates wins. A `freeTier` block can carry a usd spend cap
	// (config.FreeTier.Limits), so the block alone cannot assert $0.
	if m.NoCharge(remote(config.Model{Type: "claude", FreeTier: free})) {
		t.Error("a backend whose type bills per token is paid, whatever else is declared")
	}
	// OUR OWN HARDWARE IS NEVER FREE. The electricity is real however the config
	// is written, and reporting it as somebody's gift is the worst failure here.
	if m.NoCharge(config.Model{Type: "local", Cmd: "llama-server", FreeTier: free}) {
		t.Error("a model this box runs burns power; a freeTier block does not change that")
	}
	// An unpriced type is not a free tier either — it is a type nobody costed.
	if m.NoCharge(remote(config.Model{Type: "nonesuch"})) {
		t.Error("an unconfigured type is unpriced, not declared free")
	}
}
