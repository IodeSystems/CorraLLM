package host

import (
	"fmt"
	"os/exec"
	"sync"

	"github.com/iodesystems/corrallm/internal/gpu"
)

// Local runs backends as child processes of this corrallm. It is the behavior
// corrallm has always had, moved behind the interface unchanged.
type Local struct{ name string }

// NewLocal returns the host backing the named `servers:` entry on this machine.
func NewLocal(name string) *Local { return &Local{name: name} }

func (l *Local) Name() string { return l.name }

// Start spawns the command in its own process group and begins reaping it.
//
// The returned Handle owns cmd.Wait(): it may be called exactly once, so the
// handle calls it and publishes the result through Done/Err rather than letting
// callers race for it.
func (l *Local) Start(s Spec) (Handle, error) {
	cmd := exec.Command("sh", "-c", s.Cmd)
	cmd.Stdout, cmd.Stderr = s.Out, s.Out
	cmd.SysProcAttr = sysProcAttr()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn %q: %w", s.Cmd, err)
	}
	h := &localHandle{cmd: cmd, pid: cmd.Process.Pid, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		h.mu.Lock()
		h.err = err
		h.mu.Unlock()
		close(h.done)
	}()
	return h, nil
}

type localHandle struct {
	cmd  *exec.Cmd
	pid  int // process GROUP id: cmd was started with Setpgid, so pgid == leader pid
	done chan struct{}

	mu  sync.Mutex
	err error
}

func (h *localHandle) ID() string { return fmt.Sprintf("pgid:%d", h.pid) }

func (h *localHandle) Done() <-chan struct{} { return h.done }

func (h *localHandle) Err() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

func (h *localHandle) Signal(s Sig) error {
	if h.pid == 0 {
		return nil
	}
	switch s {
	case SigKill:
		return killGroupHard(h.pid)
	default:
		return killGroup(h.pid)
	}
}

func (h *localHandle) Alive() bool { return groupAlive(h.pid) }

// MemoryMiB sums the VRAM of every compute process in the group. The vendor
// tool reports the llama-server CHILD's pid, not the shell's, which is why this
// is a group sum rather than a lookup of the leader.
func (h *localHandle) MemoryMiB() (int, error) {
	if h.pid == 0 {
		return 0, fmt.Errorf("no process")
	}
	return gpu.GroupVRAM(h.pid)
}
