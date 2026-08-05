//go:build !darwin && !linux

package gpu

import "fmt"

// groupResidentMiB is unimplemented on platforms with neither /proc nor ps in a
// known shape.
//
// It exists for unified memory, where a process's resident set IS its device
// footprint. Everywhere else the two are genuinely different numbers and the
// vendor tool (ProcVRAM) is the one that answers the question being asked —
// resident memory on a discrete-GPU box would report host RAM and charge it
// against a VRAM pool.
func groupResidentMiB(int) (int, error) {
	return 0, fmt.Errorf("resident-set attribution is only meaningful on unified memory")
}

func perProcessAvailable() bool { return false }
