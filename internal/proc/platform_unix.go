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
