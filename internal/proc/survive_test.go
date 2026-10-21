package proc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/agent"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/host"
)

// fakeAgent answers the agent control plane well enough for a RemoteHost:
// every backend it is asked about is alive, and it records spawns and signals.
type fakeAgent struct {
	srv *httptest.Server

	mu       sync.Mutex
	spawns   int
	signaled []string
}

func newFakeAgent(t *testing.T) *fakeAgent {
	t.Helper()
	f := &fakeAgent{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agent/v1/hello", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(agent.Hello{Protocol: agent.Protocol})
	})
	mux.HandleFunc("POST /agent/v1/backends", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.spawns++
		n := f.spawns
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(agent.Backend{ID: fmt.Sprintf("spawned%d", n), Alive: true})
	})
	mux.HandleFunc("GET /agent/v1/backends/{id}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(agent.Backend{ID: r.PathValue("id"), Alive: true, MemoryMiB: -1})
	})
	mux.HandleFunc("GET /agent/v1/backends/{id}/logs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"lines":[],"next":1}`))
	})
	mux.HandleFunc("POST /agent/v1/backends/{id}/signal", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.signaled = append(f.signaled, r.PathValue("id"))
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAgent) counts() (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spawns, append([]string(nil), f.signaled...)
}

// surviveMgr is a primary with one agent-backed model whose backend is a real
// listener answering /health. The agent endpoint is loopback, so the data
// plane dials the backend directly (same machine) and health checks reach it.
func surviveMgr(t *testing.T) (*Manager, *fakeAgent, int, string) {
	t.Helper()
	fa := newFakeAgent(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(backend.Close)
	u, _ := url.Parse(backend.URL)
	var port int
	_, _ = fmt.Sscanf(u.Port(), "%d", &port)
	cmd := fmt.Sprintf("exec llama-server --port %d", port)
	cfg, err := config.LoadBytesForTest([]byte(fmt.Sprintf(`
servers:
  mac1:
    pools: { system: 8GB }
    devicePool: system
    agent: { endpoints: [%q], token: t }
models:
  m:
    cmd: %q
    server: mac1
    ramUsage: { system: 1GB }
    proxy: %d
`, fa.srv.URL, cmd, port)))
	if err != nil {
		t.Fatal(err)
	}
	return NewManager(cfg), fa, port, cmd
}

func old() int64 { return time.Now().Add(-10 * time.Minute).UnixMilli() }

// A primary restart must cost an agent-hosted model nothing: the agent still
// runs it, and the first heartbeat ADOPTS it as ready — pools charged, no
// second spawn, and an explicit unload still stops it.
func TestReconcile_AdoptsABackendTheConfigWouldSpawn(t *testing.T) {
	m, fa, port, cmd := surviveMgr(t)
	a, r, v := m.ReconcileAgent(context.Background(), "mac1", []agent.Backend{{
		ID: "b7-x", Key: "m", Model: "m", Alive: true, Started: old(),
		Cmd: cmd + " --parallel 3", ConfigCmd: cmd, Port: port,
	}})
	if a != 1 || r != 0 || v != 0 {
		t.Fatalf("adopted=%d reaped=%d vanished=%d; want 1/0/0", a, r, v)
	}
	m.mu.Lock()
	p := m.procs["m"]
	used := m.used["mac1"]["system"]
	m.mu.Unlock()
	if p == nil || p.state != StateReady {
		t.Fatalf("no ready process after adoption: %+v", p)
	}
	if used != int64(1e9) {
		t.Errorf("pool charge = %d, want 1GB: an adopted model must count against the budget", used)
	}
	if p.tunedSlots != 3 {
		t.Errorf("tunedSlots = %d, want 3 read from the running cmd", p.tunedSlots)
	}

	// Requests are served by the adopted process — no spawn.
	got, release, loaded, err := m.EnsureReady(context.Background(), "m", m.config().Models["m"], nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if got != p || loaded {
		t.Errorf("EnsureReady returned %p loaded=%v; want the adopted process, not a load", got, loaded)
	}
	if n, _ := fa.counts(); n != 0 {
		t.Errorf("%d spawns on the agent; the adopted backend should have been used", n)
	}

	// Steady state: the next beat changes nothing.
	if a, r, _ := m.ReconcileAgent(context.Background(), "mac1", []agent.Backend{{
		ID: "b7-x", Key: "m", Alive: true, Started: old(), ConfigCmd: cmd, Port: port,
	}}); a != 1 || r != 0 {
		t.Errorf("second beat adopted=%d reaped=%d; want 1/0", a, r)
	}

	// Explicit unload still stops it.
	if _, err := m.UnloadModel("m"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, sig := fa.counts(); len(sig) > 0 && sig[0] == "b7-x" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("unload did not signal the adopted backend on its agent")
}

// Everything that is not exactly what config would spawn is reaped, as an
// orphan always was: a removed model, a changed cmd, a backend too old to say
// what it was configured as, and a second copy under a key we already hold.
func TestReconcile_ReapsWhatConfigWouldNotSpawn(t *testing.T) {
	cases := []struct {
		name string
		mut  func(b *agent.Backend, cmd string, port int)
	}{
		{"model no longer configured", func(b *agent.Backend, _ string, _ int) { b.Key = "gone" }},
		{"cmd changed in config", func(b *agent.Backend, _ string, _ int) { b.ConfigCmd = "exec llama-server --old" }},
		{"no configured cmd reported", func(b *agent.Backend, _ string, _ int) { b.ConfigCmd = "" }},
		{"port changed", func(b *agent.Backend, _ string, port int) { b.Port = port + 1 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, fa, port, cmd := surviveMgr(t)
			b := agent.Backend{ID: "b1-x", Key: "m", Alive: true, Started: old(), ConfigCmd: cmd, Port: port}
			c.mut(&b, cmd, port)
			a, r, _ := m.ReconcileAgent(context.Background(), "mac1", []agent.Backend{b})
			if a != 0 || r != 1 {
				t.Fatalf("adopted=%d reaped=%d; want 0/1", a, r)
			}
			if _, sig := fa.counts(); len(sig) != 1 || sig[0] != "b1-x" {
				t.Errorf("signals = %v; want b1-x reaped", sig)
			}
			m.mu.Lock()
			used := m.used["mac1"]["system"]
			m.mu.Unlock()
			if used != 0 {
				t.Errorf("a reaped backend left %d bytes charged", used)
			}
		})
	}
}

// Two backends under one key is a duplicate spawn, the exact failure the
// startup hold prevents; if it happened anyway, keep one and reap the other.
func TestReconcile_ReapsADuplicateOfAnAdoptedKey(t *testing.T) {
	m, fa, port, cmd := surviveMgr(t)
	a, r, _ := m.ReconcileAgent(context.Background(), "mac1", []agent.Backend{
		{ID: "b1-x", Key: "m", Alive: true, Started: old(), ConfigCmd: cmd, Port: port},
		{ID: "b2-x", Key: "m", Alive: true, Started: old(), ConfigCmd: cmd, Port: port},
	})
	if a != 1 || r != 1 {
		t.Fatalf("adopted=%d reaped=%d; want 1/1", a, r)
	}
	if _, sig := fa.counts(); len(sig) != 1 || sig[0] != "b2-x" {
		t.Errorf("signals = %v; want only the duplicate b2-x reaped", sig)
	}
}

// After a restart, a request for an agent-hosted model WAITS for the agent's
// first report instead of spawning a second copy of something that is very
// likely already loaded there.
func TestEnsureReady_HoldsSpawnUntilTheAgentReports(t *testing.T) {
	m, fa, port, cmd := surviveMgr(t)
	m.SetLiveness(agent.NewLiveness())

	type res struct {
		p   *Process
		err error
	}
	out := make(chan res, 1)
	go func() {
		p, release, _, err := m.EnsureReady(context.Background(), "m", m.config().Models["m"], nil, nil)
		if release != nil {
			release()
		}
		out <- res{p, err}
	}()
	time.Sleep(300 * time.Millisecond)
	select {
	case r := <-out:
		t.Fatalf("EnsureReady returned before the agent reported: %+v", r)
	default:
	}
	if n, _ := fa.counts(); n != 0 {
		t.Fatalf("spawned %d before the agent reported", n)
	}

	m.ReconcileAgent(context.Background(), "mac1", []agent.Backend{{
		ID: "b1-x", Key: "m", Alive: true, Started: old(), ConfigCmd: cmd, Port: port,
	}})
	select {
	case r := <-out:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if id := r.p.handle.(interface{ AgentID() string }).AgentID(); id != "b1-x" {
			t.Errorf("served by %s, want the adopted b1-x", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("EnsureReady still held after the agent reported")
	}
	if n, _ := fa.counts(); n != 0 {
		t.Errorf("spawned %d despite adopting the running backend", n)
	}
}

// The hold is bounded: an agent that never reports must not stall its models
// forever. Past the window, spawning proceeds as it always did.
func TestEnsureReady_HoldEndsAfterTheTimeout(t *testing.T) {
	m, fa, _, _ := surviveMgr(t)
	m.SetLiveness(agent.NewLiveness())
	m.born = time.Now().Add(-2 * agentHoldTimeout)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, release, _, err := m.EnsureReady(ctx, "m", m.config().Models["m"], nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if n, _ := fa.counts(); n != 1 {
		t.Errorf("spawns = %d, want 1 once the hold window has passed", n)
	}
}

// recHandle records signals and never exits on its own.
type recHandle struct {
	mu  sync.Mutex
	sig []host.Sig
}

func (h *recHandle) ID() string            { return "rec" }
func (h *recHandle) Done() <-chan struct{} { return nil }
func (h *recHandle) Err() error            { return nil }
func (h *recHandle) Signal(s host.Sig) error {
	h.mu.Lock()
	h.sig = append(h.sig, s)
	h.mu.Unlock()
	return nil
}
func (h *recHandle) Alive() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sig) == 0
}
func (h *recHandle) MemoryMiB() (int, error)         { return 0, nil }
func (h *recHandle) MemoryOnMiB(string) (int, error) { return 0, nil }
func (h *recHandle) signals() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sig)
}

// A primary shutdown leaves agent-hosted backends running for the next
// primary to adopt, and still stops its own local children.
func TestShutdown_LeavesAgentBackendsStopsLocalOnes(t *testing.T) {
	cfg, err := config.LoadBytesForTest([]byte(`
servers:
  box1:
    pools: { gpu0: 30GB }
  mac1:
    pools: { system: 8GB }
    devicePool: system
    agent: { endpoints: ["http://192.168.1.42:6503"] }
models:
  local: { cmd: "exec sleep 300", server: box1, proxy: 5800 }
  remote: { cmd: "exec sleep 300", server: mac1, proxy: 5810, ramUsage: { system: 1GB } }
`))
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(cfg)
	lh, rh := &recHandle{}, &recHandle{}
	m.mu.Lock()
	m.procs["local"] = &Process{Name: "local", key: "local", server: "box1", handle: lh, state: StateReady}
	m.procs["remote"] = &Process{Name: "remote", key: "remote", server: "mac1", handle: rh, state: StateReady}
	m.mu.Unlock()

	m.Shutdown()
	if rh.signals() != 0 {
		t.Error("shutdown signalled an agent-hosted backend; it should survive for the next primary")
	}
	if lh.signals() == 0 {
		t.Error("shutdown did not stop a local backend")
	}
}

// Sanity on the key lookup adoption relies on: extensions resolve to their
// shared key, a plain model to its served name.
func TestConfiguredFor_FindsTheProcessKey(t *testing.T) {
	m, _, _, cmd := surviveMgr(t)
	served, mdl, _, ok := configuredFor(m.config(), "mac1", "m")
	if !ok || served != "m" || mdl.Cmd != cmd {
		t.Errorf("configuredFor = %q %q %v", served, mdl.Cmd, ok)
	}
	if _, _, _, ok := configuredFor(m.config(), "other", "m"); ok {
		t.Error("a key on another server matched")
	}
	if !strings.Contains(cmd, "--port") {
		t.Fatal("precondition")
	}
}
