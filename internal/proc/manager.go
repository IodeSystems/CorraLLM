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
	"regexp"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iodesystems/corrallm/internal/agent"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/gpu"
	"github.com/iodesystems/corrallm/internal/host"
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

// reCtx matches llama.cpp's context-size flag in either spelling so a model
// declaring contextPerRequest can have the TOTAL rewritten in place. Same
// additive contract as reParallel: a cmd with no ctx flag is left untouched.
var reCtx = regexp.MustCompile(`(-c|--ctx-size)\s+\d+`)

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
	// Reason, when set, replaces the capacity wording entirely. Used when the
	// refusal is not about capacity at all — a host that is down has plenty of
	// room, it simply cannot be reached, and reporting that as "no capacity"
	// sends the operator looking at pool sizes.
	Reason string
}

func (e *CapacityError) Error() string {
	if e.Reason != "" {
		return e.Reason
	}
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
	// StateDraining is an unload that is waiting for in-flight requests. The
	// process still serves them; it admits nothing new.
	StateDraining State = "draining"
)

// Process tracks one backend (spawned or pure-proxy).
type Process struct {
	Name      string // "<servedModel>#<backendIndex>"
	ModelName string
	Target    *config.ProxyTarget

	// key is this process's identity in Manager.procs. For an ordinary model it
	// is the served name; for every model of one extension it is the SAME
	// "extension:<name>", which is what makes them share a process and therefore
	// load and unload together. ModelName stays whichever model first triggered
	// the spawn, so config lookups and the residency UI keep working.
	key string

	// remote marks a backend on a host we do not run: no local process and a
	// non-loopback target. It has no residency to report — it is never loaded,
	// never evicted, and consumes no pool — so every read surface reports it as
	// a proxy rather than folding it into the resident set.
	remote bool

	server     string           // "" for pure-proxy (consumes no pools)
	usage      map[string]int64 // reserved bytes per pool
	persistent bool             // pinned: never evicted
	evictRank  int              // 0 low … 2 high (resistance to eviction)
	ttl        time.Duration    // idle keep-warm window

	logs *logBuffer // captured stdout/stderr (spawned backends only; nil for pure-proxy)

	hasUI atomic.Int32 // 0 unknown · 1 has a web UI · 2 none (probed once when ready, P11b)

	mu    sync.Mutex
	state State
	// handle is the running process GROUP, wherever it runs. Replaces the
	// *exec.Cmd this used to hold; see internal/host.
	handle host.Handle
	// spawnedCmd is the command string ACTUALLY run, after the slot auto-tuner
	// rewrote --parallel. It differs from the model's configured cmd whenever
	// tuning fired, and that difference was previously only visible by reading
	// the live process's argv.
	spawnedCmd string
	ready      chan struct{} // closed when load resolves; supports coalescing
	err        error
	draining   bool      // unload requested: finish in-flight work, admit nothing new
	refs       int       // in-flight requests holding this backend
	readyAt    time.Time // when it became ready (min-residency anchor)
	lastUsed   time.Time
	tunedSlots int // --parallel N actually applied by the auto-tuner; 0 = untuned (config default stands)
}

// Manager owns all processes and the per-server residency ledger.
type Manager struct {
	// cfg is swapped wholesale on reload rather than mutated. Its maps are read
	// unlocked from dozens of places, so editing them in place would be a data
	// race; replacing the pointer atomically means every reader sees one
	// self-consistent config or the other, never a half-applied one.
	//
	// Read it through m.config(), never directly — a bare field read would
	// defeat the atomic.
	cfg atomic.Pointer[config.Config]

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

	// live tracks which agent-backed servers have reported in. Nil for a
	// single-host deployment, where nothing ever heartbeats.
	live *agent.Liveness

	// paused holds the operator's out-of-service orders, keyed by served model
	// name. A paused model is never spawned — not by a request, a lane
	// fall-through, an explicit load, or boot preload. Entries with a resume
	// time expire lazily on read (and are swept, so an unrequested pinned model
	// still comes back); see pause.go. Guarded by mu.
	paused     map[string]Pause
	pauseStore PauseStore // durable pauses (nil = memory-only)

	// stopping tracks process keys whose previous process is still being torn
	// down: eviction only REQUESTS an exit (SIGTERM, then up to evictGrace
	// before SIGKILL), and it drops the entry from procs immediately — so
	// without this the manager has no memory of a process that is still alive
	// and still holding whatever it holds. A spawn into that window collides
	// with the dying process over a port or a fixed systemd unit name.
	// The channel closes once the old process group is confirmed gone.
	stopping map[string]chan struct{}

	// hosts maps a `servers:` name → where its backends actually run. Every
	// entry is a local host today; a server bound to a remote agent gets a
	// different implementation here and nothing else in this file changes.
	// Guarded by mu.
	hosts map[string]host.Host
}

// SetLiveness attaches the agent heartbeat tracker.
func (m *Manager) SetLiveness(l *agent.Liveness) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.live = l
}

// hostFor returns the host backing a server, defaulting to this machine.
//
// An unknown server (or the empty one a pure proxy carries) still yields a
// local host rather than nil: callers are on the spawn path and a nil here
// would be a panic where a spawn failure is the honest outcome.
func (m *Manager) hostFor(server string) host.Host {
	m.mu.Lock()
	defer m.mu.Unlock()
	if h, ok := m.hosts[server]; ok {
		return h
	}
	// An agent-bound server must NOT fall through to local. Spawning a model
	// declared for another machine on this one is worse than refusing: it would
	// take the GPU the model was configured to stay off, running a command
	// whose binary and weights may not even exist here.
	var h host.Host = host.NewLocal(server)
	if cfg := m.cfg.Load(); cfg != nil {
		if srv, ok := cfg.Servers[server]; ok && srv.Agent != nil {
			h = agent.NewRemoteHost(server, srv.Agent.Endpoints, srv.Agent.ExpandedToken())
		}
	}
	if m.hosts == nil {
		m.hosts = map[string]host.Host{}
	}
	m.hosts[server] = h
	return h
}

// InvalidateHost drops the cached client for a server, so the next spawn rebuilds
// it from current config.
//
// hostFor memoises a RemoteHost per server, and a RemoteHost captures its
// endpoint list at construction. Without this, an agent that moved networks
// stayed unreachable for the life of the daemon even after config was corrected
// — the cache outlived the facts it was built from, and nothing invalidated it,
// not even a config reload.
func (m *Manager) InvalidateHost(server string) {
	if server == "" {
		return
	}
	m.mu.Lock()
	delete(m.hosts, server)
	m.mu.Unlock()
}

// NewManager constructs a Manager and precomputes each server's pool budgets.
func NewManager(cfg *config.Config) *Manager {
	m := &Manager{
		procs:         map[string]*Process{},
		used:          map[string]map[string]int64{},
		budget:        map[string]map[string]int64{},
		healthCli:     &http.Client{Timeout: 2 * time.Second},
		healthTimeout: 120 * time.Second,
		activeUse:     defaultActiveUse,
		vramMargin:    defaultVRAMMargin,
	}
	m.cfg.Store(cfg)
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
	// A provided model carries no cmd/server/ramUsage — those live on its
	// extension. Overlay here, at the one door every caller comes through, so
	// nothing upstream has to know the difference.
	if mdl.Extension != "" && m.config() != nil {
		if eff, ok := m.config().Effective(name); ok {
			mdl = eff
		}
	}

	// TargetFor, not ProxyTarget: a model on an agent-bound server writes
	// `proxy: <port>` meaning "the port MY backend listens on", which resolves
	// to loopback — this machine. Routing there would forward its traffic to
	// whatever local process holds that port.
	target, err := m.config().TargetFor(name, mdl)
	if err != nil {
		return nil, nil, false, err
	}

	// Choose WHERE before anything else: the placement decides the cmd, the
	// port, the pool it draws on and the process it keys to, so every step
	// below depends on it. ForPlacement resolves it into the model once, which
	// is why nothing downstream had to learn about placements.
	pl, err := m.selectPlacement(name, mdl)
	if err != nil {
		return nil, nil, false, err
	}
	mdl = mdl.ForPlacement(pl)
	key := mdl.PlacementProcKey(name, pl)

	// A paused process is refused at the ONE door every load comes through, so
	// the request path, boot preload, explicit load and /upstream all obey the
	// pause without each needing its own check. Keyed on the PROCESS, so an
	// extension's pause covers every model it provides for free.
	if p, ok := m.pauseByKey(key); ok {
		return nil, nil, false, pauseError(name, p)
	}

	// Never spawn into a teardown: the process this would replace may still be
	// alive and still holding its port or its systemd unit name.
	if err := m.awaitStop(ctx, key); err != nil {
		return nil, nil, false, err
	}

	m.mu.Lock()
	p := m.procs[key]
	triggered := p == nil
	if p == nil {
		// A server whose agent has stopped reporting in cannot be spawned onto.
		// Refuse BEFORE reserving pools: otherwise every cold load onto a dead
		// host burns the full health timeout (600s in ml-kit's launcher) while
		// holding a reservation, and the lane's walk waits on it instead of
		// spilling to a host that can actually serve.
		//
		// Transient, not permanent: the host is expected back, and a permanent
		// error would turn a network blip into a 503 the caller cannot retry.
		if mdl.Server != "" && m.live != nil && !m.live.Reachable(mdl.Server, time.Now()) {
			m.mu.Unlock()
			return nil, nil, false, &CapacityError{
				Permanent:  false,
				RetryAfter: agent.HeartbeatInterval,
				Reason:     fmt.Sprintf("server %q is down: its agent has not reported in for over %s", mdl.Server, agent.MissWindow),
			}
		}
		usage := m.effectiveUsage(name, mdl)
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
			key:        key,
			Target:     target,
			remote:     mdl.Remote(),
			server:     mdl.Server,
			usage:      usage,
			persistent: mdl.Persistent,
			evictRank:  evictRank(st),
			ttl:        stickyTTL(st),
			logs:       lb,
			state:      StateAbsent,
			ready:      make(chan struct{}),
		}
		m.procs[key] = p
		m.mu.Unlock()
		go m.load(name, mdl, p)
	} else {
		m.mu.Unlock()
	}

	select {
	case <-p.ready:
		p.mu.Lock()
		defer p.mu.Unlock()
		// A draining backend is finishing what it has and taking nothing new.
		// Checked before the state test because a drained-and-evicted process
		// would otherwise report the less useful "not ready".
		if p.draining {
			return nil, nil, false, fmt.Errorf("backend %s is draining (unload requested)", name)
		}
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

// selectPlacement picks which way to serve this model.
//
// Preference order, and each step exists for a reason:
//
//  1. one that is ALREADY LOADED — serving from a warm process beats a cold
//     load somewhere else, and it is also what keeps repeat requests on one
//     placement instead of oscillating between boxes.
//  2. one whose server is reachable AND has room — a placement on a box that is
//     down, or full, is not a way to serve anything right now.
//  3. one whose server is merely reachable — capacity may free up by the time
//     admission runs, and refusing here would turn a queueable request into a
//     hard failure.
//  4. the first — so a single-placement model behaves exactly as before, and a
//     model whose boxes are all down still produces the specific "server is
//     down" error rather than a vague one about placement.
//
// A pure proxy has no placements and resolves to the zero value, which
// ForPlacement leaves alone.
func (m *Manager) selectPlacement(name string, mdl config.Model) (config.Placement, error) {
	ps := mdl.PlacementList()
	switch len(ps) {
	case 0:
		return config.Placement{}, nil
	case 1:
		return ps[0], nil
	}

	m.mu.Lock()
	for _, p := range ps {
		if proc := m.procs[mdl.PlacementProcKey(name, p)]; proc != nil {
			proc.mu.Lock()
			ready := proc.state == StateReady && !proc.draining
			proc.mu.Unlock()
			if ready {
				m.mu.Unlock()
				return p, nil
			}
		}
	}
	m.mu.Unlock()

	now := time.Now()
	var reachable []config.Placement
	for _, p := range ps {
		if p.Server == "" || m.live == nil || m.live.Reachable(p.Server, now) {
			reachable = append(reachable, p)
		}
	}
	if len(reachable) == 0 {
		// Every box is down. Fall through to the first so the caller gets the
		// existing, specific down-server error.
		return ps[0], nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range reachable {
		usage := m.effectiveUsage(name, mdl.ForPlacement(p))
		if len(usage) == 0 || m.fitsLocked(p.Server, usage, nil) {
			return p, nil
		}
	}
	return reachable[0], nil
}

// releaser drops one residency ref (the backend stays warm), and completes a
// pending drain when it releases the last one.
func (m *Manager) releaser(p *Process) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			p.refs--
			p.lastUsed = time.Now()
			drained := p.draining && p.refs == 0
			p.mu.Unlock()
			if !drained {
				return
			}
			// The last in-flight request just finished, so the unload that asked
			// for this drain can now complete. Done here rather than by a poller
			// so the process goes the instant it is idle.
			m.mu.Lock()
			if m.procs[p.key] == p {
				slog.Info("unload: drain complete, evicting", "key", p.key)
				m.evictLocked(p)
			}
			m.mu.Unlock()
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
		// is still the same backing string as m.config().Models[name].Cmd until copied).
		cmdStr := mdl.Cmd
		tunedSlots := m.tuneCmd(name, &cmdStr, mdl.Slots(), mdl.ContextPerRequest)
		if tunedSlots > 0 {
			p.mu.Lock()
			p.tunedSlots = tunedSlots
			p.mu.Unlock()
		}

		// Tee output to our stdout AND the per-backend ring buffer (for the logs
		// view + n_ctx/n_slots/KV-size parsing).
		out := io.Writer(os.Stdout)
		if p.logs != nil {
			out = io.MultiWriter(os.Stdout, p.logs)
		}
		h, err := m.hostFor(mdl.Server).Start(host.Spec{
			Name: name, Key: p.key, Cmd: cmdStr, Out: out})
		if err != nil {
			finish(StateFailed, err)
			return
		}
		p.mu.Lock()
		p.handle, p.spawnedCmd = h, cmdStr
		p.mu.Unlock()
		go func() {
			<-h.Done()
			slog.Info("backend exited", "name", name, "err", h.Err())
			m.onProcExit(name, p) // free pools if it exited on its own (idempotent)
		}()
		// Track this process's VRAM footprint over its lifetime so a burst well
		// after boot (long-context growth, a big batch) still feeds the NEXT
		// spawn's tuning, not just the boot-time snapshot below.
		go m.sampleVRAMPeak(name, h)
		slog.Info("backend spawned", "name", name, "id", h.ID(), "target", p.Target.URL.String())

		// Wait until the spawned server can actually serve.
		if err := m.waitHealthy(p.Target, h.Done()); err != nil {
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
		m.measure(name, mdl, p, h)
	}

	// A pure-proxy backend (no cmd) usually targets a remote we do not own, and
	// gating readiness on its /health would be wrong — proxy immediately and let
	// per-request errors surface.
	//
	// But some proxies point at a LOCAL port that ANOTHER model in this config
	// spawns: ml-kit's stt-diarize, tts and realtime-stt all proxy onto the
	// oidio process that the `stt` model owns. For those the readiness IS
	// knowable, and skipping the check declared them ready before the process
	// could answer:
	//
	//   11:12:33  backend spawned name=stt          (oidio, ~8s to load models)
	//   11:12:34  backend ready   name=stt-diarize  <- 7s early
	//   11:12:41  oidio listening on :5806
	//
	// A bench firing into that window got HTTP 502 and recorded it as a
	// capability failure. Nothing was wrong with the model.
	if mdl.Cmd == "" {
		if owner, ok := m.spawnerFor(name, mdl); ok {
			slog.Info("proxy backend waits for the model that owns its port",
				"name", name, "owner", owner, "target", p.Target.URL.String())
			if err := m.waitHealthy(p.Target, nil); err != nil {
				finish(StateFailed, fmt.Errorf("%s: port owned by %q never became ready: %w", name, owner, err))
				return
			}
		}
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
	// BaseURLString, not the bare URL: an agent-hosted backend is reached through
	// the agent's proxy prefix, and dropping it would probe the AGENT's root and
	// report the agent's 404 as the backend having no UI.
	root := p.Target.BaseURLString() + "/"
	resp, err := m.getWithTarget(root, p.Target)
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
// put — by measured footprint (PeakMiB), falling back to the config ramUsage of
// the server's device pool. Evictable residents are deliberately NOT
// subtracted. Never negative.
//
// EVERY term is scoped to forModel's own server. `stats` describes one host's
// device, so folding in another host's residents would subtract memory that was
// never on this device: with two servers loaded, each would under-count its own
// budget by the other's footprint and tune itself down to fewer slots — or, if
// the arithmetic went far enough, to none.
func (m *Manager) vramBudget(stats gpu.Stats, forModel string) int {
	// The server comes from the model rather than a parameter because every
	// caller already resolved it, and vramBudget already consults m.config().Models
	// below for the same reason.
	server := ""
	if m.config() != nil {
		server = m.config().Models[forModel].Server
	}

	m.mu.Lock()
	procs := make([]*Process, 0, len(m.procs))
	for _, p := range m.procs {
		if p.server != server {
			continue
		}
		procs = append(procs, p)
	}
	m.mu.Unlock()

	ownUsed := 0
	for _, p := range procs {
		p.mu.Lock()
		h := p.handle
		p.mu.Unlock()
		if h != nil {
			if v, err := h.MemoryMiB(); err == nil {
				ownUsed += v
			}
		}
	}
	preCrowded := stats.UsedMiB - ownUsed
	if preCrowded < 0 {
		preCrowded = 0
	}

	devicePool := m.vramPool(server)
	nonEvictable := 0
	for name, mc := range m.config().Models {
		if name == forModel || !mc.Persistent || mc.Server != server {
			continue
		}
		if prof, ok := m.tuneCache.Get(stats.Name, name); ok && prof.PeakMiB > 0 {
			nonEvictable += prof.PeakMiB
		} else if b, err := config.ParseSize(mc.RAMUsage[devicePool]); err == nil && b > 0 {
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

// getWithTarget GETs url carrying whatever headers the target requires.
//
// Every probe here used to be a bare Get, which was correct only while local
// backends needed no credential. Routing agent-hosted backends through the
// agent put an authenticated hop in front of them, and a bare Get then gets a
// 401 that reads like the BACKEND rejecting us — it took a trial transcript
// showing "model loaded / listening" three lines above "/health returned 401"
// to see that the backend was fine and the probe was the problem.
func (m *Manager) getWithTarget(url string, t *config.ProxyTarget) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if t != nil {
		for k, v := range t.Headers {
			req.Header.Set(k, v)
		}
	}
	return m.healthCli.Do(req)
}

// defaultVRAMPool is the pool treated as device memory when a server does not
// say otherwise — the value this was hardcoded to before servers could differ.
const defaultVRAMPool = "gpu0"

// vramPool is the pool on `server` that a MEASURED footprint is charged
// against. Per-server because hosts differ in shape: a discrete-GPU box has
// "gpu0", a unified-memory box has only "system", and charging the latter's
// measurement to "gpu0" would bill it against a pool with no budget.
func (m *Manager) vramPool(server string) string {
	if m.config() == nil {
		return defaultVRAMPool
	}
	return m.config().DevicePoolFor(server)
}

// unknownIfEmpty handles a model whose size is genuinely UNKNOWN: no measured
// profile and no declared ramUsage.
//
// The old behavior was the worst possible one. ParseSizes(nil) yields an empty
// map, len(usage) > 0 is false, and the spawn skipped reservation entirely — so
// corrallm admitted a model of unknown size while believing it consumed nothing,
// and kept believing that until something OOMed.
//
// "Unknown" now means "assume it needs the whole pool": every evictable resident
// is cleared, the model spawns alone, and the measurement taken on that spawn is
// what governs from then on. It costs one heavy-handed eviction, exactly once
// per (gpu, model), and it is honest — the alternative is guessing, and every
// guessed number on this box turned out wrong (the pool understated the card by
// 2 GB, bonsai's ramUsage by 7 GB, Qwen's by 1 GB).
//
// This is why ramUsage is now advisory: it is a bootstrap hint that saves one
// eviction, not a fact anything relies on.
func (m *Manager) unknownIfEmpty(name string, mdl config.Model, usage map[string]int64) map[string]int64 {
	if len(usage) > 0 || mdl.Server == "" {
		return usage
	}
	budget := m.budget[mdl.Server]
	if len(budget) == 0 {
		return usage
	}
	// On a host that cannot attribute memory per process, "reserve everything,
	// then measure" never reaches the measuring part — there is nothing to
	// measure with. The reservation would stand forever and the server would
	// serve exactly one model, silently, with no error anywhere.
	//
	// Config validation requires ramUsage on such a server, so reaching here
	// means the config changed under a running daemon. Say so loudly rather
	// than quietly becoming single-tenant.
	if cfg := m.config(); cfg != nil {
		if srv, ok := cfg.Servers[mdl.Server]; ok && srv.NoProcessMemory {
			slog.Warn("model has no ramUsage on a host that reports it cannot measure per-process "+
				"memory — it will hold the entire pool until something corrects that. If this is an "+
				"Apple-silicon host, re-enrol it: newer agents measure the resident set, and this "+
				"stops being true",
				"model", name, "server", mdl.Server)
		}
	}
	// The whole pool MINUS what other tenants hold.
	//
	// Reserving the literal pool deadlocks on a shared machine: the budget is
	// never entirely available, so the reservation never fits, so the model
	// never spawns, so it is never measured — and it stays unknown forever. The
	// intent was "evict our own residents and run alone", and running alone
	// still does not hand you memory somebody else is using.
	//
	// Evictable residents are deliberately not subtracted; makeRoomLocked
	// clears those, which is the eviction this path is willing to pay for.
	out := make(map[string]int64, len(budget))
	for pool, b := range budget {
		if avail := b - m.foreignUsedLocked(mdl.Server, pool); avail > 0 {
			out[pool] = avail
		} else {
			out[pool] = b
		}
	}
	slog.Info("model size unknown (no measured profile, no ramUsage) — reserving what the machine can offer for one spawn, then measuring",
		"model", name, "server", mdl.Server, "reserving", out)
	return out
}

// effectiveUsage returns the pool reservation for a spawn, preferring a MEASURED
// VRAM footprint over the config's hand-written ramUsage.
//
// ramUsage is what admission and eviction actually trust, and it is a number a
// human typed. It goes stale the moment anything about the model changes:
// ternary-bonsai-27b declared 16GB and really took 23098 MiB after its context
// window was restored, a 7 GB under-declaration that would have let the
// scheduler admit a neighbour which could not fit. The tune profile had the
// truth the whole time and nothing consulted it.
//
// The estimate is computed for THIS spawn's slot count rather than reused
// verbatim, since a profile measured at one slot must not be applied unchanged
// to two. PeakMiB is a floor: it is the largest footprint ever observed, so
// anything below it is known to be an under-estimate.
//
// Falls back to the config value when there is no usable profile — a fresh
// install must schedule before anything has been measured. The non-GPU pools
// (system RAM) always come from config; the profile only measures VRAM.
func (m *Manager) effectiveUsage(name string, mdl config.Model) map[string]int64 {
	usage, _ := config.ParseSizes(mdl.RAMUsage) // validated at config load
	if m.tuneCache == nil || mdl.Server == "" {
		return m.unknownIfEmpty(name, mdl, usage)
	}
	// Keyed by the device that RAN it, not the primary's own card — a model on
	// an attached machine was previously filed under the primary's GPU.
	dev := m.deviceNameFor(mdl.Server)
	if dev == "" {
		return m.unknownIfEmpty(name, mdl, usage)
	}
	prof, ok := m.tuneCache.Get(dev, name)
	if !ok || prof.PeakMiB <= 0 {
		return m.unknownIfEmpty(name, mdl, usage)
	}

	slots := mdl.Slots()
	est := prof.BaseMiB + slots*prof.PerSlotMiB
	if prof.PeakMiB > est {
		est = prof.PeakMiB
	}
	if est <= 0 {
		return usage
	}
	measured := int64(est) * 1024 * 1024

	pool := m.vramPool(mdl.Server)
	declared := usage[pool]
	if declared == measured {
		return usage
	}
	// Surface drift in BOTH directions. Under-declaring risks over-commitment
	// and an OOM; over-declaring silently wastes VRAM that could hold another
	// model. Either way the config is lying and the operator should know.
	dir := "under-declared"
	if declared > measured {
		dir = "over-declared"
	}
	slog.Info("using measured VRAM footprint instead of configured ramUsage",
		"model", name, "config", dir,
		"configuredMiB", declared/(1024*1024), "measuredMiB", est, "slots", slots)

	out := make(map[string]int64, len(usage))
	for k, v := range usage {
		out[k] = v
	}
	out[pool] = measured
	return out
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
func (m *Manager) tuneCmd(model string, cmdStr *string, maxConcurrent, perReq int) int {
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
	// CONTEXT IS THE INVARIANT. When the model declares contextPerRequest, the
	// window each request gets is a requirement, and concurrency is what bends
	// to fit it — the opposite of llama.cpp's native behavior, where --parallel
	// silently divides --ctx-size.
	if perReq > 0 {
		if fit, ok := m.tuneCache.SlotsForContext(stats.Name, model, budget, perReq); ok {
			switch {
			case fit == 0:
				// Not even one slot at the requested window. Do NOT quietly
				// serve a shorter context: that is the failure mode this whole
				// field exists to remove. Spawn at 1 slot so the operator gets
				// llama.cpp's own OOM instead of a model that looks fine and
				// silently truncates.
				slog.Error("contextPerRequest does not fit in VRAM even at one slot; spawning anyway so the failure is visible rather than silent",
					"model", model, "contextPerRequest", perReq, "budgetMiB", budget)
				n = 1
			case fit < n:
				slog.Info("reduced slots to preserve the requested context window",
					"model", model, "contextPerRequest", perReq, "slotsWanted", n, "slotsThatFit", fit)
				n = fit
			}
		}
	}
	*cmdStr = reParallel.ReplaceAllString(*cmdStr, fmt.Sprintf("--parallel %d", n))
	// Total = per-request * slots, because llama.cpp divides it back out. This
	// is what keeps the declared window intact as concurrency changes: bump
	// maxConcurrent and the total grows with it, instead of every request
	// quietly getting half as much room.
	if perReq > 0 && reCtx.MatchString(*cmdStr) {
		total := perReq * n
		*cmdStr = reCtx.ReplaceAllString(*cmdStr, fmt.Sprintf("-c %d", total))
		slog.Info("context sized from contextPerRequest",
			"model", model, "contextPerRequest", perReq, "slots", n, "totalCtx", total)
	}
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
func (m *Manager) measure(model string, mdl config.Model, p *Process, h host.Handle) {
	if m.tuneCache == nil {
		return
	}
	// The device that ran it names the profile. On an agent-backed server that
	// is the agent's hardware, reported on its heartbeat — not this machine's.
	dev := m.deviceNameFor(mdl.Server)
	if dev == "" {
		slog.Debug("no device name; skipping vram measurement", "model", model)
		return
	}
	// Attributed by process GROUP — see host.Handle.MemoryMiB: the vendor tool
	// reports the llama-server CHILD, not the `sh -c` leader we spawned.
	footprint, err := h.MemoryMiB()
	if err != nil {
		slog.Debug("per-process memory unavailable; skipping vram measurement", "model", model, "err", err)
		return
	}
	if footprint <= 0 {
		slog.Debug("no vram usage reported for process group; skipping vram measurement", "model", model, "id", h.ID())
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
	existing, _ := m.tuneCache.Get(dev, model)
	// Shared derivation — the SAME code llm-bench's published measurement runs
	// through. Two implementations of "what is this model's per-slot cost"
	// would drift.
	prof := tune.Derive(existing, tune.SourceServing, footprint, kvMiB, nSlots, nCtx, time.Now().Unix())
	base, perSlot, samples := prof.BaseMiB, prof.PerSlotMiB, prof.Samples
	derivedFromSlope := kvMiB <= 0 && perSlot > 0

	// Update applies precedence: this serving measurement will NOT overwrite a
	// bench-published profile's shape, only contribute its sample and peak.
	m.tuneCache.Update(dev, model, prof)
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
func (m *Manager) sampleVRAMPeak(model string, h host.Handle) {
	if m.tuneCache == nil {
		return
	}
	server := ""
	if cfg := m.config(); cfg != nil {
		server = cfg.Models[model].Server
	}
	t := time.NewTicker(vramSampleInterval)
	defer t.Stop()
	for {
		select {
		case <-h.Done():
			return
		case <-t.C:
			dev := m.deviceNameFor(server)
			if dev == "" {
				slog.Debug("vram peak sample: no device name", "model", model)
				continue
			}
			footprint, err := h.MemoryMiB()
			if err != nil {
				slog.Debug("vram peak sample: per-process memory unavailable", "model", model, "err", err)
				continue
			}
			if footprint <= 0 {
				continue
			}
			// Filed under the device that ran it. This is the write that keeps
			// the running high-water mark, so a wrong key here means one host's
			// peak silently overwrites another's for the same model name.
			m.tuneCache.BumpPeak(dev, model, footprint)
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

// deviceNameFor is the name a measurement should be FILED under: the device of
// the machine that actually ran the model.
//
// gpu.Probe() answers for the process calling it, which on the primary is the
// primary's own card — so every profile measured on an attached machine was
// keyed to box1's RTX 5090, including one taken on an Apple M-series Mac. Two
// hosts running the same model overwrite each other, and their footprints are
// genuinely different numbers: the same weights cost differently on different
// hardware, which is the entire reason the cache is keyed by device at all.
//
// The agent reports its device name on every heartbeat, so for an agent-backed
// server that is the authority. Falls back to the local probe for a local
// server, which is what it always was.
func (m *Manager) deviceNameFor(server string) string {
	if m.live != nil && server != "" {
		if cap, ok := m.live.Capacity(server); ok {
			if cap.GPU != nil && cap.GPU.Name != "" {
				return cap.GPU.Name
			}
			if cap.Host != nil && cap.Host.Name != "" {
				return cap.Host.Name
			}
		}
	}
	if stats, err := gpu.Probe(); err == nil {
		return stats.Name
	}
	return ""
}

// TuneProfilePeak is the largest footprint ever measured for a model on this
// host's device, or 0 if it has never been sampled.
//
// Exposed because a spot reading is not the number anyone wants: admission
// reserves the high-water mark, and a probe that reported "16 MiB" for a model
// still faulting in its weights would be worse than reporting nothing.
func (m *Manager) TuneProfilePeak(model string) int {
	if m.tuneCache == nil {
		return 0
	}
	server := ""
	if cfg := m.config(); cfg != nil {
		server = cfg.Models[model].Server
	}
	dev := m.deviceNameFor(server)
	if dev == "" {
		return 0
	}
	if prof, ok := m.tuneCache.Get(dev, model); ok {
		return prof.PeakMiB
	}
	return 0
}

// ModelVRAM returns the live VRAM footprint (MiB) of model's resident process
// group, for the residency view (P-vram). Fail-safe by construction: model
// not resident, no pid yet, or GPU introspection unavailable all resolve to
// 0, never an error.
func (m *Manager) ModelVRAM(model string) int {
	m.mu.Lock()
	p := m.procs[m.procKey(model)]
	m.mu.Unlock()
	if p == nil {
		return 0
	}
	p.mu.Lock()
	h := p.handle
	p.mu.Unlock()
	if h == nil {
		return 0
	}
	v, err := h.MemoryMiB()
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
	// Keyed by p.key, not name: an extension's process is registered under
	// "extension:<n>" while name is whichever model happened to spawn it.
	if m.procs[p.key] == p {
		delete(m.procs, p.key)
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

// foreignUsedLocked is memory on server that corrallm did not place.
//
// A declared pool minus corrallm's own reservations is NOT what is free: the
// machine has other tenants. A laptop runs a browser, an IDE and a compositor,
// and on unified memory those compete for the very same bytes a model needs to
// wire. Measured on carlsmacbookpro: 39.3 GB wired of a ~51.5 GB ceiling, of
// which 5.8 GB was nothing to do with corrallm — enough that filling the
// declared budget would have overrun the ceiling by 4.3 GB and started failing
// Metal allocations.
//
// Zero when the agent has not reported capacity (an older agent, or a local
// server), which restores the previous declaration-only arithmetic exactly.
//
// Caller holds m.mu.
func (m *Manager) foreignUsedLocked(server, pool string) int64 {
	if m.live == nil {
		return 0
	}
	cap, ok := m.live.Capacity(server)
	if !ok {
		return 0
	}
	cfg := m.config()
	if cfg == nil || cfg.DevicePoolFor(server) != pool {
		// Only the device pool has a live reading behind it. Other pools keep
		// their declared arithmetic rather than borrowing an unrelated number.
		return 0
	}
	var measuredUsed int64
	switch {
	case cap.GPU != nil && cap.GPU.UsedBytes > 0:
		measuredUsed = cap.GPU.UsedBytes
	case cap.Host != nil && cap.Host.UsedBytes > 0:
		measuredUsed = cap.Host.UsedBytes
	default:
		return 0
	}
	// Everything we believe we placed here. The remainder is somebody else's.
	var ours int64
	for _, p := range m.procs {
		if p.server == server {
			ours += p.usage[pool]
		}
	}
	if foreign := measuredUsed - ours; foreign > 0 {
		return foreign
	}
	return 0
}

// fitsLocked reports whether usage fits on server, pretending the processes in
// `ignore` (eviction candidates) are already gone. Caller holds m.mu.
func (m *Manager) fitsLocked(server string, usage map[string]int64, ignore map[string]*Process) bool {
	for pool, want := range usage {
		used := m.used[server][pool]
		for _, e := range ignore {
			used -= e.usage[pool]
		}
		// Other tenants count against the budget too. Without this the ledger
		// measures only its own footprint and calls the rest free.
		used += m.foreignUsedLocked(server, pool)
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
// awaitStop blocks until any in-flight teardown of key has finished.
//
// This is what makes "unload then load" safe rather than lucky. Eviction is
// asynchronous, so a spawn issued right after one races the process it just
// asked to exit; the loser dies on whatever the winner still holds. Waiting is
// the right answer on the automatic paths (a request, a lane fall-through, a
// resume): the wait is bounded by evictGrace and ends the moment the old group
// is confirmed gone, which is almost always immediate. The explicit control
// ops refuse instead — see loadableLocked.
//
// The channel is always closed by the reaper's defer, so this cannot hang past
// the teardown itself.
func (m *Manager) awaitStop(ctx context.Context, key string) error {
	for {
		m.mu.Lock()
		ch, stopping := m.stopping[key]
		m.mu.Unlock()
		if !stopping {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// loadableLocked reports why an EXPLICIT load of key must be refused, or nil.
//
// Coalescing onto an in-flight load is right for a request — two callers wanting
// the same model should share one spawn — but wrong for an operator action: a
// second Load while the first is still running is not a request to wait, it is a
// mistake, and reporting "loaded" for a load someone else started hides which
// action actually did the work. Caller holds m.mu.
func (m *Manager) loadableLocked(key, label string) error {
	if _, ok := m.stopping[key]; ok {
		return fmt.Errorf("%q is still stopping; wait for it to exit before loading it again", label)
	}
	p := m.procs[key]
	if p == nil {
		return nil
	}
	p.mu.Lock()
	st, draining := p.state, p.draining
	p.mu.Unlock()
	switch {
	case draining:
		return fmt.Errorf("%q is draining (an unload is finishing its in-flight requests)", label)
	case st == StateLoading:
		return fmt.Errorf("%q is already loading", label)
	case st == StateEvicting:
		return fmt.Errorf("%q is being evicted", label)
	}
	return nil
}

func (m *Manager) evictLocked(p *Process) {
	p.mu.Lock()
	p.state = StateEvicting
	h := p.handle
	p.mu.Unlock()
	delete(m.procs, p.key)
	m.freeLocked(p.server, p.usage)
	if h != nil {
		slog.Info("evicting backend", "name", p.Name, "id", h.ID())
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
		// Signal INSIDE the goroutine, not here: evictLocked runs with m.mu
		// held, and against a remote host Signal is a network call. One
		// unreachable agent would otherwise stall every request for every
		// model on every host behind a TCP timeout, because m.mu serialises
		// EnsureReady, Snapshot and onProcExit alike.
		// Remember the teardown until the group is actually gone, so nothing
		// spawns into the window where the old process still exists.
		done := make(chan struct{})
		if m.stopping == nil {
			m.stopping = map[string]chan struct{}{}
		}
		m.stopping[p.key] = done
		go func() {
			defer func() {
				m.mu.Lock()
				if m.stopping[p.key] == done {
					delete(m.stopping, p.key)
				}
				m.mu.Unlock()
				close(done)
			}()
			_ = h.Signal(host.SigTerm)
			m.reapGroup(p.Name, h)
		}()
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
func (m *Manager) reapGroup(name string, h host.Handle) {
	deadline := time.Now().Add(evictGrace)
	for time.Now().Before(deadline) {
		if !h.Alive() {
			return // clean exit
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !h.Alive() {
		return
	}
	slog.Warn("backend ignored SIGTERM; sending SIGKILL",
		"name", name, "id", h.ID(), "grace", evictGrace)
	if err := h.Signal(host.SigKill); err != nil {
		slog.Error("SIGKILL failed — process group may still hold VRAM",
			"name", name, "id", h.ID(), "err", err)
		return
	}
	// Confirm rather than assume: an unkillable process (uninterruptible driver
	// wait) leaves VRAM stranded and corrallm over-committed, and an operator
	// needs to know that from the log rather than from OOMing spawns.
	time.Sleep(2 * time.Second)
	if h.Alive() {
		slog.Error("backend SURVIVED SIGKILL — VRAM is stranded and this server is now over-committed",
			"name", name, "id", h.ID())
	}
}

// spawnerFor reports which OTHER model in the config spawns a process on this
// pure-proxy model's target port.
//
// This is the difference between "a remote we don't own" and "a local port a
// sibling is still starting". Only the second is worth waiting for: the first
// may be a paid API that is legitimately unreachable, and blocking a load on it
// would turn someone else's outage into a failed spawn here.
func (m *Manager) spawnerFor(name string, mdl config.Model) (string, bool) {
	cfg := m.config()
	if cfg == nil {
		return "", false
	}
	target, err := cfg.TargetFor(name, mdl)
	if err != nil || target == nil {
		return "", false
	}
	for other, om := range cfg.Models {
		if other == name || om.Cmd == "" {
			continue
		}
		// Matching on the RESOLVED host:port is what keeps this correct once
		// hosts stop being loopback. TargetFor rewrites an agent-bound model's
		// loopback port to its agent's host, so two models sharing a port
		// number on two different machines no longer collide, and a pure proxy
		// (which cannot declare a server — Validate forbids it) still finds the
		// sibling that owns the port on THIS box.
		//
		// This also replaces the old loopback guard: "not loopback" used to
		// mean "not a port we own" back when we owned exactly one host, and a
		// remote target simply matches no owner now.
		ot, err := cfg.TargetFor(other, om)
		if err != nil || ot == nil {
			continue
		}
		if ot.URL.Host == target.URL.Host {
			return other, true
		}
	}
	return "", false
}

// procKey resolves a served model name to the identity of the process backing
// it. Models provided by one extension all resolve to the same key.
func (m *Manager) procKey(served string) string {
	if m.config() == nil {
		return served
	}
	return m.config().Models[served].ProcKey(served)
}

// isLoopback reports whether a host refers to this machine. A hostname we
// cannot resolve is treated as remote — the conservative direction, since
// waiting on something that will never come up is worse than not waiting.
func isLoopback(host string) bool {
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// waitHealthy polls the target until it accepts connections, or healthTimeout.
//
// exited, when non-nil, is the spawned process's Done channel: a backend that
// dies during startup is a failure NOW, not in healthTimeout. Without this a
// crash-on-start burned the whole window (600s on the production box) while
// polling a port whose owner was already gone — during which the model reads
// as loading, its pools stay reserved, and anything waiting on the load waits
// with it. Observed: a spawn that lost a race for its systemd unit name exited
// 1 within a second and still held the slot for ten minutes.
//
// Pure-proxy waits pass nil: there is no process of ours to watch, and the port
// belongs to whichever model owns it.
func (m *Manager) waitHealthy(t *config.ProxyTarget, exited <-chan struct{}) error {
	return m.waitHealthyFor(t, exited, m.healthTimeout)
}

// waitHealthyFor is waitHealthy with an explicit budget, for callers whose
// notion of "too long" differs from a serving spawn's.
//
// A probe of a never-before-run model may sit through a multi-gigabyte download
// before the port is even bound; a production spawn of a cached model should
// not wait nearly that long. Same polling, different patience.
func (m *Manager) waitHealthyFor(t *config.ProxyTarget, exited <-chan struct{}, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	addr := t.URL.Host
	if t.URL.Port() == "" {
		if t.URL.Scheme == "https" {
			addr += ":443"
		} else {
			addr += ":80"
		}
	}
	url := t.BaseURLString() + "/health"
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			// A nil channel never fires, so a pure-proxy wait is unaffected.
			return fmt.Errorf("backend exited during startup (%s): %v", addr, lastErr)
		default:
		}
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
		resp, herr := m.getWithTarget(url, t)
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
	return fmt.Errorf("backend not healthy within %s (%s): %v", budget, addr, lastErr)
}

// Preload spawns models marked persistent so they are warm at boot and exempt
// from eviction. Runs in the background; failures are logged, not fatal.
func (m *Manager) Preload(ctx context.Context) {
	for name := range m.config().Models {
		// Effective, not the raw model: an extension's persistence is declared on
		// the extension, so reading it off the provided model would skip it and
		// nothing would preload.
		model, ok := m.config().Effective(name)
		if !ok || !model.Persistent {
			continue
		}
		// EnsureReady would refuse it anyway; skipping here keeps boot quiet
		// (a pause is not a "preload failed" warning) and makes the intent
		// explicit — pinning is what preloads a model, and a pause overrides it.
		if p, paused := m.PauseOf(name); paused {
			slog.Info("preload skipped: model is paused", "model", name, "until", p.ResumeAt)
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
		h := p.handle
		p.mu.Unlock()
		if h != nil {
			slog.Info("stopping backend", "name", name, "id", h.ID())
			_ = h.Signal(host.SigTerm)
			pending = append(pending, procRef{name: name, h: h})
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
	h    host.Handle
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
			if r.h.Alive() {
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
		if !r.h.Alive() {
			continue
		}
		slog.Warn("backend ignored SIGTERM on shutdown; sending SIGKILL", "name", r.name, "id", r.h.ID())
		if err := r.h.Signal(host.SigKill); err != nil {
			slog.Error("SIGKILL failed — VRAM may be stranded after exit", "name", r.name, "id", r.h.ID(), "err", err)
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
	model, ok := m.config().Effective(served)
	if !ok {
		return "", fmt.Errorf("unknown model %q", served)
	}
	if model.Cmd == "" {
		return "", fmt.Errorf("model %q has no cmd (pure proxy); nothing to load", served)
	}
	m.mu.Lock()
	err := m.loadableLocked(model.ProcKey(served), served)
	m.mu.Unlock()
	if err != nil {
		return "", err
	}
	_, release, _, err := m.EnsureReady(ctx, served, model, nil)
	if err != nil {
		return "", err
	}
	release() // drop the ref; the model stays warm (evictable / pinned per config)
	return served, nil
}

// UnloadModel evicts the resident backend of a served model, freeing its pools.
// It refuses a persistent (pinned) backend. Returns the number evicted (0 if the
// model wasn't resident, or if it went to draining instead).
//
// In-flight requests are DRAINED, not broken: the backend stops admitting new
// work and is evicted once the last one finishes. Unloading an extension takes
// every model it provides down with it — they are one process, so there is no
// coherent way to unload half of it. A 44-minute diarization can hold the drain
// open for minutes; that is the trade for not killing work in progress.
func (m *Manager) UnloadModel(served string) (int, error) {
	// Match on the PROCESS key: an extension's process is registered under
	// "extension:<n>", so matching ModelName would miss it for every provided
	// model except whichever one happened to spawn it.
	return m.unloadKey(m.procKey(served), served, false)
}

// LoadExtension warms an extension by spawning its process, addressed by the
// extension's own name rather than by one of the models it happens to provide.
func (m *Manager) LoadExtension(ctx context.Context, name string) (string, error) {
	if m.config() == nil {
		return "", fmt.Errorf("unknown extension %q", name)
	}
	if _, ok := m.config().Extensions[name]; !ok {
		return "", fmt.Errorf("unknown extension %q", name)
	}
	provided := m.config().ExtensionModels(name)
	if len(provided) == 0 {
		return "", fmt.Errorf("extension %q provides no models", name)
	}
	// Any provided model reaches the same process; they share a ProcKey. Sorted
	// by ExtensionModels, so which one is deterministic.
	served := provided[0]
	mdl, ok := m.config().Effective(served)
	if !ok {
		return "", fmt.Errorf("unknown model %q", served)
	}
	m.mu.Lock()
	err := m.loadableLocked("extension:"+name, name)
	m.mu.Unlock()
	if err != nil {
		return "", err
	}
	_, release, _, err := m.EnsureReady(ctx, served, mdl, nil)
	if err != nil {
		return "", err
	}
	release()
	return name, nil
}

// UnloadExtension stops an extension's process, taking every model it provides
// with it — they are one process, so there is no coherent way to unload half.
func (m *Manager) UnloadExtension(name string) (int, error) {
	if m.config() == nil {
		return 0, fmt.Errorf("unknown extension %q", name)
	}
	if _, ok := m.config().Extensions[name]; !ok {
		return 0, fmt.Errorf("unknown extension %q", name)
	}
	return m.unloadKey("extension:"+name, name, false)
}

// unloadKey is the shared body of model- and extension-addressed unload. label
// is what the caller asked for, used only in messages.
//
// force overrides the pinned (persistent) refusal. It exists for pause, where
// the operator has explicitly ordered the model out of service: a pin is a
// scheduling preference ("keep this warm under memory pressure"), and letting
// it veto a pause would make pausing a pinned model a no-op. Nothing else sets
// it — an ordinary unload of a pinned model is still an error, because there it
// is almost certainly a mistake.
//
// In-flight requests are DRAINED, not broken: the backend stops admitting new
// work and is evicted once the last one finishes (see releaser). A 44-minute
// diarization can hold a drain open for minutes; that is the trade for not
// killing work in progress.
func (m *Manager) unloadKey(key, label string, force bool) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	p := m.procs[key]
	if p == nil {
		return 0, nil
	}

	p.mu.Lock()
	if p.persistent && !force {
		p.mu.Unlock()
		return 0, fmt.Errorf("%q is persistent (pinned); cannot unload", label)
	}
	if p.draining {
		refs := p.refs
		p.mu.Unlock()
		return 0, fmt.Errorf("%q is already draining (%d in flight)", label, refs)
	}
	if p.refs > 0 {
		p.draining = true
		p.state = StateDraining
		refs := p.refs
		p.mu.Unlock()
		slog.Info("unload: draining backend", "target", label, "key", key, "inflight", refs)
		return 0, nil
	}
	p.mu.Unlock()

	m.evictLocked(p)
	return 1, nil
}

// ExtensionState is one extension's process, for the control plane.
type ExtensionState struct {
	Name     string   `json:"name"`
	Provides []string `json:"provides"`
	State    State    `json:"state"`
	Draining bool     `json:"draining"`
	InFlight int      `json:"in_flight"`
	Pinned   bool     `json:"pinned"`
	// Paused: out of service by operator order. An extension is the unit a
	// pause acts on whenever its models are involved, so this is where the
	// dashboard reads it from.
	Paused        bool   `json:"paused"`
	PauseReason   string `json:"pause_reason"`
	PausedAtMS    int64  `json:"paused_at_ms"`
	PauseResumeMS int64  `json:"pause_resume_ms"`
}

// ExtensionStates reports every declared extension and whether its process is up.
func (m *Manager) ExtensionStates() []ExtensionState {
	if m.config() == nil {
		return nil
	}
	names := make([]string, 0, len(m.config().Extensions))
	for n := range m.config().Extensions {
		names = append(names, n)
	}
	sort.Strings(names)

	out := make([]ExtensionState, 0, len(names))
	for _, n := range names {
		st := ExtensionState{
			Name:     n,
			Provides: m.config().ExtensionModels(n),
			State:    StateAbsent,
			Pinned:   m.config().Extensions[n].Persistent,
		}
		m.mu.Lock()
		p := m.procs["extension:"+n]
		m.mu.Unlock()
		if p != nil {
			p.mu.Lock()
			st.State, st.Draining, st.InFlight = p.state, p.draining, p.refs
			p.mu.Unlock()
		}
		if pause, ok := m.PauseOfExtension(n); ok {
			st.Paused, st.PauseReason = true, pause.Reason
			st.PausedAtMS = pause.At.UnixMilli()
			if !pause.ResumeAt.IsZero() {
				st.PauseResumeMS = pause.ResumeAt.UnixMilli()
			}
		}
		out = append(out, st)
	}
	return out
}

// Draining reports whether an unload is waiting on this process's in-flight
// requests, so the request edge can refuse new work with a 503.
func (p *Process) Draining() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.draining
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
	Name      string // "<servedModel>#<backendIndex>"
	ModelName string
	// ProcKey is the identity of the backing process ("extension:<name>" when an
	// extension hosts it, else the served name). Consumers resolve a model's
	// state through this, not through ModelName: an extension's sibling models
	// share one process, and keying by ModelName reported the sibling that
	// happened to trigger the spawn as ready while the rest read absent.
	ProcKey string
	// Remote: no local process, non-loopback target. Holds no residency — State
	// is not a residency fact for these and must not be counted as loaded.
	Remote     bool
	Server     string // "" for pure-proxy (consumes no pools)
	State      string
	Refs       int  // in-flight requests holding it
	Persistent bool // pinned: exempt from eviction
	LastUsedMS int64
	ReadyAtMS  int64  // unix ms the backend became ready (uptime anchor; 0 if unknown)
	NCtx       int    // parsed context length (spawned backends; 0 if unknown)
	NSlots     int    // parsed slot count (spawned backends; 0 if unknown)
	HasUI      string // unknown | yes | no — does the backend serve a web UI at / (P11b)
	Usage      []PoolUsage
}

// Logs returns the captured stdout/stderr (oldest first) of a spawned backend,
// or nil for an unknown or pure-proxy backend.
func (m *Manager) Logs(name string) []string {
	m.mu.Lock()
	p := m.procs[m.procKey(name)]
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
	// Stopping lists PROCESS KEYS whose process is still being torn down.
	//
	// These deliberately are not Models entries: eviction frees the pool
	// reservation before the process actually exits, so a stopping process
	// holds no residency and listing it as resident would double-count
	// capacity that is already available. But it is not absent either — a load
	// aimed at it is refused until it goes — and reporting it as absent is what
	// let the dashboard offer a Load button whose only outcome was an error.
	Stopping []string
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
			ProcKey:    p.key,
			Remote:     p.remote,
			Server:     p.server,
			State:      string(p.state),
			Refs:       p.refs,
			Persistent: p.persistent,
		}
		if !p.lastUsed.IsZero() {
			rm.LastUsedMS = p.lastUsed.UnixMilli()
		}
		if !p.readyAt.IsZero() {
			rm.ReadyAtMS = p.readyAt.UnixMilli()
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

	for key := range m.stopping {
		snap.Stopping = append(snap.Stopping, key)
	}
	sort.Strings(snap.Stopping)
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

// config returns the manager's current config. Always use this rather than
// reading the field: the pointer is swapped on reload, and a direct read would
// bypass the atomic.
func (m *Manager) config() *config.Config {
	return m.cfg.Load()
}

// SetConfig swaps in a reloaded config.
//
// Pool budgets are RECOMPUTED, but reservations already held by resident
// backends are left exactly as they are. A backend that is resident holds real
// memory on a real device; forgetting its reservation because the operator
// edited a file would let the next spawn over-commit the box. Shrinking a pool
// below what is already reserved is therefore allowed to leave it temporarily
// over-subscribed — admission simply refuses new work there until something
// evicts, which is the honest outcome and self-heals.
//
// A pool that disappeared from the config keeps its ledger entry for the same
// reason: something is still holding it.
func (m *Manager) SetConfig(cfg *config.Config) {
	if cfg == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg.Store(cfg)
	for name, srv := range cfg.Servers {
		totals, _ := config.ParseSizes(srv.Pools) // validated at load
		reserve, _ := config.ParseSizes(srv.Reserve)
		b := m.budget[name]
		if b == nil {
			b = map[string]int64{}
			m.budget[name] = b
		}
		for pool, total := range totals {
			v := total - reserve[pool]
			if v < 0 {
				v = 0
			}
			b[pool] = v
		}
	}
}
