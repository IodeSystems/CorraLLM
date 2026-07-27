package proc

import (
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

// resident registers a ready process directly, so drain can be tested without
// spawning anything.
func resident(t *testing.T, m *Manager, key string, refs int) *Process {
	t.Helper()
	p := &Process{
		Name: key, ModelName: key, key: key,
		state: StateReady, refs: refs,
		ready: make(chan struct{}),
	}
	close(p.ready)
	m.mu.Lock()
	m.procs[key] = p
	m.mu.Unlock()
	return p
}

// An unload with work in flight must not break it: the backend goes to draining
// and is evicted only when the last request releases.
func TestUnloadDrainsInsteadOfKillingInFlightWork(t *testing.T) {
	m := NewManager(&config.Config{})
	p := resident(t, m, "oidio-stt", 2)

	n, err := m.UnloadModel("oidio-stt")
	if err != nil {
		t.Fatalf("unload: %v", err)
	}
	if n != 0 {
		t.Errorf("evicted = %d, want 0 (it should be draining, not gone)", n)
	}
	if !p.Draining() {
		t.Fatal("expected draining")
	}
	m.mu.Lock()
	_, still := m.procs["oidio-stt"]
	m.mu.Unlock()
	if !still {
		t.Fatal("evicted while requests were in flight")
	}

	rel := m.releaser(p)
	rel()
	m.mu.Lock()
	_, afterFirst := m.procs["oidio-stt"]
	m.mu.Unlock()
	if !afterFirst {
		t.Fatal("evicted with one request still in flight")
	}

	// releaser is once-guarded, so the second release needs its own closure —
	// exactly as two concurrent requests would each hold their own.
	m.releaser(p)()
	m.mu.Lock()
	_, afterLast := m.procs["oidio-stt"]
	m.mu.Unlock()
	if afterLast {
		t.Fatal("last request released but the backend was not evicted")
	}
}

// Idle unload stays immediate — draining is only for in-flight work.
func TestUnloadIdleEvictsImmediately(t *testing.T) {
	m := NewManager(&config.Config{})
	resident(t, m, "oidio-tts", 0)
	n, err := m.UnloadModel("oidio-tts")
	if err != nil {
		t.Fatalf("unload: %v", err)
	}
	if n != 1 {
		t.Errorf("evicted = %d, want 1", n)
	}
	m.mu.Lock()
	_, still := m.procs["oidio-tts"]
	m.mu.Unlock()
	if still {
		t.Error("idle backend was not evicted")
	}
}

// A draining backend admits nothing new; a second unload says so rather than
// silently starting another drain.
func TestDrainingRefusesASecondUnload(t *testing.T) {
	m := NewManager(&config.Config{})
	resident(t, m, "oidio-stt", 1)
	if _, err := m.UnloadModel("oidio-stt"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.UnloadModel("oidio-stt"); err == nil {
		t.Fatal("expected an error on a second unload while draining")
	}
}
