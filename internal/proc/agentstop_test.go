package proc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/agent"
	"github.com/iodesystems/corrallm/internal/config"
)

// slowAgent is an agent whose backends take exitDelay to exit after SIGTERM
// and each hold memMiB on its one GPU while alive. Its capacity endpoint reads
// that GPU as a real agent's nvidia-smi probe would.
type slowAgent struct {
	srv       *httptest.Server
	exitDelay time.Duration
	memMiB    int64

	mu      sync.Mutex
	n       int
	termAt  map[string]time.Time
	started map[string]time.Time
}

func newSlowAgent(t *testing.T, exitDelay time.Duration, memMiB int64) *slowAgent {
	t.Helper()
	a := &slowAgent{exitDelay: exitDelay, memMiB: memMiB,
		termAt: map[string]time.Time{}, started: map[string]time.Time{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agent/v1/hello", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(agent.Hello{Protocol: agent.Protocol})
	})
	mux.HandleFunc("POST /agent/v1/backends", func(w http.ResponseWriter, r *http.Request) {
		var req agent.StartRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		a.mu.Lock()
		a.n++
		id := fmt.Sprintf("b%d-%s", a.n, req.Key)
		a.started[id] = time.Now()
		a.mu.Unlock()
		_ = json.NewEncoder(w).Encode(agent.Backend{ID: id, Key: req.Key, Alive: true})
	})
	mux.HandleFunc("GET /agent/v1/backends/{id}", func(w http.ResponseWriter, r *http.Request) {
		alive := a.alive(r.PathValue("id"), time.Now())
		_ = json.NewEncoder(w).Encode(agent.Backend{ID: r.PathValue("id"), Alive: alive, Exited: !alive, MemoryMiB: -1})
	})
	mux.HandleFunc("GET /agent/v1/backends/{id}/logs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"lines":[],"next":1}`))
	})
	mux.HandleFunc("POST /agent/v1/backends/{id}/signal", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		if _, ok := a.termAt[r.PathValue("id")]; !ok {
			a.termAt[r.PathValue("id")] = time.Now()
		}
		a.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /agent/v1/capacity", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(a.capacity())
	})
	a.srv = httptest.NewServer(mux)
	t.Cleanup(a.srv.Close)
	return a
}

// alive reports a backend's state at now.
func (a *slowAgent) alive(id string, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.started[id]; !ok {
		return false
	}
	t, signalled := a.termAt[id]
	if !signalled || now.Sub(t) < a.exitDelay {
		return true
	}
	return false
}

// capacity is the GPU as the agent would measure it now.
func (a *slowAgent) capacity() agent.Capacity {
	now := time.Now()
	a.mu.Lock()
	ids := make([]string, 0, len(a.started))
	for id := range a.started {
		ids = append(ids, id)
	}
	a.mu.Unlock()
	var used int64
	for _, id := range ids {
		if a.alive(id, now) {
			used += a.memMiB << 20
		}
	}
	const total = 32 << 30
	mem := agent.DeviceMem{Name: "RTX 5090", TotalBytes: total, UsedBytes: used, FreeBytes: total - used}
	return agent.Capacity{GPU: &mem, MeasuredAt: now.UnixMilli(),
		GPUs: []agent.GPUDevice{{DeviceMem: mem, UUID: "GPU-0"}}}
}

func (a *slowAgent) times(key string) (started, exited time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, t := range a.started {
		if len(id) > len(key) && id[len(id)-len(key):] == key {
			started = t
			if term, ok := a.termAt[id]; ok {
				exited = term.Add(a.exitDelay) // when it exits, seen or not
			}
		}
	}
	return
}

// slowAgentMgr is a primary whose one server is backed by a slowAgent, with
// the given models (served name → GiB on the GPU) all answering /health on one
// real listener.
func slowAgentMgr(t *testing.T, a *slowAgent, models map[string]int) (*Manager, *agent.Liveness) {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(backend.Close)
	u, _ := url.Parse(backend.URL)
	var port int
	_, _ = fmt.Sscanf(u.Port(), "%d", &port)
	y := fmt.Sprintf(`
servers:
  box:
    pools: { gpu: 32GiB }
    devicePool: gpu
    agent: { endpoints: [%q], token: t }
models:
`, a.srv.URL)
	for name, gib := range models {
		y += fmt.Sprintf(`
  %s:
    cmd: "exec llama-server --port %d"
    server: box
    ramUsage: { gpu: %dGiB }
    proxy: %d
`, name, port, gib, port)
	}
	cfg, err := config.LoadBytesForTest([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(cfg)
	t.Cleanup(m.Shutdown)
	live := agent.NewLiveness()
	live.Beat("box", time.Now())
	live.RecordCapacity("box", a.capacity())
	m.SetLiveness(live)
	m.markReported("box") // no post-restart hold: this primary "has heard" its agent
	return m, live
}

// The live failure, replayed: a build benchmark's trial holds ~29 GB on an
// agent's GPU; a heartbeat lands while it runs; the trial is stopped and the
// production model reloaded at once. The trial takes seconds to exit.
//
// Before: the teardown returned on the agent's "exited" while the ledger still
// held the heartbeat's reading with the trial in it, now counted as foreign —
// "no capacity: exceeds pool budget (blocking=[])", and the model stayed down.
// Now the teardown returns only after the group is gone AND a fresh reading is
// in, and the reload is admitted.
func TestTrialOnAgent_StopWaitsForExitAndFreesTheLedger(t *testing.T) {
	const exitDelay = 2 * time.Second
	a := newSlowAgent(t, exitDelay, 29<<10)
	m, live := slowAgentMgr(t, a, map[string]int{"swift": 29})
	mdl := m.config().Models["swift"]

	start := time.Now()
	var trialStopped time.Time
	_, err := m.TrialWith(context.Background(), "bench-swift", mdl, func(TrialEvent) {},
		func(context.Context, string) error {
			// A heartbeat while the trial runs: its reading has the trial in it.
			live.RecordCapacity("box", a.capacity())
			return nil
		})
	trialStopped = time.Now()
	if err != nil {
		t.Fatalf("trial: %v", err)
	}
	_, exited := a.times("trial:bench-swift")
	if exited.IsZero() || trialStopped.Before(exited) {
		t.Fatalf("trial teardown returned at +%s, before the backend exited (exited=%v)",
			trialStopped.Sub(start), exited)
	}

	if _, err := m.LoadModel(context.Background(), "swift"); err != nil {
		t.Fatalf("reload right after the trial: %v", err)
	}
}

// Making room on an agent server: the spawn of the model that needed the room
// starts only after the evicted backend has exited — its reservation is freed
// at eviction, its memory only at exit.
func TestMakeRoomOnAgent_SpawnWaitsForVictimExit(t *testing.T) {
	const exitDelay = 1500 * time.Millisecond
	a := newSlowAgent(t, exitDelay, 20<<10)
	m, _ := slowAgentMgr(t, a, map[string]int{"a": 20, "b": 20})
	m.activeUse = 0

	if _, err := m.LoadModel(context.Background(), "a"); err != nil {
		t.Fatalf("load a: %v", err)
	}
	if _, err := m.LoadModel(context.Background(), "b"); err != nil {
		t.Fatalf("load b (evicting a): %v", err)
	}
	_, aExited := a.times("a")
	bStarted, _ := a.times("b")
	if aExited.IsZero() {
		t.Fatal("a was never stopped")
	}
	if bStarted.Before(aExited) {
		t.Errorf("b spawned %s before a exited; the GPU still held a", aExited.Sub(bStarted))
	}
}
