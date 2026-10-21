//go:build unix

package toolchain

import (
	"os/exec"
	"syscall"
)

// killWithGroup makes cancelling cmd's context kill its whole process GROUP.
//
// exec.CommandContext on its own kills only the direct child — here `bash` —
// while the work is its children: git, cmake, nvcc. Those kept running after a
// cancel, holding the GPU and the build directory, and because they also held
// the stdout pipe, cmd.Run did not return until they finished on their own:
// "cancelled" meant "will stop in fifteen minutes".
func killWithGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
