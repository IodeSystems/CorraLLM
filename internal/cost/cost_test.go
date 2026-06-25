package cost

import (
	"math"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

func model() *Model {
	return NewModel(&config.Config{
		CostPerKwh: 0.14,
		CommandCosts: map[string]map[string]any{
			"local": {
				"generateWattsPerToken": 0.9,
				"processWattsPerToken":  0.3,
			},
			"claude": {
				"extract": map[string]any{"costFactor": 0.8},
			},
		},
	})
}

func approx(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestLocalEnergyUSD(t *testing.T) {
	// (500·0.9 + 1000·0.3) = 750 Wh = 0.75 kWh × $0.14 = $0.105.
	approx(t, model().RequestUSD("local", 1000, 500), 0.105)
}

func TestPaidExtractionUSD(t *testing.T) {
	// (1000 + 500) tokens × 0.8 = 1200.
	approx(t, model().RequestUSD("claude", 1000, 500), 1200)
}

func TestUnknownTypeIsFree(t *testing.T) {
	approx(t, model().RequestUSD("mystery", 1000, 500), 0)
}

func TestSwapUSD(t *testing.T) {
	// 18s × 300W = 5400 Ws = 1.5 Wh = 0.0015 kWh × $0.14 = $0.00021.
	approx(t, model().SwapUSD(18, 300), 0.00021)
}

func TestSwapWithoutWattsIsFree(t *testing.T) {
	approx(t, model().SwapUSD(18, 0), 0)
}

func TestIntCoefficientsParse(t *testing.T) {
	// YAML may decode whole numbers as int when the target is `any`.
	m := NewModel(&config.Config{
		CostPerKwh: 1,
		CommandCosts: map[string]map[string]any{
			"local": {"generateWattsPerToken": 1000, "processWattsPerToken": 0},
		},
	})
	// 1 token · 1000 Wh = 1 kWh × $1 = $1.
	approx(t, m.RequestUSD("local", 0, 1), 1)
}

// TestAudioRequestUSD: audio is costed by byte size (P9c). A local type bills
// processing energy (audioWhPerMiB → kWh × costPerKwh); a paid type bills
// audioUSDPerMiB directly; an unpriced type is $0.
func TestAudioRequestUSD(t *testing.T) {
	m := NewModel(&config.Config{
		CostPerKwh: 0.14,
		CommandCosts: map[string]map[string]any{
			"stt":     {"audioWhPerMiB": 10},    // local energy basis
			"stt-api": {"audioUSDPerMiB": 0.05}, // paid $ basis
		},
	})
	const mib = 1 << 20
	// Local: 2 MiB · 10 Wh/MiB = 20 Wh = 0.02 kWh × $0.14 = $0.0028.
	approx(t, m.AudioRequestUSD("stt", 2*mib), 0.0028)
	// Paid: 3 MiB · $0.05 = $0.15.
	approx(t, m.AudioRequestUSD("stt-api", 3*mib), 0.15)
	// Unpriced type → $0.
	approx(t, m.AudioRequestUSD("local", 5*mib), 0)
	// Zero bytes → $0.
	approx(t, m.AudioRequestUSD("stt", 0), 0)
}
