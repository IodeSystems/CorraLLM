package proc

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/iodesystems/corrallm/internal/config"
)

// deadPortModel builds a spawnable model whose target port nothing is listening
// on, so a load stays in StateLoading until its health timeout.
func deadPortModel(t *testing.T) *config.Config {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	var pn yaml.Node
	if err := pn.Encode(port); err != nil {
		t.Fatal(err)
	}
	return &config.Config{
		Servers: map[string]config.Server{"box": {Pools: map[string]string{"gpu": "10"}}},
		Models: map[string]config.Model{
			"A": {Cmd: "exec sleep 30", Server: "box", RAMUsage: map[string]string{"gpu": "6"}, Proxy: pn, Type: "local"},
		},
	}
}

// TestExplicitLoadRefusedWhileLoading: a second Load while the first is still
// running is refused rather than silently coalescing onto it.
//
// Coalescing is right for a REQUEST — two callers wanting the same model should
// share one spawn — but wrong for an operator action, where "loaded" for work
// somebody else started hides which click actually did anything.
func TestExplicitLoadRefusedWhileLoading(t *testing.T) {
	cfg := deadPortModel(t)
	mgr := NewManager(cfg)
	mgr.healthTimeout = 3 * time.Second
	defer mgr.Shutdown()

	// First load stalls in health-wait (nothing is listening).
	go func() { _, _ = mgr.LoadModel(context.Background(), "A") }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap := mgr.Snapshot()
		if len(snap.Models) == 1 && snap.Models[0].State == string(StateLoading) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	_, err := mgr.LoadModel(context.Background(), "A")
	if err == nil {
		t.Fatal("a second load while the first is in flight must be refused")
	}
	if !strings.Contains(err.Error(), "already loading") {
		t.Errorf("err = %v, want 'already loading'", err)
	}
}

// TestExplicitLoadRefusedWhileStopping: an explicit load is refused while the
// previous process is still being torn down.
//
// Eviction only REQUESTS an exit and drops the entry from procs at once, so
// without the stopping set the manager would happily spawn a replacement while
// the old process still held its port (or, as seen live, its fixed systemd unit
// name — "Unit oidio.scope was already loaded", spawn exits 1).
func TestExplicitLoadRefusedWhileStopping(t *testing.T) {
	cfg := deadPortModel(t)
	cfg.Extensions = map[string]config.Extension{"ext": {Cmd: "exec sleep 30", Server: "box"}}
	m := cfg.Models["A"]
	cfg.Models["hosted"] = config.Model{Extension: "ext", ExtensionHosted: true, Proxy: m.Proxy, Type: "local"}
	mgr := NewManager(cfg)
	defer mgr.Shutdown()

	// Stand in for a teardown that has not finished.
	stop := make(chan struct{})
	mgr.mu.Lock()
	mgr.stopping = map[string]chan struct{}{"A": stop, "extension:ext": stop}
	mgr.mu.Unlock()

	if _, err := mgr.LoadModel(context.Background(), "A"); err == nil {
		t.Error("load during teardown must be refused")
	} else if !strings.Contains(err.Error(), "still stopping") {
		t.Errorf("model err = %v, want 'still stopping'", err)
	}
	if _, err := mgr.LoadExtension(context.Background(), "ext"); err == nil {
		t.Error("extension load during teardown must be refused")
	} else if !strings.Contains(err.Error(), "still stopping") {
		t.Errorf("extension err = %v, want 'still stopping'", err)
	}
}

// TestEnsureReadyWaitsForTeardown: the REQUEST path waits the teardown out
// instead of failing, then spawns.
//
// The two paths differ on purpose. An operator issuing a load gets told no,
// because they can see the state and choose. A request has nobody to ask, and a
// lane fall-through firing on a 200ms teardown window would be a worse answer
// than waiting for it.
func TestEnsureReadyWaitsForTeardown(t *testing.T) {
	portA := listenTCP(t)
	cfg := resConfig(t, "10", "6", portA, listenTCP(t))
	mgr := NewManager(cfg)
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()

	stop := make(chan struct{})
	mgr.mu.Lock()
	mgr.stopping = map[string]chan struct{}{"A": stop}
	mgr.mu.Unlock()

	type result struct {
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		_, release, _, err := mgr.EnsureReady(context.Background(), "A", cfg.Models["A"], nil)
		if release != nil {
			release()
		}
		resCh <- result{err}
	}()

	// It must NOT have spawned while the teardown is outstanding.
	select {
	case r := <-resCh:
		t.Fatalf("EnsureReady returned during a teardown (err=%v); it must wait", r.err)
	case <-time.After(300 * time.Millisecond):
	}
	if got := mgr.Snapshot(); len(got.Models) != 0 {
		t.Fatalf("spawned into the teardown window: %+v", got.Models)
	}

	// Teardown completes → the spawn proceeds.
	mgr.mu.Lock()
	delete(mgr.stopping, "A")
	mgr.mu.Unlock()
	close(stop)

	select {
	case r := <-resCh:
		if r.err != nil {
			t.Fatalf("EnsureReady after teardown: %v", r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("EnsureReady never resumed after the teardown finished")
	}
}

// TestEnsureReadyTeardownWaitRespectsContext: a caller that gives up while
// waiting on a teardown is released, not pinned to it.
func TestEnsureReadyTeardownWaitRespectsContext(t *testing.T) {
	cfg := resConfig(t, "10", "6", listenTCP(t), listenTCP(t))
	mgr := NewManager(cfg)
	defer mgr.Shutdown()

	stop := make(chan struct{})
	defer close(stop)
	mgr.mu.Lock()
	mgr.stopping = map[string]chan struct{}{"A": stop}
	mgr.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, _, _, err := mgr.EnsureReady(ctx, "A", cfg.Models["A"], nil)
	if err == nil {
		t.Fatal("expected the wait to end with the context")
	}
	if !strings.Contains(err.Error(), "deadline exceeded") {
		t.Errorf("err = %v, want a context error", err)
	}
}

// TestUnloadRegistersTeardown: an eviction is remembered until the process
// group is confirmed gone, and forgotten afterwards.
func TestUnloadRegistersTeardown(t *testing.T) {
	cfg := resConfig(t, "10", "6", listenTCP(t), listenTCP(t))
	mgr := NewManager(cfg)
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()

	if _, err := mgr.LoadModel(context.Background(), "A"); err != nil {
		t.Fatalf("LoadModel: %v", err)
	}
	if _, err := mgr.UnloadModel("A"); err != nil {
		t.Fatalf("UnloadModel: %v", err)
	}

	// `sleep` dies on SIGTERM, so the teardown clears quickly — but it must
	// have been registered, and it must clear rather than leak.
	deadline := time.Now().Add(evictGrace + 5*time.Second)
	for time.Now().Before(deadline) {
		mgr.mu.Lock()
		n := len(mgr.stopping)
		mgr.mu.Unlock()
		if n == 0 {
			// Cleared: a later load is allowed again.
			if _, err := mgr.LoadModel(context.Background(), "A"); err != nil {
				t.Errorf("load after teardown completed: %v", err)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("teardown entry never cleared")
}

// TestHealthWaitAbortsOnProcessExit: a backend that dies during startup fails
// immediately instead of burning the whole health timeout.
//
// The wait polled a port for healthTimeout regardless of whether the process it
// was waiting for still existed — 600s on the production box. That kept a dead
// backend in "loading" with its pools reserved, and delayed every retry behind
// it. The spawn here exits at once and nothing ever listens on the port, so
// only the exit signal can end the wait.
func TestHealthWaitAbortsOnProcessExit(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	var pn yaml.Node
	if err := pn.Encode(port); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Servers: map[string]config.Server{"box": {Pools: map[string]string{"gpu": "10"}}},
		Models: map[string]config.Model{
			"A": {Cmd: "exit 1", Server: "box", RAMUsage: map[string]string{"gpu": "6"}, Proxy: pn, Type: "local"},
		},
	}
	mgr := NewManager(cfg)
	mgr.healthTimeout = 60 * time.Second // must NOT be what ends this
	defer mgr.Shutdown()

	start := time.Now()
	_, _, _, err = mgr.EnsureReady(context.Background(), "A", cfg.Models["A"], nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a backend that exits during startup must not report ready")
	}
	if elapsed > 15*time.Second {
		t.Errorf("waited %s for a process that died immediately; the exit signal was ignored", elapsed)
	}
	// And its pools are released, not held for the timeout.
	if got := mgr.Snapshot(); len(got.Models) != 0 {
		t.Errorf("dead backend still holds residency: %+v", got.Models)
	}
}
