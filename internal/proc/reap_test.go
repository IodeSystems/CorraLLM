package proc

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// startStubborn launches a process group that IGNORES SIGTERM — the behavior
// that caused the leak: a llama-server stuck in CUDA teardown outlived its
// SIGTERM by minutes while corrallm had already freed its pool reservation and
// dropped it from tracking.
func startStubborn(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sh", "-c", `trap "" TERM; sleep 60`)
	cmd.SysProcAttr = sysProcAttr()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Reap in the background, exactly as the spawn path does. Without a Wait
	// the killed process lingers as a ZOMBIE, and signal-0 succeeds against a
	// zombie — so groupAlive would report a corpse as running.
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	})
	// Let the shell install its trap before anyone signals it.
	time.Sleep(150 * time.Millisecond)
	return cmd
}

func TestReapGroup_EscalatesToSIGKILL(t *testing.T) {
	cmd := startStubborn(t)
	pid := cmd.Process.Pid
	if !groupAlive(pid) {
		t.Fatal("stub should be alive")
	}
	if err := killGroup(cmd); err != nil {
		t.Fatal(err)
	}
	// SIGTERM is trapped, so it must still be alive — this is the precondition
	// the old fire-and-forget eviction never checked.
	time.Sleep(300 * time.Millisecond)
	if !groupAlive(pid) {
		t.Skip("stub honored SIGTERM; cannot exercise escalation on this shell")
	}

	m := &Manager{}
	done := make(chan struct{})
	go func() { m.reapGroup("stub", pid); close(done) }()

	select {
	case <-done:
	case <-time.After(evictGrace + 10*time.Second):
		t.Fatal("reapGroup did not return")
	}
	// Allow the reaping goroutine to collect the corpse before asserting: a
	// zombie is indistinguishable from a live process via signal 0.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && groupAlive(pid) {
		time.Sleep(50 * time.Millisecond)
	}
	if groupAlive(pid) {
		t.Error("process group survived reapGroup — VRAM would stay stranded")
	}
}

// A backend that exits promptly must NOT wait out the grace period: eviction is
// on the path to loading something else, and a fixed delay would make every
// swap slow.
func TestReapGroup_ReturnsImmediatelyOnCleanExit(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 30")
	cmd.SysProcAttr = sysProcAttr()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = cmd.Wait()

	m := &Manager{}
	start := time.Now()
	m.reapGroup("gone", pid)
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("reapGroup took %s for an already-dead group; must not wait out the grace", el)
	}
}

// groupAlive must watch the GROUP, not the leader. The leader is `sh -c`, whose
// exit is what cmd.Wait() reports as "backend exited" — while the grandchild
// holding the GPU can still be running. Conflating them is the bug.
func TestGroupAlive_TracksGrandchildNotLeader(t *testing.T) {
	// The shell exec's sleep, so the LEADER pid becomes sleep itself; the group
	// stays alive as long as any member is.
	cmd := exec.Command("sh", "-c", "sleep 30 & wait")
	cmd.SysProcAttr = sysProcAttr()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }() // reap, or the corpse reads as alive
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	time.Sleep(200 * time.Millisecond)
	if !groupAlive(pid) {
		t.Fatal("group should be alive while a member runs")
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !groupAlive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("group still reported alive after SIGKILL")
}
