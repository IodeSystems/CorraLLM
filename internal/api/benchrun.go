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
// things a UI needs and a shell does not — captured output and a cancel button.
//
// The run is NOT privileged. It holds no lease, evicts nothing, and turns no
// caller away: it queues for admission like any other client and waits out 429
// backpressure, which llm-bench retries without limit and subtracts from its
// timings. Isolating model time from queue time was the only thing exclusivity
// bought, and measuring the queue directly buys it without an outage.

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

// randomKey mints the caller key for this run — the identity corrallm attributes
// its requests to in the activity log and the fairshare scheduler.
//
// Per-run rather than configured, so one run's traffic is separable from
// another's. Note the consequence: a minted key is absent from config `keys:`,
// so a spawned run resolves to the `default` priority group. A run that should
// be low priority has to be started from a shell with a key that IS mapped.
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
	ProbeDirs  []string
	Models     []string
	Classes    []string
	TTLSeconds int
	Reason     string
}

// Start spawns llm-bench as an ordinary caller.
//
// It holds no lease and evicts nothing: the run competes for admission slots
// like any other client, waits out 429 backpressure and subtracts that wait
// from its timings. Separating model time from queue time is what exclusivity
// used to buy, and measuring the queue gets it without an outage.
func (b *BenchRunner) Start(opts BenchStartOptions) (BenchRunStatus, error) {
	if b == nil {
		return BenchRunStatus{}, fmt.Errorf("bench runner unavailable")
	}
	// Resolve the binary BEFORE taking the lock. --bench-bin defaults to
	// "llm-bench" on $PATH, which a fresh install does not have, and discovering
	// that at cmd.Start() means the operator reads "executable file not found in
	// $PATH" instead of which flag to set.
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

	args := []string{"run"}
	if opts.ConfigPath != "" {
		args = append(args, "--config", opts.ConfigPath)
	}
	if len(opts.ProbeDirs) > 0 {
		args = append(args, "--tasks-dir", strings.Join(opts.ProbeDirs, ","))
	}
	if len(opts.Models) > 0 {
		args = append(args, "--models", strings.Join(opts.Models, ","))
	}
	if len(opts.Classes) > 0 {
		args = append(args, "--classes", strings.Join(opts.Classes, ","))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, opts.Bin, args...)
	// The bench presents this key as its caller identity, and carries the admin
	// token so it can still drive load/unload.
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
