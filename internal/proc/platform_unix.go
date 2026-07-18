//go:build unix

package proc

import (
	"os/exec"
	"syscall"
)

// sysProcAttr starts the backend in its own process group so a kill reaches the
// whole tree (sh -c → llama-server → children), not just the shell.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// killGroup signals the backend's entire process group.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	// Negative pid → the process group led by pid.
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}

// killGroupHard SIGKILLs the backend's entire process group. Used only after a
// SIGTERM grace period has expired — see Manager.reapGroup.
func killGroupHard(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}

// groupAlive reports whether ANY process remains in the group led by pid.
//
// Signal 0 performs the permission/existence check without delivering
// anything. This is deliberately a group check, not a check on the leader: the
// leader is `sh -c`, which exits as soon as it has exec'd or been signalled,
// while the llama-server GRANDCHILD is the process actually holding tens of GB
// of VRAM. Watching only the leader is exactly how a "backend exited" log line
// coexisted with a live process still owning the GPU.
func groupAlive(pid int) bool {
	return syscall.Kill(-pid, 0) == nil
}
