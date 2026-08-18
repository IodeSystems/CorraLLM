//go:build unix

package agent

import (
	"fmt"
	"syscall"
)

// signalStaleGroup asks a leftover process GROUP to stop — the group, not the
// leader, because the shell may already be gone while the process actually
// holding the memory is its child.
func signalStaleGroup(pgid int) error { return syscall.Kill(-pgid, syscall.SIGTERM) }

// pidAlive reports whether the process exists. Signal 0 performs the
// existence/permission check without delivering anything.
func pidAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// psCommand renders the argv that prints a pid's command line, which is the
// safety catch on pid reuse: a stale record from days ago could name a pid the
// OS has since handed to something the operator cares about.
func psCommand(pid int) []string {
	return []string{"ps", "-o", "command=", "-p", fmt.Sprint(pid)}
}
