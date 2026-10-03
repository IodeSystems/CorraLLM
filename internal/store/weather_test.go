package store

import (
	"context"
	"testing"
)

// TestWeatherCacheAndCostByHour: weather is cached per location and hour and
// replaced on upsert; cost is summed per model per UTC hour.
func TestWeatherCacheAndCostByHour(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	const h = int64(3_600_000)
	if err := st.UpsertWeather("47.61,-122.33", map[int64]float64{0: 60, h: 62}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertWeather("47.61,-122.33", map[int64]float64{h: 63}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertWeather("40.00,-74.00", map[int64]float64{0: 90}); err != nil {
		t.Fatal(err)
	}
	got, err := st.WeatherBetween("47.61,-122.33", 0, 2*h)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != 60 || got[h] != 63 {
		t.Errorf("weather = %v, want {0:60, 1h:63} for this location only", got)
	}

	for _, a := range []Activity{
		{TS: 10, Served: "m", CostUSD: 0.01},
		{TS: h - 1, Served: "m", CostUSD: 0.02},
		{TS: h + 5, Served: "m", CostUSD: 0.04},
		{TS: h + 6, Served: "n", CostUSD: 0.08},
		{TS: h + 7, Served: "n", CostUSD: 0}, // no cost: not a row
	} {
		if err := st.InsertActivity(a); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.CostByHour(Window{})
	if err != nil {
		t.Fatal(err)
	}
	sum := map[string]float64{}
	for _, r := range rows {
		sum[r.Served+"@"+map[bool]string{true: "0", false: "1"}[r.HourMS == 0]] += r.CostUSD
	}
	if len(rows) != 3 || sum["m@0"] < 0.0299 || sum["m@0"] > 0.0301 || sum["m@1"] != 0.04 || sum["n@1"] != 0.08 {
		t.Errorf("cost by hour = %+v", rows)
	}
}
