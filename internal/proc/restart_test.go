package proc

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/iodesystems/corrallm/internal/config"
)

// restartCfg is one spawnable model whose cmd is `sleep` and whose port a
// fake backend answers, so a load passes its health check at once.
func restartCfg(t *testing.T, cmd string) *config.Config {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(srv.Close)
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	return restartCfgOn(t, cmd, port)
}

func restartCfgOn(t *testing.T, cmd string, port int) *config.Config {
	t.Helper()
	var pn yaml.Node
	if err := pn.Encode(port); err != nil {
		t.Fatal(err)
	}
	return &config.Config{
		Servers: map[string]config.Server{"box": {Pools: map[string]string{"gpu": "10"}}},
		Models: map[string]config.Model{
			"A": {Cmd: cmd, Server: "box", RAMUsage: map[string]string{"gpu": "6"}, Proxy: pn, Type: "local"},
		},
	}
}

func portOf(cfg *config.Config) int {
	var n int
	pn := cfg.Models["A"].Proxy
	_ = pn.Decode(&n)
	return n
}

func current(m *Manager, key string) *Process {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.procs[key]
}

// The drain-restart: the in-flight request keeps its backend until it ends, a
// request arriving meanwhile waits and is served by the replacement, and the
// replacement runs the cmd the config holds NOW.
func TestRestartDrainsHoldsAndRespawnsFromCurrentConfig(t *testing.T) {
	defer func(d time.Duration) { refreshPoll = d }(refreshPoll)
	refreshPoll = 10 * time.Millisecond
	cfg := restartCfg(t, "exec sleep 300")
	m := NewManager(cfg)
	defer m.Shutdown()

	old, releaseOld, _, err := m.EnsureReady(context.Background(), "A", cfg.Models["A"], nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A new build: same model, new cmd.
	m.SetConfig(restartCfgOn(t, "exec sleep 301", portOf(cfg)))

	type restartOut struct {
		res RestartResult
		err error
	}
	restarted := make(chan restartOut, 1)
	stages := make(chan string, 8)
	go func() {
		res, err := m.Restart(context.Background(), "A", RestartOptions{Reason: "test",
			DrainTimeout: 10 * time.Second, Progress: func(s, _ string) { stages <- s }})
		restarted <- restartOut{res, err}
	}()
	if s := <-stages; s != "draining" {
		t.Fatalf("first stage %q, want draining", s)
	}

	held := make(chan *Process, 1)
	go func() {
		p, release, _, err := m.EnsureReady(context.Background(), "A", m.config().Models["A"], nil, nil)
		if err != nil {
			t.Errorf("held request refused: %v", err)
			held <- nil
			return
		}
		release()
		held <- p
	}()
	select {
	case p := <-held:
		t.Fatalf("a request during the drain was answered (%v) instead of held", p)
	case <-time.After(150 * time.Millisecond):
	}
	if current(m, "A") != old || !old.Draining() {
		t.Fatal("the in-flight request lost its backend before it finished")
	}

	releaseOld() // the in-flight request completes
	var got *Process
	select {
	case got = <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("held request never served")
	}
	if got == nil || got == old {
		t.Fatalf("held request got %v, want the replacement", got)
	}
	out := <-restarted
	if out.err != nil {
		t.Fatalf("restart: %v", out.err)
	}
	if !out.res.Running || out.res.InFlight != 1 || out.res.Cut != 0 {
		t.Errorf("result %+v, want running, 1 in flight, 0 cut", out.res)
	}
	if !out.res.CmdChanged || out.res.OldID == out.res.NewID {
		t.Errorf("result %+v: the replacement must run the new cmd in a new process", out.res)
	}
	if got.spawnedCmd != "exec sleep 301" {
		t.Errorf("replacement runs %q", got.spawnedCmd)
	}
}

// A drain that does not finish in time stops the backend anyway and reports
// the requests it cut.
func TestRestartDrainTimeoutStopsAnyway(t *testing.T) {
	defer func(d time.Duration) { refreshPoll = d }(refreshPoll)
	refreshPoll = 10 * time.Millisecond
	cfg := restartCfg(t, "exec sleep 300")
	m := NewManager(cfg)
	defer m.Shutdown()

	old, releaseOld, _, err := m.EnsureReady(context.Background(), "A", cfg.Models["A"], nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseOld()
	res, err := m.Restart(context.Background(), "A", RestartOptions{DrainTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if res.Cut != 1 {
		t.Errorf("cut %d, want 1", res.Cut)
	}
	if !old.Stopping() {
		t.Error("the old process is not marked stopped")
	}
	if p := current(m, "A"); p == nil || p == old {
		t.Error("no replacement after the timed-out drain")
	}
	releaseOld() // the cut request ends late; it must not touch the replacement
	if p := current(m, "A"); p == nil || p == old {
		t.Error("a late release of the cut request evicted the replacement")
	}
}

// A model with no process is not started by a restart.
func TestRestartNotRunning(t *testing.T) {
	m := NewManager(restartCfg(t, "exec sleep 300"))
	defer m.Shutdown()
	res, err := m.Restart(context.Background(), "A", RestartOptions{})
	if err != nil || res.Running {
		t.Fatalf("res %+v err %v, want not running, no error", res, err)
	}
	if current(m, "A") != nil {
		t.Error("a restart of a stopped model started it")
	}
}

// A plain unload's drain is backpressure, not a failure: transient capacity
// with a Retry-After, so an exhausted walk answers 429.
func TestDrainingRefusalIsTransient(t *testing.T) {
	m := NewManager(&config.Config{Models: map[string]config.Model{"oidio-stt": {Proxy: mustNode(t, "127.0.0.1:1")}}})
	p := resident(t, m, "oidio-stt", 1)
	p.mu.Lock()
	p.draining = true
	p.mu.Unlock()
	_, _, _, err := m.EnsureReady(context.Background(), "oidio-stt", m.config().Models["oidio-stt"], nil, nil)
	var ce *CapacityError
	if !errors.As(err, &ce) || ce.Permanent || ce.RetryAfter <= 0 {
		t.Fatalf("err %v, want a transient CapacityError with a Retry-After", err)
	}
}

func mustNode(t *testing.T, v string) yaml.Node {
	t.Helper()
	var n yaml.Node
	if err := n.Encode(v); err != nil {
		t.Fatal(err)
	}
	return n
}
