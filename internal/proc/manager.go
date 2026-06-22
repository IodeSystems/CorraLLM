// Package proc manages spawned backend processes: it starts a backend's `cmd`,
// waits for its proxy target to become healthy, and tears it down on shutdown.
//
// P1 is deliberately simple — one process per backend, started lazily on first
// use, no eviction or residency accounting (that is P4). The Manager is the seam
// the residency layer will later grow into.
package proc

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

// State is a backend process's lifecycle state.
type State string

const (
	StateAbsent  State = "absent"
	StateLoading State = "loading"
	StateReady   State = "ready"
	StateFailed  State = "failed"
)

// Process tracks one spawned backend.
type Process struct {
	Name   string // "<servedModel>#<backendIndex>"
	Target *config.ProxyTarget

	mu    sync.Mutex
	state State
	cmd   *exec.Cmd
	ready chan struct{} // closed when ready; supports load coalescing
	err   error
}

// Manager owns all spawned processes, keyed by backend name.
type Manager struct {
	mu        sync.Mutex
	procs     map[string]*Process
	healthCli *http.Client
	// healthTimeout bounds how long EnsureReady waits for a backend to come up.
	healthTimeout time.Duration
}

// NewManager constructs a Manager.
func NewManager() *Manager {
	return &Manager{
		procs:         map[string]*Process{},
		healthCli:     &http.Client{Timeout: 2 * time.Second},
		healthTimeout: 120 * time.Second,
	}
}

// EnsureReady returns a ready Process for backend, spawning + health-checking it
// on first use. Concurrent callers for the same backend coalesce behind a single
// in-flight load (the load-coalescing lesson) rather than spawning duplicates.
func (m *Manager) EnsureReady(ctx context.Context, name string, b config.Backend) (*Process, error) {
	target, err := b.ProxyTarget()
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	p, ok := m.procs[name]
	if !ok {
		p = &Process{Name: name, Target: target, state: StateAbsent, ready: make(chan struct{})}
		m.procs[name] = p
		m.mu.Unlock()
		// We created it → we own the (possibly spawned) load.
		go m.load(name, b, p)
	} else {
		m.mu.Unlock()
	}

	select {
	case <-p.ready:
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.state != StateReady {
			return nil, fmt.Errorf("backend %s not ready: %w", name, p.err)
		}
		return p, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// load spawns the backend (if it has a cmd) and waits for health, then closes
// p.ready exactly once.
func (m *Manager) load(name string, b config.Backend, p *Process) {
	p.mu.Lock()
	p.state = StateLoading
	p.mu.Unlock()

	finish := func(st State, err error) {
		p.mu.Lock()
		p.state, p.err = st, err
		p.mu.Unlock()
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
		// Reap on exit so a killed (or self-exiting) backend doesn't linger as a
		// zombie. The result is logged; lifecycle state is owned by load/Shutdown.
		go func() {
			err := cmd.Wait()
			slog.Info("backend exited", "name", name, "pid", cmd.Process.Pid, "err", err)
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

// waitHealthy polls the target until it accepts connections (and, if it serves
// one, a 2xx/4xx on /health), or healthTimeout elapses. A TCP dial succeeding is
// the floor; an HTTP response is preferred but not required (some upstreams 404
// /health yet serve inference).
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
			// Best-effort HTTP probe; any response means the server is up.
			resp, herr := m.healthCli.Get(t.URL.String() + "/health")
			if herr == nil {
				_ = resp.Body.Close()
			}
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("backend not healthy within %s (%s)", m.healthTimeout, addr)
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
