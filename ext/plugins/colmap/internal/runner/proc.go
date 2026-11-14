package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/lease"
)

// outcome of one child run.
type outcome int

const (
	exited  outcome = iota // the child exited by itself (see err)
	yielded                // stopped for a lease yield request
	stopped                // stopped because ctx ended (cancel or shutdown)
)

// child is a COLMAP (or ffmpeg) process in its own process group.
type child struct {
	cmd  *exec.Cmd
	done chan error
}

// startChild starts argv in its own process group. Output lines go to log
// and to onLine. The child gets SIGKILL if this service dies (Pdeathsig), so
// a hard stop of the service never leaves a GPU child holding VRAM.
func startChild(argv []string, env []string, dir string, log io.Writer, onLine func(string)) (*child, error) {
	c := &child{done: make(chan error, 1)}
	started := make(chan error, 1)
	// Pdeathsig fires when the THREAD that forked exits, so the fork happens
	// on a locked thread that lives as long as the child.
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = dir
		cmd.Env = env
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
		pr, pw := io.Pipe()
		cmd.Stdout, cmd.Stderr = pw, pw
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			sc := bufio.NewScanner(pr)
			sc.Buffer(make([]byte, 64<<10), 4<<20)
			for sc.Scan() {
				line := sc.Text()
				if log != nil {
					fmt.Fprintln(log, line)
				}
				if onLine != nil {
					onLine(line)
				}
			}
			_, _ = io.Copy(io.Discard, pr)
		}()
		if err := cmd.Start(); err != nil {
			pw.Close()
			wg.Wait()
			started <- err
			return
		}
		c.cmd = cmd
		started <- nil
		err := cmd.Wait()
		pw.Close()
		wg.Wait()
		c.done <- err
	}()
	if err := <-started; err != nil {
		return nil, err
	}
	return c, nil
}

func (c *child) signal(sig syscall.Signal) {
	if c.cmd != nil && c.cmd.Process != nil {
		_ = syscall.Kill(-c.cmd.Process.Pid, sig)
	}
}

// stop sends SIGTERM to the group (COLMAP's first SIGTERM is a graceful
// stop that keeps finished work), then SIGKILL at killAt.
func (c *child) stop(killAt time.Time) error {
	c.signal(syscall.SIGTERM)
	d := time.Until(killAt)
	if d < 0 {
		d = 0
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case err := <-c.done:
		return err
	case <-t.C:
		c.signal(syscall.SIGKILL)
		return <-c.done
	}
}

// runOnce runs argv until it exits, a yield arrives, or ctx ends.
// A yield SIGTERMs, then SIGKILLs after yieldKill (or at deadline − 10 s if
// sooner): on the 3080 the exhaustive matcher ignored SIGTERM inside a block
// (killed after 20 s), so waiting for the whole grace window only delays the
// incoming model; a kill loses the in-flight block, which the rerun redoes.
func runOnce(ctx context.Context, argv, env []string, dir string, log io.Writer, onLine func(string),
	yield <-chan lease.Yield, stopGrace, yieldKill time.Duration) (outcome, error) {
	c, err := startChild(argv, env, dir, log, onLine)
	if err != nil {
		return exited, err
	}
	select {
	case err := <-c.done:
		return exited, err
	case y := <-yield:
		// Kill margin: leave 10 s of the deadline for teardown and DELETE.
		killAt := y.Deadline.Add(-10 * time.Second)
		if k := time.Now().Add(yieldKill); k.Before(killAt) {
			killAt = k
		}
		if min := time.Now().Add(2 * time.Second); killAt.Before(min) {
			killAt = min
		}
		err := c.stop(killAt)
		if err == nil {
			return exited, nil // finished before it saw the signal
		}
		return yielded, err
	case <-ctx.Done():
		err := c.stop(time.Now().Add(stopGrace))
		if err == nil {
			return exited, nil
		}
		return stopped, err
	}
}

// exitDetail is "exit status N" or the error.
func exitDetail(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.String()
	}
	return err.Error()
}

// tail returns the last n lines of a file.
func tail(p string, n int) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
