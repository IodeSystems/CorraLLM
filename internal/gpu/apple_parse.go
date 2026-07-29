package gpu

import (
	"fmt"
	"regexp"
	"strconv"
)

// Pure helpers for the Apple prober, kept separate so they can be tested on any
// platform — the darwin path cannot be run on the machine that builds it, and
// parsing is where the bugs are.

var (
	applePageSizeRe = regexp.MustCompile(`page size of (\d+) bytes`)
	appleWiredRe    = regexp.MustCompile(`(?m)^"?Pages wired down"?:\s+(\d+)\.?\s*$`)
)

// parseWiredBytes pulls wired memory out of `vm_stat`.
//
// Wired is the interesting number on unified memory: it cannot be reclaimed,
// and it is where a Metal backend's weights sit. Free/inactive say how much
// room is left; wired says how much is genuinely committed.
func parseWiredBytes(out string) (int64, error) {
	pageSize := int64(4096)
	if m := applePageSizeRe.FindStringSubmatch(out); len(m) == 2 {
		if v, err := strconv.ParseInt(m[1], 10, 64); err == nil && v > 0 {
			pageSize = v
		}
	}
	m := appleWiredRe.FindStringSubmatch(out)
	if len(m) != 2 {
		return 0, fmt.Errorf("vm_stat: no wired page count")
	}
	pages, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, err
	}
	return pages * pageSize, nil
}

// defaultWiredLimitBytes approximates macOS's own default when
// iogpu.wired_limit_mb is unset.
//
// Apple does not document a single number and it has varied by release; the
// widely-observed behaviour is roughly 75% of physical memory on machines with
// plenty (>= 36 GB) and about two thirds on smaller ones. This only ever
// AUTO-FILLS a pool the operator did not declare, so being a few percent out
// shifts a default rather than breaking a placement — and an operator who cares
// sets iogpu.wired_limit_mb, which is then read directly.
func defaultWiredLimitBytes(memBytes int64) int64 {
	const gb = int64(1) << 30
	if memBytes >= 36*gb {
		return memBytes * 3 / 4
	}
	return memBytes * 2 / 3
}
