package sysmem

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Pure parsers, kept out of the platform files so they can be tested anywhere.
// The darwin path cannot be exercised on the machine that builds it, so the
// parsing — which is where the bugs live — is testable from fixtures on Linux.

var (
	pageSizeRe = regexp.MustCompile(`page size of (\d+) bytes`)
	vmStatRe   = regexp.MustCompile(`(?m)^"?([A-Za-z][A-Za-z0-9 _-]*?)"?:\s+(\d+)\.?\s*$`)
)

// parseVMStatAvailable turns `vm_stat` output into available bytes.
//
// Available = free + inactive + speculative + purgeable. macOS keeps almost
// nothing strictly free — it fills memory with reclaimable cache — so counting
// only "Pages free" would report a mostly-idle 64 GB machine as full, and the
// scheduler would refuse to place anything on it.
//
// Wired pages are deliberately NOT counted as available: they cannot be
// reclaimed, and on Apple silicon that is exactly where a Metal backend's
// weights live.
func parseVMStatAvailable(out string) (int64, error) {
	pageSize := int64(4096)
	if m := pageSizeRe.FindStringSubmatch(out); len(m) == 2 {
		if v, err := strconv.ParseInt(m[1], 10, 64); err == nil && v > 0 {
			pageSize = v
		}
	}
	pages := map[string]int64{}
	for _, m := range vmStatRe.FindAllStringSubmatch(out, -1) {
		key := strings.ToLower(strings.TrimSpace(m[1]))
		v, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			continue
		}
		pages[key] = v
	}
	if len(pages) == 0 {
		return 0, fmt.Errorf("vm_stat: no page counts found")
	}
	var avail int64
	for _, k := range []string{"pages free", "pages inactive", "pages speculative", "pages purgeable"} {
		avail += pages[k]
	}
	if avail <= 0 {
		return 0, fmt.Errorf("vm_stat: reported no reclaimable pages")
	}
	return avail * pageSize, nil
}

// parseInt reads a bare integer from command output.
func parseInt(s string) (int64, error) {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("not an integer: %q", strings.TrimSpace(s))
	}
	return v, nil
}
