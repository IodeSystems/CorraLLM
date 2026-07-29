//go:build linux

package sysmem

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// meminfoPath is a var so tests can point it at a fixture.
var meminfoPath = "/proc/meminfo"

// Probe reads MemTotal/MemAvailable from /proc/meminfo (Linux).
//
// MemAvailable, not MemFree: free excludes reclaimable page cache, so on any
// box that has done I/O it reports a near-empty machine as full. Available is
// the kernel's own estimate of what a new allocation can actually get, which is
// the number a "can another model fit" question means.
func Probe() (Stats, error) {
	f, err := os.Open(meminfoPath)
	if err != nil {
		return Stats{}, fmt.Errorf("meminfo: %w", err)
	}
	defer func() { _ = f.Close() }()

	var total, avail int64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, kb, ok := parseMeminfoLine(sc.Text())
		if !ok {
			continue
		}
		switch key {
		case "MemTotal":
			total = kb * 1024
		case "MemAvailable":
			avail = kb * 1024
		}
	}
	if err := sc.Err(); err != nil {
		return Stats{}, fmt.Errorf("meminfo: %w", err)
	}
	if total <= 0 {
		return Stats{}, fmt.Errorf("meminfo: no MemTotal")
	}
	used := total - avail
	if used < 0 {
		used = 0
	}
	return Stats{TotalBytes: total, AvailableBytes: avail, UsedBytes: used}, nil
}

// parseMeminfoLine parses `MemTotal:       65780392 kB` → ("MemTotal", 65780392).
func parseMeminfoLine(line string) (string, int64, bool) {
	key, rest, ok := strings.Cut(line, ":")
	if !ok {
		return "", 0, false
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", 0, false
	}
	v, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return key, v, true
}
