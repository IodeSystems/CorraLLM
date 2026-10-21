//go:build linux

package host

import (
	"fmt"
	"os"
	"strings"
)

// ProcIdentity reads pid's start time and argv from /proc.
//
// Start is the kernel's start time in clock ticks since boot (stat field 22),
// prefixed with the boot id so a record from before a reboot can never match a
// process after it. Cmdline is the raw NUL-separated argv — exact, with no
// quoting ambiguity, which is why /proc is preferred over ps here.
func ProcIdentity(pid int) (Identity, error) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return Identity{}, err
	}
	// comm is in parentheses and may itself contain spaces or ')', so fields
	// are counted from the LAST ')'. After it, field 3 (state) is index 0,
	// which puts starttime (field 22) at index 19.
	s := string(stat)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return Identity{}, fmt.Errorf("pid %d: unparseable stat", pid)
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 20 {
		return Identity{}, fmt.Errorf("pid %d: short stat", pid)
	}
	cmd, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return Identity{}, err
	}
	if len(cmd) == 0 {
		return Identity{}, fmt.Errorf("pid %d: empty cmdline (zombie or kernel thread)", pid)
	}
	boot, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return Identity{
		Start:   strings.TrimSpace(string(boot)) + ":" + f[19],
		Cmdline: string(cmd),
	}, nil
}
