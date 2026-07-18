// Package proc manages spawned backend processes and the residency layer: which
// models are loaded where, bounded by each server's per-pool memory budget, with
// an eviction solver and stickiness shaping load/evict decisions.
//
// Scheduling (internal/sched) decides who/where among ready backends; residency
// decides what's warm. A spawn is admitted only if it fits its server's pool
// budget — else the eviction solver frees idle, lower-value, non-pinned
// residents to make room (swap), and if it can't, EnsureReady returns
// ErrNoCapacity so the request edge spills to the next backend (evict-then-spill).
package proc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/gpu"
	"github.com/iodesystems/corrallm/internal/tune"
)

// defaultVRAMMargin is the MiB of free VRAM kept back (unused) when sizing
// --parallel from a cached profile — headroom against measurement noise and
// whatever else shares the GPU.
const defaultVRAMMargin = 512

// vramSampleInterval is how often the runtime peak sampler re-probes a
// resident process's VRAM footprint (see sampleVRAMPeak).
const vramSampleInterval = 15 * time.Second

// reParallel matches a llama-server `--parallel N` flag so it can be rewritten
// in place. If a model's cmd has no --parallel flag, tuneCmd leaves it
// untouched entirely rather than injecting one (spec: additive only).
var reParallel = regexp.MustCompile(`--parallel\s+\d+`)

// ErrNoCapacity means a backend can't be made to fit its server even after
// considering eviction — the caller should spill to the next backend.
// Returned wrapped in a *CapacityError; match with errors.Is.
var ErrNoCapacity = errors.New("no capacity")

// CapacityError carries WHY a backend didn't fit, so the request edge can tell
// "wait, a resident is about to become evictable" (transient → 429 with a
// Retry-After the client can actually shape against) apart from "this will
// never fit here" (permanent → 503, a real operator-visible fault).
//
// The distinction matters because the two get opposite client treatment: an
// agentkit-style client retries 429 against its whole retry budget but caps 5xx
// at a handful of attempts, so mislabeling a mid-swap capacity miss as 5xx made
// every cold load inside a ~15s window unretryable.
//
// RetryAfter is the wall-clock until the earliest blocking resident leaves its
// protection window (activeUse / minResidency) and becomes a legal victim. It
// is a lower bound on "when could this succeed", not a promise — another
// request may take the freed room first.
type CapacityError struct {
	// Permanent reports that usage exceeds the server's pool budget even with
	// every non-persistent resident evicted. Retrying cannot help.
	Permanent bool
	// RetryAfter is when the earliest blocker becomes evictable. Zero when
	// Permanent, or when no blocker has a predictable expiry (refs still held).
	RetryAfter time.Duration
	// Blocking names the residents standing in the way, for diagnostics.
	Blocking []string
}

func (e *CapacityError) Error() string {
	if e.Permanent {
		return fmt.Sprintf("no capacity: exceeds pool budget (blocking=%v)", e.Blocking)
	}
	return fmt.Sprintf("no capacity: retry_after=%s (blocking=%v)", e.RetryAfter, e.Blocking)
}

func (e *CapacityError) Unwrap() error { return ErrNoCapacity }

// minResidency protects a just-loaded backend from eviction for a short window,
// damping load/evict thrash under bursty contention.
const minResidency = 10 * time.Second

// defaultActiveUse treats a model as non-idle for eviction if a request
// touched it this recently. refs only guards a model DURING a request — a
// multi-turn agent session drops refs to 0 for milliseconds between turns, and
// a competing load in that window would evict a model that is in active
// conversational use (observed: 107 no-capacity spills during a bench run as
// live chat-lane traffic and the bench evicted each other between turns).
// Within this window a model can't be chosen as an eviction victim; the
// incoming load spills/queues per its stage instead.
const defaultActiveUse = 30 * time.Second

// State is a backend process's lifecycle state.
type State string

const (
	StateAbsent   State = "absent"
	StateLoading  State = "loading"
	StateReady    State = "ready"
	StateFailed   State = "failed"
	StateEvicting State = "evicting"
)

// Process tracks one backend (spawned or pure-proxy).
type Process struct {
	Name      string // "<servedModel>#<backendIndex>"
	ModelName string
	Target    *config.ProxyTarget

	server     string           // "" for pure-proxy (consumes no pools)
	usage      map[string]int64 // reserved bytes per pool
	persistent bool             // pinned: never evicted
	evictRank  int              // 0 low … 2 high (resistance to eviction)
	ttl        time.Duration    // idle keep-warm window

	logs *logBuffer // captured stdout/stderr (spawned backends only; nil for pure-proxy)

	hasUI atomic.Int32 // 0 unknown · 1 has a web UI · 2 none (probed once when ready, P11b)

	mu         sync.Mutex
	state      State
	cmd        *exec.Cmd
	ready      chan struct{} // closed when load resolves; supports coalescing
	err        error
	refs       int       // in-flight requests holding this backend
	readyAt    time.Time // when it became ready (min-residency anchor)
	lastUsed   time.Time
	tunedSlots int // --parallel N actually applied by the auto-tuner; 0 = untuned (config default stands)
}

// Manager owns all processes and the per-server residency ledger.
type Manager struct {
	cfg *config.Config

	mu     sync.Mutex
	procs  map[string]*Process
	used   map[string]map[string]int64 // server → pool → reserved bytes
	budget map[string]map[string]int64 // server → pool → (total − reserve)

	healthCli     *http.Client
	healthTimeout time.Duration
	activeUse     time.Duration // recently-used models are not eviction victims

	// tuneCache is the VRAM slot auto-tuner's profile store. Unset (nil, the
	// zero value) — the default — means introspection is entirely disabled:
	// every spawn uses its configured cmd/maxConcurrent verbatim. Set via
	// SetTuneCache before the first EnsureReady/Preload.
	tuneCache  *tune.Cache
	vramMargin int // MiB of free VRAM kept back when sizing --parallel (default defaultVRAMMargin)
}

// NewManager constructs a Manager and precomputes each server's pool budgets.
func NewManager(cfg *config.Config) *Manager {
	m := &Manager{
		cfg:           cfg,
		procs:         map[string]*Process{},
		used:          map[string]map[string]int64{},
		budget:        map[string]map[string]int64{},
		healthCli:     &http.Client{Timeout: 2 * time.Second},
		healthTimeout: 120 * time.Second,
		activeUse:     defaultActiveUse,
		vramMargin:    defaultVRAMMargin,
	}
	for name, srv := range cfg.Servers {
		totals, _ := config.ParseSizes(srv.Pools) // validated at config load
		reserve, _ := config.ParseSizes(srv.Reserve)
		b := map[string]int64{}
		for pool, total := range totals {
			budget := total - reserve[pool]
			if budget < 0 {
				budget = 0
			}
			b[pool] = budget
		}
		m.budget[name] = b
		m.used[name] = map[string]int64{}
	}
	return m
}

// SetHealthTimeout overrides how long a cold spawn may take to become healthy
// before it's marked failed (default 120s). Large models with big KV caches can
// need longer (llama-swap's healthCheckTimeout analog). A non-positive d is
// ignored. Set before the first EnsureReady.
func (m *Manager) SetHealthTimeout(d time.Duration) {
	if d > 0 {
		m.healthTimeout = d
	}
}

// SetTuneCache wires the VRAM slot auto-tuner's profile cache. Unset (the
// nil default), every spawn uses its configured cmd/maxConcurrent verbatim —
// tuning is entirely opt-in and additive on top of that. Set before the
// first EnsureReady/Preload.
func (m *Manager) SetTuneCache(c *tune.Cache) {
	m.tuneCache = c
}

// SetVRAMMargin overrides the MiB of free VRAM kept back (unused) when sizing
// --parallel from a cached profile (default 512). A non-positive mb is
// ignored.
func (m *Manager) SetVRAMMargin(mb int) {
	if mb > 0 {
		m.vramMargin = mb
	}
}

// EnsureReady returns a ready Process for backend (spawning + health-checking on
// first use, coalescing concurrent loads) plus a release func that MUST be
// called when the request finishes — it drops the residency ref so the backend
// becomes evictable. A spawn that won't fit triggers eviction; if that can't
// free enough, EnsureReady returns ErrNoCapacity.
//
// loaded reports whether THIS call initiated the (cold) load rather than
// coalescing behind an in-flight or already-warm backend — the caller charges
// the load's swap cost to the request that triggered it (P6).
// sticky optionally overrides the model's own residency stickiness (a lane
// member loaded on the lane's behalf may unload sooner); nil → model's own.
func (m *Manager) EnsureReady(ctx context.Context, name string, mdl config.Model, sticky *config.Sticky) (proc *Process, release func(), loaded bool, err error) {
	target, err := mdl.ProxyTarget()
	if err != nil {
		return nil, nil, false, err
	}

	m.mu.Lock()
	p := m.procs[name]
	triggered := p == nil
	if p == nil {
		usage, _ := config.ParseSizes(mdl.RAMUsage) // validated at config load
		// Residency applies to spawned models bound to a server pool; pure
		// proxies (remote/paid) consume no local pools.
		if mdl.Server != "" && len(usage) > 0 {
			if err := m.makeRoomLocked(mdl.Server, usage); err != nil {
				m.mu.Unlock()
				return nil, nil, false, err
			}
			m.reserveLocked(mdl.Server, usage)
		}
		st := mdl.Sticky
		if sticky != nil {
			st = sticky
		}
		var lb *logBuffer
		if mdl.Cmd != "" {
			lb = newLogBuffer(500) // capture spawned-backend output for the logs view
		}
		p = &Process{
			Name:       name,
			ModelName:  name,
			Target:     target,
			server:     mdl.Server,
			usage:      usage,
			persistent: mdl.Persistent,
			evictRank:  evictRank(st),
			ttl:        stickyTTL(st),
			logs:       lb,
			state:      StateAbsent,
			ready:      make(chan struct{}),
		}
		m.procs[name] = p
		m.mu.Unlock()
		go m.load(name, mdl, p)
	} else {
		m.mu.Unlock()
	}

	select {
	case <-p.ready:
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.state != StateReady {
			return nil, nil, false, fmt.Errorf("backend %s not ready: %w", name, p.err)
		}
		p.refs++
		p.lastUsed = time.Now()
		return p, m.releaser(p), triggered, nil
	case <-ctx.Done():
		return nil, nil, false, ctx.Err()
	}
}

// releaser drops one residency ref (the backend stays warm).
func (m *Manager) releaser(p *Process) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			p.refs--
			p.lastUsed = time.Now()
			p.mu.Unlock()
		})
	}
}

// load spawns the model's process (if it has a cmd) and waits for health.
func (m *Manager) load(name string, mdl config.Model, p *Process) {
	p.mu.Lock()
	p.state = StateLoading
	p.mu.Unlock()

	finish := func(st State, err error) {
		p.mu.Lock()
		p.state, p.err = st, err
		if st == StateReady {
			p.readyAt = time.Now()
			p.lastUsed = p.readyAt
		}
		p.mu.Unlock()
		if st == StateFailed {
			// Release reserved pools and drop the entry so a later request retries.
			m.onProcExit(name, p)
		}
		close(p.ready)
	}

	if mdl.Cmd != "" {
		// A local copy: tuneCmd may rewrite --parallel N in place, and it must
		// NEVER mutate mdl (config.Model is passed by value into load, but mdl.Cmd
		// is still the same backing string as m.cfg.Models[name].Cmd until copied).
		cmdStr := mdl.Cmd
		tunedSlots := m.tuneCmd(name, &cmdStr, mdl.Slots())
		if tunedSlots > 0 {
			p.mu.Lock()
			p.tunedSlots = tunedSlots
			p.mu.Unlock()
		}

		cmd := exec.Command("sh", "-c", cmdStr)
		// Tee output to our stdout AND the per-backend ring buffer (for the logs
		// view + n_ctx/n_slots/KV-size parsing).
		out := io.Writer(os.Stdout)
		if p.logs != nil {
			out = io.MultiWriter(os.Stdout, p.logs)
		}
		cmd.Stdout, cmd.Stderr = out, out
		cmd.SysProcAttr = sysProcAttr()
		if err := cmd.Start(); err != nil {
			finish(StateFailed, fmt.Errorf("spawn %q: %w", cmdStr, err))
			return
		}
		p.mu.Lock()
		p.cmd = cmd
		p.mu.Unlock()
		stopped := make(chan struct{}) // closed when cmd.Wait() returns — stops the peak sampler
		go func() {
			err := cmd.Wait()
			close(stopped)
			slog.Info("backend exited", "name", name, "err", err)
			m.onProcExit(name, p) // free pools if it exited on its own (idempotent)
		}()
		// Track this process's VRAM footprint over its lifetime so a burst well
		// after boot (long-context growth, a big batch) still feeds the NEXT
		// spawn's tuning, not just the boot-time snapshot below.
		go m.sampleVRAMPeak(name, cmd.Process.Pid, stopped)
		slog.Info("backend spawned", "name", name, "pid", cmd.Process.Pid, "target", p.Target.URL.String())

		// Wait until the spawned server can actually serve. A pure-proxy backend
		// (no cmd) targets a remote we don't own — don't gate readiness on its
		// /health; proxy immediately and let per-request errors surface.
		if err := m.waitHealthy(p.Target); err != nil {
			finish(StateFailed, err)
			return
		}

		// Boot-time measurement: an exact per-process VRAM read (we spawned it, so
		// the PID is exact — no guessing at "GPU used minus everyone else") minus
		// the KV cache total gives BaseMiB; KV/nSlots gives PerSlotMiB, when
		// llama.cpp's log reports a KV size at all. When it doesn't (kvMiB==0),
		// BaseMiB/PerSlotMiB fall back to the slope between this and a prior
		// spawn's footprint at a different slot count (tune.SlopeFromSamples).
		// Feeds this model's NEXT spawn, never this one. Best-effort: any
		// gpu/tune failure is logged and skipped, never fatal — the backend is
		// already StateReady.
		m.measure(name, mdl, p, cmd.Process.Pid)
	}

	slog.Info("backend ready", "name", name, "target", p.Target.URL.String())
	finish(StateReady, nil)
	// Probe whether the backend serves a web UI at its root (P11b) so the dashboard
	// can disable a dead "Open UI" button. Spawned backends only — we don't poke a
	// remote/paid endpoint's root. Async: never gates readiness.
	if mdl.Cmd != "" {
		go m.probeUI(p)
	}
}

// probeUI records whether the backend answers a non-error status at its root, so
// the UI knows if "Open UI" (/upstream/<model>/) would 404 (P11b).
func (m *Manager) probeUI(p *Process) {
	u := *p.Target.URL
	u.Path, u.RawQuery = "/", ""
	resp, err := m.healthCli.Get(u.String())
	if err != nil {
		p.hasUI.Store(2)
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 400 {
		p.hasUI.Store(1)
	} else {
		p.hasUI.Store(2)
	}
}

// --- VRAM slot auto-tuner ("introspect") ---
//
// tuneCmd/measure/sampleVRAMPeak/TunedSlots are the whole mechanism: size
// --parallel from a PRIOR spawn's measured footprint, then measure THIS
// spawn's footprint for the NEXT one. Every step is fail-safe by
// construction — a nil tuneCache, an unprobeable GPU, or no cached profile
// all resolve to "do nothing, use the configured cmd/maxConcurrent exactly
// as today." A bug here can only leave a model untuned, never unlaunchable.

// vramBudget returns the VRAM (MiB) available to forModel AFTER the residency
// solver evicts what it can. Using current-free VRAM under-counts, because
// evictable (sticky) residents free when forModel loads:
//
//	budget = Total − preCrowded − nonEvictable(forModel) − margin
//
// preCrowded is non-corrallm usage (total used minus corrallm's own resident
// model process groups). nonEvictable is the persistent/pinned models that stay
// put — by measured footprint (PeakMiB), falling back to config ramUsage.gpu0.
// Evictable residents are deliberately NOT subtracted. Never negative.
func (m *Manager) vramBudget(stats gpu.Stats, forModel string) int {
	m.mu.Lock()
	procs := make([]*Process, 0, len(m.procs))
	for _, p := range m.procs {
		procs = append(procs, p)
	}
	m.mu.Unlock()

	ownUsed := 0
	for _, p := range procs {
		p.mu.Lock()
		pid := 0
		if p.cmd != nil && p.cmd.Process != nil {
			pid = p.cmd.Process.Pid
		}
		p.mu.Unlock()
		if pid > 0 {
			if v, err := gpu.GroupVRAM(pid); err == nil {
				ownUsed += v
			}
		}
	}
	preCrowded := stats.UsedMiB - ownUsed
	if preCrowded < 0 {
		preCrowded = 0
	}

	nonEvictable := 0
	for name, mc := range m.cfg.Models {
		if name == forModel || !mc.Persistent {
			continue
		}
		if prof, ok := m.tuneCache.Get(stats.Name, name); ok && prof.PeakMiB > 0 {
			nonEvictable += prof.PeakMiB
		} else if b, err := config.ParseSize(mc.RAMUsage["gpu0"]); err == nil && b > 0 {
			nonEvictable += int(b / (1024 * 1024))
		}
	}

	budget := stats.TotalMiB - preCrowded - nonEvictable - m.vramMargin
	if budget < 0 {
		budget = 0
	}
	slog.Debug("vram budget (post-eviction)", "model", forModel, "budgetMiB", budget,
		"totalMiB", stats.TotalMiB, "preCrowdedMiB", preCrowded, "nonEvictableMiB", nonEvictable, "marginMiB", m.vramMargin)
	return budget
}

// tuneCmd rewrites `--parallel N` in *cmdStr to the cached tuned slot count
// for model on the current GPU, if a profile exists and the GPU is
// probeable. Fail-safe by construction: any error (no tune cache, no GPU, no
// profile, or no --parallel flag present in the configured cmd) leaves
// *cmdStr byte-for-byte unchanged and returns 0 (TunedSlots then falls back
// to the config default). Returns the tuned slot count actually applied.
//
// When PerSlotMiB isn't computable yet (KV size wasn't in llama.cpp's log,
// and fewer than two distinct-slots spawns have been measured), tuneCmd
// falls back to calibrationProbe: a provably-safe one-slot-higher spawn that
// gathers the second data point SlopeFromSamples needs, so the model
// converges to a real tuned profile within two spawns instead of staying
// stuck at whatever --parallel the config happens to say forever.
func (m *Manager) tuneCmd(model string, cmdStr *string, maxConcurrent int) int {
	if m.tuneCache == nil {
		return 0
	}
	stats, err := gpu.Probe()
	if err != nil {
		slog.Debug("gpu probe unavailable; spawning with configured cmd", "model", model, "err", err)
		return 0
	}
	if !reParallel.MatchString(*cmdStr) {
		// No --parallel flag to tune: leave the cmd completely untouched rather
		// than injecting one (spec: additive only, never alter cmd shape).
		return 0
	}
	budget := m.vramBudget(stats, model)
	n, ok := m.tuneCache.SlotsFor(stats.Name, model, budget)
	if !ok {
		n, ok = m.calibrationProbe(stats, budget, model)
		if ok {
			slog.Info("calibration probe: spawning one slot higher to derive per-slot VRAM cost",
				"model", model, "probeSlots", n)
		}
	}
	if !ok {
		return 0
	}
	// maxConcurrent is a CEILING the tuner may lower but never raise.
	//
	// Two reasons, either sufficient. First, slots beyond maxConcurrent are
	// unreachable: the scheduler admits at most maxConcurrent concurrent
	// requests to this backend, so extra llama.cpp slots can never be used.
	// Second, and worse, --parallel DIVIDES the context window — llama.cpp
	// gives each slot n_ctx/n_parallel. Sizing slots purely by free VRAM
	// therefore trades a context window the operator explicitly configured for
	// concurrency that cannot be reached.
	//
	// Observed: gemma-4-12b, configured `-c 131072` with maxConcurrent 2, was
	// spawned at --parallel 32 (the DefaultCap) because 32 slots happened to
	// fit in VRAM. Each request got n_ctx_slot=4096 — a 32x silent cut to the
	// usable context, with no error and nothing in the config to explain it.
	if maxConcurrent > 0 && n > maxConcurrent {
		slog.Debug("tuner clamped to configured maxConcurrent",
			"model", model, "wanted", n, "maxConcurrent", maxConcurrent)
		n = maxConcurrent
	}
	*cmdStr = reParallel.ReplaceAllString(*cmdStr, fmt.Sprintf("--parallel %d", n))
	return n
}

// calibrationProbe is the FALLBACK second-data-point source, used only when
// llm-bench has not published a profile for this (gpu, model).
//
// It perturbs a live server to take a measurement: it deliberately spawns one
// extra slot during real serving purely to gather the second (slots, footprint)
// point the per-slot slope needs. That was the only option while corrallm was
// the only thing that could observe a spawn. llm-bench can now measure
// deliberately, in isolation, with residency under its control, and publish via
// POST /api/v1/measurements/tune — which fills PerSlotMiB directly and makes
// this probe a no-op (it returns early once PerSlotMiB > 0).
//
// Kept because a host where no bench has ever run must still schedule sanely: a
// fresh install cannot be required to run a benchmark before it can serve.
//
// calibrationProbe looks for a profile that has exactly ONE distinct
// measured slot count (PerSlotMiB not yet derivable — no KV-log support on
// this host, and no second distinct --parallel spawn yet) and, if probing
// one more slot is PROVABLY safe, returns the higher slot count so tuneCmd
// spawns there instead of the config default — gathering the second
// (slots, footprint) point tune.SlopeFromSamples needs.
//
// Safety: for k slots at measured footprint f(k) = base + k*perSlot with
// base >= 0, footprint at k+1 is bounded by f(k)*(k+1)/k — scaling the
// WHOLE measured footprint (including base) by (k+1)/k always over-estimates
// the true f(k+1), because base doesn't grow with slots but this bound
// charges it as if it did. So probing k+1 is safe exactly when the
// post-eviction budget covers that worst case (rounded up, so integer
// truncation never makes the bound optimistic). Returns ok=false — no probe,
// caller leaves the config cmd/slots untouched — when: no profile, the
// profile already has 2+ distinct samples (nothing to calibrate), the
// recorded footprint/slots are non-positive, the probe would exceed
// tune.DefaultCap, or the safety bound doesn't clear budget.
func (m *Manager) calibrationProbe(stats gpu.Stats, budget int, model string) (int, bool) {
	p, ok := m.tuneCache.Get(stats.Name, model)
	if !ok || p.PerSlotMiB > 0 {
		return 0, false // no profile yet, or already tuned (KV-log or 2-point slope)
	}
	if len(p.Samples) != 1 {
		return 0, false // 0 samples (shouldn't happen if a profile exists) or already 2+ (not our job)
	}
	k := p.Samples[0].Slots
	footprintK := p.Samples[0].FootprintMiB
	if k <= 0 || footprintK <= 0 {
		return 0, false
	}
	probe := k + 1
	if probe > tune.DefaultCap {
		return 0, false
	}
	worst := (footprintK*probe + k - 1) / k // ceil(footprintK*(k+1)/k): round UP so the bound stays conservative
	if budget < worst {
		return 0, false
	}
	return probe, true
}

// measure records this spawn's empirical VRAM footprint into the tune cache.
// Best-effort: any gpu/tune error here just skips the measurement (logged at
// Debug/Warn) — never fatal, the backend is already StateReady regardless.
func (m *Manager) measure(model string, mdl config.Model, p *Process, pid int) {
	if m.tuneCache == nil {
		return
	}
	stats, err := gpu.Probe()
	if err != nil {
		slog.Debug("gpu probe unavailable; skipping vram measurement", "model", model, "err", err)
		return
	}
	// Attribute by process GROUP: we spawn `sh -c` (Setpgid), and nvidia-smi
	// reports the llama-server CHILD's pid, not the shell's. pid here is the
	// shell (== the group's pgid), so sum the whole group.
	footprint, err := gpu.GroupVRAM(pid)
	if err != nil {
		slog.Debug("nvidia-smi proc query unavailable; skipping vram measurement", "model", model, "err", err)
		return
	}
	if footprint <= 0 {
		slog.Debug("no vram usage reported for process group; skipping vram measurement", "model", model, "pgid", pid)
		return
	}
	nCtx, nSlots, kvMiB := 0, 0, 0
	if p.logs != nil {
		nCtx, nSlots, kvMiB = p.logs.Stats()
	}
	if nSlots <= 0 {
		nSlots = mdl.Slots() // banner not parsed yet (or --slots omitted): fall back to config
	}

	// Record this spawn's (slots, footprint) sample every time, regardless of
	// whether the KV-log fast path below is available this run — it's the
	// data the two-point slope fallback needs, and costs nothing to keep.
	existing, _ := m.tuneCache.Get(stats.Name, model)
	// Shared derivation — the SAME code llm-bench's published measurement runs
	// through. Two implementations of "what is this model's per-slot cost"
	// would drift.
	prof := tune.Derive(existing, tune.SourceServing, footprint, kvMiB, nSlots, nCtx, time.Now().Unix())
	base, perSlot, samples := prof.BaseMiB, prof.PerSlotMiB, prof.Samples
	derivedFromSlope := kvMiB <= 0 && perSlot > 0

	// Update applies precedence: this serving measurement will NOT overwrite a
	// bench-published profile's shape, only contribute its sample and peak.
	m.tuneCache.Update(stats.Name, model, prof)
	if err := m.tuneCache.Save(); err != nil {
		slog.Warn("save tune cache", "model", model, "err", err)
		return
	}
	if derivedFromSlope {
		slog.Info("vram per-slot cost derived from two-point measurement (no KV log on this host)",
			"model", model, "baseMiB", base, "perSlotMiB", perSlot, "samples", samples)
	}
	slog.Info("vram measured", "model", model, "footprintMiB", footprint,
		"baseMiB", base, "perSlotMiB", perSlot, "slots", nSlots, "kvMiB", kvMiB, "ctx", nCtx)
}

// sampleVRAMPeak periodically re-probes a resident process's VRAM footprint
// and raises the cached profile's PeakMiB if it grew — a burst well after
// boot (long-context growth, a big batch) that the one-shot measure() at
// health-check time wouldn't see. Only ever raises an EXISTING profile
// (BumpPeak is a no-op otherwise); never synthesizes one. Stops when stopped
// closes (tied to the process's cmd.Wait() returning) so it never leaks past
// the process's life or blocks shutdown.
func (m *Manager) sampleVRAMPeak(model string, pid int, stopped <-chan struct{}) {
	if m.tuneCache == nil {
		return
	}
	t := time.NewTicker(vramSampleInterval)
	defer t.Stop()
	for {
		select {
		case <-stopped:
			return
		case <-t.C:
			stats, err := gpu.Probe()
			if err != nil {
				slog.Debug("vram peak sample: gpu probe unavailable", "model", model, "err", err)
				continue
			}
			footprint, err := gpu.GroupVRAM(pid) // pid = spawned shell == process-group pgid
			if err != nil {
				slog.Debug("vram peak sample: nvidia-smi proc query unavailable", "model", model, "err", err)
				continue
			}
			if footprint <= 0 {
				continue
			}
			m.tuneCache.BumpPeak(stats.Name, model, footprint)
		}
	}
}

// TunedSlots returns the slot count the auto-tuner applied at model's last
// spawn (via --parallel rewriting), or configDefault if the model isn't
// resident, or was spawned without tuning (no cached profile, no GPU, or
// --parallel absent from its cmd). This is the fail-safe fallback surfaced
// through /v1/models: Slots always reflects the truth of what was launched.
func (m *Manager) TunedSlots(model string, configDefault int) int {
	m.mu.Lock()
	var p *Process
	for _, q := range m.procs {
		if q.ModelName == model {
			p = q
			break
		}
	}
	m.mu.Unlock()
	if p == nil {
		return configDefault
	}
	p.mu.Lock()
	tuned := p.tunedSlots
	logs := p.logs
	p.mu.Unlock()
	if tuned > 0 {
		return tuned
	}
	// Untuned but RESIDENT: report the actual n_slots the process launched with
	// (parsed from its llama.cpp banner), which is the truth even when config
	// maxConcurrent disagrees with the cmd's --parallel. Falls back to config
	// only when the banner hasn't been parsed yet (or the model isn't resident).
	if logs != nil {
		if _, nSlots, _ := logs.Stats(); nSlots > 0 {
			return nSlots
		}
	}
	return configDefault
}

// ModelVRAM returns the live VRAM footprint (MiB) of model's resident process
// group, for the residency view (P-vram). Fail-safe by construction: model
// not resident, no pid yet, or GPU introspection unavailable all resolve to
// 0, never an error.
func (m *Manager) ModelVRAM(model string) int {
	m.mu.Lock()
	p := m.procs[model]
	m.mu.Unlock()
	if p == nil {
		return 0
	}
	p.mu.Lock()
	pid := 0
	if p.cmd != nil && p.cmd.Process != nil {
		pid = p.cmd.Process.Pid
	}
	p.mu.Unlock()
	if pid <= 0 {
		return 0
	}
	v, err := gpu.GroupVRAM(pid)
	if err != nil {
		return 0
	}
	return v
}

// PublishTuneProfile stores an EXTERNALLY measured VRAM profile and persists the
// cache immediately.
//
// The measurement source is llm-bench, which can load/unload deliberately and
// measure in isolation. corrallm's own in-serving measurement remains the
// fallback for a host where no bench has run — this is additive, so a fresh
// install still schedules correctly without a benchmark pass.
func (m *Manager) PublishTuneProfile(gpuName, model string, p tune.Profile) error {
	if m.tuneCache == nil {
		return fmt.Errorf("tune cache not configured")
	}
	m.tuneCache.Update(gpuName, model, p)
	return m.tuneCache.Save()
}

// TuneProfile returns the tune cache's measured VRAM profile for (gpuName,
// model), for the residency view (P-vram). ok=false when introspection is
// disabled (nil tuneCache) or the pair has never been measured — the
// fail-safe "unmeasured" case, not an error.
func (m *Manager) TuneProfile(gpuName, model string) (tune.Profile, bool) {
	if m.tuneCache == nil {
		return tune.Profile{}, false
	}
	return m.tuneCache.Get(gpuName, model)
}

// onProcExit removes p from the ledger and frees its pools, but only if p is
// still the registered process for name (eviction may have already removed it).
func (m *Manager) onProcExit(name string, p *Process) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.procs[name] == p {
		delete(m.procs, name)
		m.freeLocked(p.server, p.usage)
	}
}

// makeRoomLocked ensures `usage` fits on server, evicting idle non-pinned
// residents constrained to the binding pool(s) if needed. All-or-nothing: it
// evicts only if the chosen victim set frees enough, else returns ErrNoCapacity
// without evicting anything (no thrash). Caller holds m.mu.
func (m *Manager) makeRoomLocked(server string, usage map[string]int64) error {
	if m.fitsLocked(server, usage, nil) {
		return nil
	}
	// Candidate victims on this server: idle (refs==0 AND not used within the
	// activeUse window — between-turn gaps of an agent session don't count as
	// idle), not pinned, ready, and touching at least one pool we need.
	now := time.Now()
	var victims []*Process
	for _, q := range m.procs {
		if q.server != server || q.persistent {
			continue
		}
		q.mu.Lock()
		idle := q.refs == 0 && q.state == StateReady && now.Sub(q.lastUsed) >= m.activeUse
		q.mu.Unlock()
		if idle && touchesAny(q.usage, usage) {
			victims = append(victims, q)
		}
	}
	sortVictims(victims)

	// Greedily select victims until the request fits.
	freed := map[string]*Process{}
	for _, v := range victims {
		freed[v.Name] = v
		if m.fitsLocked(server, usage, freed) {
			for _, e := range freed {
				m.evictLocked(e)
			}
			slog.Info("evicted for capacity", "server", server, "count", len(freed))
			return nil
		}
	}
	return m.capacityErrorLocked(server, usage, now)
}

// capacityErrorLocked classifies a failed makeRoom into permanent vs transient.
// Caller holds m.mu and has already established that the idle victim set is
// insufficient.
//
// Permanent = it wouldn't fit even with EVERY non-persistent resident on this
// server gone. That is a config/hardware fault (declared ramUsage above the
// pool budget), not contention, and no amount of waiting fixes it.
//
// Otherwise some resident that is currently protected — held by an in-flight
// request (refs>0), inside its activeUse window, or inside minResidency — would
// have made it fit. RetryAfter is the soonest any of them becomes a legal
// victim, so a client that waits exactly that long finds room.
func (m *Manager) capacityErrorLocked(server string, usage map[string]int64, now time.Time) error {
	all := map[string]*Process{}
	var blocking []string
	for _, q := range m.procs {
		if q.server != server || q.persistent {
			continue
		}
		if !touchesAny(q.usage, usage) {
			continue
		}
		all[q.Name] = q
		blocking = append(blocking, q.Name)
	}
	sort.Strings(blocking) // stable diagnostics
	if !m.fitsLocked(server, usage, all) {
		return &CapacityError{Permanent: true, Blocking: blocking}
	}

	// Transient: find the soonest protection expiry among the blockers.
	// refs>0 has no predictable expiry (the request ends when it ends), so it
	// contributes nothing — if every blocker is refs-held, RetryAfter stays 0
	// and the edge falls back to its own default.
	var soonest time.Duration
	for _, q := range all {
		q.mu.Lock()
		refs, lastUsed, readyAt := q.refs, q.lastUsed, q.readyAt
		q.mu.Unlock()
		if refs > 0 {
			continue
		}
		wait := m.activeUse - now.Sub(lastUsed)
		if r := minResidency - now.Sub(readyAt); r > wait {
			wait = r
		}
		if wait < 0 {
			wait = 0
		}
		if soonest == 0 || wait < soonest {
			soonest = wait
		}
	}
	return &CapacityError{RetryAfter: soonest, Blocking: blocking}
}

// fitsLocked reports whether usage fits on server, pretending the processes in
// `ignore` (eviction candidates) are already gone. Caller holds m.mu.
func (m *Manager) fitsLocked(server string, usage map[string]int64, ignore map[string]*Process) bool {
	for pool, want := range usage {
		used := m.used[server][pool]
		for _, e := range ignore {
			used -= e.usage[pool]
		}
		if want > m.budget[server][pool]-used {
			return false
		}
	}
	return true
}

func (m *Manager) reserveLocked(server string, usage map[string]int64) {
	for pool, b := range usage {
		m.used[server][pool] += b
	}
}

func (m *Manager) freeLocked(server string, usage map[string]int64) {
	if server == "" {
		return
	}
	for pool, b := range usage {
		m.used[server][pool] -= b
		if m.used[server][pool] < 0 {
			m.used[server][pool] = 0
		}
	}
}

// evictLocked stops a resident backend and frees its pools. Caller holds m.mu.
func (m *Manager) evictLocked(p *Process) {
	p.mu.Lock()
	p.state = StateEvicting
	cmd := p.cmd
	p.mu.Unlock()
	delete(m.procs, p.Name)
	m.freeLocked(p.server, p.usage)
	if cmd != nil && cmd.Process != nil {
		slog.Info("evicting backend", "name", p.Name, "pid", cmd.Process.Pid)
		_ = killGroup(cmd)
		// SIGTERM is a REQUEST. A llama-server in CUDA teardown (or still
		// initialising one) can ignore it for minutes, and by this point the
		// pool reservation has already been freed and the process dropped from
		// m.procs — so a survivor is untracked, unkillable by any later
		// eviction, and holding VRAM corrallm believes is available. Every
		// subsequent spawn then dies with a cudaMalloc OOM (observed live: a
		// 16 GB backend outlived its "backend exited" log by 5+ minutes and
		// crash-looped every replacement).
		//
		// So verify, and escalate. Asynchronously: the caller holds m.mu and
		// blocking eviction on a stuck process would wedge the scheduler.
		go m.reapGroup(p.Name, cmd.Process.Pid)
	}
}

// evictGrace is how long a backend gets to honor SIGTERM before it is SIGKILLed.
// Generous enough for an orderly CUDA teardown, short enough that the VRAM does
// not stay stranded through the next few load attempts.
const evictGrace = 15 * time.Second

// reapGroup waits for an evicted backend's process GROUP to actually die,
// escalating to SIGKILL if it outlives the grace period.
//
// Checking the group rather than the leader is the whole point: the leader is
// the `sh -c` wrapper, whose exit is what cmd.Wait() reports as "backend
// exited", while the llama-server grandchild is what owns the GPU memory.
func (m *Manager) reapGroup(name string, pid int) {
	deadline := time.Now().Add(evictGrace)
	for time.Now().Before(deadline) {
		if !groupAlive(pid) {
			return // clean exit
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !groupAlive(pid) {
		return
	}
	slog.Warn("backend ignored SIGTERM; sending SIGKILL",
		"name", name, "pid", pid, "grace", evictGrace)
	if err := killGroupHard(pid); err != nil {
		slog.Error("SIGKILL failed — process group may still hold VRAM",
			"name", name, "pid", pid, "err", err)
		return
	}
	// Confirm rather than assume: an unkillable process (uninterruptible driver
	// wait) leaves VRAM stranded and corrallm over-committed, and an operator
	// needs to know that from the log rather than from OOMing spawns.
	time.Sleep(2 * time.Second)
	if groupAlive(pid) {
		slog.Error("backend SURVIVED SIGKILL — VRAM is stranded and this server is now over-committed",
			"name", name, "pid", pid)
	}
}

// waitHealthy polls the target until it accepts connections, or healthTimeout.
func (m *Manager) waitHealthy(t *config.ProxyTarget) error {
	deadline := time.Now().Add(m.healthTimeout)
	addr := t.URL.Host
	if t.URL.Port() == "" {
		if t.URL.Scheme == "https" {
			addr += ":443"
		} else {
			addr += ":80"
		}
	}
	url := t.URL.String() + "/health"
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			lastErr = err
			time.Sleep(300 * time.Millisecond)
			continue
		}
		_ = conn.Close()
		// Listening is not enough: llama-server binds its port early and returns
		// 503 "Loading model" until weights + KV cache are fully loaded. Only a
		// 2xx /health means it can actually serve a request.
		resp, herr := m.healthCli.Get(url)
		if herr != nil {
			lastErr = herr
			time.Sleep(300 * time.Millisecond)
			continue
		}
		code := resp.StatusCode
		_ = resp.Body.Close()
		if code >= 200 && code < 300 {
			return nil
		}
		lastErr = fmt.Errorf("/health returned %d", code)
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("backend not healthy within %s (%s): %v", m.healthTimeout, addr, lastErr)
}

// Preload spawns models marked persistent so they are warm at boot and exempt
// from eviction. Runs in the background; failures are logged, not fatal.
func (m *Manager) Preload(ctx context.Context) {
	for name, model := range m.cfg.Models {
		if !model.Persistent {
			continue
		}
		_, done, _, err := m.EnsureReady(ctx, name, model, nil)
		if err != nil {
			slog.Warn("preload failed", "model", name, "err", err)
			continue
		}
		done() // drop the ref; persistent flag keeps it resident
		slog.Info("preloaded", "model", name)
	}
}

// Shutdown stops every spawned process.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	var pending []procRef
	for name, p := range m.procs {
		p.mu.Lock()
		cmd := p.cmd
		p.mu.Unlock()
		if cmd != nil && cmd.Process != nil {
			slog.Info("stopping backend", "name", name, "pid", cmd.Process.Pid)
			_ = killGroup(cmd)
			pending = append(pending, procRef{name: name, pid: cmd.Process.Pid})
		}
	}
	// Wait for the groups to actually die, escalating if they do not. On
	// shutdown this matters MORE than on eviction: a survivor outlives the
	// corrallm that spawned it, so nothing will ever reap it and the next
	// corrallm starts against a GPU that is mysteriously full.
	//
	// Polled, not slept: this returns the instant everything is gone, so an
	// orderly shutdown stays fast and only a genuinely stuck backend pays the
	// grace period.
	m.mu.Unlock()
	reapAll(pending)
	m.mu.Lock()
}

// procRef is a backend's identity for reaping after its Process may be gone.
type procRef struct {
	name string
	pid  int
}

// reapAll waits for every group to exit, SIGKILLing any that outlive the grace
// period. Returns as soon as all are gone.
func reapAll(refs []procRef) {
	if len(refs) == 0 {
		return
	}
	deadline := time.Now().Add(evictGrace)
	for time.Now().Before(deadline) {
		alive := refs[:0:0]
		for _, r := range refs {
			if groupAlive(r.pid) {
				alive = append(alive, r)
			}
		}
		if len(alive) == 0 {
			return
		}
		refs = alive
		time.Sleep(100 * time.Millisecond)
	}
	for _, r := range refs {
		if !groupAlive(r.pid) {
			continue
		}
		slog.Warn("backend ignored SIGTERM on shutdown; sending SIGKILL", "name", r.name, "pid", r.pid)
		if err := killGroupHard(r.pid); err != nil {
			slog.Error("SIGKILL failed — VRAM may be stranded after exit", "name", r.name, "pid", r.pid, "err", err)
		}
	}
}

// --- explicit load / unload (P8-beyond control plane) ---

// LoadModel warms a served model by spawning its process and immediately
// dropping the residency ref, leaving it resident and evictable (like Preload,
// but on demand). Pure-proxy models have nothing to load. Returns the process
// name loaded, or an error if the model isn't spawnable or the load fails
// (e.g. ErrNoCapacity).
func (m *Manager) LoadModel(ctx context.Context, served string) (string, error) {
	model, ok := m.cfg.Models[served]
	if !ok {
		return "", fmt.Errorf("unknown model %q", served)
	}
	if model.Cmd == "" {
		return "", fmt.Errorf("model %q has no cmd (pure proxy); nothing to load", served)
	}
	_, release, _, err := m.EnsureReady(ctx, served, model, nil)
	if err != nil {
		return "", err
	}
	release() // drop the ref; the model stays warm (evictable / pinned per config)
	return served, nil
}

// UnloadModel evicts every resident backend of a served model, freeing its
// pools. It refuses if a backend is persistent (pinned) or has in-flight
// requests. Returns the number evicted (0 if the model wasn't resident).
func (m *Manager) UnloadModel(served string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var targets []*Process
	for _, p := range m.procs {
		if p.ModelName != served {
			continue
		}
		p.mu.Lock()
		persistent, refs := p.persistent, p.refs
		p.mu.Unlock()
		if persistent {
			return 0, fmt.Errorf("model %q is persistent (pinned); cannot unload", served)
		}
		if refs > 0 {
			return 0, fmt.Errorf("model %q has %d in-flight request(s); cannot unload", served, refs)
		}
		targets = append(targets, p)
	}
	for _, p := range targets {
		m.evictLocked(p)
	}
	return len(targets), nil
}

// UnloadAll evicts every evictable resident, returning how many went and which
// were SKIPPED with why.
//
// This is the calibration primitive. A measurement is only trustworthy if the
// model under test has the GPU to itself: a footprint read while a second model
// is resident (or mid-load) measures the neighbour as much as the subject.
// Evicting everything up front is both simpler and more correct than trying to
// free "just enough" — under an exclusive lease nothing else should be resident
// anyway, and a partial eviction leaves exactly the contamination the lease
// exists to remove.
//
// Unlike UnloadModel this NEVER returns an error for an unevictable resident:
// a persistent model (a pinned embedder) or one with an in-flight request is
// reported as skipped and the rest still go. Failing the whole call because one
// model is pinned would make calibration impossible on any box with a preloaded
// model — which is most of them.
func (m *Manager) UnloadAll() (evicted int, skipped map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	skipped = map[string]string{}
	var targets []*Process
	for _, p := range m.procs {
		p.mu.Lock()
		persistent, refs, name := p.persistent, p.refs, p.ModelName
		p.mu.Unlock()
		switch {
		case persistent:
			skipped[name] = "persistent (pinned)"
		case refs > 0:
			skipped[name] = fmt.Sprintf("%d in-flight request(s)", refs)
		default:
			targets = append(targets, p)
		}
	}
	for _, p := range targets {
		m.evictLocked(p)
	}
	return len(targets), skipped
}

// --- residency introspection (P8) ---

// PoolResidency is one memory pool's budget and current reservation.
type PoolResidency struct {
	Pool   string
	Budget int64 // bytes available to spawned backends (total − reserve)
	Used   int64 // bytes currently reserved by resident backends
}

// ServerResidency is a server's per-pool budget/usage.
type ServerResidency struct {
	Server string
	Pools  []PoolResidency
}

// PoolUsage is a resident backend's reservation against one pool.
type PoolUsage struct {
	Pool  string
	Bytes int64
}

// ResidentModel is one loaded (or loading) backend for the UI.
type ResidentModel struct {
	Name       string // "<servedModel>#<backendIndex>"
	ModelName  string
	Server     string // "" for pure-proxy (consumes no pools)
	State      string
	Refs       int  // in-flight requests holding it
	Persistent bool // pinned: exempt from eviction
	LastUsedMS int64
	NCtx       int    // parsed context length (spawned backends; 0 if unknown)
	NSlots     int    // parsed slot count (spawned backends; 0 if unknown)
	HasUI      string // unknown | yes | no — does the backend serve a web UI at / (P11b)
	Usage      []PoolUsage
}

// Logs returns the captured stdout/stderr (oldest first) of a spawned backend,
// or nil for an unknown or pure-proxy backend.
func (m *Manager) Logs(name string) []string {
	m.mu.Lock()
	p := m.procs[name]
	m.mu.Unlock()
	if p == nil || p.logs == nil {
		return nil
	}
	return p.logs.Lines()
}

// ResidencySnapshot is a point-in-time view of the residency layer.
type ResidencySnapshot struct {
	Servers []ServerResidency
	Models  []ResidentModel
}

// Snapshot returns a stable (sorted) view of server pool budgets/usage and the
// currently resident backends — the read surface behind the P8 usage view.
func (m *Manager) Snapshot() ResidencySnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	var snap ResidencySnapshot
	for server, budget := range m.budget {
		sr := ServerResidency{Server: server}
		for pool, b := range budget {
			sr.Pools = append(sr.Pools, PoolResidency{Pool: pool, Budget: b, Used: m.used[server][pool]})
		}
		sort.Slice(sr.Pools, func(i, j int) bool { return sr.Pools[i].Pool < sr.Pools[j].Pool })
		snap.Servers = append(snap.Servers, sr)
	}
	sort.Slice(snap.Servers, func(i, j int) bool { return snap.Servers[i].Server < snap.Servers[j].Server })

	for _, p := range m.procs {
		p.mu.Lock()
		rm := ResidentModel{
			Name:       p.Name,
			ModelName:  p.ModelName,
			Server:     p.server,
			State:      string(p.state),
			Refs:       p.refs,
			Persistent: p.persistent,
		}
		if !p.lastUsed.IsZero() {
			rm.LastUsedMS = p.lastUsed.UnixMilli()
		}
		for pool, b := range p.usage {
			rm.Usage = append(rm.Usage, PoolUsage{Pool: pool, Bytes: b})
		}
		logs := p.logs
		p.mu.Unlock()
		switch p.hasUI.Load() {
		case 1:
			rm.HasUI = "yes"
		case 2:
			rm.HasUI = "no"
		default:
			rm.HasUI = "unknown"
		}
		if logs != nil {
			rm.NCtx, rm.NSlots, _ = logs.Stats()
		}
		sort.Slice(rm.Usage, func(i, j int) bool { return rm.Usage[i].Pool < rm.Usage[j].Pool })
		snap.Models = append(snap.Models, rm)
	}
	sort.Slice(snap.Models, func(i, j int) bool { return snap.Models[i].Name < snap.Models[j].Name })
	return snap
}

// --- victim ordering ---

// sortVictims orders eviction candidates best-first: ttl-expired before warm,
// unprotected before min-residency-protected, then low evictCost, then LRU.
func sortVictims(vs []*Process) {
	now := time.Now()
	sort.SliceStable(vs, func(i, j int) bool {
		a, b := vs[i], vs[j]
		if ea, eb := a.expired(now), b.expired(now); ea != eb {
			return ea // expired first
		}
		if pa, pb := a.protected(now), b.protected(now); pa != pb {
			return !pa // unprotected first
		}
		if a.evictRank != b.evictRank {
			return a.evictRank < b.evictRank // low evictCost first
		}
		return a.lastUsed.Before(b.lastUsed) // LRU
	})
}

func (p *Process) expired(now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ttl > 0 && now.Sub(p.lastUsed) > p.ttl
}

func (p *Process) protected(now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return now.Sub(p.readyAt) < minResidency
}

func touchesAny(a, b map[string]int64) bool {
	for pool := range b {
		if a[pool] > 0 {
			return true
		}
	}
	return false
}

func stickyTTL(s *config.Sticky) time.Duration {
	if s == nil || s.TTL == "" {
		return 0
	}
	d, err := time.ParseDuration(s.TTL)
	if err != nil {
		return 0
	}
	return d
}

func evictRank(s *config.Sticky) int {
	if s == nil {
		return 1 // medium default
	}
	switch s.EvictCost {
	case "low":
		return 0
	case "high":
		return 2
	default:
		return 1
	}
}
