// Package cost converts a request's resource use into dollars. Everything in
// corrallm's cost model resolves to $: local backends bill energy (token work →
// kWh × costPerKwh), paid backends bill extracted usage × a cost factor, and a
// cold load bills its swap energy. The typed Model is built once from the parsed
// (but otherwise untyped) commandCosts config and is read on every metered
// request.
package cost

import "github.com/iodesystems/corrallm/internal/config"

// Model is the cost model resolved from config: a $/kWh rate plus per-backend-
// type coefficients. It is immutable after NewModel and safe for concurrent use.
type Model struct {
	costPerKwh float64
	byType     map[string]typeCost
}

// typeCost holds the cost coefficients for one backend `type`. A type is treated
// as paid iff it declares a costFactor; otherwise it bills local energy. The
// "WattsPerToken" coefficients are watt-hours per token despite the config name
// — they multiply token counts directly into Wh, matching the plan's arithmetic.
type typeCost struct {
	genWhPerTok  float64 // completion-token generation energy (Wh/token)
	procWhPerTok float64 // prompt-token processing energy (Wh/token)
	costFactor   float64 // paid: $ per token of extracted usage (>0 ⇒ paid type)

	// Audio coefficients (P9c). Audio replies carry no token usage, so audio
	// requests are costed by byte size: a paid type bills audioUSDPerMiB, a local
	// type bills audioWhPerMiB (processing energy → kWh × costPerKwh).
	audioWhPerMiB  float64 // local: processing energy per MiB of audio (Wh/MiB)
	audioUSDPerMiB float64 // paid: $ per MiB of audio (>0 ⇒ paid audio type)
}

// NewModel builds the cost model from config. Unknown/missing coefficients are
// zero — an unpriced type simply costs $0, never an error.
func NewModel(c *config.Config) *Model {
	m := &Model{costPerKwh: c.CostPerKwh, byType: map[string]typeCost{}}
	for typ, params := range c.CommandCosts {
		tc := typeCost{
			genWhPerTok:    toFloat(params["generateWattsPerToken"]),
			procWhPerTok:   toFloat(params["processWattsPerToken"]),
			audioWhPerMiB:  toFloat(params["audioWhPerMiB"]),
			audioUSDPerMiB: toFloat(params["audioUSDPerMiB"]),
		}
		// Paid factor is nested under <type>.extract.costFactor.
		if extract, ok := params["extract"].(map[string]any); ok {
			tc.costFactor = toFloat(extract["costFactor"])
		}
		m.byType[typ] = tc
	}
	return m
}

// RequestUSD is the dollar cost of one served request on a backend of the given
// type. Paid types bill extracted usage (prompt+completion tokens) × costFactor;
// local types bill energy: (completion·genWh + prompt·procWh) Wh → kWh ×
// costPerKwh. An unknown/unpriced type costs $0.
func (m *Model) RequestUSD(typ string, promptTokens, completionTokens int) float64 {
	tc := m.byType[typ]
	if tc.costFactor > 0 {
		return float64(promptTokens+completionTokens) * tc.costFactor
	}
	wh := float64(completionTokens)*tc.genWhPerTok + float64(promptTokens)*tc.procWhPerTok
	return wh / 1000 * m.costPerKwh
}

// AudioRequestUSD is the dollar cost of one audio request (STT/TTS) on a backend
// of the given type, costed by byte size (P9c, file-bytes basis — audio replies
// carry no token usage). A paid type bills audioUSDPerMiB; a local type bills the
// processing energy audioWhPerMiB → kWh × costPerKwh. An unpriced type costs $0.
func (m *Model) AudioRequestUSD(typ string, bytes int) float64 {
	tc := m.byType[typ]
	mib := float64(bytes) / (1 << 20)
	if tc.audioUSDPerMiB > 0 {
		return mib * tc.audioUSDPerMiB
	}
	return mib * tc.audioWhPerMiB / 1000 * m.costPerKwh
}

// IsAudioType reports whether a backend type is an audio type — i.e. it declares
// audio cost coefficients (P9d modality inference). A model is "audio" iff any of
// its backends use such a type; the catalog/UI flag it from this.
func (m *Model) IsAudioType(typ string) bool {
	tc := m.byType[typ]
	return tc.audioWhPerMiB > 0 || tc.audioUSDPerMiB > 0
}

// SwapUSD is the dollar cost of one cold load: load energy (loadSeconds ×
// loadWatts → Wh) → kWh × costPerKwh. With no loadWatts configured it is $0 —
// the load's latency still feeds scheduling; only its energy is unpriced.
func (m *Model) SwapUSD(loadSeconds, loadWatts float64) float64 {
	wh := loadSeconds * loadWatts / 3600 // watt-seconds → Wh
	return wh / 1000 * m.costPerKwh
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}
