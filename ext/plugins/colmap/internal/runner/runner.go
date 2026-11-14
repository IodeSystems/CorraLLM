// Package runner runs reconstruction jobs: one at a time, each as a list of
// steps that run COLMAP CLI commands in their own process groups. GPU steps
// (extract, match, dense) run only while the runner holds a gpu1 lease; a
// yield request stops the child (SIGTERM: COLMAP keeps finished work), the
// lease is released and re-acquired, and the same command resumes, skipping
// what is done (D4a). Finished steps are recorded in the job, so a restarted
// service resumes from disk (D7).
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/files"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/inputs"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/jobs"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/lease"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/pack"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/priors"
)

// Config configures a Runner.
type Config struct {
	Colmap    string // colmap binary
	FFmpeg    string // "" = video input unsupported
	DataDir   string // jobs live in DataDir/jobs/<id>
	Leaser    lease.Leaser
	Extension string // lease extension name ("colmap")
	// Usage is each step's lease usage (pool → size). A GPU step must name
	// GPUPool; a CPU step with no entry takes no lease.
	Usage     map[string]map[string]string
	GPUPool   string
	GPUIndex  string        // COLMAP gpu_index inside CUDA_VISIBLE_DEVICES ("0")
	StopGrace time.Duration // SIGTERM → SIGKILL on cancel/shutdown
	YieldKill time.Duration // SIGTERM → SIGKILL on a yield (default 10 s)
	OutputTTL time.Duration // outputs live this long after the job ends (D8: 7 days)
	LeaseOpts lease.Options
	Env       []string // child environment (default os.Environ())
	MaxImages int
	GPUMax    int64 // cap on a scaled GPU lease, bytes (lease.max; default 8 GB)
}

// DefaultUsage is the per-step lease table. GPU sizes are for
// max_image_size 3200 and scale with it (GPUUsage). Measured on the 3080
// (README): peak VRAM extract 5,846 MiB / match 5,336 MiB at 3072 px images,
// 1,712 / 1,348 MiB at 1600 px; plus headroom.
func DefaultUsage(gpuPool string) map[string]map[string]string {
	return map[string]map[string]string{
		StepExtract: {gpuPool: "6144MiB", "system": "4GB"},
		StepMatch:   {gpuPool: "5632MiB", "system": "4GB"},
		StepDense:   {gpuPool: "5120MiB", "system": "8GB"}, // 1,230 MiB measured at 1600 px; 3200 px scaled, not measured
		StepMap:     {"system": "8GB"},
		StepFuse:    {"system": "8GB"},
		StepMesh:    {"system": "8GB"},
	}
}

// Runner steps. API stage names (job.stage) are in apiStage.
const (
	StepPrepare   = "prepare"
	StepExtract   = "extract"
	StepPriors    = "priors"
	StepMatch     = "match"
	StepMap       = "map"
	StepAlign     = "align"
	StepUndistort = "undistort"
	StepDense     = "dense"
	StepFuse      = "fuse"
	StepMesh      = "mesh"
	StepPackage   = "package"
)

var apiStage = map[string]string{
	StepPrepare: "extract", StepExtract: "extract", StepPriors: "extract", StepMatch: "match",
	StepMap: "map", StepAlign: "align", StepUndistort: "undistort", StepDense: "dense",
	StepFuse: "fuse", StepMesh: "mesh", StepPackage: "package",
}

var gpuStep = map[string]bool{StepExtract: true, StepMatch: true, StepDense: true}

// GPUUsage scales the GPU pool's size (given for max_image_size 3200) by
// (size/3200)², which is how SIFT and PatchMatch memory grows with the image,
// with a floor of a third of it and a cap of max bytes.
func GPUUsage(u map[string]string, pool string, size int, max int64) map[string]string {
	base, err := ParseSize(u[pool])
	if err != nil || base <= 0 {
		return u
	}
	r := float64(size) / 3200
	v := int64(float64(base) * r * r)
	v = min(maxI(v, base/3), max)
	out := make(map[string]string, len(u))
	for k, x := range u {
		out[k] = x
	}
	out[pool] = strconv.FormatInt(v>>20, 10) + "MiB"
	return out
}

func maxI(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// ParseSize reads 50GiB, 512MiB, 3GB, 1024 (bytes). GB = 1e9, GiB = 2^30
// (corrallm's config.ParseSize uses the same suffixes).
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	units := []struct {
		suf string
		mul float64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
		{"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3}, {"B", 1}}
	for _, u := range units {
		if n, ok := strings.CutSuffix(s, u.suf); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
			if err != nil || f < 0 {
				return 0, fmt.Errorf("bad size %q", s)
			}
			return int64(f * u.mul), nil
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad size %q (use e.g. 50GiB)", s)
	}
	return n, nil
}

// Steps is the step list for a request.
func Steps(req *jobs.Request) []string {
	s := []string{StepPrepare, StepExtract, StepPriors, StepMatch, StepMap, StepAlign}
	if req.Dense {
		s = append(s, StepUndistort, StepDense, StepFuse)
		if req.Mesh != nil {
			s = append(s, StepMesh)
		}
	}
	return append(s, StepPackage)
}

// stageWeights: extract 15, match 35, map 30, the rest share 20 (spec §4.4 [A]).
func stageWeights(req *jobs.Request) ([]string, map[string]float64) {
	var order []string
	seen := map[string]bool{}
	for _, st := range Steps(req) {
		if a := apiStage[st]; !seen[a] {
			seen[a] = true
			order = append(order, a)
		}
	}
	w := map[string]float64{"extract": 15, "match": 35, "map": 30}
	rest := len(order) - 3
	for _, a := range order[3:] {
		w[a] = 20 / float64(rest)
	}
	return order, w
}

// Progress is 0..99 while running: finished stages plus frac of the current one.
func Progress(req *jobs.Request, stage string, frac float64) int {
	order, w := stageWeights(req)
	sum := 0.0
	for _, a := range order {
		if a == stage {
			sum += w[a] * math.Max(0, math.Min(1, frac))
			break
		}
		sum += w[a]
	}
	return min(99, int(sum))
}

// Runner runs jobs.
type Runner struct {
	cfg   Config
	Jobs  *jobs.Store
	Files *files.Store

	wake chan struct{}
	mu   sync.Mutex
	cur  string
	stop context.CancelCauseFunc
}

// New makes a Runner.
func New(cfg Config, js *jobs.Store, fs *files.Store) *Runner {
	if cfg.Leaser == nil {
		cfg.Leaser = lease.Always{}
	}
	if cfg.Extension == "" {
		cfg.Extension = "colmap"
	}
	if cfg.GPUPool == "" {
		cfg.GPUPool = "gpu1"
	}
	if cfg.Usage == nil {
		cfg.Usage = DefaultUsage(cfg.GPUPool)
	}
	if cfg.GPUIndex == "" {
		cfg.GPUIndex = "0"
	}
	if cfg.StopGrace <= 0 {
		cfg.StopGrace = 30 * time.Second
	}
	if cfg.YieldKill <= 0 {
		cfg.YieldKill = 10 * time.Second
	}
	if cfg.OutputTTL <= 0 {
		cfg.OutputTTL = jobs.DefaultOutputTTLDays * 24 * time.Hour
	}
	if cfg.Env == nil {
		cfg.Env = os.Environ()
	}
	if cfg.GPUMax <= 0 {
		cfg.GPUMax = 8e9
	}
	if cfg.MaxImages <= 0 {
		cfg.MaxImages = jobs.DefaultMaxImages
	}
	return &Runner{cfg: cfg, Jobs: js, Files: fs, wake: make(chan struct{}, 1)}
}

// errCancelled is the cause when the caller cancels the running job.
var errCancelled = errors.New("cancelled by the caller")

// Wake tells the loop a job may be ready.
func (r *Runner) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Cancel stops the running job if it is id.
func (r *Runner) Cancel(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == id && r.stop != nil {
		r.stop(errCancelled)
	}
	r.Wake()
}

// JobDir is a job's directory.
func (r *Runner) JobDir(id string) string { return filepath.Join(r.cfg.DataDir, "jobs", id) }

// Run is the loop; it returns when ctx ends (in_progress jobs stay
// in_progress and resume at the next start).
func (r *Runner) Run(ctx context.Context) {
	for ctx.Err() == nil {
		rec, err := r.Jobs.Next()
		if err != nil {
			slog.Error("runner: next job", "err", err)
		}
		if rec == nil {
			select {
			case <-ctx.Done():
				return
			case <-r.wake:
			case <-time.After(30 * time.Second):
			}
			continue
		}
		r.runJob(ctx, rec)
	}
}

// stepError fails the job with a code.
type stepError struct{ code, msg string }

func (e *stepError) Error() string { return e.code + ": " + e.msg }

func fail(code, format string, a ...any) error { return &stepError{code, fmt.Sprintf(format, a...)} }

type job struct {
	r   *Runner
	rec *jobs.Record
	dir string
	ws  string
	log *os.File

	pmu      sync.Mutex
	stage    string
	sp       *jobs.StageProgress
	dirty    bool
	waiting  bool
	lastSave time.Time

	alignFailed bool // model_aligner could not align (global mapper)
}

func (r *Runner) runJob(ctx context.Context, rec *jobs.Record) {
	jctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	r.mu.Lock()
	r.cur, r.stop = rec.ID, stop
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.cur, r.stop = "", nil
		r.mu.Unlock()
	}()

	j := &job{r: r, rec: rec, dir: r.JobDir(rec.ID)}
	j.ws = filepath.Join(j.dir, "ws")
	if rec.Status == jobs.Cancelling {
		j.finishCancelled()
		return
	}
	if err := os.MkdirAll(filepath.Join(j.dir, "out"), 0o755); err != nil {
		j.failed(fail("internal", "%v", err))
		return
	}
	lf, err := os.OpenFile(filepath.Join(j.dir, "colmap.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		j.failed(fail("internal", "%v", err))
		return
	}
	j.log = lf
	defer lf.Close()

	if rec.Status == jobs.Queued {
		now := time.Now().Unix()
		if err := j.update(func(x *jobs.Record) error {
			x.Status, x.StartedAt = jobs.InProgress, &now
			return nil
		}); err != nil {
			slog.Error("runner: start job", "job", rec.ID, "err", err)
			return
		}
	} else {
		fmt.Fprintf(j.log, "=== colmap-svc: resuming job %s at %s\n", rec.ID, time.Now().Format(time.RFC3339))
	}
	slog.Info("job running", "job", rec.ID, "owner", rec.Owner, "done", rec.Done)

	for _, st := range Steps(rec.Request) {
		if j.rec.IsDone(st) {
			continue
		}
		j.setStage(st, nil)
		fmt.Fprintf(j.log, "=== step %s (%s)\n", st, time.Now().Format(time.RFC3339))
		t0 := time.Now()
		err := j.runStep(jctx, st)
		if err == nil {
			fmt.Fprintf(j.log, "=== step %s done in %.1f s\n", st, time.Since(t0).Seconds())
			if err := j.update(func(x *jobs.Record) error { x.Done = append(x.Done, st); return nil }); err != nil {
				slog.Error("runner: record step", "job", rec.ID, "err", err)
				return
			}
			continue
		}
		switch {
		case errors.Is(context.Cause(jctx), errCancelled):
			j.finishCancelled()
		case ctx.Err() != nil:
			// Shutdown: stay in_progress; the next start resumes this step.
			fmt.Fprintf(j.log, "=== colmap-svc stopping during %s; job resumes on restart\n", st)
			j.setWaiting(false)
			j.flush(true)
		default:
			j.failed(err)
		}
		return
	}
	j.completed()
}

func (j *job) update(fn func(*jobs.Record) error) error {
	rec, err := j.r.Jobs.Update(j.rec.ID, fn)
	if err != nil {
		return err
	}
	j.rec = rec
	return nil
}

func (j *job) setStage(step string, sp *jobs.StageProgress) {
	j.pmu.Lock()
	j.stage, j.sp, j.dirty = step, sp, true
	j.pmu.Unlock()
	j.flush(true)
}

func (j *job) setProgress(sp *jobs.StageProgress) {
	j.pmu.Lock()
	j.sp, j.dirty = sp, true
	j.pmu.Unlock()
	j.flush(false)
}

func (j *job) setWaiting(v bool) {
	j.pmu.Lock()
	changed := j.waiting != v
	j.waiting, j.dirty = v, j.dirty || changed
	j.pmu.Unlock()
	if changed {
		j.flush(true)
	}
}

// flush writes stage/progress/waiting, at most every 2 s unless forced.
func (j *job) flush(force bool) {
	j.pmu.Lock()
	if !j.dirty || (!force && time.Since(j.lastSave) < 2*time.Second) {
		j.pmu.Unlock()
		return
	}
	step, sp, waiting := j.stage, j.sp, j.waiting
	j.dirty, j.lastSave = false, time.Now()
	j.pmu.Unlock()
	a := apiStage[step]
	frac := 0.0
	if sp != nil && sp.Total > 0 {
		frac = float64(sp.Done) / float64(sp.Total)
	}
	err := j.update(func(x *jobs.Record) error {
		if jobs.Terminal(x.Status) {
			return nil
		}
		x.Stage, x.StageProgress, x.WaitingForGPU = &a, sp, waiting
		x.Progress = Progress(x.Request, a, frac)
		return nil
	})
	if err != nil {
		slog.Warn("runner: save progress", "job", j.rec.ID, "err", err)
	}
}

func (j *job) end(fn func(*jobs.Record)) {
	now := time.Now().Unix()
	exp := now + int64(j.r.cfg.OutputTTL/time.Second)
	err := j.update(func(x *jobs.Record) error {
		x.Stage, x.StageProgress, x.WaitingForGPU, x.ExpiresAt = nil, nil, false, &exp
		fn(x)
		return nil
	})
	if err != nil {
		slog.Error("runner: end job", "job", j.rec.ID, "err", err)
	}
	_ = os.RemoveAll(j.ws)
}

func (j *job) finishCancelled() {
	now := time.Now().Unix()
	j.end(func(x *jobs.Record) {
		if x.Status == jobs.InProgress {
			x.Status = jobs.Cancelling
		}
		x.Status, x.CancelledAt = jobs.Cancelled, &now
	})
	slog.Info("job cancelled", "job", j.rec.ID)
}

func (j *job) failed(err error) {
	var se *stepError
	if !errors.As(err, &se) {
		se = &stepError{"internal", err.Error()}
	}
	if j.log != nil {
		fmt.Fprintf(j.log, "=== job failed: %s\n", se.Error())
	}
	logBytes := j.copyLog()
	j.end(func(x *jobs.Record) {
		x.Status = jobs.Failed
		x.Error = &jobs.JobError{Code: se.code, Message: se.msg}
		x.Outputs = []jobs.Output{}
		if logBytes > 0 {
			x.Outputs = append(x.Outputs, jobs.Output{Variant: "log", Bytes: logBytes})
		}
	})
	slog.Warn("job failed", "job", j.rec.ID, "code", se.code, "msg", se.msg)
}

func (j *job) completed() {
	now := time.Now().Unix()
	j.end(func(x *jobs.Record) {
		x.Status, x.CompletedAt, x.Progress = jobs.Completed, &now, 100
	})
	slog.Info("job completed", "job", j.rec.ID, "yields", j.rec.Yields)
}

// copyLog copies the job log to out/log.txt; returns its size.
func (j *job) copyLog() int64 {
	if j.log != nil {
		_ = j.log.Sync()
	}
	dst := filepath.Join(j.dir, "out", "log.txt")
	if err := inputs.CopyFile(filepath.Join(j.dir, "colmap.log"), dst); err != nil {
		return 0
	}
	st, err := os.Stat(dst)
	if err != nil {
		return 0
	}
	return st.Size()
}

func (j *job) db() string     { return filepath.Join(j.ws, "database.db") }
func (j *job) images() string { return filepath.Join(j.ws, "images") }

func (j *job) runStep(ctx context.Context, st string) error {
	req := j.rec.Request
	g := j.r.cfg.GPUIndex
	maxSize := strconv.Itoa(req.MaxImageSize)
	switch st {
	case StepPrepare:
		return j.prepare(ctx)
	case StepExtract:
		single := "0"
		if req.Camera.SingleCamera {
			single = "1"
		}
		return j.colmap(ctx, st, extractProgress, "feature_extractor",
			"--database_path", j.db(), "--image_path", j.images(),
			"--ImageReader.camera_model", req.Camera.Model, "--ImageReader.single_camera", single,
			"--FeatureExtraction.use_gpu", "1", "--FeatureExtraction.gpu_index", g,
			"--FeatureExtraction.max_image_size", maxSize)
	case StepPriors:
		return j.writePriors()
	case StepMatch:
		cmd := req.Matcher + "_matcher"
		parse := imageProgress
		if req.Matcher == "exhaustive" {
			parse = blockProgress
		}
		return j.colmap(ctx, st, parse, cmd, "--database_path", j.db(),
			"--FeatureMatching.use_gpu", "1", "--FeatureMatching.gpu_index", g)
	case StepMap:
		return j.mapStep(ctx)
	case StepAlign:
		return j.align(ctx)
	case StepUndistort:
		best, _ := pack.BestModel(filepath.Join(j.ws, "sparse"))
		dense := filepath.Join(j.ws, "dense")
		_ = os.RemoveAll(dense)
		return j.colmap(ctx, st, nil, "image_undistorter", "--image_path", j.images(), "--input_path", best,
			"--output_path", dense, "--output_type", "COLMAP", "--max_image_size", maxSize)
	case StepDense:
		return j.colmap(ctx, st, twoPassProgress(), "patch_match_stereo",
			"--workspace_path", filepath.Join(j.ws, "dense"), "--workspace_format", "COLMAP",
			"--PatchMatchStereo.geom_consistency", "true", "--PatchMatchStereo.max_image_size", maxSize,
			"--PatchMatchStereo.gpu_index", g)
	case StepFuse:
		dense := filepath.Join(j.ws, "dense")
		return j.colmap(ctx, st, fuseProgress, "stereo_fusion", "--workspace_path", dense,
			"--workspace_format", "COLMAP", "--input_type", "geometric", "--output_path", filepath.Join(dense, "fused.ply"))
	case StepMesh:
		dense := filepath.Join(j.ws, "dense")
		if *req.Mesh == "poisson" {
			return j.colmap(ctx, st, nil, "poisson_mesher", "--input_path", filepath.Join(dense, "fused.ply"),
				"--output_path", filepath.Join(dense, "meshed-poisson.ply"))
		}
		return j.colmap(ctx, st, nil, "delaunay_mesher", "--input_path", dense, "--input_type", "dense",
			"--output_path", filepath.Join(dense, "meshed-delaunay.ply"))
	case StepPackage:
		return j.pack()
	}
	return fmt.Errorf("unknown step %q", st)
}

// colmap runs one COLMAP command, under a lease if the step has usage.
func (j *job) colmap(ctx context.Context, step string, parse func(string) *jobs.StageProgress, args ...string) error {
	argv := append([]string{j.r.cfg.Colmap}, args...)
	return j.run(ctx, step, argv, parse)
}

func (j *job) run(ctx context.Context, step string, argv []string, parse func(string) *jobs.StageProgress) error {
	onLine := func(line string) {
		if parse != nil {
			if sp := parse(line); sp != nil {
				j.setProgress(sp)
			}
		}
	}
	fmt.Fprintf(j.log, "$ %s\n", strings.Join(argv, " "))
	usage := j.r.cfg.Usage[step]
	if gpuStep[step] {
		usage = GPUUsage(usage, j.r.cfg.GPUPool, j.rec.Request.MaxImageSize, j.r.cfg.GPUMax)
	}
	if len(usage) == 0 {
		out, err := runOnce(ctx, argv, j.r.cfg.Env, j.ws, j.log, onLine, nil, j.r.cfg.StopGrace, j.r.cfg.YieldKill)
		return j.result(ctx, step, out, err)
	}
	req := lease.Request{
		Extension: j.r.cfg.Extension, Job: j.rec.ID, Stage: step, Owner: j.rec.Owner,
		Usage: usage, Yieldable: gpuStep[step],
	}
	for {
		o := j.r.cfg.LeaseOpts
		o.OnWait = func(waiting bool, why string) {
			if waiting {
				fmt.Fprintf(j.log, "=== waiting for a %s lease: %s\n", step, why)
			}
			j.setWaiting(waiting)
		}
		held, err := lease.Hold(ctx, j.r.cfg.Leaser, req, o)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, lease.ErrGaveUp) || errors.Is(err, lease.ErrNeverFits) {
				return fail("gpu_unavailable", "%v", err)
			}
			return fail("internal", "lease: %v", err)
		}
		fmt.Fprintf(j.log, "=== lease %s granted for %s\n", held.ID(), step)
		j.setWaiting(false)
		out, err := runOnce(ctx, argv, j.r.cfg.Env, j.ws, j.log, onLine, held.Yield(), j.r.cfg.StopGrace, j.r.cfg.YieldKill)
		held.Release()
		if out == yielded {
			fmt.Fprintf(j.log, "=== yielded %s (child: %s); resuming when the lease is granted again\n", step, exitDetail(err))
			_ = j.update(func(x *jobs.Record) error { x.Yields++; return nil })
			j.setWaiting(true)
			continue
		}
		return j.result(ctx, step, out, err)
	}
}

func (j *job) result(ctx context.Context, step string, out outcome, err error) error {
	switch {
	case out == stopped || ctx.Err() != nil:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return context.Canceled
	case err != nil:
		return fail("colmap_failed", "%s failed (%s):\n%s", step, exitDetail(err), tail(filepath.Join(j.dir, "colmap.log"), 20))
	}
	return nil
}

// prepare puts every input image under ws/images with its job name.
func (j *job) prepare(ctx context.Context) error {
	_ = os.RemoveAll(j.ws)
	if err := os.MkdirAll(j.images(), 0o755); err != nil {
		return err
	}
	names := append([]string(nil), j.rec.Names...)
	for _, in := range j.rec.Inputs {
		src := j.r.Files.Path(in.FileID)
		switch {
		case in.Kind == inputs.Image:
			if err := linkOrCopy(src, filepath.Join(j.images(), filepath.FromSlash(in.Name))); err != nil {
				return fail("internal", "input %s: %v", in.FileID, err)
			}
		case inputs.IsArchive(in.Kind):
			if _, err := inputs.Extract(src, in.Kind, j.images()); err != nil {
				return fail("invalid_image", "archive %s: %v", in.FileID, err)
			}
		case in.Kind == inputs.Video:
			fr, err := inputs.Frames(ctx, j.r.cfg.FFmpeg, src, j.rec.Request.FPS, j.images(), in.Name)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return fail("invalid_image", "video %s: %v", in.FileID, err)
			}
			names = append(names, fr...)
		}
	}
	if len(names) < jobs.MinImages {
		return fail("invalid_image", "%d images after unpacking; a reconstruction needs at least %d", len(names), jobs.MinImages)
	}
	if len(names) > j.r.cfg.MaxImages {
		return fail("invalid_image", "%d images after unpacking; the limit is %d", len(names), j.r.cfg.MaxImages)
	}
	return j.update(func(x *jobs.Record) error {
		x.Names = names
		x.Images.Total = len(names)
		return nil
	})
}

func linkOrCopy(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	_ = os.Remove(dst)
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	return inputs.CopyFile(src, dst)
}

// plan merges the request priors with the EXIF snapshot.
func (j *job) plan() (priors.Plan, error) {
	var exif map[string]priors.LLA
	if _, err := readJSON(filepath.Join(j.ws, "exif.json"), &exif); err != nil {
		return priors.Plan{}, err
	}
	req := j.rec.Request
	return priors.Merge(j.rec.Names, j.rec.Priors, exif, priors.Options{
		EXIFGPS: req.EXIFGPS == nil || *req.EXIFGPS, DefaultSigma: req.DefaultPositionSigmaM, FrameOrigin: req.FrameOrigin,
	}), nil
}

// writePriors replaces COLMAP's pose_priors rows (§1c). The EXIF rows are
// snapshotted on the first run, so a rerun does not read its own rows back.
func (j *job) writePriors() error {
	imgs, exif, err := priors.ReadDB(j.db())
	if err != nil {
		return fail("internal", "read database: %v", err)
	}
	snap := filepath.Join(j.ws, "exif.json")
	if ok, _ := readJSON(snap, new(map[string]priors.LLA)); !ok {
		if _, err := pack.WriteJSON(snap, exif); err != nil {
			return err
		}
	}
	plan, err := j.plan()
	if err != nil {
		return err
	}
	n, err := priors.WriteDB(j.db(), imgs, plan)
	if err != nil {
		return fail("internal", "write pose_priors: %v", err)
	}
	fmt.Fprintf(j.log, "=== pose priors: %d rows (%d EXIF GPS rows read, %d request priors), frame %s\n", n, len(exif), len(j.rec.Priors), plan.Frame.Type)
	return j.update(func(x *jobs.Record) error { x.Images.WithPrior = n; return nil })
}

func (j *job) mapStep(ctx context.Context) error {
	plan, err := j.plan()
	if err != nil {
		return err
	}
	req := j.rec.Request
	sparse := filepath.Join(j.ws, "sparse")
	run := filepath.Join(j.ws, "sparse.run")
	_ = os.RemoveAll(run)
	if err := os.MkdirAll(run, 0o755); err != nil {
		return err
	}
	args := []string{"--database_path", j.db(), "--image_path", j.images(), "--output_path", run}
	const outArg = 5 // index of the output_path value
	cmd := "mapper"
	resumable := true
	switch {
	case req.Mapper == "global":
		cmd, resumable = "global_mapper", false
		_ = os.RemoveAll(sparse)
	case len(plan.Rows) >= priors.MinPriors:
		cmd = "pose_prior_mapper"
		args = append(args, "--use_robust_loss_on_prior_position", "1")
	}
	if resumable {
		if best, n := pack.BestModel(sparse); best != "" && n > 0 {
			fmt.Fprintf(j.log, "=== resuming the mapper from %s (%d images registered)\n", best, n)
			// A continued model is written straight into output_path (not
			// output_path/0; exe/sfm.cc RunIncrementalMapperImpl), so point
			// it at run/0 to keep the sparse/<n> layout.
			if err := os.MkdirAll(filepath.Join(run, "0"), 0o755); err != nil {
				return err
			}
			args[outArg] = filepath.Join(run, "0")
			args = append(args, "--input_path", best)
		}
	}
	total := len(j.rec.Names)
	parse := func(line string) *jobs.StageProgress {
		if m := regRe.FindStringSubmatch(line); m != nil {
			n, _ := strconv.Atoi(m[1])
			return &jobs.StageProgress{Done: n, Total: total, Unit: "images"}
		}
		return nil
	}
	err = j.colmap(ctx, StepMap, parse, append([]string{cmd}, args...)...)
	// A graceful stop (shutdown) writes the partial model; keep it so the
	// restarted mapper continues from it.
	if best, n := pack.BestModel(run); best != "" && n > 0 && (err == nil || resumable) {
		_ = os.RemoveAll(sparse)
		if rerr := os.Rename(run, sparse); rerr != nil && err == nil {
			return rerr
		}
	}
	if err != nil {
		return err
	}
	if best, _ := pack.BestModel(sparse); best == "" {
		return fail("no_images_registered", "the mapper registered no images: too little overlap or texture between them")
	}
	return nil
}

func (j *job) align(ctx context.Context) error {
	plan, err := j.plan()
	if err != nil {
		return err
	}
	best, _ := pack.BestModel(filepath.Join(j.ws, "sparse"))
	if best == "" {
		return fail("no_images_registered", "no sparse model")
	}
	if j.rec.Request.Mapper == "global" && len(plan.Rows) >= priors.MinPriors {
		ref := filepath.Join(j.ws, "ref_images.txt")
		var b strings.Builder
		for n, row := range plan.Rows {
			fmt.Fprintf(&b, "%s %.9f %.9f %.9f\n", n, row.Pos[0], row.Pos[1], row.Pos[2])
		}
		if err := os.WriteFile(ref, []byte(b.String()), 0o644); err != nil {
			return err
		}
		maxErr := math.Max(3*priors.MedianSigma(plan), 0.1)
		aligned := best + ".aligned"
		_ = os.RemoveAll(aligned)
		if err := os.MkdirAll(aligned, 0o755); err != nil {
			return err
		}
		err := j.colmap(ctx, StepAlign, nil, "model_aligner", "--input_path", best, "--output_path", aligned,
			"--ref_images_path", ref, "--ref_is_gps", "0", "--alignment_type", "custom",
			"--alignment_max_error", strconv.FormatFloat(maxErr, 'f', 3, 64))
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, nerr := pack.NumRegImages(aligned); err == nil && nerr == nil {
			_ = os.RemoveAll(best)
			if err := os.Rename(aligned, best); err != nil {
				return err
			}
		} else {
			fmt.Fprintf(j.log, "=== model_aligner did not align (%v); keeping the unaligned model\n", err)
			j.alignFailed = true
		}
	}
	txt := filepath.Join(j.ws, "txt")
	_ = os.RemoveAll(txt)
	if err := os.MkdirAll(txt, 0o755); err != nil {
		return err
	}
	if err := j.colmap(ctx, StepAlign, nil, "model_converter", "--input_path", best, "--output_path", txt, "--output_type", "TXT"); err != nil {
		return err
	}
	imgs, err := pack.ReadImagesTxt(filepath.Join(txt, "images.txt"))
	if err != nil {
		return fail("internal", "read images.txt: %v", err)
	}
	if len(imgs) == 0 {
		return fail("no_images_registered", "the model has no registered images")
	}
	centres := map[string][3]float64{}
	for _, im := range imgs {
		centres[im.Name] = im.Centre
	}
	logb, _ := os.ReadFile(filepath.Join(j.dir, "colmap.log"))
	restore := j.alignFailed || strings.Contains(string(logb), "Final alignment w.r.t. prior positions failed")
	rep := priors.Report(plan, centres, restore)
	frame := priors.Frame{Type: "arbitrary", Units: "unknown"}
	if rep.Status == "aligned" {
		frame = plan.Frame
	}
	reg := len(imgs)
	if reg*2 < len(j.rec.Names) {
		fmt.Fprintf(j.log, "=== only %d of %d images registered\n", reg, len(j.rec.Names))
	}
	return j.update(func(x *jobs.Record) error {
		x.Images.Registered = &reg
		x.Frame, x.Alignment = &frame, &rep
		return nil
	})
}

func (j *job) pack() error {
	out := filepath.Join(j.dir, "out")
	best, _ := pack.BestModel(filepath.Join(j.ws, "sparse"))
	txt := filepath.Join(j.ws, "txt")
	frame := j.rec.Frame
	members := map[string]string{}
	for _, f := range []string{"cameras.bin", "images.bin", "points3D.bin", "rigs.bin", "frames.bin"} {
		if _, err := os.Stat(filepath.Join(best, f)); err == nil {
			members[f] = filepath.Join(best, f)
		}
	}
	ents, _ := os.ReadDir(txt)
	for _, e := range ents {
		members[e.Name()] = filepath.Join(txt, e.Name())
	}
	var outs []jobs.Output
	n, err := pack.Zip(filepath.Join(out, "sparse.zip"), members, frame)
	if err != nil {
		return err
	}
	cams, err := pack.ReadCamerasTxt(filepath.Join(txt, "cameras.txt"))
	if err != nil {
		return fail("internal", "read cameras.txt: %v", err)
	}
	imgs, err := pack.ReadImagesTxt(filepath.Join(txt, "images.txt"))
	if err != nil {
		return fail("internal", "read images.txt: %v", err)
	}
	cn, err := pack.WriteJSON(filepath.Join(out, "cameras.json"), map[string]any{
		"object": "reconstruction.cameras", "id": j.rec.ID, "frame": frame, "alignment": j.rec.Alignment,
		"cameras": cams, "images": imgs,
	})
	if err != nil {
		return err
	}
	outs = append(outs, jobs.Output{Variant: "cameras", Bytes: cn})
	if tf, err := pack.Transforms(cams, imgs, map[string]any{"frame": frame}); err != nil {
		fmt.Fprintf(j.log, "=== no transforms variant: %v\n", err)
	} else if tn, err := pack.WriteJSON(filepath.Join(out, "transforms.json"), tf); err != nil {
		return err
	} else {
		outs = append(outs, jobs.Output{Variant: "transforms", Bytes: tn})
	}
	outs = append(outs, jobs.Output{Variant: "sparse", Bytes: n})
	dense := filepath.Join(j.ws, "dense")
	if j.rec.Request.Dense {
		n, err := pack.Zip(filepath.Join(out, "dense.zip"), map[string]string{"fused.ply": filepath.Join(dense, "fused.ply")}, frame)
		if err != nil {
			return fail("internal", "pack dense: %v", err)
		}
		outs = append(outs, jobs.Output{Variant: "dense", Bytes: n})
		if m := j.rec.Request.Mesh; m != nil {
			name := "meshed-" + *m + ".ply"
			n, err := pack.Zip(filepath.Join(out, "mesh.zip"), map[string]string{name: filepath.Join(dense, name)}, frame)
			if err != nil {
				return fail("internal", "pack mesh: %v", err)
			}
			outs = append(outs, jobs.Output{Variant: "mesh", Bytes: n})
			if mesh, err := pack.ReadPLYMesh(filepath.Join(dense, name)); err != nil {
				fmt.Fprintf(j.log, "=== no glb variant: %v\n", err)
			} else if gn, err := pack.WriteGLB(filepath.Join(out, "mesh.glb"), mesh, "corrallm colmap-svc"); err != nil {
				fmt.Fprintf(j.log, "=== no glb variant: %v\n", err)
			} else {
				outs = append(outs, jobs.Output{Variant: "glb", Bytes: gn})
			}
		}
	}
	fmt.Fprintf(j.log, "=== packaged %d variants\n", len(outs)+1)
	if ln := j.copyLog(); ln > 0 {
		outs = append(outs, jobs.Output{Variant: "log", Bytes: ln})
	}
	return j.update(func(x *jobs.Record) error { x.Outputs = outs; return nil })
}

// Sweep deletes the outputs of jobs past expires_at.
func (r *Runner) Sweep() {
	recs, err := r.Jobs.Expired()
	if err != nil {
		slog.Warn("sweep jobs", "err", err)
		return
	}
	for _, rec := range recs {
		if err := os.RemoveAll(r.JobDir(rec.ID)); err != nil {
			slog.Warn("sweep job dir", "job", rec.ID, "err", err)
			continue
		}
		_, _ = r.Jobs.Update(rec.ID, func(x *jobs.Record) error {
			x.Purged, x.Outputs = true, []jobs.Output{}
			return nil
		})
		slog.Info("job outputs expired", "job", rec.ID)
	}
}

var (
	processedRe = regexp.MustCompile(`Processed file \[(\d+)/(\d+)\]`)
	blockRe     = regexp.MustCompile(`Processing block \[(\d+)/(\d+), (\d+)/(\d+)\]`)
	imageRe     = regexp.MustCompile(`Processing image \[(\d+)/(\d+)\]`)
	regRe       = regexp.MustCompile(`Registering image #\d+ \(num_reg_frames=(\d+)\)`)
	viewRe      = regexp.MustCompile(`Processing view (\d+) / (\d+)`)
	fuseRe      = regexp.MustCompile(`Fusing image \[(\d+)/(\d+)\]`)
)

func pair(re *regexp.Regexp, line, unit string) *jobs.StageProgress {
	m := re.FindStringSubmatch(line)
	if m == nil {
		return nil
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[2])
	return &jobs.StageProgress{Done: a, Total: b, Unit: unit}
}

func extractProgress(l string) *jobs.StageProgress { return pair(processedRe, l, "images") }
func imageProgress(l string) *jobs.StageProgress   { return pair(imageRe, l, "images") }

// twoPassProgress counts PatchMatch with geom_consistency: a photometric
// then a geometric pass over the same N views, each logging "view i / N".
// A smaller i than the last one starts the second pass.
func twoPassProgress() func(string) *jobs.StageProgress {
	pass, last := 0, 0
	return func(l string) *jobs.StageProgress {
		sp := pair(viewRe, l, "views")
		if sp == nil {
			return nil
		}
		if sp.Done < last && pass == 0 {
			pass = 1
		}
		last = sp.Done
		return &jobs.StageProgress{Done: pass*sp.Total + sp.Done - 1, Total: 2 * sp.Total, Unit: "views"}
	}
}
func fuseProgress(l string) *jobs.StageProgress { return pair(fuseRe, l, "images") }

// blockProgress: exhaustive matching walks n×n blocks; [i/n, j/n] is the
// ((i-1)·n + j)-th, started (so done = that − 1).
func blockProgress(l string) *jobs.StageProgress {
	m := blockRe.FindStringSubmatch(l)
	if m == nil {
		return nil
	}
	i, _ := strconv.Atoi(m[1])
	n, _ := strconv.Atoi(m[2])
	k, _ := strconv.Atoi(m[3])
	return &jobs.StageProgress{Done: (i-1)*n + k - 1, Total: n * n, Unit: "blocks"}
}

func readJSON(p string, v any) (bool, error) {
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(b, v)
}
