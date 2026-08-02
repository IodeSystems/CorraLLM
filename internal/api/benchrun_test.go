package api

import (
	"strings"
	"testing"
	"time"
)

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

// A bench run is an ORDINARY caller. It holds no lease, evicts nothing, and
// turns nobody away — it queues for admission and waits out 429 backpressure
// like any other client. This test is the guard on that: the exclusive lease
// was removed because isolating model time from queue time is already achieved
// by measuring the queue wait and subtracting it, which costs no other caller
// anything.
func TestBenchRunner_NeverAsksForExclusivity(t *testing.T) {
	b := NewBenchRunner()
	if _, err := b.Start(BenchStartOptions{Bin: "true"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	st := waitDone(t, b)
	if joined := strings.Join(st.Args, " "); strings.Contains(joined, "--exclusive") {
		t.Errorf("a run must never request exclusivity, got %q", joined)
	}
}

// A run starts unconditionally: there is no lease to contend for, so nothing
// outside this runner can block one.
func TestBenchRunner_StartsWithoutAnyLease(t *testing.T) {
	b := NewBenchRunner()
	if _, err := b.Start(BenchStartOptions{Bin: "true"}); err != nil {
		t.Fatalf("a run must not depend on any lease: %v", err)
	}
	waitDone(t, b)
}

// A binary that does not exist must fail before anything is spawned or marked
// running. This is the easiest misconfiguration to hit — --bench-bin defaults
// to "llm-bench" on $PATH, which a fresh install does not have — so the error
// has to name the flag that fixes it.
func TestBenchRunner_FailsCleanlyWhenBinaryMissing(t *testing.T) {
	b := NewBenchRunner()
	_, err := b.Start(BenchStartOptions{Bin: "/nonexistent/llm-bench-xyz"})
	if err == nil {
		t.Fatal("expected a start error")
	}
	if !strings.Contains(err.Error(), "--bench-bin") {
		t.Errorf("the error must name the flag to fix; got %q", err)
	}
	if b.Status().Running {
		t.Error("runner must not stay marked running after a failed spawn")
	}
}

// Two concurrent runs would interleave their measurements on the same box.
func TestBenchRunner_RejectsConcurrentRun(t *testing.T) {
	b := NewBenchRunner()
	if _, err := b.Start(BenchStartOptions{Bin: "sleep", Models: []string{"1"}}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Cancel()
	if _, err := b.Start(BenchStartOptions{Bin: "true"}); err == nil {
		t.Error("a second concurrent run must be refused")
	}
}

// The logged invocation is the contract with the CLI: llm-bench stays scriptable,
// so a UI-started run must be reproducible by copying its arguments.
func TestBenchRunner_RecordsReproducibleInvocation(t *testing.T) {
	b := NewBenchRunner()
	_, err := b.Start(BenchStartOptions{
		Bin: "true", Models: []string{"a", "b"}, Classes: []string{"capability"},
		ProbeDirs: []string{"probes"}, ConfigPath: "llm-bench.yaml",
	})
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
	if _, err := b.Start(BenchStartOptions{}); err == nil {
		t.Error("nil runner must refuse to start")
	}
}
