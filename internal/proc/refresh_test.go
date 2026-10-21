package proc

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/agent"
	"github.com/iodesystems/corrallm/internal/host"
)

// exitHandle is a backend that exits when signalled. No process behind it,
// so these tests run on every platform.
type exitHandle struct {
	once sync.Once
	done chan struct{}
}

func newExitHandle() *exitHandle { return &exitHandle{done: make(chan struct{})} }

func (h *exitHandle) ID() string                      { return "fake" }
func (h *exitHandle) Done() <-chan struct{}           { return h.done }
func (h *exitHandle) Err() error                      { return nil }
func (h *exitHandle) Signal(host.Sig) error           { h.once.Do(func() { close(h.done) }); return nil }
func (h *exitHandle) MemoryMiB() (int, error)         { return 0, fmt.Errorf("none") }
func (h *exitHandle) MemoryOnMiB(string) (int, error) { return 0, fmt.Errorf("none") }
func (h *exitHandle) Alive() bool {
	select {
	case <-h.done:
		return false
	default:
		return true
	}
}

// refreshMgr is box1 with one device pool and system RAM, neither ECC by
// detection; "eccbox" declares its gpu0 ECC by override.
func refreshMgr(t *testing.T, maxAge string) *Manager {
	t.Helper()
	fakeLocalECC(t, map[string]bool{eccUUIDA: false}, nil, false, false)
	res := ""
	if maxAge != "" {
		res = "residency: { nonEccMaxAge: \"" + maxAge + "\" }\n"
	}
	return NewManager(eccCfg(t, res+`
servers:
  box1:
    pools: { gpu0: 24GB, system: 64GB }
  eccbox:
    pools: { gpu0: 24GB, system: 64GB }
    ecc: { gpu0: true, system: true }
`))
}

// loaded registers a ready spawned backend whose weights loaded `ago`.
func loaded(m *Manager, key, server string, ago time.Duration, opts ...func(*Process)) *Process {
	now := time.Now()
	p := &Process{
		key: key, Name: key, ModelName: key, server: server, state: StateReady,
		handle: newExitHandle(), usage: map[string]int64{"gpu0": 1 << 30},
		ready: make(chan struct{}), readyAt: now.Add(-ago), lastUsed: now, loadedAt: now.Add(-ago),
	}
	close(p.ready)
	for _, o := range opts {
		o(p)
	}
	m.mu.Lock()
	m.procs[p.key] = p
	m.mu.Unlock()
	return p
}

func isCurrent(m *Manager, key string, p *Process) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.procs[key] == p
}

func TestRefreshDecision(t *testing.T) {
	cases := []struct {
		name, maxAge, server string
		ago                  time.Duration
		opts                 []func(*Process)
		want                 bool
	}{
		{name: "non-ECC and older than the max", server: "box1", ago: 49 * time.Hour, want: true},
		{name: "non-ECC, younger than the max", server: "box1", ago: 47 * time.Hour},
		{name: "ECC pool", server: "eccbox", ago: 100 * time.Hour},
		{name: "refresh off", maxAge: "0", server: "box1", ago: 100 * time.Hour},
		{name: "custom max", maxAge: "1h", server: "box1", ago: 2 * time.Hour, want: true},
		{name: "a trial is not refreshed", server: "box1", ago: 100 * time.Hour,
			opts: []func(*Process){func(p *Process) { p.key = TrialKeyPrefix + "x" }}},
		{name: "a pure proxy is not refreshed", server: "", ago: 100 * time.Hour},
		{name: "no load time known", server: "box1", ago: 100 * time.Hour,
			opts: []func(*Process){func(p *Process) { p.loadedAt = time.Time{} }}},
		{name: "no reservation falls back to the device pool", server: "box1", ago: 100 * time.Hour,
			opts: []func(*Process){func(p *Process) { p.usage = nil }}, want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := refreshMgr(t, c.maxAge)
			p := loaded(m, "m1", c.server, c.ago, c.opts...)
			refreshed, deferred := m.refreshNonECC(time.Now())
			if got := len(refreshed) == 1; got != c.want {
				t.Fatalf("refreshed=%v deferred=%v; want refreshed %v", refreshed, deferred, c.want)
			}
			if c.want == isCurrent(m, p.key, p) {
				t.Errorf("resident after the tick = %v, want %v", !c.want, !c.want)
			}
		})
	}
}

// The reason names the age and the pool.
func TestRefreshReason(t *testing.T) {
	m := refreshMgr(t, "")
	p := loaded(m, "m1", "box1", 49*time.Hour)
	why := m.refreshDue(p, time.Now(), 48*time.Hour)
	if !strings.Contains(why, "non-ECC refresh: loaded 49h0m0s ago") || !strings.Contains(why, "pool gpu0 on box1") {
		t.Errorf("reason = %q", why)
	}
}

// Deferred, not skipped: a bench against the model, a handoff, a backend
// mid-load or already draining all wait for the next tick, and the refresh
// happens once nothing stands in the way.
func TestRefreshDefers(t *testing.T) {
	t.Run("bench running", func(t *testing.T) {
		m := refreshMgr(t, "")
		p := loaded(m, "m1", "box1", 49*time.Hour)
		benching := true
		m.SetRefreshHold(func(model string) string {
			if benching && model == "m1" {
				return "a bench run is measuring it"
			}
			return ""
		})
		refreshed, deferred := m.refreshNonECC(time.Now())
		if len(refreshed) != 0 || !slices.Equal(deferred, []string{"m1"}) || !isCurrent(m, "m1", p) {
			t.Fatalf("during the bench: refreshed=%v deferred=%v", refreshed, deferred)
		}
		benching = false
		if refreshed, _ := m.refreshNonECC(time.Now()); len(refreshed) != 1 {
			t.Errorf("after the bench: refreshed=%v, want m1", refreshed)
		}
	})
	t.Run("handoff", func(t *testing.T) {
		m := refreshMgr(t, "")
		loaded(m, "m1", "box1", 49*time.Hour)
		m.HoldLocalSpawns()
		if refreshed, deferred := m.refreshNonECC(time.Now()); len(refreshed) != 0 || len(deferred) != 1 {
			t.Fatalf("held: refreshed=%v deferred=%v", refreshed, deferred)
		}
		m.ReleaseLocalSpawns()
		if refreshed, _ := m.refreshNonECC(time.Now()); len(refreshed) != 1 {
			t.Errorf("released: refreshed=%v", refreshed)
		}
	})
	t.Run("handed over", func(t *testing.T) {
		m := refreshMgr(t, "")
		loaded(m, "m1", "box1", 49*time.Hour)
		m.draining.Store(true)
		if refreshed, deferred := m.refreshNonECC(time.Now()); len(refreshed)+len(deferred) != 0 {
			t.Errorf("a predecessor refreshes nothing: refreshed=%v deferred=%v", refreshed, deferred)
		}
	})
	t.Run("loading", func(t *testing.T) {
		m := refreshMgr(t, "")
		loaded(m, "m1", "box1", 49*time.Hour, func(p *Process) { p.state = StateLoading })
		if refreshed, deferred := m.refreshNonECC(time.Now()); len(refreshed) != 0 || len(deferred) != 1 {
			t.Errorf("refreshed=%v deferred=%v", refreshed, deferred)
		}
	})
}

// In-flight requests finish: a busy backend drains, and goes when the last
// request releases it.
func TestRefreshDrainsInFlight(t *testing.T) {
	m := refreshMgr(t, "")
	p := loaded(m, "m1", "box1", 49*time.Hour, func(p *Process) { p.refs = 1 })
	if refreshed, _ := m.refreshNonECC(time.Now()); len(refreshed) != 1 {
		t.Fatalf("refreshed=%v", refreshed)
	}
	if !isCurrent(m, "m1", p) || !p.Draining() {
		t.Fatal("a busy backend must drain, not go at once")
	}
	m.releaser(p)() // the in-flight request finishes
	if isCurrent(m, "m1", p) {
		t.Error("still resident after its last request finished")
	}
}

// A request arriving during a refresh drain waits for the old process to go,
// then gets the replacement — instead of being refused as "draining".
func TestEnsureReadyWaitsOutARefreshDrain(t *testing.T) {
	defer func(d time.Duration) { refreshPoll = d }(refreshPoll)
	refreshPoll = 10 * time.Millisecond
	fakeLocalECC(t, nil, nil, false, false)
	// A pure proxy: its "load" spawns nothing, so the replacement is ready
	// at once and the test needs no process.
	m := NewManager(eccCfg(t, `
servers:
  box1:
    pools: { gpu0: 24GB }
models:
  m1:
    proxy: "127.0.0.1:1"
`))
	old := loaded(m, "m1", "box1", 49*time.Hour, func(p *Process) { p.refs = 1; p.draining, p.refreshing = true, true })

	type result struct {
		p   *Process
		err error
	}
	got := make(chan result, 1)
	go func() {
		p, release, _, err := m.EnsureReady(context.Background(), "m1", m.config().Models["m1"], nil, nil)
		if release != nil {
			release()
		}
		got <- result{p, err}
	}()
	select {
	case r := <-got:
		t.Fatalf("returned during the drain: %v, %v", r.p, r.err)
	case <-time.After(100 * time.Millisecond):
	}
	m.releaser(old)() // the in-flight request finishes; the drain completes
	select {
	case r := <-got:
		if r.err != nil || r.p == nil || r.p == old {
			t.Fatalf("after the drain: %v, %v; want the replacement", r.p, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still waiting after the drain completed")
	}
}

// An adopted agent backend keeps the start time the agent reports: the load
// time is the weights', not this primary's, so a restart or handoff does not
// reset the refresh clock.
func TestAdoptedBackendKeepsItsLoadTime(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(backend.Close)
	agentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/hello"):
			_, _ = fmt.Fprintf(w, `{"protocol":%d}`, agent.Protocol)
		case strings.HasSuffix(r.URL.Path, "/logs"):
			_, _ = w.Write([]byte(`{"lines":[],"next":1}`))
		default:
			_, _ = w.Write([]byte(`{"id":"b1","alive":true,"memoryMiB":0}`))
		}
	}))
	t.Cleanup(agentSrv.Close)
	u, _ := url.Parse(backend.URL)
	cmd := "exec llama-server --port " + u.Port()
	fakeLocalECC(t, nil, nil, false, false)
	m := NewManager(eccCfg(t, fmt.Sprintf(`
servers:
  box1:
    pools: { gpu0: 24GB }
    agent: { endpoints: [%q], token: t }
models:
  swift:
    cmd: %q
    server: box1
    ramUsage: { gpu0: 20GB }
    proxy: %s
`, agentSrv.URL, cmd, u.Port())))
	live := agent.NewLiveness()
	m.SetLiveness(live)
	live.RecordCapacity("box1", agent.Capacity{GPUs: []agent.GPUDevice{{UUID: eccUUIDA}}})

	started := time.Now().Add(-50 * time.Hour).Truncate(time.Millisecond)
	if a, _, _ := m.ReconcileAgent(context.Background(), "box1", []agent.Backend{{
		ID: "b1", Key: "swift", Model: "swift", Alive: true, Started: started.UnixMilli(),
		Cmd: cmd, ConfigCmd: cmd, Port: atoiPort(u.Port()),
	}}); a != 1 {
		t.Fatalf("adopted %d, want 1", a)
	}
	m.mu.Lock()
	p := m.procs["swift"]
	m.mu.Unlock()
	if p == nil {
		t.Fatal("not adopted")
	}
	defer stopWatching(p.handle)
	if !p.loadedAt.Equal(started) {
		t.Errorf("loadedAt = %v, want the agent's start %v", p.loadedAt, started)
	}
	if why := m.refreshDue(p, time.Now(), 48*time.Hour); why == "" {
		t.Error("an adopted backend loaded 50h ago is due; adoption must not reset its clock")
	}
}

func atoiPort(s string) int {
	var n int
	_, _ = fmt.Sscanf(s, "%d", &n)
	return n
}
