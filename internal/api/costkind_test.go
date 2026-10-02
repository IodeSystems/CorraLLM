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
			// THE GAP: a third-party host on an energy-priced type, with NOTHING
			// declared about what it charges. Its tokens were processed on somebody
			// else's hardware and then charged against this box's watt-hour
			// coefficients.
			"openrouter-something": {Type: "chat", Proxy: proxyNode("openrouter.ai", 443)},
			// DECLARED FREE. The shape config.VirtualModelFor produces for a pooled
			// model: the source provider's proxy, the template's `type: chat`, and
			// the template's freeTier block copied onto it. Same energy-priced type
			// as the row above — the declaration is the only difference, which is
			// the point.
			"openrouter-free-thing": {
				Type:     "chat",
				Proxy:    proxyNode("openrouter.ai", 443),
				FreeTier: &config.FreeTier{Provider: "openrouter", Refresh: true},
			},
			// A freeTier block does NOT outrank a type that names rates: if the
			// backend bills, it is paid, whatever else is declared about it. This
			// is what keeps a `usd` spend cap under freeTier from reading as $0.
			"claude-with-freetier": {
				Type:     "anthropic-haiku",
				Proxy:    proxyNode("api.anthropic.com", 443),
				FreeTier: &config.FreeTier{Provider: "anthropic"},
			},
			// A freeTier block on a model THIS BOX RUNS is not free — the
			// electricity is real. Remote() is the guard, and it is tested because
			// the honest failure here would be silently reporting our own power as
			// somebody's gift.
			"local-with-freetier": {
				Type:     "chat",
				Cmd:      "llama-server --port 5801",
				FreeTier: &config.FreeTier{Provider: "confused"},
			},
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
		// DECLARED FREE, AND IN THE COMPARISON. No exclusion: what it cost is
		// $0 and $0 is not an estimate, so "what would this have cost on a paid
		// API" is answerable — and it is the traffic the question is most worth
		// asking about, since the work was real and nobody was billed.
		{"openrouter-free-thing", BillFree, ""},
		// The paid test runs first, so a declaration cannot talk a billing
		// backend into being free.
		{"claude-with-freetier", BillPaid, ExcludePaid},
		// Free is about SOMEBODY ELSE's hardware. Ours burns power whatever the
		// config claims.
		{"local-with-freetier", BillEnergy, ""},
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
	// THE LIVE ROW'S NUMBERS, to the cent: openrouter-cohere-north-mini-code over
	// 24 h to 2026-09-26 recorded 13,352 prompt / 2,155 completion at
	// $0.000485674 — which is 13,352 × 0.00005 Wh + 2,155 × 0.0013 Wh = 3.469 Wh
	// × $0.14/kWh, i.e. this box's coefficients on OpenRouter's hardware.
	//
	// The window's seven requests are collapsed into one metered row: the rollup
	// sums rows, so the per-request split changes no figure under test, and
	// keeping the token counts exact is what makes the arithmetic checkable.
	must(t, st.InsertActivity(store.Activity{
		TS: now.UnixMilli(), Served: "openrouter-free-thing", Status: 200,
		PromptTokens: 13_352, CompletionTokens: 2_155, CostUSD: 0.000485674,
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

	// DECLARED FREE: $0 BECAUSE THAT IS THE PRICE, and the comparison prices it.
	// The recorded dollars are not charged to any kind — they were this box's
	// watt-hours applied to OpenRouter's hardware — but they are not discarded
	// either: mispricedUsd carries them so a total that shrank can be checked.
	free := byName["openrouter-free-thing"]
	if free.Billing != BillFree {
		t.Errorf("free-tier billing = %q, want free", free.Billing)
	}
	near(t, "free costUsd", free.CostUSD, 0)
	near(t, "free energyUsd", free.EnergyUSD, 0)
	near(t, "free unattributedUsd", free.UnattributedUSD, 0)
	near(t, "free mispricedUsd", free.MispricedUSD, 0.000485674)
	if free.FreeRequests != free.Requests || free.FreeRequests == 0 {
		t.Errorf("freeRequests = %d, want requests (%d) — the $0 needs a count behind it",
			free.FreeRequests, free.Requests)
	}
	// IN the comparison, and this is the whole reason free traffic is not
	// excluded: 13,352 processed × 1e-6 + 2,155 generated × 5e-6, nothing cached.
	if free.BaselineExcluded != "" {
		t.Errorf("free baselineExcluded = %q, want covered — $0 is not an estimate", free.BaselineExcluded)
	}
	near(t, "free baseline processed", free.BaselineProcess, 0.013352)
	near(t, "free baseline generated", free.BaselineGenerate, 0.010775)
	near(t, "free baseline total", free.BaselineUSD, 0.024127)

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
	// The free row adds its baseline to the comparison and nothing to the spend.
	near(t, "total baselineUsd", tot.BaselineUSD, 0.53+0.024127)
	near(t, "total mispricedUsd", tot.MispricedUSD, 0.000485674)
	if tot.FreeRequests != 1 {
		t.Errorf("total freeRequests = %d, want 1", tot.FreeRequests)
	}
	// And the three PRICED kinds still add up to the one figure they were split
	// from. Free is outside the sum by construction, which is why costUsd on that
	// row is 0 — a "free" row carrying dollars would contradict its own label.
	near(t, "split sums to costUsd", tot.EnergyUSD+tot.PaidUSD+tot.UnattributedUSD, tot.CostUSD)
}

// A DECLARED FREE TIER THAT STARTED CHARGING MUST NOT READ AS $0.
//
// The meter lets a provider-reported cost outrank the rate table, so a pooled
// model that quietly went paid under its own freeTier declaration records real
// spend. Dropping that as "the phantom" would hide a bill — the failure this
// whole split exists to prevent, pointed the other way. The energy table is the
// ceiling of what the phantom can be, so anything above it is somebody's charge
// and keeps the name for money nothing explains.
func TestUsageRollupKeepsARealChargeOnADeclaredFreeBackend(t *testing.T) {
	st := openTestStore(t)
	// Same tokens as the live row, whose energy-table figure is $0.000485674 —
	// but recorded at a hundred times that, which no watt-hour coefficient here
	// could produce.
	must(t, st.InsertActivity(store.Activity{
		TS: time.Now().UnixMilli(), Served: "openrouter-free-thing", Status: 200,
		PromptTokens: 13_352, CompletionTokens: 2_155, CostUSD: 0.05,
	}))
	h := &Handlers{Store: st, Cfg: costFixtureConfig()}
	out, err := h.UsageRollup(context.Background(), &UsageRollupInput{WindowHours: 1})
	if err != nil {
		t.Fatal(err)
	}
	got := out.Body.Rows[0]
	if got.Billing != BillUnattributed {
		t.Errorf("billing = %q, want unattributed — a charge above the energy ceiling is real money", got.Billing)
	}
	near(t, "the charge survives in costUsd", got.CostUSD, 0.05)
	near(t, "and is reported as unattributed", got.UnattributedUSD, 0.05)
	near(t, "nothing is written off", got.MispricedUSD, 0)
	if got.FreeRequests != 0 {
		t.Errorf("freeRequests = %d, want 0 — this traffic was not free", got.FreeRequests)
	}
	// And it leaves the comparison, because its figure is a provider's number on
	// a backend nothing here can price.
	if got.BaselineExcluded != ExcludeOffBox {
		t.Errorf("baselineExcluded = %q, want off-box", got.BaselineExcluded)
	}
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

// TestUsageRollupPricesCostEquivalent: a model that declares a costEquivalent
// is priced at THAT paid type; one that does not keeps the window's default;
// one naming a type with no per-token rates falls back rather than reading $0.
func TestUsageRollupPricesCostEquivalent(t *testing.T) {
	st := openTestStore(t)
	now := time.Now()
	for _, served := range []string{"local-chat", "local-sonnetish", "local-badequiv"} {
		must(t, st.InsertActivity(store.Activity{
			TS: now.UnixMilli(), Served: served, Status: 200,
			PromptTokens: 1_000_000, CachedTokens: 800_000, CompletionTokens: 50_000, CostUSD: 0.35,
		}))
	}
	cfg := costFixtureConfig()
	cfg.Models["local-sonnetish"] = config.Model{Type: "chat", Cmd: "llama-server --port 5801", CostEquivalent: "anthropic-sonnet"}
	cfg.Models["local-badequiv"] = config.Model{Type: "chat", Cmd: "llama-server --port 5802", CostEquivalent: "chat"}

	out, err := (&Handlers{Store: st, Cfg: cfg}).UsageRollup(context.Background(), &UsageRollupInput{WindowHours: 1})
	if err != nil {
		t.Fatal(err)
	}
	if out.Body.BaselineType != "anthropic-haiku" {
		t.Errorf("window default = %q, want anthropic-haiku", out.Body.BaselineType)
	}
	by := map[string]RollupRow{}
	for _, r := range out.Body.Rows {
		by[r.Served] = r
	}
	if r := by["local-chat"]; r.BaselineType != "anthropic-haiku" {
		t.Errorf("local-chat baselineType = %q, want the default", r.BaselineType)
	} else {
		near(t, "local-chat at haiku", r.BaselineUSD, 0.53)
	}
	r := by["local-sonnetish"]
	if r.BaselineType != "anthropic-sonnet" {
		t.Errorf("local-sonnetish baselineType = %q, want anthropic-sonnet", r.BaselineType)
	}
	// 800,000 × 3e-7 + 200,000 × 3e-6 + 50,000 × 1.5e-5 = 0.24 + 0.60 + 0.75.
	near(t, "local-sonnetish at sonnet", r.BaselineUSD, 1.59)
	if r := by["local-badequiv"]; r.BaselineType != "anthropic-haiku" {
		t.Errorf("an equivalent with no per-token rates must fall back, got %q", r.BaselineType)
	}
	if out.Body.Total.BaselineType != "" {
		t.Errorf("total baselineType = %q, want empty (it spans types)", out.Body.Total.BaselineType)
	}
	near(t, "total baseline", out.Body.Total.BaselineUSD, 0.53+1.59+0.53)
}

// TestUsageRollupHeatAndPerf: heat is metered energy x 3412.14 BTU/kWh,
// averaged over the window for BTU/h, and the per-request speed distributions
// are recomputed on the total rather than left unset.
func TestUsageRollupHeatAndPerf(t *testing.T) {
	st := openTestStore(t)
	now := time.Now()
	must(t, st.InsertActivity(store.Activity{
		TS: now.UnixMilli(), Served: "local-chat", Status: 200,
		PromptTokens: 1000, CachedTokens: 500, CompletionTokens: 100, CostUSD: 0.35,
		PromptPerSec: 800, PredictedPerSec: 100,
	}))
	out, err := (&Handlers{Store: st, Cfg: costFixtureConfig()}).UsageRollup(context.Background(), &UsageRollupInput{WindowHours: 2})
	if err != nil {
		t.Fatal(err)
	}
	tot := out.Body.Total
	// $0.35 at $0.14/kWh = 2.5 kWh = 8530.35 BTU, over a 2 h window.
	near(t, "energyBtu", tot.EnergyBTU, 2.5*3412.14)
	near(t, "heatBtuPerHour", tot.HeatBTUh, 2.5*3412.14/2)
	if tot.TgN != 1 || tot.TgAvg != 100 || tot.TpAvg != 800 || tot.CacheN != 1 || tot.CacheAvg != 0.5 {
		t.Errorf("total perf = tg %d/%v tp %v cache %d/%v", tot.TgN, tot.TgAvg, tot.TpAvg, tot.CacheN, tot.CacheAvg)
	}
}
