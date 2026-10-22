package host

import (
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/iodesystems/corrallm/internal/gpu"
	"github.com/iodesystems/corrallm/internal/sysmem"
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
	// Load from disk, not from the page cache (pagecache.go). Here because
	// this is the one place every backend starts — on the primary and inside
	// an agent alike.
	EvictPageCache(s.Cmd)
	if err := portFree(s.Port); err != nil {
		return nil, err
	}
	shell, args := shellFor(s.Cmd)
	cmd := exec.Command(shell, args...)
	cmd.Stdout, cmd.Stderr = s.Out, s.Out
	cmd.SysProcAttr = sysProcAttr(s.Detach)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn %q: %w", s.Cmd, err)
	}
	// No-op on unix, where Setpgid already grouped the tree. On Windows this is
	// what creates the job that makes the tree killable as a unit; a failure
	// there is reported but not fatal, because a running backend we can only
	// half-reap still beats refusing to serve.
	if err := adoptGroup(cmd.Process.Pid); err != nil {
		fmt.Fprintf(s.Out, "corrallm: could not group this backend's process tree: %v\n", err)
	}
	h := &localHandle{cmd: cmd, pid: cmd.Process.Pid, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		releaseGroup(h.pid)
		h.mu.Lock()
		h.err = err
		h.mu.Unlock()
		close(h.done)
	}()
	return h, nil
}

// CheckPortFree turns portFree on. Off only in tests that stand a fake backend
// up on the port BEFORE the spawn, which is exactly what the check refuses.
var CheckPortFree = true

// portFree refuses a spawn whose port something else already answers on.
//
// Without it the failure is silent and misleading: the new backend cannot bind
// and exits, while the health check may reach the OTHER process — a copy left
// running in a terminal — and report it ready. corrallm then proxies to a
// process it did not start and cannot stop. 127.0.0.1 is enough to dial: a
// backend bound to 0.0.0.0 answers there too.
func portFree(port int) error {
	if port <= 0 || !CheckPortFree {
		return nil
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return nil
	}
	_ = conn.Close()
	return fmt.Errorf("port %d is already in use by a process corrallm did not start (a copy left running in a terminal?) — stop it, or give this model another port", port)
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

// MemoryOnMiB restricts the same group sum to one card. gpu.GroupVRAMOn errors
// when the prober cannot attribute per device, which is the caller's signal to
// fall back — not something to paper over with the whole-host figure.
func (h *localHandle) MemoryOnMiB(uuid string) (int, error) {
	if h.pid == 0 {
		return 0, fmt.Errorf("no process")
	}
	return gpu.GroupVRAMOn(h.pid, uuid)
}

// MemoryByDeviceMiB is the group's footprint on every card at once (see
// DeviceMemory).
func (h *localHandle) MemoryByDeviceMiB() (map[string]int, error) {
	if h.pid == 0 {
		return nil, fmt.Errorf("no process")
	}
	return gpu.GroupVRAMByDevice(h.pid)
}

// AnonBytes is the group's anonymous system RAM (see HostMemory).
func (h *localHandle) AnonBytes() (int64, error) {
	if h.pid == 0 {
		return 0, fmt.Errorf("no process")
	}
	return sysmem.GroupAnonBytes(h.pid)
}
