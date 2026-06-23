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
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"sync"
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
		p = &Process{
			Name:       name,
			ModelName:  modelName,
			Target:     target,
			server:     b.Server,
			usage:      usage,
			persistent: model.Persistent,
			evictRank:  evictRank(model.Sticky),
			ttl:        stickyTTL(model.Sticky),
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
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
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
	}

	if err := m.waitHealthy(p.Target); err != nil {
		finish(StateFailed, err)
		return
	}
	slog.Info("backend ready", "name", name, "target", p.Target.URL.String())
	finish(StateReady, nil)
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
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
			if resp, herr := m.healthCli.Get(t.URL.String() + "/health"); herr == nil {
				_ = resp.Body.Close()
			}
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("backend not healthy within %s (%s)", m.healthTimeout, addr)
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
