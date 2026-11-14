package runner_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/db"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/fakecolmap"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/files"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/jobs"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/lease"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/lease/leasetest"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/priors"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/runner"
)

func TestMain(m *testing.M) {
	if fakecolmap.Active() {
		fakecolmap.Main()
	}
	os.Exit(m.Run())
}

type env struct {
	t     *testing.T
	dir   string
	log   string
	files *files.Store
	jobs  *jobs.Store
	delay string
	fail  string
	lease lease.Leaser
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "state.db"), files.Schema, jobs.Schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	js := &jobs.Store{DB: d}
	return &env{t: t, dir: dir, log: filepath.Join(dir, "fake.log"), jobs: js,
		files: &files.Store{DB: d, Dir: dir, DefaultTTL: time.Hour, InUse: js.InUse}, lease: lease.Always{}}
}

func (e *env) runner() *runner.Runner {
	environ := append(os.Environ(), "FAKE_COLMAP=1", "FAKE_COLMAP_LOG="+e.log, "FAKE_COLMAP_DELAY="+e.delay, "FAKE_COLMAP_FAIL="+e.fail)
	return runner.New(runner.Config{
		Colmap: os.Args[0], DataDir: e.dir, Leaser: e.lease, Env: environ, StopGrace: 5 * time.Second,
		LeaseOpts: lease.Options{Poll: 200 * time.Millisecond, Backoff: 50 * time.Millisecond},
	}, e.jobs, e.files)
}

// job queues a job over n uploaded images named name(i).
func (e *env) job(id string, n int, name func(int) string, mut func(*jobs.Request), ps map[string]*priors.Prior) {
	e.t.Helper()
	req := &jobs.Request{Model: "colmap-sfm", Images: []jobs.ImageEntry{}}
	rec := &jobs.Record{Job: jobs.Job{ID: id, Model: "colmap-sfm", Metadata: map[string]string{}}, Owner: "alice", Request: req, Priors: ps}
	for i := 0; i < n; i++ {
		nm := name(i)
		f, err := e.files.Create("alice", filepath.Base(nm), files.PurposeUser, 0, strings.NewReader("\xff\xd8\xff fake "+nm))
		if err != nil {
			e.t.Fatal(err)
		}
		req.Images = append(req.Images, jobs.ImageEntry{Name: nm, FileID: f.ID})
		rec.Inputs = append(rec.Inputs, jobs.Input{FileID: f.ID, Kind: "image", Name: nm})
		rec.Names = append(rec.Names, nm)
	}
	if mut != nil {
		mut(req)
	}
	if err := req.Normalize(5000); err != nil {
		e.t.Fatal(err)
	}
	rec.Images.Total = n
	if err := e.jobs.Create(rec); err != nil {
		e.t.Fatal(err)
	}
}

func img(i int) string { return "img" + string(rune('a'+i)) + ".jpg" }

func (e *env) fakeLog() []string {
	b, _ := os.ReadFile(e.log)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func (e *env) argv(cmd string) []string {
	var out []string
	for _, l := range e.fakeLog() {
		if strings.HasPrefix(l, "argv "+cmd+" ") {
			out = append(out, l)
		}
	}
	return out
}

// units counts each "kind unit" line; a resumed stage must not redo a unit.
func (e *env) units(kind string) map[string]int {
	m := map[string]int{}
	for _, l := range e.fakeLog() {
		if u, ok := strings.CutPrefix(l, kind+" "); ok {
			m[u]++
		}
	}
	return m
}

// runUntil runs a runner until the job reaches a terminal status.
func (e *env) runUntil(r *runner.Runner, id string, timeout time.Duration) *jobs.Record {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	return e.waitFor(id, timeout, func(r *jobs.Record) bool { return jobs.Terminal(r.Status) })
}

func (e *env) waitFor(id string, timeout time.Duration, ok func(*jobs.Record) bool) *jobs.Record {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		rec, err := e.jobs.Load(id)
		if err == nil && ok(rec) {
			return rec
		}
		time.Sleep(20 * time.Millisecond)
	}
	rec, _ := e.jobs.Load(id)
	e.t.Fatalf("job %s: condition not met in %v; status %s done %v\nfake log:\n%s", id, timeout, rec.Status, rec.Done, strings.Join(e.fakeLog(), "\n"))
	return nil
}

func has(line string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(line, p) {
			return false
		}
	}
	return true
}

func TestStageCommandLinesDenseMesh(t *testing.T) {
	e := newEnv(t)
	mesh := "poisson"
	e.job("recon_a", 3, img, func(r *jobs.Request) { r.Dense, r.Mesh, r.MaxImageSize = true, &mesh, 1600 }, nil)
	rec := e.runUntil(e.runner(), "recon_a", 30*time.Second)
	if rec.Status != jobs.Completed || rec.Progress != 100 || rec.CompletedAt == nil || rec.ExpiresAt == nil || rec.Stage != nil {
		t.Fatalf("job %+v", rec.Job)
	}
	want := map[string][]string{
		"feature_extractor":  {"--ImageReader.camera_model SIMPLE_RADIAL", "--ImageReader.single_camera 0", "--FeatureExtraction.use_gpu 1", "--FeatureExtraction.gpu_index 0", "--FeatureExtraction.max_image_size 1600"},
		"exhaustive_matcher": {"--FeatureMatching.use_gpu 1", "--database_path"},
		"mapper":             {"--image_path", "--output_path"},
		"model_converter":    {"--output_type TXT"},
		"image_undistorter":  {"--output_type COLMAP", "--max_image_size 1600"},
		"patch_match_stereo": {"--PatchMatchStereo.geom_consistency true", "--PatchMatchStereo.max_image_size 1600", "--workspace_format COLMAP"},
		"stereo_fusion":      {"--input_type geometric", "fused.ply"},
		"poisson_mesher":     {"meshed-poisson.ply"},
	}
	for cmd, parts := range want {
		a := e.argv(cmd)
		if len(a) != 1 || !has(a[0], parts...) {
			t.Errorf("%s: %v, want one line with %v", cmd, a, parts)
		}
	}
	if len(e.argv("pose_prior_mapper")) != 0 {
		t.Error("< 3 priors must run plain mapper")
	}
	var vs []string
	for _, o := range rec.Outputs {
		vs = append(vs, o.Variant)
	}
	if strings.Join(vs, ",") != "cameras,transforms,sparse,dense,mesh,glb,log" {
		t.Fatalf("outputs %v", vs)
	}
	for _, f := range []string{"sparse.zip", "dense.zip", "mesh.zip", "mesh.glb", "cameras.json", "transforms.json", "log.txt"} {
		if _, err := os.Stat(filepath.Join(e.dir, "jobs", "recon_a", "out", f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}
	if _, err := os.Stat(filepath.Join(e.dir, "jobs", "recon_a", "ws")); !os.IsNotExist(err) {
		t.Error("workspace not removed")
	}
	if *rec.Images.Registered != 3 || rec.Frame.Type != "arbitrary" || rec.Alignment.Status != "none" {
		t.Fatalf("images %+v frame %+v alignment %+v", rec.Images, rec.Frame, rec.Alignment)
	}
}

func cartPrior(x float64) *priors.Prior {
	return &priors.Prior{CRS: priors.Cartesian, V: [3]float64{x, 2 * x, 1}, Frame: "site"}
}

func TestPriorsChooseMapperAndAlign(t *testing.T) {
	t.Run("cartesian priors on 4 → pose_prior_mapper, aligned", func(t *testing.T) {
		e := newEnv(t)
		ps := map[string]*priors.Prior{}
		for i := 0; i < 4; i++ {
			ps[img(i)] = cartPrior(float64(i))
		}
		e.job("recon_p", 5, img, nil, ps)
		rec := e.runUntil(e.runner(), "recon_p", 30*time.Second)
		if rec.Status != jobs.Completed {
			t.Fatalf("%+v", rec.Error)
		}
		a := e.argv("pose_prior_mapper")
		if len(a) != 1 || !has(a[0], "--use_robust_loss_on_prior_position 1") || len(e.argv("mapper")) != 0 {
			t.Fatalf("pose_prior_mapper %v mapper %v", a, e.argv("mapper"))
		}
		if rec.Alignment.Status != "aligned" || rec.Alignment.RegisteredWithPrior != 4 || rec.Frame.Type != "cartesian" || rec.Frame.Frame != "site" || rec.Images.WithPrior != 4 {
			t.Fatalf("alignment %+v frame %+v images %+v", rec.Alignment, rec.Frame, rec.Images)
		}
	})
	t.Run("2 priors → mapper, insufficient_priors", func(t *testing.T) {
		e := newEnv(t)
		e.job("recon_q", 4, img, nil, map[string]*priors.Prior{img(0): cartPrior(0), img(1): cartPrior(1)})
		rec := e.runUntil(e.runner(), "recon_q", 30*time.Second)
		if len(e.argv("mapper")) != 1 || len(e.argv("pose_prior_mapper")) != 0 || rec.Alignment.Status != "insufficient_priors" || rec.Frame.Type != "arbitrary" {
			t.Fatalf("mapper %v alignment %+v", e.argv("mapper"), rec.Alignment)
		}
	})
	t.Run("EXIF GPS → ENU, gravity-only rows dropped", func(t *testing.T) {
		e := newEnv(t)
		names := []string{"gps1.jpg", "gps2.jpg", "grav.jpg", "gps3.jpg"}
		e.job("recon_g", 4, func(i int) string { return names[i] }, nil, nil)
		rec := e.runUntil(e.runner(), "recon_g", 30*time.Second)
		if rec.Status != jobs.Completed || rec.Images.WithPrior != 3 || len(e.argv("pose_prior_mapper")) != 1 {
			t.Fatalf("status %s with_prior %d", rec.Status, rec.Images.WithPrior)
		}
		// the fake mapper centres a priored image on its prior, so this aligns in ENU
		if rec.Frame.Type != "enu" || rec.Frame.Origin == nil || rec.Frame.Origin.Lat != 40 || rec.Alignment.Status != "aligned" {
			t.Fatalf("frame %+v alignment %+v", rec.Frame, rec.Alignment)
		}
	})
	t.Run("exif_gps false → no priors", func(t *testing.T) {
		e := newEnv(t)
		names := []string{"gps1.jpg", "gps2.jpg", "gps3.jpg"}
		f := false
		e.job("recon_h", 3, func(i int) string { return names[i] }, func(r *jobs.Request) { r.EXIFGPS = &f }, nil)
		rec := e.runUntil(e.runner(), "recon_h", 30*time.Second)
		if rec.Images.WithPrior != 0 || len(e.argv("mapper")) != 1 {
			t.Fatalf("with_prior %d", rec.Images.WithPrior)
		}
	})
	t.Run("global mapper aligns with model_aligner", func(t *testing.T) {
		e := newEnv(t)
		ps := map[string]*priors.Prior{}
		for i := 0; i < 3; i++ {
			ps[img(i)] = cartPrior(float64(i))
		}
		e.job("recon_m", 3, img, func(r *jobs.Request) { r.Mapper = "global"; r.Matcher = "sequential" }, ps)
		rec := e.runUntil(e.runner(), "recon_m", 30*time.Second)
		if len(e.argv("global_mapper")) != 1 || len(e.argv("sequential_matcher")) != 1 {
			t.Fatalf("argv %v", e.fakeLog())
		}
		al := e.argv("model_aligner")
		if len(al) != 1 || !has(al[0], "--ref_is_gps 0", "--alignment_type custom", "--ref_images_path") || rec.Alignment.Status != "aligned" {
			t.Fatalf("aligner %v alignment %+v", al, rec.Alignment)
		}
	})
}

func TestFailureKeepsLog(t *testing.T) {
	e := newEnv(t)
	e.fail = "exhaustive_matcher"
	e.job("recon_f", 3, img, nil, nil)
	rec := e.runUntil(e.runner(), "recon_f", 30*time.Second)
	if rec.Status != jobs.Failed || rec.Error.Code != "colmap_failed" || !strings.Contains(rec.Error.Message, "fake failure in exhaustive_matcher") {
		t.Fatalf("job %+v err %+v", rec.Status, rec.Error)
	}
	if len(rec.Outputs) != 1 || rec.Outputs[0].Variant != "log" {
		t.Fatalf("outputs %+v", rec.Outputs)
	}
}

// TestYieldMidStageResume is the D4a path against the fake corrallm lease
// server over HTTP: a yield during match stops the matcher (graceful
// SIGTERM), the lease is released, re-acquisition waits through 429s with
// waiting_for_gpu true, and the rerun skips finished pairs.
func TestYieldMidStageResume(t *testing.T) {
	e := newEnv(t)
	srv := leasetest.New("tok")
	defer srv.Close()
	e.lease = &lease.Client{Base: srv.URL, Token: "tok"}
	e.delay = "60ms"
	var once sync.Once
	yieldSent := make(chan time.Time, 1)
	srv.OnGrant = func(id, stage string) {
		if stage != "match" {
			return
		}
		once.Do(func() {
			go func() {
				time.Sleep(400 * time.Millisecond)
				srv.Busy(3) // the guard is "loading": the next grants are 429
				srv.RequestYield("match", 30*time.Second)
				yieldSent <- time.Now()
			}()
		})
	}
	e.job("recon_y", 7, img, nil, nil) // 21 pairs × 60 ms
	r := e.runner()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	sent := <-yieldSent
	waiting := e.waitFor("recon_y", 5*time.Second, func(r *jobs.Record) bool { return r.WaitingForGPU })
	if waiting.Stage == nil || *waiting.Stage != "match" {
		t.Fatalf("waiting in stage %v", waiting.Stage)
	}
	var released time.Time
	for _, ev := range srv.Events() {
		if ev.Kind == "release" && ev.Stage == "match" && released.IsZero() {
			released = time.Now()
		}
	}
	rec := e.waitFor("recon_y", 30*time.Second, func(r *jobs.Record) bool { return jobs.Terminal(r.Status) })
	if rec.Status != jobs.Completed || rec.Yields != 1 {
		t.Fatalf("status %s yields %d err %+v", rec.Status, rec.Yields, rec.Error)
	}
	pairs := e.units("match")
	if len(pairs) != 21 {
		t.Fatalf("%d pairs matched, want 21", len(pairs))
	}
	for p, n := range pairs {
		if n != 1 {
			t.Fatalf("pair %s matched %d times: the rerun must skip finished work", p, n)
		}
	}
	if len(e.argv("exhaustive_matcher")) != 2 {
		t.Fatalf("matcher runs %d, want 2 (yield + resume)", len(e.argv("exhaustive_matcher")))
	}
	var kinds []string
	for _, ev := range srv.Events() {
		if ev.Stage == "match" {
			kinds = append(kinds, ev.Kind)
		}
	}
	if got := strings.Join(kinds, ","); got != "grant,yield,release,busy,busy,busy,grant,release" {
		t.Fatalf("match lease events %s", got)
	}
	if len(srv.Held()) != 0 {
		t.Fatalf("leases left: %v", srv.Held())
	}
	_ = sent
	_ = released
	// GPU stages lease gpu1 and are yieldable; usage comes from the table.
	for _, ev := range srv.Events() {
		if ev.Kind == "grant" && ev.Stage == "extract" && ev.Usage["gpu1"] == "" {
			t.Fatalf("extract lease without gpu1: %+v", ev)
		}
		if ev.Kind == "grant" && ev.Stage == "map" && ev.Usage["gpu1"] != "" {
			t.Fatalf("map lease names gpu1: %+v", ev)
		}
	}
}

// TestRestartResumesFromDisk is D7: the service is stopped mid-extract and
// again mid-map (as corrallm does when a yield is ignored); each new runner
// on the same data dir resumes the job — extraction skips done images, the
// mapper continues from the partial model it wrote on SIGTERM.
func TestRestartResumesFromDisk(t *testing.T) {
	e := newEnv(t)
	srv := leasetest.New("tok")
	defer srv.Close()
	e.lease = &lease.Client{Base: srv.URL, Token: "tok"}
	e.delay = "80ms"
	e.job("recon_d", 8, img, nil, nil)

	stopWhen := func(cond func() bool) {
		r := e.runner()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { r.Run(ctx); close(done) }()
		deadline := time.Now().Add(20 * time.Second)
		for !cond() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		<-done
	}
	stopWhen(func() bool { return len(e.units("extract")) >= 3 })
	rec, _ := e.jobs.Load("recon_d")
	if rec.Status != jobs.InProgress || rec.IsDone(runner.StepExtract) {
		t.Fatalf("after stop 1: %s done %v", rec.Status, rec.Done)
	}
	if len(srv.Held()) != 0 {
		t.Fatalf("lease kept over a shutdown: %v", srv.Held())
	}
	stopWhen(func() bool { return len(e.units("map")) >= 3 })
	rec, _ = e.jobs.Load("recon_d")
	if rec.Status != jobs.InProgress || rec.IsDone(runner.StepMap) || !rec.IsDone(runner.StepMatch) {
		t.Fatalf("after stop 2: %s done %v", rec.Status, rec.Done)
	}
	rec = e.runUntil(e.runner(), "recon_d", 30*time.Second)
	if rec.Status != jobs.Completed || *rec.Images.Registered != 8 {
		t.Fatalf("final %s %+v", rec.Status, rec.Error)
	}
	for kind, want := range map[string]int{"extract": 8, "map": 8} {
		u := e.units(kind)
		if len(u) != want {
			t.Fatalf("%s units %d, want %d", kind, len(u), want)
		}
		for k, n := range u {
			if n != 1 {
				t.Fatalf("%s %s done %d times", kind, k, n)
			}
		}
	}
	if len(e.argv("feature_extractor")) != 2 {
		t.Fatalf("extractor runs %d, want 2", len(e.argv("feature_extractor")))
	}
	resumed := false
	for _, a := range e.argv("mapper") {
		resumed = resumed || has(a, "--input_path", "sparse.run/0 ")
	}
	if !resumed {
		t.Fatalf("mapper did not resume from the partial model: %v", e.argv("mapper"))
	}
}

func TestCancelRunningAndQueued(t *testing.T) {
	e := newEnv(t)
	srv := leasetest.New("tok")
	defer srv.Close()
	e.lease = &lease.Client{Base: srv.URL, Token: "tok"}
	e.delay = "100ms"
	e.job("recon_c1", 6, img, nil, nil)
	e.job("recon_c2", 3, img, nil, nil)
	r := e.runner()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	e.waitFor("recon_c1", 10*time.Second, func(x *jobs.Record) bool { return x.Stage != nil && *x.Stage == "match" })
	if _, _, err := e.jobs.Cancel("alice", "recon_c2"); err != nil {
		t.Fatal(err)
	}
	if rec, running, _ := e.jobs.Cancel("alice", "recon_c1"); !running || rec.Status != jobs.Cancelling {
		t.Fatalf("cancel running → %s", rec.Status)
	}
	r.Cancel("recon_c1")
	rec := e.waitFor("recon_c1", 10*time.Second, func(x *jobs.Record) bool { return jobs.Terminal(x.Status) })
	if rec.Status != jobs.Cancelled || rec.CancelledAt == nil || rec.WaitingForGPU {
		t.Fatalf("c1 %s", rec.Status)
	}
	if q, _ := e.jobs.Load("recon_c2"); q.Status != jobs.Cancelled {
		t.Fatalf("c2 %s", q.Status)
	}
	time.Sleep(300 * time.Millisecond)
	if len(srv.Held()) != 0 {
		t.Fatalf("lease kept after cancel: %v", srv.Held())
	}
	if _, err := os.Stat(filepath.Join(e.dir, "jobs", "recon_c1", "ws")); !os.IsNotExist(err) {
		t.Fatal("workspace kept after cancel")
	}
}

func TestProgress(t *testing.T) {
	sparse := &jobs.Request{}
	if p := runner.Progress(sparse, "match", 0.5); p != 15+17 {
		t.Fatalf("match half: %d", p)
	}
	if p := runner.Progress(sparse, "package", 1); p != 99 {
		t.Fatalf("end caps at 99: %d", p)
	}
	mesh := "poisson"
	dense := &jobs.Request{Dense: true, Mesh: &mesh}
	// rest = align, undistort, dense, fuse, mesh, package share 20 → 3.33 each
	if p := runner.Progress(dense, "dense", 0); p != 15+35+30+6 {
		t.Fatalf("dense start: %d", p)
	}
}

func TestGPUUsageScalesWithImageSize(t *testing.T) {
	u := map[string]string{"gpu1": "6144MiB", "system": "4GB"}
	cases := map[int]string{3200: "6144MiB", 1600: "2048MiB" /* floor: a third */, 2400: "3456MiB", 4800: "7629MiB" /* cap 8 GB */}
	for size, want := range cases {
		got := runner.GPUUsage(u, "gpu1", size, 8e9)
		if got["gpu1"] != want || got["system"] != "4GB" {
			t.Errorf("size %d: %v, want gpu1 %s", size, got, want)
		}
	}
	if u["gpu1"] != "6144MiB" {
		t.Fatal("input table modified")
	}
}

// The pose_prior_mapper resumes the same way (its extra flags must not
// disturb the output path).
func TestRestartResumesPriorMapper(t *testing.T) {
	e := newEnv(t)
	e.delay = "80ms"
	ps := map[string]*priors.Prior{}
	for i := 0; i < 4; i++ {
		ps[img(i)] = cartPrior(float64(i))
	}
	e.job("recon_pp", 8, img, nil, ps)
	r := e.runner()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	for deadline := time.Now().Add(20 * time.Second); len(e.units("map")) < 3 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	rec := e.runUntil(e.runner(), "recon_pp", 30*time.Second)
	a := e.argv("pose_prior_mapper")
	if rec.Status != jobs.Completed || *rec.Images.Registered != 8 || len(a) != 2 || !has(a[1], "--input_path", "sparse.run/0 ", "--use_robust_loss_on_prior_position 1") {
		t.Fatalf("status %s registered %v argv %v", rec.Status, rec.Images.Registered, a)
	}
}
