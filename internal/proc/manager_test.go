package proc

import (
	"context"
	"net"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/iodesystems/corrallm/internal/config"
)

// backendCmd builds a backend that spawns cmd and proxies to port.
func backendCmd(t *testing.T, cmd string, port int) config.Backend {
	t.Helper()
	var pn yaml.Node
	if err := pn.Encode(port); err != nil {
		t.Fatal(err)
	}
	return config.Backend{Cmd: cmd, Proxy: pn, Type: "local"}
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// TestSpawnHealthAndProcessGroupKill verifies the full lifecycle: spawn a child,
// pass health-check against a real listener, then Shutdown reaps the child's
// whole process group (no orphan leak).
func TestSpawnHealthAndProcessGroupKill(t *testing.T) {
	// A listener stands in for the backend's health port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	addr := ln.Addr().(*net.TCPAddr)

	mgr := NewManager(&config.Config{})
	mgr.healthTimeout = 5 * time.Second

	// exec sleep so the leaf process replaces the shell — the group still
	// contains it; killGroup(-pgid) must reach it.
	b := backendCmd(t, "exec sleep 30", addr.Port)

	p, _, _, err := mgr.EnsureReady(context.Background(), "sleeper#0", "sleeper", b)
	if err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	if p.state != StateReady {
		t.Fatalf("state = %s, want ready", p.state)
	}
	pid := p.cmd.Process.Pid
	if !alive(pid) {
		t.Fatalf("spawned process %d not alive", pid)
	}

	mgr.Shutdown()

	// The child should die promptly once its group is signalled.
	deadline := time.Now().Add(3 * time.Second)
	for alive(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if alive(pid) {
		t.Fatalf("process %d still alive after Shutdown (orphan leak)", pid)
	}
}

// TestEnsureReadyLoadedFlag: the first EnsureReady triggers the cold load
// (loaded=true); a later call for the warm backend coalesces (loaded=false), so
// only the trigger is charged the swap cost.
func TestEnsureReadyLoadedFlag(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	addr := ln.Addr().(*net.TCPAddr)

	mgr := NewManager(&config.Config{})
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()

	// Pure-proxy backend (no cmd): the listener is its health target.
	var pn yaml.Node
	if err := pn.Encode(addr.Port); err != nil {
		t.Fatal(err)
	}
	b := config.Backend{Proxy: pn, Type: "local"}

	_, done1, loaded1, err := mgr.EnsureReady(context.Background(), "warm#0", "warm", b)
	if err != nil {
		t.Fatalf("first EnsureReady: %v", err)
	}
	done1()
	if !loaded1 {
		t.Errorf("first call loaded = false, want true (it triggered the load)")
	}

	_, done2, loaded2, err := mgr.EnsureReady(context.Background(), "warm#0", "warm", b)
	if err != nil {
		t.Fatalf("second EnsureReady: %v", err)
	}
	done2()
	if loaded2 {
		t.Errorf("second call loaded = true, want false (backend already warm)")
	}
}

// TestLoadCoalescing: concurrent EnsureReady for the same backend share one load
// (one spawn), not N duplicate spawns.
func TestLoadCoalescing(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	addr := ln.Addr().(*net.TCPAddr)

	mgr := NewManager(&config.Config{})
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()

	b := backendCmd(t, "exec sleep 30", addr.Port)

	const n = 8
	pids := make(chan int, n)
	for range n {
		go func() {
			p, _, _, err := mgr.EnsureReady(context.Background(), "shared#0", "shared", b)
			if err != nil {
				pids <- -1
				return
			}
			pids <- p.cmd.Process.Pid
		}()
	}
	seen := map[int]bool{}
	for range n {
		pid := <-pids
		if pid == -1 {
			t.Fatal("EnsureReady failed")
		}
		seen[pid] = true
	}
	if len(seen) != 1 {
		t.Fatalf("expected 1 spawned pid across %d callers, got %d distinct", n, len(seen))
	}
}
