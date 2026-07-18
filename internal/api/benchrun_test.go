package api

import (
	"strings"
	"testing"
	"time"
)

// leaseSpy records begin/end so a test can assert the lease is ALWAYS released.
type leaseSpy struct {
	begun, ended []string
	refuse       bool
}

func (l *leaseSpy) begin(key, _ string, _ time.Duration) (time.Time, bool) {
	if l.refuse {
		return time.Time{}, false
	}
	l.begun = append(l.begun, key)
	return time.Now().Add(time.Minute), true
}
func (l *leaseSpy) end(key string) { l.ended = append(l.ended, key) }

func waitDone(t *testing.T, b *BenchRunner) BenchRunStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st := b.Status(); st.Done {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("bench run did not finish")
	return BenchRunStatus{}
}

// The lease MUST be released when the process exits. A lease outliving its run
// turns a finished benchmark into an ongoing outage; the TTL is the backstop,
// not the plan.
func TestBenchRunner_ReleasesLeaseOnExit(t *testing.T) {
	b := NewBenchRunner()
	spy := &leaseSpy{}
	if _, err := b.Start(BenchStartOptions{Bin: "true"}, spy.begin, spy.end); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitDone(t, b)
	if len(spy.begun) != 1 || len(spy.ended) != 1 {
		t.Fatalf("lease begun=%v ended=%v — must be balanced", spy.begun, spy.ended)
	}
	if spy.begun[0] != spy.ended[0] {
		t.Errorf("released a different key than it took: %q vs %q", spy.begun[0], spy.ended[0])
	}
}

// A FAILING bench must still release the lease — otherwise a crash locks out
// every caller until the TTL expires.
func TestBenchRunner_ReleasesLeaseOnFailure(t *testing.T) {
	b := NewBenchRunner()
	spy := &leaseSpy{}
	if _, err := b.Start(BenchStartOptions{Bin: "false"}, spy.begin, spy.end); err != nil {
		t.Fatalf("Start: %v", err)
	}
	st := waitDone(t, b)
	if len(spy.ended) != 1 {
		t.Errorf("a failing run must still release the lease: %v", spy.ended)
	}
	if st.Error == "" {
		t.Error("a non-zero exit must be surfaced, not swallowed")
	}
}

// A binary that does not exist must release the lease too — this is the easiest
// misconfiguration to hit (wrong --bench-bin) and would otherwise lock the box.
func TestBenchRunner_ReleasesLeaseWhenBinaryMissing(t *testing.T) {
	b := NewBenchRunner()
	spy := &leaseSpy{}
	_, err := b.Start(BenchStartOptions{Bin: "/nonexistent/llm-bench-xyz"}, spy.begin, spy.end)
	if err == nil {
		t.Fatal("expected a start error")
	}
	if len(spy.ended) != 1 {
		t.Errorf("a failed spawn must release the lease it took: begun=%v ended=%v", spy.begun, spy.ended)
	}
	if b.Status().Running {
		t.Error("runner must not stay marked running after a failed spawn")
	}
}

// Two concurrent runs would evict each other's models and produce garbage.
func TestBenchRunner_RejectsConcurrentRun(t *testing.T) {
	b := NewBenchRunner()
	spy := &leaseSpy{}
	if _, err := b.Start(BenchStartOptions{Bin: "sleep", Models: []string{"1"}}, spy.begin, spy.end); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Cancel()
	if _, err := b.Start(BenchStartOptions{Bin: "true"}, spy.begin, spy.end); err == nil {
		t.Error("a second concurrent run must be refused")
	}
}

// If the lease cannot be acquired, no process may start — otherwise a bench
// would run against contended traffic and produce noise.
func TestBenchRunner_NoSpawnWithoutLease(t *testing.T) {
	b := NewBenchRunner()
	spy := &leaseSpy{refuse: true}
	if _, err := b.Start(BenchStartOptions{Bin: "true"}, spy.begin, spy.end); err == nil {
		t.Error("must refuse to spawn when the lease is unavailable")
	}
	if b.Status().Running {
		t.Error("no run should be marked in flight")
	}
}

// The logged invocation is the contract with the CLI: llm-bench stays scriptable,
// so a UI-started run must be reproducible by copying its arguments.
func TestBenchRunner_RecordsReproducibleInvocation(t *testing.T) {
	b := NewBenchRunner()
	spy := &leaseSpy{}
	_, err := b.Start(BenchStartOptions{
		Bin: "true", Models: []string{"a", "b"}, Classes: []string{"capability"},
		ProbesDir: "probes", ConfigPath: "llm-bench.yaml",
	}, spy.begin, spy.end)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	st := waitDone(t, b)
	joined := strings.Join(st.Args, " ")
	for _, want := range []string{"run", "--models a,b", "--classes capability", "--tasks-dir probes", "--config llm-bench.yaml"} {
		if !strings.Contains(joined, want) {
			t.Errorf("invocation %q missing %q", joined, want)
		}
	}
	if len(st.Log) == 0 || !strings.HasPrefix(st.Log[0], "$ ") {
		t.Errorf("log should open with the copyable command, got %v", st.Log)
	}
}

// A nil runner is inert rather than a panic (endpoints disabled).
func TestBenchRunner_NilSafe(t *testing.T) {
	var b *BenchRunner
	b.Cancel()
	if st := b.Status(); st.Running {
		t.Error("nil runner must not report running")
	}
	if _, err := b.Start(BenchStartOptions{}, nil, nil); err == nil {
		t.Error("nil runner must refuse to start")
	}
}
