package api

import (
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/cost"
)

// WHAT KIND OF MONEY A ROW OF USAGE IS.
//
// The activity log records one `cost_usd` per request and nothing about how it
// was arrived at, and "how" is the difference between two unrelated facts: a
// local backend's number is kilowatt-hours × the electricity rate, and a paid
// provider's is an invoice. Summing them produces a figure that is neither, and
// labelling that sum "electricity" — which is what the dashboard did — is a
// claim about a bill nobody pays.
//
// There is no column to read it off, so the class is resolved the only honest
// way available: the served model's name, looked up in the live config, gives
// its cost `type`, and the cost model already knows which types declare paid
// per-token rates. Nothing is stored; the answer is recomputed per request and
// follows the config, which is right — repricing a type is meant to move the
// history, and the plan records that being done deliberately (§8, the
// calibration backfill).
const (
	// BillEnergy — this fleet's electricity. An energy-priced type on a model
	// corrallm itself runs (its own box or an enrolled one). kWh × $/kWh.
	BillEnergy = "energy"
	// BillPaid — a provider's bill. The type declares per-token rates.
	BillPaid = "paid"
	// BillFree — somebody else's hardware, at no charge.
	//
	// A remote backend the operator has DECLARED a free tier for (`freeTier:`),
	// on a type naming no per-token rates. Not electricity — the tokens were
	// processed off this box. Not an invoice — nobody sends one. $0 because $0
	// is the price, which is the whole difference from BillUnattributed, where
	// $0 would mean a coefficient is missing.
	//
	// THE DECLARATION IS `freeTier`, AND cost.NoCharge IS THE ONE PLACE THAT
	// SAYS SO — the proxy's meter reads the same predicate, so what a request is
	// charged and what the dashboard calls it cannot disagree. What was rejected:
	//
	//   A flag on the cost TYPE. The type is a rate table — "how much" — and
	//   free is a fact about who serves it. On the live box `type: chat` is
	//   shared by local models and by every pooled remote, so a flag there
	//   cannot separate them without minting a new type and repointing models:
	//   a config migration to express something the model entry already carries.
	//   Put that flag on a type a LOCAL model uses and the box's own electricity
	//   reads as free.
	//
	//   All-zero cost factors. They do not express it at all — cost.paid() reads
	//   all-zero as unpriced and RequestUSDByClass then bills LOCAL ENERGY, so
	//   the row comes back priced at this box's watt-hours and classified here as
	//   unattributed all over again. Right number, wrong kind, nothing fixed.
	//
	//   A provider-level `free: true`. Wrong granularity: a provider serves free
	//   and paid models at once, and declaring the provider free mis-bills the
	//   paid ones — the bug, not the fix. Note in particular that the OpenRouter
	//   provider's `directory: {filter: {free: true}}` is NOT evidence of this:
	//   config.Provider.Directory enrols nothing, it is the default filter a
	//   person browsing the catalogue starts on and can clear, and the provider
	//   is `manual: true`, so models are hand-picked one at a time and a
	//   hand-picked paid one sits behind that same filter. Billing read off a
	//   browse default would be a guess.
	//
	// What DOES carry it by construction is the virtual pool: `extensions.free.
	// virtual.template.freeTier` is copied onto every contributed model by
	// config.VirtualModelFor, and `refresh: true` marks one stale when it stops
	// being free. So the pool needs no config change to classify correctly.
	BillFree = "free"
	// BillUnattributed — neither, and a config gap rather than a reading.
	//
	// An energy-priced type on a model served by a host corrallm does not run,
	// with NOTHING said about what that host charges: the tokens were processed
	// on somebody else's hardware and then charged against this box's watt-hour
	// coefficients. It is not electricity anybody paid here, it is not the
	// provider's rate, and — unlike BillFree — nothing in the config claims it
	// is free either. Also the answer for a model that has left the config,
	// where the type cannot be looked up at all — activity is retained 30 days
	// and a model can be removed inside that.
	//
	// It exists so the number is visible and actionable (OB-6): the fix is to
	// declare what that backend charges — a paid type if it bills, `freeTier:`
	// if it does not — and the screen can say so. Since BillFree took the
	// declared-free traffic, everything left here is a real gap, which is what
	// makes the tile worth showing at all; it is hidden when empty, because an
	// empty category is not a reading.
	BillUnattributed = "unattributed"
)

// Reasons a row is outside the "would have cost" comparison. Empty means it is
// inside. Each is a real limit of the cost model, not a rounding — the
// comparison prices TOKENS at a provider's per-token rates, and a request that
// was never billed in tokens has no term in that arithmetic.
const (
	// ExcludeAudio — STT/TTS/realtime, billed per MiB of audio (audioWhPerMiB).
	// A provider's audio pricing is per second or per character, neither of
	// which is recorded, so there is nothing to compare against.
	ExcludeAudio = "audio"
	// ExcludeImage — image generation, billed per image (imageWhPerImage). On
	// this box that coefficient is UNSET, so image requests already cost $0; the
	// comparison would be against a local figure that is missing, not small.
	ExcludeImage = "image"
	// ExcludePaid — already on a paid provider. "What it would have cost on a
	// paid API" is not a question about traffic that was on one.
	ExcludePaid = "paid"
	// ExcludeOffBox — an unattributed row on a model that IS in the config but
	// runs elsewhere (see BillUnattributed). Its local figure is not a local
	// figure, so a saving computed against it is not a saving.
	//
	// DECLARED-FREE TRAFFIC IS NOT EXCLUDED, deliberately, and this is the one
	// place the two off-box cases part. The reason this exclusion exists is that
	// the figure being compared against is junk — an energy number for work done
	// elsewhere — so the difference means nothing. A BillFree row has no such
	// problem: what it cost is $0, and $0 is not an estimate. It is also the
	// traffic the question is most worth asking about, since real work was done
	// for nothing, so "what would this have cost on a paid API" is the entire
	// value of the free tier stated in dollars. What remains excluded here is
	// only the genuine gap.
	ExcludeOffBox = "off-box"
	// ExcludeUnknownModel — the served model is not in the config any more, so
	// nothing says what priced its rows or where they ran.
	//
	// Separate from ExcludeOffBox because the two need different sentences and
	// the count is not small: on this box, 693 of one day's 2,629 requests are
	// queue-timeout 429s recorded against a model the Swift cutover removed.
	// "Served by a machine we do not run" would be a guess about those; "no
	// longer in the config" is the fact.
	ExcludeUnknownModel = "unknown-model"
	// ExcludeNoTokens — the window recorded no tokens for this model: every
	// request failed before usage was extracted, or the route carries none.
	// Zero tokens price to $0 on any rate table, and reporting that as "no
	// saving" would read as a measurement.
	ExcludeNoTokens = "no-tokens"
)

// costKind resolves how one served model's usage was priced, and whether the
// paid-API comparison can speak for it.
type costKind struct {
	// Type is the model's cost class, the `commandCosts` key. Empty when the
	// model is no longer in the config.
	Type string
	// Billing is one of the Bill* constants.
	Billing string
	// Exclude is one of the Exclude* constants, or empty when the comparison
	// covers this row. ExcludeNoTokens is NOT decided here — it depends on the
	// window's token counts, which the caller holds.
	Exclude string
}

// classify resolves the cost kind of one served model name.
//
// `known` is the resolved model set (config.AllModels — discovered models
// included, since a discovered model serves traffic and therefore bills).
func classify(known map[string]config.Model, cm *cost.Model, served string) costKind {
	m, ok := known[served]
	if !ok {
		// A model that has left the config. Its rows survive in the activity log
		// for the retention window, and nothing left says what priced them.
		return costKind{Billing: BillUnattributed, Exclude: ExcludeUnknownModel}
	}
	k := costKind{Type: m.Type}
	switch {
	case cm.IsPaidType(m.Type):
		k.Billing, k.Exclude = BillPaid, ExcludePaid
	case cm.NoCharge(m):
		// Declared free, so the cost is $0 and the comparison SPEAKS for it —
		// no Exclude. The audio/image test below still applies: a free backend
		// billing per MiB or per picture has no token term either.
		k.Billing = BillFree
	case m.Remote():
		// Remote is the predicate for "a host corrallm does not run", and it is
		// deliberately false for an enrolled machine's models: those carry a cmd
		// corrallm spawns, so their watt-hours are this fleet's. A LAN box we do
		// not manage, and every third-party API, are remote.
		k.Billing, k.Exclude = BillUnattributed, ExcludeOffBox
	default:
		k.Billing = BillEnergy
	}
	// Audio and image bill by size and by picture, not by token, whatever the
	// billing class above. Checked after it so a paid audio provider still reads
	// as paid spend while staying outside the token comparison.
	if k.Exclude == "" {
		switch {
		case cm.IsAudioType(m.Type):
			k.Exclude = ExcludeAudio
		case config.ModelIO(served, m, cm.IsAudioType(m.Type)).Emits(config.ModalityImage):
			k.Exclude = ExcludeImage
		}
	}
	return k
}

// baselineType picks the paid type the comparison prices against: the operator's
// declared choice when it names a type that actually bills per token, else the
// cheapest configured one. Empty when nothing paid is configured at all — in
// which case there is no comparison to make and the UI says so rather than
// showing a zero.
func baselineType(cfg *config.Config, cm *cost.Model) string {
	if cfg != nil {
		if want := cfg.CostBaseline; want != "" && cm.IsPaidType(want) {
			return want
		}
	}
	if paid := cm.PaidTypes(); len(paid) > 0 {
		return paid[0]
	}
	return ""
}
