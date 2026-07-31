package api

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Bench run orchestration — corrallm spawns llm-bench.
//
// llm-bench remains a first-class CLI: this exec's the SAME binary with the same
// flags a human would type, so anything the UI can start is scriptable, and a
// scripted run behaves identically to a clicked one. corrallm adds only the
// things a UI needs and a shell does not — a lease held for the run's duration,
// captured output, and a cancel button.
//
// The run is destructive by design: it evicts models to take cold measurements
// and turns away every other caller. So the lease is acquired BEFORE the process
// starts and released when it exits, including on crash or cancel.

// benchLogCap bounds captured output so a runaway benchmark cannot exhaust
// memory through its log alone.
const benchLogCap = 2000

// BenchRunner owns the at-most-one in-flight bench process.
type BenchRunner struct {
	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	started time.Time
	args    []string
	key     string

	logMu sync.RWMutex
	lines []string
	done  bool
	err   string
}

// NewBenchRunner builds an idle runner.
func NewBenchRunner() *BenchRunner { return &BenchRunner{} }

// BenchRunStatus is the UI's view of the current or last run.
type BenchRunStatus struct {
	Running   bool     `json:"running"`
	StartedAt int64    `json:"startedAt,omitempty"`
	Args      []string `json:"args,omitempty" doc:"The exact llm-bench invocation — copy it to reproduce this run from a shell."`
	Log       []string `json:"log,omitempty"`
	Done      bool     `json:"done"`
	Error     string   `json:"error,omitempty"`
}

// Status snapshots the runner.
func (b *BenchRunner) Status() BenchRunStatus {
	if b == nil {
		return BenchRunStatus{}
	}
	b.mu.Lock()
	running, started, args := b.running, b.started, append([]string(nil), b.args...)
	b.mu.Unlock()
	b.logMu.RLock()
	defer b.logMu.RUnlock()
	st := BenchRunStatus{
		Running: running, Args: args,
		Log:  append([]string(nil), b.lines...),
		Done: b.done, Error: b.err,
	}
	if !started.IsZero() {
		st.StartedAt = started.Unix()
	}
	return st
}

func (b *BenchRunner) appendLine(s string) {
	b.logMu.Lock()
	defer b.logMu.Unlock()
	b.lines = append(b.lines, s)
	if len(b.lines) > benchLogCap {
		b.lines = b.lines[len(b.lines)-benchLogCap:]
	}
}

// Cancel stops an in-flight run. Idempotent.
func (b *BenchRunner) Cancel() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cancel != nil {
		b.cancel()
	}
}

// randomKey mints the caller key for this run. Generated per-run rather than
// configured: it is the credential that distinguishes the bench from everyone
// else during the lockout, so it must not be guessable or reused across runs.
func randomKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "bench-" + hex.EncodeToString(b), nil
}

// BenchStartOptions configures one spawn.
type BenchStartOptions struct {
	Bin        string
	ConfigPath string
	ProbesDir  string
	Models     []string
	Classes    []string
	TTLSeconds int
	Reason     string
	// Exclusive takes corrallm's calibration lease for the run: every other
	// caller gets 429 until it finishes, and cold passes may evict the GPU.
	// Required for cold-mode probes to measure anything; otherwise a needless
	// outage.
	Exclusive bool
}

// Start spawns llm-bench, optionally under the exclusive calibration lease.
//
// beginLease/endLease are injected so the runner does not import the proxy: the
// caller wires them to the CalibrationState. endLease is called on EVERY exit
// path, because a lease outliving its run is an outage.
func (b *BenchRunner) Start(
	opts BenchStartOptions,
	beginLease func(key, reason string, ttl time.Duration) (time.Time, bool),
	endLease func(key string),
) (BenchRunStatus, error) {
	if b == nil {
		return BenchRunStatus{}, fmt.Errorf("bench runner unavailable")
	}
	// Resolve the binary BEFORE taking the lock or the lease. --bench-bin
	// defaults to "llm-bench" on $PATH, which a fresh install does not have, and
	// discovering that at cmd.Start() means the failure arrives after the
	// exclusive lease was granted — a lockout that evicts every model in service
	// of a run that was never going to happen. It also means the operator reads
	// "executable file not found in $PATH" instead of which flag to set.
	if _, err := exec.LookPath(opts.Bin); err != nil {
		return BenchRunStatus{}, fmt.Errorf(
			"llm-bench is not available: %q could not be resolved. "+
				"Build it from the corrallm repo and point --bench-bin at it", opts.Bin)
	}
	b.mu.Lock()
	if b.running {
		b.mu.Unlock()
		return b.Status(), fmt.Errorf("a bench run is already in flight")
	}
	key, err := randomKey()
	if err != nil {
		b.mu.Unlock()
		return BenchRunStatus{}, err
	}

	// The LEASE is the lockout, not the --exclusive flag: holding it is what
	// turns every other caller away with 429. A shared run must therefore not
	// take it at all, rather than take it and behave politely.
	//
	// Stubbed rather than branched at each call site because endLease runs on
	// every exit path below, and a lease released on a path that never acquired
	// one is the kind of asymmetry that later gets "fixed" by removing the
	// release.
	if !opts.Exclusive {
		beginLease = func(string, string, time.Duration) (time.Time, bool) { return time.Time{}, true }
		endLease = func(string) {}
	}
	ttl := time.Duration(opts.TTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = defaultCalibrationTTL * time.Second
	}
	if _, ok := beginLease(key, opts.Reason, ttl); !ok {
		b.mu.Unlock()
		return BenchRunStatus{}, fmt.Errorf("could not acquire the calibration lease (another run holds it)")
	}

	args := []string{"run"}
	if opts.ConfigPath != "" {
		args = append(args, "--config", opts.ConfigPath)
	}
	if opts.ProbesDir != "" {
		args = append(args, "--tasks-dir", opts.ProbesDir)
	}
	if len(opts.Models) > 0 {
		args = append(args, "--models", strings.Join(opts.Models, ","))
	}
	if len(opts.Classes) > 0 {
		args = append(args, "--classes", strings.Join(opts.Classes, ","))
	}
	// Exclusive is OPT-IN, not the default it used to be.
	//
	// Holding the lease locks every other caller out of the box for the whole
	// run — 429 + Retry-After to everyone, every resident model evicted — which
	// is a lot to charge for a benchmark on a machine that is also serving. A
	// shared run instead waits out the backpressure (llm-bench retries 429
	// without limit) and subtracts the waiting from its timings, so the numbers
	// still describe the model rather than the queue.
	//
	// The cost is cold passes: they need eviction rights to prove anything, so a
	// shared run skips them and records the skip. Ask for exclusive when the
	// cold path is what you came to measure.
	if opts.Exclusive {
		args = append(args, "--exclusive")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, opts.Bin, args...)
	// The bench presents this key so the lease serves it while everyone else is
	// turned away, and carries the admin token so it can drive load/unload.
	cmd.Env = append(os.Environ(), "CORRALLM_BENCH_KEY="+key)
	// llm-bench resolves its MCP helper (llm-bench-mcp) from local/bin RELATIVE
	// TO ITS CWD, or from $PATH. corrallm's cwd is wherever corrallm was
	// started — typically an ops repo, not the bench's build tree — so the
	// relative lookup finds nothing. The two binaries are always built side by
	// side, so putting the bench binary's own directory on PATH makes the helper
	// resolvable no matter where corrallm runs from.
	if dir := filepath.Dir(opts.Bin); dir != "" && dir != "." {
		if abs, err := filepath.Abs(dir); err == nil {
			cmd.Env = append(cmd.Env, "PATH="+abs+string(os.PathListSeparator)+os.Getenv("PATH"))
		}
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		endLease(key)
		b.mu.Unlock()
		return BenchRunStatus{}, err
	}
	// Merge stderr into the same pipe: llm-bench logs progress to stderr, and a
	// UI showing only stdout would look frozen for the whole run.
	cmd.Stderr = cmd.Stdout

	b.running, b.cancel, b.started, b.key = true, cancel, time.Now(), key
	b.args = append([]string{opts.Bin}, args...)
	b.mu.Unlock()

	b.logMu.Lock()
	b.lines, b.done, b.err = nil, false, ""
	b.logMu.Unlock()
	b.appendLine(fmt.Sprintf("$ %s %s", opts.Bin, strings.Join(args, " ")))

	if err := cmd.Start(); err != nil {
		cancel()
		endLease(key)
		b.mu.Lock()
		b.running = false
		b.mu.Unlock()
		return BenchRunStatus{}, fmt.Errorf("start %s: %w", opts.Bin, err)
	}

	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			b.appendLine(sc.Text())
		}
		werr := cmd.Wait()
		cancel()
		// Release on EVERY exit path. A lease that outlives its run turns a
		// finished benchmark into an ongoing outage; the lease's own TTL is the
		// backstop, not the plan.
		endLease(key)
		b.mu.Lock()
		b.running = false
		b.mu.Unlock()
		b.logMu.Lock()
		b.done = true
		if werr != nil {
			b.err = werr.Error()
			b.lines = append(b.lines, "llm-bench exited: "+werr.Error())
		} else {
			b.lines = append(b.lines, "llm-bench finished")
		}
		b.logMu.Unlock()
	}()

	return b.Status(), nil
}
