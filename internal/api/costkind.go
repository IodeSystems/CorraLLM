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
	// BillUnattributed — neither, and a config gap rather than a reading.
	//
	// An energy-priced type on a model served by a host corrallm does not run:
	// the tokens were processed on somebody else's hardware and then charged
	// against this box's watt-hour coefficients. It is not electricity anybody
	// paid here and it is not the provider's rate either. Also the answer for a
	// model that has left the config, where the type cannot be looked up at all
	// — activity is retained 30 days and a model can be removed inside that.
	//
	// It exists so the number is visible and actionable (OB-6): the fix is to
	// declare a paid type for that model, and the screen can say so.
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
