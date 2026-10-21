//go:build unix && !linux

package host

import (
	"fmt"
	"os/exec"
	"strings"
)

// ProcIdentity asks ps, which is what macOS and the BSDs offer instead of
// /proc. lstart has one-second resolution; together with the pid and the full
// command line that is enough to tell a reused pid from the original.
//
// -ww: without it ps truncates the command to the terminal width, and a
// llama-server argv is longer than any terminal.
func ProcIdentity(pid int) (Identity, error) {
	start, err := exec.Command("ps", "-o", "lstart=", "-p", fmt.Sprint(pid)).Output()
	if err != nil {
		return Identity{}, err
	}
	cmd, err := exec.Command("ps", "-ww", "-o", "command=", "-p", fmt.Sprint(pid)).Output()
	if err != nil {
		return Identity{}, err
	}
	id := Identity{Start: strings.TrimSpace(string(start)), Cmdline: strings.TrimSpace(string(cmd))}
	if id.Start == "" || id.Cmdline == "" {
		return Identity{}, fmt.Errorf("pid %d: ps reported nothing", pid)
	}
	return id, nil
}
