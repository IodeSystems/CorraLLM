package proc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/agent"
	"github.com/iodesystems/corrallm/internal/config"
)

const helperPortEnv = "CORRALLM_PROC_HELPER_PORT"

// TestHelperBackend is a stand-in local backend: the test binary re-executed
// by a spawn, answering /health on the port it is given. It does nothing
// unless started that way.
func TestHelperBackend(t *testing.T) {
	port := os.Getenv(helperPortEnv)
	if port == "" {
		t.Skip("stand-in backend for the handoff tests")
	}
	_ = http.ListenAndServe("127.0.0.1:"+port, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
}

// localMgr is a manager with one LOCAL spawnable model whose process is the
// helper above.
func localMgr(t *testing.T) *Manager {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	cmd := fmt.Sprintf("%s=%d exec %s -test.run=^TestHelperBackend$", helperPortEnv, port, os.Args[0])
	cfg, err := config.LoadBytesForTest([]byte(fmt.Sprintf(`
servers:
  box:
    pools: { gpu: 10GB }
models:
  m:
    cmd: %q
    server: box
    ramUsage: { gpu: 6GB }
    proxy: %d
`, cmd, port)))
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(cfg)
	m.healthTimeout = 20 * time.Second
	t.Cleanup(m.Shutdown)
	return m
}

func waitLocal(t *testing.T, m *Manager, want int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if m.LocalBackends() == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("LocalBackends = %d, want %d", m.LocalBackends(), want)
}

// The draining predecessor keeps a local backend for the request it is still
// serving, stops it the moment that request releases it, and spawns nothing
// after that — so the successor can load onto a GPU that is free.
func TestHandoff_DrainReleasesLocalBackends(t *testing.T) {
	m := localMgr(t)
	ctx := context.Background()
	mdl := m.config().Models["m"]
	_, release, _, err := m.EnsureReady(ctx, "m", mdl, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitLocal(t, m, 1)

	m.Drain()
	// Busy: kept, and still usable for work it already admitted.
	waitLocal(t, m, 1)
	_, again, _, err := m.EnsureReady(ctx, "m", mdl, nil, nil)
	if err != nil {
		t.Fatalf("resident backend refused while draining: %v", err)
	}
	again()
	release()
	waitLocal(t, m, 0)

	if _, _, _, err := m.EnsureReady(ctx, "m", mdl, nil, nil); !errors.Is(err, ErrDraining) {
		t.Fatalf("spawn while draining = %v, want ErrDraining", err)
	}
}

// The successor holds a local spawn until the predecessor has released the
// GPU, then spawns normally.
func TestHandoff_LocalSpawnHeld(t *testing.T) {
	m := localMgr(t)
	m.HoldLocalSpawns()
	got := make(chan error, 1)
	go func() {
		_, rel, _, err := m.EnsureReady(context.Background(), "m", m.config().Models["m"], nil, nil)
		if err == nil {
			rel()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("spawned during the hold (err %v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	if n := m.LocalBackends(); n != 0 {
		t.Fatalf("LocalBackends = %d during the hold", n)
	}
	m.ReleaseLocalSpawns()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("not spawned after the hold was released")
	}
	// The hold applies only while it stands; a context ends a held wait.
	m.HoldLocalSpawns()
	if _, err := m.UnloadModel("m"); err != nil {
		t.Fatal(err)
	}
	waitLocal(t, m, 0)
	cctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, _, _, err := m.EnsureReady(cctx, "m", m.config().Models["m"], nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("held spawn with a deadline = %v", err)
	}
}

// A draining predecessor must not reconcile agents: reaping what it does not
// claim could kill a backend the successor has just adopted.
func TestHandoff_DrainingSkipsAgentReconcile(t *testing.T) {
	m, fa, port, _ := surviveMgr(t)
	m.Drain()
	a, r, v := m.ReconcileAgent(context.Background(), "mac1", []agent.Backend{{
		ID: "b1-x", Key: "gone", Alive: true, Started: old(), ConfigCmd: "x", Port: port,
	}})
	if a+r+v != 0 {
		t.Fatalf("reconciled while draining: adopted=%d reaped=%d vanished=%d", a, r, v)
	}
	if _, sig := fa.counts(); len(sig) != 0 {
		t.Errorf("signals %v sent while draining", sig)
	}
}

// A server that moved under an agent beside the primary (local in the
// predecessor's config, agent-backed in the successor's) spawns onto the GPU
// the predecessor's local children still hold. The successor holds that agent
// spawn with the local ones; an agent spawn onto a server the predecessor ran
// nothing local on is not held.
func TestHandoff_AgentSpawnHeldOnThePredecessorsLocalServer(t *testing.T) {
	spawnAsync := func(m *Manager) chan error {
		got := make(chan error, 1)
		go func() {
			_, rel, _, err := m.EnsureReady(context.Background(), "m", m.config().Models["m"], nil, nil)
			if err == nil {
				rel()
			}
			got <- err
		}()
		return got
	}
	held := func(t *testing.T, got chan error, fa *fakeAgent) {
		t.Helper()
		select {
		case err := <-got:
			t.Fatalf("agent spawn ran during the hold (err %v)", err)
		case <-time.After(300 * time.Millisecond):
		}
		if n, _ := fa.counts(); n != 0 {
			t.Fatalf("agent asked to spawn %d during the hold", n)
		}
	}
	ran := func(t *testing.T, got chan error, fa *fakeAgent) {
		t.Helper()
		select {
		case err := <-got:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("agent spawn still held")
		}
		if n, _ := fa.counts(); n != 1 {
			t.Fatalf("agent spawns = %d, want 1", n)
		}
	}

	t.Run("before the predecessor reports", func(t *testing.T) {
		m, fa, _, _ := surviveMgr(t)
		m.HoldLocalSpawns()
		got := spawnAsync(m)
		held(t, got, fa)
		m.ReleaseLocalSpawns()
		ran(t, got, fa)
	})
	t.Run("predecessor runs local children on that server", func(t *testing.T) {
		m, fa, _, _ := surviveMgr(t)
		m.HoldLocalSpawns()
		m.SetHoldServers([]string{"mac1"})
		got := spawnAsync(m)
		held(t, got, fa)
		m.ReleaseLocalSpawns()
		ran(t, got, fa)
	})
	t.Run("predecessor runs nothing local there", func(t *testing.T) {
		m, fa, _, _ := surviveMgr(t)
		m.HoldLocalSpawns()
		m.SetHoldServers([]string{"box1"})
		ran(t, spawnAsync(m), fa)
	})
}

// The predecessor reports WHERE its local children run, and "none" as an empty
// list — distinct from nil, which a successor reads as "not reported".
func TestHandoff_LocalBackendsOn(t *testing.T) {
	m := localMgr(t)
	if n, servers := m.LocalBackendsOn(); n != 0 || servers == nil || len(servers) != 0 {
		t.Fatalf("idle: %d %#v, want 0 and an empty non-nil list", n, servers)
	}
	_, release, _, err := m.EnsureReady(context.Background(), "m", m.config().Models["m"], nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	waitLocal(t, m, 1)
	if n, servers := m.LocalBackendsOn(); n != 1 || len(servers) != 1 || servers[0] != "box" {
		t.Errorf("one local backend: %d %v, want 1 [box]", n, servers)
	}
}
