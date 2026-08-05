//go:build darwin

package gpu

import (
	"fmt"
	"strconv"
	"strings"
)

// groupResidentMiB sums the resident memory of every process in pgid.
//
// On unified memory this IS the device footprint. There is no separate VRAM to
// attribute, so "how much GPU memory does this backend hold" and "how much
// memory does this process hold" are the same question, and macOS answers the
// second one readily. The code used to conclude the opposite: ProcVRAM has no
// macOS implementation, so the whole per-process path reported "cannot
// attribute" — and a number that was one `ps` call away became a field humans
// had to type, get wrong, and never find out they had got wrong.
//
// `ps`, not proc_pid_rusage's ri_phys_footprint, which is the better primitive:
// reading it needs cgo, and agent binaries are cross-compiled CGO_ENABLED=0 so
// that a Mac can be attached without a toolchain on it. A shell out to ps keeps
// that property.
//
// KNOWN FLOOR, not an exact figure: llama.cpp mmaps weights, so pages count as
// resident only once touched. A 32.6 GB model measured 23.5 GB immediately
// after load and 33.5 GB after serving one request. Callers get a number that
// starts low and converges upward as the model is used, which is the safe
// direction for a value that drives admission — it grows into the truth rather
// than over-promising room that is not there.
func groupResidentMiB(pgid int) (int, error) {
	// -A rather than -g: the group flag's meaning varies, while "list everything
	// with its pgid" is portable and unambiguous.
	out, err := runCmd("ps", "-A", "-o", "pgid=,rss=")
	if err != nil {
		return 0, fmt.Errorf("ps: %w", err)
	}
	total, found := 0, false
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		g, err1 := strconv.Atoi(f[0])
		rssKiB, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil || g != pgid {
			continue
		}
		total += rssKiB
		found = true
	}
	if !found {
		return 0, fmt.Errorf("no processes in group %d", pgid)
	}
	return total / 1024, nil
}

// perProcessAvailable reports that this platform can attribute memory to a
// process group even without a vendor GPU tool.
func perProcessAvailable() bool { return true }
