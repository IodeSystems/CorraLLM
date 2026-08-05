package api

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// svcRow builds an activity row whose SERVICE time is exactly svcMS — dwell
// inflated by queue and load, which the stats must subtract back out.
func svcRow(ts int64, served, key string, svcMS int64) store.Activity {
	return store.Activity{
		TS: ts, Served: served, Key: key, Backend: served, Status: 200,
		QueuedMS: 1000, LoadMS: 500, DwellMS: svcMS + 1500,
	}
}

// TestServiceProfiles: service time excludes queueing and cold-load, per-caller
// distributions are reported separately, and a small sample is blended toward
// the model prior rather than standing on its own.
func TestServiceProfiles(t *testing.T) {
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	now := time.Now().UnixMilli()
	// steady: 40 requests, all exactly 1000ms → mean 1000, CV 0.
	for i := 0; i < 40; i++ {
		if err := st.InsertActivity(svcRow(now-int64(i)*1000, "m", "steady", 1000)); err != nil {
			t.Fatal(err)
		}
	}
	// spiky: 2 requests, 1000ms and 9000ms → mean 5000, CV 0.8. Tiny sample.
	for _, ms := range []int64{1000, 9000} {
		if err := st.InsertActivity(svcRow(now-5000, "m", "spiky", ms)); err != nil {
			t.Fatal(err)
		}
	}

	h := &Handlers{Store: st, Sched: sched.New(), Cfg: &config.Config{}}
	out, err := h.ServiceProfiles(context.Background(), &ServiceProfilesInput{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]ServiceProfileRow{}
	for _, r := range out.Body.Rows {
		got[r.Key] = r
	}

	// Queue + load are subtracted: 2500ms dwell is a 1000ms service time.
	steady := got["steady"]
	if steady.MeanMS != 1000 {
		t.Errorf("steady mean = %dms, want 1000 (dwell minus queued minus load)", steady.MeanMS)
	}
	if steady.CV != 0 {
		t.Errorf("steady CV = %v, want 0 (identical requests)", steady.CV)
	}
	// Constant-time work costs whoever is behind it nothing extra.
	if steady.VarianceFactor != 0.5 {
		t.Errorf("steady varianceFactor = %v, want 0.5 ((1+0)/2)", steady.VarianceFactor)
	}

	spiky := got["spiky"]
	if spiky.MeanMS != 5000 {
		t.Errorf("spiky mean = %dms, want 5000", spiky.MeanMS)
	}
	if math.Abs(spiky.CV-0.8) > 0.01 {
		t.Errorf("spiky CV = %v, want 0.8", spiky.CV)
	}

	// Shrinkage: 2 samples against a pseudo-count of 30 must land far closer to
	// the model prior than to spiky's own mean, or one outlier sets policy.
	if spiky.ModelMeanMS == 0 {
		t.Fatal("model prior not reported")
	}
	distToPrior := math.Abs(float64(spiky.ShrunkMeanMS - spiky.ModelMeanMS))
	distToSelf := math.Abs(float64(spiky.ShrunkMeanMS - spiky.MeanMS))
	if distToPrior >= distToSelf {
		t.Errorf("shrunk mean %d should sit nearer the prior %d than its own %d",
			spiky.ShrunkMeanMS, spiky.ModelMeanMS, spiky.MeanMS)
	}
	// A large sample barely moves: steady's 40 requests dominate the pseudo-count.
	if math.Abs(float64(steady.ShrunkMeanMS-steady.MeanMS)) > 0.5*float64(steady.MeanMS) {
		t.Errorf("steady (n=40) shrunk %d too far from its own mean %d",
			steady.ShrunkMeanMS, steady.MeanMS)
	}

	// Heaviest consumer first: steady's 40s of slot time beats spiky's 10s.
	if out.Body.Rows[0].Key != "steady" {
		t.Errorf("want the heaviest consumer first, got %q", out.Body.Rows[0].Key)
	}
}

// TestUtilizationDepthReachability: a queue bound deeper than maxWait can drain
// is dead config, and the row says so.
func TestUtilizationDepthReachability(t *testing.T) {
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	now := time.Now().UnixMilli()
	// 10s of service per request on a 1-slot model.
	for i := 0; i < 10; i++ {
		if err := st.InsertActivity(svcRow(now-int64(i)*1000, "big", "k", 10_000)); err != nil {
			t.Fatal(err)
		}
	}

	sc := sched.New()
	cfg := &config.Config{
		Models:    map[string]config.Model{"big": {MaxConcurrent: 1}},
		Scheduler: config.SchedulerConfig{MaxWait: "15s", MaxQueueDepth: 8},
	}
	sc.SetConfig(cfg)
	// Touch the backend so it has live capacity state.
	rel, _, err := sc.Admit(context.Background(), "big", "local", 1, "default", 1, false, config.Stage{})
	if err != nil {
		t.Fatal(err)
	}
	rel()

	h := &Handlers{Store: st, Sched: sc, Cfg: cfg}
	out, err := h.Utilization(context.Background(), &UtilizationInput{})
	if err != nil {
		t.Fatal(err)
	}
	var big *UtilizationRow
	for i := range out.Body.Rows {
		if out.Body.Rows[i].Served == "big" {
			big = &out.Body.Rows[i]
		}
	}
	if big == nil {
		t.Fatalf("no row for big: %+v", out.Body.Rows)
	}
	if big.ServiceMeanMS != 10_000 {
		t.Errorf("service mean = %d, want 10000", big.ServiceMeanMS)
	}
	// 1 slot × 15s maxWait / 10s per request = 1 reachable position.
	if big.ReachableDepth != 1 {
		t.Errorf("reachableDepth = %d, want 1", big.ReachableDepth)
	}
	if big.ConfiguredDepth != 8 {
		t.Errorf("configuredDepth = %d, want 8", big.ConfiguredDepth)
	}
	if !big.DepthUnreachable {
		t.Error("depth 8 against 1 reachable position must be flagged unreachable")
	}
}
