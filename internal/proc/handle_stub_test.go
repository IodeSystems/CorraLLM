package proc

import (
	"fmt"
	"syscall"

	"github.com/iodesystems/corrallm/internal/gpu"
	"github.com/iodesystems/corrallm/internal/host"
)

// pidHandle is a host.Handle over a pid these tests spawned themselves, for the
// tests that predate the host abstraction.
//
// It is FAITHFUL, not a stub: Alive and Signal use the same group syscalls
// host.Local does, and MemoryMiB routes through gpu.GroupVRAM so the
// fakeNvidiaSMI stub still drives it. These tests therefore keep asserting
// exactly what they always asserted — real group liveness, real escalation.
type pidHandle struct {
	pid int
	// done, when set, stands in for the process exiting. Nil blocks forever,
	// which is what a still-running backend looks like.
	done chan struct{}
}

func (h pidHandle) ID() string            { return fmt.Sprintf("pgid:%d", h.pid) }
func (h pidHandle) Done() <-chan struct{} { return h.done }
func (h pidHandle) Err() error            { return nil }

func (h pidHandle) Signal(s host.Sig) error {
	if s == host.SigKill {
		return syscall.Kill(-h.pid, syscall.SIGKILL)
	}
	return syscall.Kill(-h.pid, syscall.SIGTERM)
}

// Alive checks the GROUP, matching host.Local: the leader is `sh -c`, which
// exits early, while the grandchild is what actually holds the memory.
func (h pidHandle) Alive() bool { return syscall.Kill(-h.pid, 0) == nil }

func (h pidHandle) MemoryMiB() (int, error) { return gpu.GroupVRAM(h.pid) }

// sysProcAttr mirrors the primitive that moved to internal/host, so these tests
// can start their own process groups to reap.
func sysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

// groupAlive / killGroup mirror the primitives that moved to internal/host, so
// the reaping tests can still set up and observe group state directly.
func groupAlive(pid int) bool { return syscall.Kill(-pid, 0) == nil }
func killGroup(pid int) error { return syscall.Kill(-pid, syscall.SIGTERM) }
