//go:build unix

package host

import (
	"errors"
	"fmt"
	"sync"
	"syscall"
	"time"

	"github.com/iodesystems/corrallm/internal/gpu"
	"github.com/iodesystems/corrallm/internal/sysmem"
)

// adoptPoll is how often an adopted group is checked for exit. There is no
// Wait to block on for a process this image did not start, so exit is
// discovered by looking; half a second is far finer than any grace period the
// result feeds.
const adoptPoll = 500 * time.Millisecond

// Adopt returns a Handle for a process group that is ALREADY running and that
// this process did not start through Local.Start — typically a backend a
// previous agent spawned, found again after a restart.
//
// The caller decides whether the group really is the one it thinks it is (see
// ProcIdentity); Adopt only refuses a group that does not exist.
//
// Exit is detected by polling the group. One wrinkle makes that more than a
// kill(-pgid, 0): when the agent re-execs itself (self-update keeps the PID),
// the backends it started are still ITS children, and a child that exits stays
// a zombie — still a member of its group, so "alive" — until its parent waits
// for it. The poll therefore reaps the leader when it can, and treats ECHILD
// (not our child) as the normal case it is.
func Adopt(pgid int) (Handle, error) {
	if pgid <= 0 {
		return nil, fmt.Errorf("adopt: bad process group %d", pgid)
	}
	if !groupAlive(pgid) {
		return nil, fmt.Errorf("adopt: process group %d is not running", pgid)
	}
	h := &adoptedHandle{pid: pgid, done: make(chan struct{})}
	go h.watch()
	return h, nil
}

type adoptedHandle struct {
	pid  int
	done chan struct{}

	mu  sync.Mutex
	err error
}

func (h *adoptedHandle) watch() {
	t := time.NewTicker(adoptPoll)
	defer t.Stop()
	for {
		h.reapLeader()
		if !groupAlive(h.pid) {
			close(h.done)
			return
		}
		<-t.C
	}
}

// reapLeader collects the leader's exit status if it is our child. A no-op
// (ECHILD) in the usual case: a group inherited from a previous process.
func (h *adoptedHandle) reapLeader() {
	var ws syscall.WaitStatus
	pid, err := syscall.Wait4(h.pid, &ws, syscall.WNOHANG, nil)
	if err != nil || pid != h.pid {
		return
	}
	if ws.Exited() && ws.ExitStatus() != 0 {
		h.setErr(fmt.Errorf("exit status %d", ws.ExitStatus()))
	} else if ws.Signaled() {
		h.setErr(fmt.Errorf("signal: %s", ws.Signal()))
	}
}

func (h *adoptedHandle) setErr(err error) {
	h.mu.Lock()
	h.err = err
	h.mu.Unlock()
}

func (h *adoptedHandle) ID() string { return fmt.Sprintf("pgid:%d", h.pid) }

func (h *adoptedHandle) Done() <-chan struct{} { return h.done }

// Err is nil for a group whose exit status could not be collected — which is
// every group not started by this process. Nil here says "no status known",
// not "exited cleanly".
func (h *adoptedHandle) Err() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

func (h *adoptedHandle) Signal(s Sig) error {
	var err error
	if s == SigKill {
		err = killGroupHard(h.pid)
	} else {
		err = killGroup(h.pid)
	}
	if errors.Is(err, syscall.ESRCH) {
		return nil // already gone: the intent is satisfied
	}
	return err
}

func (h *adoptedHandle) Alive() bool { return groupAlive(h.pid) }

func (h *adoptedHandle) MemoryMiB() (int, error) { return gpu.GroupVRAM(h.pid) }

func (h *adoptedHandle) MemoryOnMiB(uuid string) (int, error) { return gpu.GroupVRAMOn(h.pid, uuid) }

func (h *adoptedHandle) MemoryByDeviceMiB() (map[string]int, error) {
	return gpu.GroupVRAMByDevice(h.pid)
}

func (h *adoptedHandle) AnonBytes() (int64, error) { return sysmem.GroupAnonBytes(h.pid) }
