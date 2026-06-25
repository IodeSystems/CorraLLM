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
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

// ErrNoCapacity means a backend can't be made to fit its server even after
// considering eviction — the caller should spill to the next backend.
var ErrNoCapacity = errors.New("no capacity")

// minResidency protects a just-loaded backend from eviction for a short window,
// damping load/evict thrash under bursty contention.
const minResidency = 10 * time.Second

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

	mu       sync.Mutex
	state    State
	cmd      *exec.Cmd
	ready    chan struct{} // closed when load resolves; supports coalescing
	err      error
	refs     int       // in-flight requests holding this backend
	readyAt  time.Time // when it became ready (min-residency anchor)
	lastUsed time.Time
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

// EnsureReady returns a ready Process for backend (spawning + health-checking on
// first use, coalescing concurrent loads) plus a release func that MUST be
// called when the request finishes — it drops the residency ref so the backend
// becomes evictable. A spawn that won't fit triggers eviction; if that can't
// free enough, EnsureReady returns ErrNoCapacity.
//
// loaded reports whether THIS call initiated the (cold) load rather than
// coalescing behind an in-flight or already-warm backend — the caller charges
// the load's swap cost to the request that triggered it (P6).
func (m *Manager) EnsureReady(ctx context.Context, name, modelName string, b config.Backend) (proc *Process, release func(), loaded bool, err error) {
	target, err := b.ProxyTarget()
	if err != nil {
		return nil, nil, false, err
	}

	m.mu.Lock()
	p := m.procs[name]
	triggered := p == nil
	if p == nil {
		usage, _ := config.ParseSizes(b.RAMUsage) // validated at config load
		// Residency applies to spawned backends bound to a server pool; pure
		// proxies (remote/paid) consume no local pools.
		if b.Server != "" && len(usage) > 0 {
			if err := m.makeRoomLocked(b.Server, usage); err != nil {
				m.mu.Unlock()
				return nil, nil, false, err
			}
			m.reserveLocked(b.Server, usage)
		}
		model := m.cfg.Models[modelName]
		var lb *logBuffer
		if b.Cmd != "" {
			lb = newLogBuffer(500) // capture spawned-backend output for the logs view
		}
		p = &Process{
			Name:       name,
			ModelName:  modelName,
			Target:     target,
			server:     b.Server,
			usage:      usage,
			persistent: model.Persistent,
			evictRank:  evictRank(model.Sticky),
			ttl:        stickyTTL(model.Sticky),
			logs:       lb,
			state:      StateAbsent,
			ready:      make(chan struct{}),
		}
		m.procs[name] = p
		m.mu.Unlock()
		go m.load(name, b, p)
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

// load spawns the backend (if it has a cmd) and waits for health.
func (m *Manager) load(name string, b config.Backend, p *Process) {
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

	if b.Cmd != "" {
		cmd := exec.Command("sh", "-c", b.Cmd)
		// Tee output to our stdout AND the per-backend ring buffer (for the logs
		// view + n_ctx/n_slots parsing).
		out := io.Writer(os.Stdout)
		if p.logs != nil {
			out = io.MultiWriter(os.Stdout, p.logs)
		}
		cmd.Stdout, cmd.Stderr = out, out
		cmd.SysProcAttr = sysProcAttr()
		if err := cmd.Start(); err != nil {
			finish(StateFailed, fmt.Errorf("spawn %q: %w", b.Cmd, err))
			return
		}
		p.mu.Lock()
		p.cmd = cmd
		p.mu.Unlock()
		go func() {
			err := cmd.Wait()
			slog.Info("backend exited", "name", name, "err", err)
			m.onProcExit(name, p) // free pools if it exited on its own (idempotent)
		}()
		slog.Info("backend spawned", "name", name, "pid", cmd.Process.Pid, "target", p.Target.URL.String())

		// Wait until the spawned server can actually serve. A pure-proxy backend
		// (no cmd) targets a remote we don't own — don't gate readiness on its
		// /health; proxy immediately and let per-request errors surface.
		if err := m.waitHealthy(p.Target); err != nil {
			finish(StateFailed, err)
			return
		}
	}

	slog.Info("backend ready", "name", name, "target", p.Target.URL.String())
	finish(StateReady, nil)
	// Probe whether the backend serves a web UI at its root (P11b) so the dashboard
	// can disable a dead "Open UI" button. Spawned backends only — we don't poke a
	// remote/paid endpoint's root. Async: never gates readiness.
	if b.Cmd != "" {
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
	// Candidate victims on this server: idle (refs==0), not pinned, ready, and
	// touching at least one pool we need.
	var victims []*Process
	for _, q := range m.procs {
		if q.server != server || q.persistent {
			continue
		}
		q.mu.Lock()
		idle := q.refs == 0 && q.state == StateReady
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
	return ErrNoCapacity
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
		if !model.Persistent || len(model.Backends) == 0 {
			continue
		}
		_, done, _, err := m.EnsureReady(ctx, name+"#0", name, model.Backends[0])
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
	for name, p := range m.procs {
		p.mu.Lock()
		cmd := p.cmd
		p.mu.Unlock()
		if cmd != nil && cmd.Process != nil {
			slog.Info("stopping backend", "name", name, "pid", cmd.Process.Pid)
			_ = killGroup(cmd)
		}
	}
}

// --- explicit load / unload (P8-beyond control plane) ---

// LoadModel warms a served model by spawning its first spawnable (cmd-bearing)
// backend and immediately dropping the residency ref, leaving it resident and
// evictable (like Preload, but on demand). Pure-proxy backends have nothing to
// load. Returns the backend name loaded, or an error if none is spawnable or the
// load fails (e.g. ErrNoCapacity).
func (m *Manager) LoadModel(ctx context.Context, served string) (string, error) {
	model, ok := m.cfg.Models[served]
	if !ok {
		return "", fmt.Errorf("unknown model %q", served)
	}
	var lastErr error
	for i, b := range model.Backends {
		if b.Cmd == "" {
			continue // pure-proxy: nothing to spawn
		}
		name := fmt.Sprintf("%s#%d", served, i)
		_, release, _, err := m.EnsureReady(ctx, name, served, b)
		if err != nil {
			lastErr = err
			continue // try the next spawnable backend
		}
		release() // drop the ref; the model stays warm (evictable / pinned per config)
		return name, nil
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("model %q has no spawnable backend", served)
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
			rm.NCtx, rm.NSlots = logs.Stats()
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
