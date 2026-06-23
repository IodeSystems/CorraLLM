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
