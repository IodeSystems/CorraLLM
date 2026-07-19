package proc

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/iodesystems/corrallm/internal/config"
)

func proxyModel(t *testing.T, port int) config.Model {
	t.Helper()
	var pn yaml.Node
	if err := pn.Encode(port); err != nil {
		t.Fatal(err)
	}
	return config.Model{Proxy: pn, Type: "local"} // no Cmd: pure proxy
}

// The race: ml-kit's stt-diarize/tts/realtime-stt are pure proxies onto the
// port the `stt` model spawns. Skipping their health check declared them ready
// 7s before oidio could answer, and a bench firing into that window recorded
// HTTP 502 as a capability failure.
func TestSpawnerFor_FindsSiblingOwningThePort(t *testing.T) {
	port := listenTCP(t)
	cfg := &config.Config{Models: map[string]config.Model{
		"stt":         resModel(t, "box", map[string]string{"system": "1"}, port), // has Cmd
		"stt-diarize": proxyModel(t, port),                                        // proxies onto it
	}}
	m := NewManager(cfg)
	owner, ok := m.spawnerFor("stt-diarize", cfg.Models["stt-diarize"])
	if !ok || owner != "stt" {
		t.Errorf("spawnerFor = %q,%v; want stt,true", owner, ok)
	}
}

// A proxy onto a port nothing in the config spawns is a remote we do not own.
// Blocking a load on it would turn someone else's outage into a failed spawn.
func TestSpawnerFor_UnownedPortIsRemote(t *testing.T) {
	cfg := &config.Config{Models: map[string]config.Model{
		"paid": proxyModel(t, 65123),
	}}
	m := NewManager(cfg)
	if owner, ok := m.spawnerFor("paid", cfg.Models["paid"]); ok {
		t.Errorf("nothing spawns that port; got owner %q", owner)
	}
}

// A spawned model is not its own owner — that would deadlock it on itself.
func TestSpawnerFor_IgnoresSelf(t *testing.T) {
	port := listenTCP(t)
	cfg := &config.Config{Models: map[string]config.Model{
		"stt": resModel(t, "box", map[string]string{"system": "1"}, port),
	}}
	m := NewManager(cfg)
	if _, ok := m.spawnerFor("stt", cfg.Models["stt"]); ok {
		t.Error("a model must not wait on itself")
	}
}

func TestIsLoopback(t *testing.T) {
	for _, h := range []string{"", "localhost", "127.0.0.1", "::1"} {
		if !isLoopback(h) {
			t.Errorf("%q should be loopback", h)
		}
	}
	// An unresolvable or clearly remote host is treated as remote: waiting on
	// something that will never come up is worse than not waiting.
	for _, h := range []string{"api.openai.com", "10.0.0.5", "192.168.1.76"} {
		if isLoopback(h) {
			t.Errorf("%q should NOT be loopback", h)
		}
	}
}

// End to end: a pure proxy whose sibling owns the port must not report ready
// until that port actually answers.
func TestProxyWaitsForOwnedPort(t *testing.T) {
	port := freePort(t)
	cfg := &config.Config{Models: map[string]config.Model{
		"owner": resModel(t, "box", map[string]string{"system": "1"}, port),
		"proxy": proxyModel(t, port),
	}}
	m := NewManager(cfg)
	m.healthTimeout = 5 * time.Second
	defer m.Shutdown()

	// Nothing is listening yet; bring the port up shortly, as a slow backend would.
	go func() {
		time.Sleep(400 * time.Millisecond)
		serveOn(t, port)
	}()

	start := time.Now()
	_, done, _, err := m.EnsureReady(context.Background(), "proxy", cfg.Models["proxy"], nil)
	if err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	defer done()
	if el := time.Since(start); el < 300*time.Millisecond {
		t.Errorf("returned ready in %s — did not wait for the port its sibling owns", el)
	}
}

// freePort reserves a port and releases it, so a test can start something on it
// later. Racy in principle; fine in a test that binds it within milliseconds.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// serveOn brings up a /health responder on port, mimicking a backend that has
// finished loading.
func serveOn(t *testing.T, port int) {
	srv := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", port),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
	}
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Close() })
}
