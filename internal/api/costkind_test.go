package api

import (
	"context"
	"math"
	"strconv"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/cost"
	"github.com/iodesystems/corrallm/internal/store"
)

// A `proxy:` pointing at a third-party host — the "host corrallm does not run"
// that config.Model.Remote() answers to.
func proxyNode(host string, port int) yaml.Node {
	var n yaml.Node
	n.SetString(host + ":" + strconv.Itoa(port))
	return n
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// The live box's shape, reduced to what the classifier reads: an energy-priced
// local chat model, a paid remote one, a remote model on the ENERGY type (the
// config gap this exists to make visible), an audio model, and an image model
// whose local coefficient is unset.
func costFixtureConfig() *config.Config {
	return &config.Config{
		CostPerKwh: 0.14,
		CommandCosts: map[string]map[string]any{
			"chat":      {"generateWattsPerToken": 0.0013, "processWattsPerToken": 0.00005},
			"stt":       {"audioWhPerMiB": 0.03},
			"diffusion": {
				// imageWhPerImage deliberately UNSET, matching the live config —
				// an image request therefore costs $0 by omission.
			},
			"anthropic-haiku": {"extract": map[string]any{
				"cachedCostFactor":   1e-7,
				"processCostFactor":  1e-6,
				"generateCostFactor": 5e-6,
			}},
			"anthropic-sonnet": {"extract": map[string]any{
				"cachedCostFactor":   3e-7,
				"processCostFactor":  3e-6,
				"generateCostFactor": 1.5e-5,
			}},
		},
		Models: map[string]config.Model{
			// Spawned here: its watt-hours are this fleet's.
			"local-chat":   {Type: "chat", Cmd: "llama-server --port 5800"},
			"oidio-stt":    {Type: "stt", Cmd: "oidio serve"},
			"local-image":  {Type: "diffusion", Cmd: "sd-server"},
			"claude-haiku": {Type: "anthropic-haiku", Proxy: proxyNode("api.anthropic.com", 443)},
			// THE GAP: a third-party host on an energy-priced type. Its tokens
			// were processed on somebody else's hardware and then charged against
			// this box's watt-hour coefficients.
			"openrouter-something": {Type: "chat", Proxy: proxyNode("openrouter.ai", 443)},
		},
	}
}

func TestClassifyBillingKinds(t *testing.T) {
	cfg := costFixtureConfig()
	cm := cost.NewModel(cfg)
	known := cfg.AllModels()

	for _, tc := range []struct {
		served  string
		billing string
		exclude string
	}{
		{"local-chat", BillEnergy, ""},
		{"claude-haiku", BillPaid, ExcludePaid},
		{"oidio-stt", BillEnergy, ExcludeAudio},
		{"local-image", BillEnergy, ExcludeImage},
		{"openrouter-something", BillUnattributed, ExcludeOffBox},
		// A model that has left the config. Activity is retained 30 days and a
		// model can be removed inside that, so nothing is left to say what
		// priced its rows.
		{"deleted-model", BillUnattributed, ExcludeUnknownModel},
		// classify() decides the kind; ExcludeNoTokens is the ROLLUP's call,
		// because only the window knows whether any tokens were recorded, and it
		// outranks whatever classify returned here.
	} {
		got := classify(known, cm, tc.served)
		if got.Billing != tc.billing || got.Exclude != tc.exclude {
			t.Errorf("%s: billing=%q exclude=%q, want %q/%q",
				tc.served, got.Billing, got.Exclude, tc.billing, tc.exclude)
		}
	}
}

func TestBaselineTypeHonoursConfigThenCheapest(t *testing.T) {
	cfg := costFixtureConfig()
	cm := cost.NewModel(cfg)

	// Unset → the cheapest configured paid type, by generated-token rate.
	if got := baselineType(cfg, cm); got != "anthropic-haiku" {
		t.Errorf("default baseline = %q, want anthropic-haiku", got)
	}
	// Declared → honoured.
	cfg.CostBaseline = "anthropic-sonnet"
	if got := baselineType(cfg, cm); got != "anthropic-sonnet" {
		t.Errorf("declared baseline = %q, want anthropic-sonnet", got)
	}
	// Declared but NOT a paid type → ignored, because comparing electricity
	// against electricity reports a saving of nothing. It falls back rather than
	// producing a silent $0.
	cfg.CostBaseline = "chat"
	if got := baselineType(cfg, cm); got != "anthropic-haiku" {
		t.Errorf("energy-typed baseline = %q, want the paid fallback", got)
	}
	// Nothing paid configured at all → no comparison, and the UI says so rather
	// than showing a zero.
	bare := &config.Config{CostPerKwh: 0.14, CommandCosts: map[string]map[string]any{
		"chat": {"generateWattsPerToken": 0.0013},
	}}
	if got := baselineType(bare, cost.NewModel(bare)); got != "" {
		t.Errorf("no paid type configured: baseline = %q, want empty", got)
	}
}

// The whole feature end to end, on the shape of the real 24h window: one busy
// local chat model with a high cache hit rate, one off-box row, one audio row.
func TestUsageRollupSplitsCostAndPricesBaseline(t *testing.T) {
	st := openTestStore(t)
	now := time.Now()

	// Cache hit rate 80%: prompt 1,000,000 of which 800,000 cached, so 200,000
	// were actually processed. This is the subtraction the whole three-class
	// vocabulary rests on — prompt_tokens INCLUDES the cached prefix.
	must(t, st.InsertActivity(store.Activity{
		TS: now.UnixMilli(), Served: "local-chat", Status: 200,
		PromptTokens: 1_000_000, CachedTokens: 800_000, CompletionTokens: 50_000,
		CostUSD: 0.35,
	}))
	must(t, st.InsertActivity(store.Activity{
		TS: now.UnixMilli(), Served: "openrouter-something", Status: 200,
		PromptTokens: 10_000, CompletionTokens: 2_000, CostUSD: 0.0005,
	}))
	must(t, st.InsertActivity(store.Activity{
		TS: now.UnixMilli(), Served: "oidio-stt", Status: 200,
		AudioBytes: 1 << 20, CostUSD: 0.000004,
	}))

	h := &Handlers{Store: st, Cfg: costFixtureConfig()}
	out, err := h.UsageRollup(context.Background(), &UsageRollupInput{WindowHours: 1})
	if err != nil {
		t.Fatal(err)
	}

	if out.Body.CostPerKwh != 0.14 {
		t.Errorf("costPerKwh = %v, want 0.14 — a cost figure without its rate cannot be checked", out.Body.CostPerKwh)
	}
	if out.Body.BaselineType != "anthropic-haiku" {
		t.Errorf("baselineType = %q, want anthropic-haiku", out.Body.BaselineType)
	}

	byName := map[string]RollupRow{}
	for _, r := range out.Body.Rows {
		byName[r.Served] = r
	}

	chat := byName["local-chat"]
	if chat.Billing != BillEnergy {
		t.Errorf("local-chat billing = %q, want energy", chat.Billing)
	}
	near(t, "local-chat energyUsd", chat.EnergyUSD, 0.35)
	near(t, "local-chat paidUsd", chat.PaidUSD, 0)
	near(t, "local-chat energyKwh", chat.EnergyKwh, 0.35/0.14)
	// 800,000 × 1e-7 + 200,000 × 1e-6 + 50,000 × 5e-6 = 0.08 + 0.20 + 0.25.
	near(t, "baseline cached", chat.BaselineCached, 0.08)
	near(t, "baseline processed", chat.BaselineProcess, 0.20)
	near(t, "baseline generated", chat.BaselineGenerate, 0.25)
	near(t, "baseline total", chat.BaselineUSD, 0.53)
	if chat.BaselineExcluded != "" {
		t.Errorf("local-chat baselineExcluded = %q, want covered", chat.BaselineExcluded)
	}

	// THE OFF-BOX ROW IS NOT ELECTRICITY. Its dollars came out of this box's
	// watt-hour coefficients applied to somebody else's hardware, and folding
	// them into an "electricity" figure is the claim this split exists to stop.
	off := byName["openrouter-something"]
	if off.Billing != BillUnattributed {
		t.Errorf("openrouter billing = %q, want unattributed", off.Billing)
	}
	near(t, "openrouter energyUsd", off.EnergyUSD, 0)
	near(t, "openrouter unattributedUsd", off.UnattributedUSD, 0.0005)
	near(t, "openrouter baselineUsd", off.BaselineUSD, 0)
	if off.BaselineExcluded != ExcludeOffBox {
		t.Errorf("openrouter baselineExcluded = %q, want off-box", off.BaselineExcluded)
	}

	// Audio bills per MiB. It is real electricity, and it has no term in a
	// per-token rate table, so it counts as energy and NOT in the comparison.
	stt := byName["oidio-stt"]
	if stt.Billing != BillEnergy || stt.BaselineExcluded != ExcludeAudio {
		t.Errorf("oidio-stt = %q/%q, want energy/audio", stt.Billing, stt.BaselineExcluded)
	}
	near(t, "oidio-stt baselineUsd", stt.BaselineUSD, 0)

	// The total keeps the three kinds apart and never names one of them.
	tot := out.Body.Total
	if tot.Billing != "" {
		t.Errorf("total billing = %q, want empty — a total spans kinds", tot.Billing)
	}
	near(t, "total energyUsd", tot.EnergyUSD, 0.350004)
	near(t, "total unattributedUsd", tot.UnattributedUSD, 0.0005)
	near(t, "total paidUsd", tot.PaidUSD, 0)
	near(t, "total baselineUsd", tot.BaselineUSD, 0.53)
	// And the three kinds still add up to the one figure they were split from.
	near(t, "split sums to costUsd", tot.EnergyUSD+tot.PaidUSD+tot.UnattributedUSD, tot.CostUSD)
}

// A window whose model recorded no tokens is not a $0 saving. Pricing zero
// tokens at any rate gives zero, and reporting that beside a real comparison
// reads as a measurement of nothing saved.
func TestUsageRollupExcludesTokenlessRows(t *testing.T) {
	st := openTestStore(t)
	must(t, st.InsertActivity(store.Activity{
		TS: time.Now().UnixMilli(), Served: "local-chat", Status: 500,
	}))
	h := &Handlers{Store: st, Cfg: costFixtureConfig()}
	out, err := h.UsageRollup(context.Background(), &UsageRollupInput{WindowHours: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Body.Rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(out.Body.Rows))
	}
	if out.Body.Rows[0].BaselineExcluded != ExcludeNoTokens {
		t.Errorf("baselineExcluded = %q, want no-tokens", out.Body.Rows[0].BaselineExcluded)
	}
}

func near(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
