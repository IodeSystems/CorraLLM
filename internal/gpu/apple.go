package gpu

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Apple probes unified memory on Apple silicon.
//
// The shape of the problem is different from a discrete card. There is no
// separate VRAM: the GPU allocates from the same pool as everything else, so
// "how much memory does the GPU have" means "how much is the GPU ALLOWED to
// wire down", which macOS exposes as iogpu.wired_limit_mb.
//
// ProcVRAM is not implementable here, and that is a supported state rather than
// a gap to paper over — see its comment.
type Apple struct{}

func (Apple) Name() string { return "apple" }

// Unified is true, and it is the defining fact about this prober: Probe's
// "total" is a slice of the SAME memory sysmem.Probe reports, not a second
// device. Anything that adds the two together has counted the machine twice.
func (Apple) Unified() bool { return true }

// Probe reports the unified-memory budget as if it were a device.
//
// Total is the wired limit — what the GPU may hold. Used is WIRED memory, which
// is where a Metal backend's weights actually live, so it tracks what matters
// even though it also counts non-GPU wired pages. Both are approximations and
// are documented as such; corrallm treats capacity as a declared budget and
// uses a probe only to auto-fill and drift-guard, so an approximation here
// cannot make a scheduling decision wrong on its own.
func (a Apple) Probe() (Stats, error) {
	memBytes, err := sysctlInt("hw.memsize")
	if err != nil {
		return Stats{}, fmt.Errorf("hw.memsize: %w", err)
	}
	limitMiB, err := wiredLimitMiB(memBytes)
	if err != nil {
		return Stats{}, err
	}
	usedMiB, err := wiredMiB()
	if err != nil {
		// The total is still worth reporting: it is the number that sizes a
		// pool. Usage is what drift-guarding wants, and its absence degrades
		// that rather than the sizing.
		return Stats{Name: a.deviceName(), TotalMiB: limitMiB}, nil
	}
	free := limitMiB - usedMiB
	if free < 0 {
		free = 0
	}
	return Stats{Name: a.deviceName(), TotalMiB: limitMiB, UsedMiB: usedMiB, FreeMiB: free}, nil
}

// ProcVRAM is unavailable on macOS.
//
// There is no equivalent of `nvidia-smi --query-compute-apps`: the OS does not
// attribute unified memory to a process in a way a supervisor can read without
// privileges. Callers treat the error as "measurement unavailable" and fall
// back — which for corrallm means a model's declared ramUsage stops being
// advisory and becomes the only number there is.
//
// Returning an error rather than an empty map is the whole point: an empty map
// reads as "this process uses nothing", and a footprint of zero is a number the
// scheduler would happily place another model against.
func (Apple) ProcVRAM() (map[int]int, error) {
	return nil, fmt.Errorf("apple: per-process GPU memory is not readable on macOS")
}

// deviceName identifies the chip, e.g. "Apple M3 Max".
//
// It must be STABLE and distinct per machine class: the slot auto-tuner keys
// its cached profiles by device name, so two different machines sharing a name
// would silently share tuning measurements taken on the other one.
func (Apple) deviceName() string {
	if out, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			return s
		}
	}
	return "Apple Silicon"
}

// wiredLimitMiB is how much the GPU may wire down.
//
// iogpu.wired_limit_mb is 0 when unset, meaning "use the system default" —
// which is a FRACTION of physical memory, not zero. Treating the literal 0 as
// the limit would declare a 64 GB machine as having no GPU memory at all and
// nothing would ever be placed on it.
func wiredLimitMiB(memBytes int64) (int, error) {
	if v, err := sysctlInt("iogpu.wired_limit_mb"); err == nil && v > 0 {
		return int(v), nil
	}
	if memBytes <= 0 {
		return 0, fmt.Errorf("cannot determine a wired limit without hw.memsize")
	}
	return int(defaultWiredLimitBytes(memBytes) / (1024 * 1024)), nil
}

func wiredMiB() (int, error) {
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return 0, err
	}
	b, err := parseWiredBytes(string(out))
	if err != nil {
		return 0, err
	}
	return int(b / (1024 * 1024)), nil
}

func sysctlInt(key string) (int64, error) {
	out, err := exec.Command("sysctl", "-n", key).Output()
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: not an integer: %q", key, strings.TrimSpace(string(out)))
	}
	return v, nil
}
