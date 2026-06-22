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

	mgr := NewManager()
	mgr.healthTimeout = 5 * time.Second

	// exec sleep so the leaf process replaces the shell — the group still
	// contains it; killGroup(-pgid) must reach it.
	b := backendCmd(t, "exec sleep 30", addr.Port)

	p, err := mgr.EnsureReady(context.Background(), "sleeper#0", b)
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

// TestLoadCoalescing: concurrent EnsureReady for the same backend share one load
// (one spawn), not N duplicate spawns.
func TestLoadCoalescing(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	addr := ln.Addr().(*net.TCPAddr)

	mgr := NewManager()
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()

	b := backendCmd(t, "exec sleep 30", addr.Port)

	const n = 8
	pids := make(chan int, n)
	for range n {
		go func() {
			p, err := mgr.EnsureReady(context.Background(), "shared#0", b)
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
