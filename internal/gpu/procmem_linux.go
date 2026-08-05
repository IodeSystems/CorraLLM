//go:build linux

package gpu

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// groupResidentMiB sums the resident memory of every process in pgid, read from
// /proc.
//
// Only reached when the prober reports UNIFIED memory (see GroupVRAM). On a
// discrete-GPU box this number is host RAM and has nothing to do with the VRAM
// pool it would be charged against, so the caller gates it rather than this
// function refusing — the gate is about what the memory MEANS, not about
// whether it can be read.
//
// It exists on Linux as well as darwin so that "can this host attribute memory
// to a process group" is a property of the memory architecture rather than of
// which file happened to get written. A unified-memory Linux host (an ARM SoC
// with shared memory) gets the same treatment as Apple silicon.
//
// KNOWN FLOOR, like its darwin twin: mmapped weights count as resident only
// once touched, so the figure starts low and converges upward as the model is
// used. That is the safe direction for a value driving admission.
func groupResidentMiB(pgid int) (int, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return 0, fmt.Errorf("/proc: %w", err)
	}
	pageKiB := int64(os.Getpagesize()) / 1024
	var totalKiB int64
	found := false
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a pid directory
		}
		g, err := PGIDFn(pid)
		if err != nil || g != pgid {
			continue // gone mid-scan, or not ours
		}
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
		if err != nil {
			continue // exited between readdir and read; not fatal
		}
		f := strings.Fields(string(b))
		if len(f) < 2 {
			continue
		}
		rssPages, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			continue
		}
		totalKiB += rssPages * pageKiB
		found = true
	}
	if !found {
		return 0, fmt.Errorf("no processes in group %d", pgid)
	}
	return int(totalKiB / 1024), nil
}

func perProcessAvailable() bool { return true }
