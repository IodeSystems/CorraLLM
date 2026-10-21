//go:build unix

package handoff

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Predecessor is the running process's side of a handoff it started.
type Predecessor struct {
	cmd    *exec.Cmd
	conn   net.Conn
	exited chan error
	mu     sync.Mutex
	last   []byte
}

// Start runs exe with args as this process's successor, handing it ln, and
// waits until it reports ready, exits, or timeout passes. On any failure the
// successor is killed and this process carries on as if nothing happened:
// it still holds ln and has not stopped accepting.
func Start(exe string, args []string, ln net.Listener, timeout time.Duration) (*Predecessor, error) {
	fl, ok := ln.(interface{ File() (*os.File, error) })
	if !ok {
		return nil, fmt.Errorf("%w: listener %T cannot be passed on", ErrUnsupported, ln)
	}
	lf, err := fl.File() // a dup; ours keeps accepting
	if err != nil {
		return nil, fmt.Errorf("handoff: listener fd: %w", err)
	}
	defer func() { _ = lf.Close() }()

	pair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("handoff: control socket: %w", err)
	}
	ours := os.NewFile(uintptr(pair[0]), "handoff-control")
	theirs := os.NewFile(uintptr(pair[1]), "handoff-control-child")
	defer func() { _ = theirs.Close() }()
	conn, err := net.FileConn(ours)
	_ = ours.Close()
	if err != nil {
		return nil, fmt.Errorf("handoff: control socket: %w", err)
	}

	// The successor writes to OUR stdout and stderr descriptors themselves (the
	// journal), never through a pipe this process copies: a pipe would close
	// when this process exits, and the successor's next log line would then
	// die of SIGPIPE (Go exits on EPIPE at fd 1 or 2). A failed start reports
	// its reason over the control socket instead (ReportStartFailure).
	cmd := exec.Command(exe, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{lf, theirs} // fd 3, fd 4
	cmd.Env = append(withoutHandoffEnv(os.Environ()),
		envFDs+"=3,4", envParent+"="+strconv.Itoa(os.Getpid()))
	if err := cmd.Start(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("handoff: start %s: %w", exe, err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	ready := make(chan error, 1)
	go func() {
		r := bufio.NewReader(conn)
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				ready <- fmt.Errorf("control socket closed before ready: %w", err)
				return
			}
			var m message
			if json.Unmarshal(line, &m) != nil {
				continue
			}
			if m.Error != "" {
				ready <- fmt.Errorf("failed to start: %s", m.Error)
				return
			}
			if m.Ready {
				ready <- nil
				return
			}
		}
	}()

	fail := func(why error) (*Predecessor, error) {
		_ = cmd.Process.Kill()
		_ = conn.Close()
		return nil, why
	}
	select {
	case err := <-ready:
		if err != nil {
			return fail(fmt.Errorf("handoff: successor pid %d: %w", cmd.Process.Pid, err))
		}
	case err := <-exited:
		// It may have said why on its way out; that beats an exit status.
		select {
		case rerr := <-ready:
			if rerr != nil && strings.HasPrefix(rerr.Error(), "failed to start") {
				return fail(fmt.Errorf("handoff: successor pid %d %v (%v)", cmd.Process.Pid, rerr, err))
			}
		case <-time.After(100 * time.Millisecond):
		}
		return fail(fmt.Errorf("handoff: successor pid %d exited before it was ready: %v", cmd.Process.Pid, err))
	case <-time.After(timeout):
		return fail(fmt.Errorf("handoff: successor pid %d not ready within %s", cmd.Process.Pid, timeout))
	}
	// From here the successor serves; its exit is reaped by the goroutine above.
	return &Predecessor{cmd: cmd, conn: conn, exited: exited}, nil
}

// Done delivers the successor's exit, should it die while this process is
// still draining.
func (p *Predecessor) Done() <-chan error { return p.exited }

// PID is the successor's.
func (p *Predecessor) PID() int { return p.cmd.Process.Pid }

// Send reports this process's occupancy to the successor. Repeats of the last
// report are not resent.
func (p *Predecessor) Send(s Status) error {
	b := encode(message{Status: &s})
	p.mu.Lock()
	defer p.mu.Unlock()
	if string(b) == string(p.last) {
		return nil
	}
	_ = p.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := p.conn.Write(b); err != nil {
		return err
	}
	p.last = b
	return nil
}

// Successor is the new process's side: the inherited listener and the control
// socket to its predecessor.
type Successor struct {
	Listener       net.Listener
	PredecessorPID int
	conn           net.Conn
}

// inherited is this process's control socket while it starts as a successor,
// so ReportStartFailure can reach the predecessor after Inherit consumed the
// environment.
var inherited net.Conn

// ReportStartFailure tells the predecessor, if this process was started by a
// handoff, why it is exiting before it became ready — so `corrallm service
// upgrade` can say "unknown flag: --x" rather than "exit status 1". A no-op
// otherwise.
func ReportStartFailure(err error) {
	if err == nil {
		return
	}
	conn := inherited
	if conn == nil && os.Getenv(envFDs) == "3,4" {
		// Failed before Inherit ran (a flag error): fd 4 is still ours.
		c, cerr := net.FileConn(os.NewFile(4, "handoff-control"))
		if cerr != nil {
			return
		}
		conn = c
	}
	if conn == nil {
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	_, _ = conn.Write(encode(message{Error: err.Error()}))
}

// Inherit picks up a handoff started by a predecessor. (nil, nil) when this
// process was started any other way.
func Inherit() (*Successor, error) {
	fds := os.Getenv(envFDs)
	if fds == "" {
		return nil, nil
	}
	parent, _ := strconv.Atoi(os.Getenv(envParent))
	// Not passed on: a later handoff from this process sets its own, and a
	// backend it spawns must not think it was handed anything.
	_ = os.Unsetenv(envFDs)
	_ = os.Unsetenv(envParent)
	if fds != "3,4" {
		return nil, fmt.Errorf("handoff: unexpected %s=%q", envFDs, fds)
	}
	lf := os.NewFile(3, "handoff-listener")
	ln, err := net.FileListener(lf)
	_ = lf.Close()
	if err != nil {
		return nil, fmt.Errorf("handoff: inherited listener: %w", err)
	}
	cf := os.NewFile(4, "handoff-control")
	conn, err := net.FileConn(cf)
	_ = cf.Close()
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("handoff: inherited control socket: %w", err)
	}
	inherited = conn
	return &Successor{Listener: ln, PredecessorPID: parent, conn: conn}, nil
}

// Ready tells the predecessor to stop accepting and drain. Call once this
// process is accepting on Listener.
func (s *Successor) Ready() error {
	inherited = nil // started: a later failure is not a start failure
	_, err := s.conn.Write(encode(message{Ready: true, PID: os.Getpid()}))
	return err
}

// Watch delivers each Status the predecessor sends, then calls gone once —
// when the predecessor exits (or the socket otherwise ends). Blocks; run it
// in a goroutine.
func (s *Successor) Watch(onStatus func(Status), gone func()) {
	defer gone()
	defer func() { _ = s.conn.Close() }()
	r := bufio.NewReader(s.conn)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if err != io.EOF {
				slog.Warn("handoff: control socket", "err", err)
			}
			return
		}
		var m message
		if json.Unmarshal(line, &m) == nil && m.Status != nil {
			onStatus(*m.Status)
		}
	}
}

func withoutHandoffEnv(env []string) []string {
	out := env[:0:0]
	for _, e := range env {
		if strings.HasPrefix(e, envFDs+"=") || strings.HasPrefix(e, envParent+"=") {
			continue
		}
		out = append(out, e)
	}
	return out
}

// Notify sends a state string to systemd's notify socket (sd_notify). A no-op
// returning false when not started by a unit that listens for it.
func Notify(state string) (bool, error) {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return false, nil
	}
	if strings.HasPrefix(addr, "@") {
		addr = "\x00" + addr[1:] // abstract namespace
	}
	c, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		return false, err
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Write([]byte(state)); err != nil {
		return false, err
	}
	return true, nil
}

// CheckSupervisor refuses a handoff that would kill the service: under
// systemd, the main pid can move to the successor only through the notify
// socket (Type=notify, NotifyAccess=all). Without one, the predecessor's exit
// reads to systemd as the service stopping, and it kills the successor with
// the rest of the unit.
func CheckSupervisor() error {
	if os.Getenv("INVOCATION_ID") != "" && os.Getenv("NOTIFY_SOCKET") == "" {
		return fmt.Errorf("%w: this systemd unit has no notify socket, so the main pid cannot move to a new process; "+
			"reinstall it (`corrallm service install`, Type=notify + NotifyAccess=all) or restart instead", ErrUnsupported)
	}
	return nil
}
