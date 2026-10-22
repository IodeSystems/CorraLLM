package proc

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

// TestLogsSurviveStartupCrash: a backend that dies before it is healthy leaves
// the process table at once, and its output is the only place the reason is
// written. The logs view must still show it.
func TestLogsSurviveStartupCrash(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // nothing answers: the backend never becomes healthy

	mgr := NewManager(&config.Config{})
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()

	mdl := modelCmd(t, "echo boom-at-startup; exit 3", port)
	if _, _, _, err := mgr.EnsureReady(context.Background(), "crasher", mdl, nil, nil); err == nil {
		t.Fatal("EnsureReady succeeded on a backend that exits")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if strings.Contains(strings.Join(mgr.Logs("crasher"), "\n"), "boom-at-startup") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("logs after a startup crash = %q, want the backend's output", mgr.Logs("crasher"))
		}
		time.Sleep(50 * time.Millisecond)
	}
}
