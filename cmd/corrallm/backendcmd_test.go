package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/host"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/store"
)

func memStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestBackendRestartCommandIsWired(t *testing.T) {
	c, _, err := newRoot().Find([]string{"backend", "restart"})
	if err != nil || c.Name() != "restart" {
		t.Fatalf("backend restart not found: %v", err)
	}
	for _, f := range []string{"reason", "drain", "timeout", "name"} {
		if c.Flags().Lookup(f) == nil {
			t.Errorf("no --%s", f)
		}
	}
	if got := c.Flags().Lookup("drain").DefValue; got != proc.DefaultRestartDrain.String() {
		t.Errorf("--drain default %s", got)
	}
}

// The CLI files a request, the "reload" is the daemon's half claiming it, and
// the CLI follows it to done: the old process is replaced by a new one.
func TestRestartViaDaemonRunsTheRestart(t *testing.T) {
	prev := host.CheckPortFree
	host.CheckPortFree = false
	t.Cleanup(func() { host.CheckPortFree = prev })
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(up.Close)
	var pn yaml.Node
	_ = pn.Encode(up.Listener.Addr().(*net.TCPAddr).Port)
	cfg := &config.Config{
		Servers: map[string]config.Server{"box": {Pools: map[string]string{"gpu": "10"}}},
		Models: map[string]config.Model{"local-swift": {Cmd: "exec sleep 300", Server: "box",
			RAMUsage: map[string]string{"gpu": "6"}, Proxy: pn, Type: "chat"}},
	}
	mgr := proc.NewManager(cfg)
	t.Cleanup(mgr.Shutdown)
	if _, err := mgr.LoadModel(context.Background(), "local-swift"); err != nil {
		t.Fatal(err)
	}
	st := memStore(t)
	ctx := context.Background()
	var out bytes.Buffer
	err := restartViaDaemon(ctx, &out, st, "local-swift", "test", time.Minute, time.Minute,
		func() error { runBackendRestarts(ctx, st, mgr, nil); return nil }, 10*time.Millisecond, time.Second)
	if err != nil {
		t.Fatalf("restart: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "done: pgid:") || !strings.Contains(out.String(), "0 cut") {
		t.Errorf("output:\n%s", out.String())
	}
}

// No daemon claims the request (an older binary): the CLI says so instead of
// waiting forever, and the request cannot run later.
func TestRestartViaDaemonUnclaimed(t *testing.T) {
	st := memStore(t)
	var out bytes.Buffer
	err := restartViaDaemon(context.Background(), &out, st, "local-swift", "", time.Minute, time.Minute,
		func() error { return nil }, 10*time.Millisecond, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not take the request") {
		t.Fatalf("err %v", err)
	}
	if got, _ := st.ClaimBackendRestarts(time.Now().Add(-time.Hour), time.Now()); len(got) != 0 {
		t.Errorf("an abandoned request was still claimable: %+v", got)
	}
}

// A rejected config reload fails the request: the restart was for that config.
func TestRestartViaDaemonRejectedReload(t *testing.T) {
	st := memStore(t)
	mgr := proc.NewManager(&config.Config{})
	t.Cleanup(mgr.Shutdown)
	ctx := context.Background()
	err := restartViaDaemon(ctx, &bytes.Buffer{}, st, "local-swift", "", time.Minute, time.Minute,
		func() error { runBackendRestarts(ctx, st, mgr, context.DeadlineExceeded); return nil }, 10*time.Millisecond, time.Second)
	if err == nil || !strings.Contains(err.Error(), "reload was rejected") {
		t.Fatalf("err %v", err)
	}
}
