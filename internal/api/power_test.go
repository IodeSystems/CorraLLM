package api

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/store"
)

// fakeProm answers query_range with a constant value per query: the gpu query
// gets one card at gpuW, the box query the wall at boxW.
func fakeProm(t *testing.T, gpuW, boxW float64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		start, _ := strconv.ParseInt(q.Get("start"), 10, 64)
		end, _ := strconv.ParseInt(q.Get("end"), 10, 64)
		step, _ := strconv.ParseInt(q.Get("step"), 10, 64)
		v, labels := boxW, `{}`
		if strings.Contains(q.Get("query"), "gpu") {
			v, labels = gpuW, `{"uuid":"ee90af07-0882-d325-182e-87137ec6d47b"}`
		}
		var pts []string
		for ts := start; ts <= end; ts += step {
			pts = append(pts, fmt.Sprintf(`[%d,"%g"]`, ts, v))
		}
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":%s,"values":[%s]}]}}`,
			labels, strings.Join(pts, ","))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A card drawing 400 W for 10 minutes, busy for the middle 5: half its energy
// goes to the model served, half is idle. The wall reads 600 W; the rest of the
// box is the wall minus the card.
func TestMeasurePowerSplitsBusyIdleAndRest(t *testing.T) {
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	from := time.Now().Add(-10 * time.Minute).Truncate(time.Second).UnixMilli()
	to := from + 600_000
	// Served from+150 s to from+450 s.
	if err := st.InsertActivity(store.Activity{TS: from + 450_000, Served: "m", Path: "/v1/chat/completions", Status: 200, DwellMS: 300_000}); err != nil {
		t.Fatal(err)
	}
	prom := fakeProm(t, 400, 600)
	cfg := &config.Config{
		Servers: map[string]config.Server{"box1": {Devices: map[string]string{"gpu0": "GPU-ee90af07-0882-d325-182e-87137ec6d47b"}}},
		Models:  map[string]config.Model{"m": {Server: "box1", Pool: "gpu0", Cmd: "x"}},
		Power: &config.Power{Prometheus: prom.URL, Servers: map[string]config.ServerPower{
			"box1": {GPU: "gpu_power_watts", Box: "ups_realpower"},
		}},
	}
	h := &Handlers{Store: st, Cfg: cfg}
	view, byModel := h.measurePower(context.Background(), store.Window{FromMS: from, ToMS: to})
	if view == nil || !view.Measured {
		t.Fatalf("not measured: %+v", view)
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-6 }
	const card = 400.0 * 600 / 3.6e6 // kWh over 10 min
	if len(view.Pools) != 1 || !near(view.Pools[0].Kwh, card) || !near(view.Pools[0].BusyKwh, card/2) || !near(view.Pools[0].IdleKwh, card/2) {
		t.Errorf("pools = %+v, want %.4f kWh split half busy, half idle", view.Pools, card)
	}
	if !near(byModel["m"], card/2) {
		t.Errorf("model m = %.6f kWh, want %.6f", byModel["m"], card/2)
	}
	const wall = 600.0 * 600 / 3.6e6
	if len(view.Boxes) != 1 || !near(view.Boxes[0].Kwh, wall) || !near(view.Boxes[0].RestKwh, wall-card) || !near(view.Kwh, wall) {
		t.Errorf("boxes = %+v total %.6f, want wall %.6f, rest %.6f", view.Boxes, view.Kwh, wall, wall-card)
	}
	if math.Abs(view.Boxes[0].AvgW-600) > 1e-6 {
		t.Errorf("avg W = %v, want 600", view.Boxes[0].AvgW)
	}
}

// Prometheus that cannot be reached keeps the estimate and says why.
func TestMeasurePowerUnreachableKeepsEstimate(t *testing.T) {
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	cfg := &config.Config{
		Servers: map[string]config.Server{"box1": {Devices: map[string]string{"gpu0": "GPU-ee90af07"}}},
		Power: &config.Power{Prometheus: "http://127.0.0.1:1", Servers: map[string]config.ServerPower{
			"box1": {GPU: "gpu_power_watts"},
		}},
	}
	h := &Handlers{Store: st, Cfg: cfg}
	now := time.Now().UnixMilli()
	view, byModel := h.measurePower(context.Background(), store.Window{FromMS: now - 600_000, ToMS: now})
	if view == nil || view.Measured || view.Error == "" || byModel != nil {
		t.Errorf("view %+v byModel %v, want unmeasured with an error and no per-model figures", view, byModel)
	}
}
