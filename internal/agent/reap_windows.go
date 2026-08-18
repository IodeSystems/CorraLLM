//go:build windows

package agent

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// signalStaleGroup asks a leftover tree to stop.
//
// A previous agent's Job Object died with that agent — KILL_ON_JOB_CLOSE means
// Windows has usually already taken the tree down, which is why this path
// matters less here than on unix. When something did survive, CTRL_BREAK is the
// closest thing to SIGTERM; it needs a console, so a service falls through to
// the caller's escalation rather than pretending it worked.
func signalStaleGroup(pgid int) error {
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(pgid))
}

// pidAlive reports whether the process exists.
//
// A handle that opens and reports STILL_ACTIVE is alive. Anything else is
// treated as gone: this only gates whether to try killing a leftover, and a
// wrong "gone" costs a missed reap rather than killing the wrong thing — the
// command check below is what actually guards against pid reuse.
func pidAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == 259 // STILL_ACTIVE
}

// psCommand renders the argv that prints a pid's command line.
//
// WMIC is deprecated but present on more installs than the PowerShell path is
// scriptable in one line; the caller only matches a substring, so the extra
// header line WMIC prints is harmless.
func psCommand(pid int) []string {
	return []string{"wmic", "process", "where", fmt.Sprintf("ProcessId=%d", pid), "get", "CommandLine"}
}
