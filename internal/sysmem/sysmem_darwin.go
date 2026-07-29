//go:build darwin

package sysmem

import (
	"fmt"
	"os/exec"
)

// Probe reports macOS memory via sysctl and vm_stat.
//
// There is no /proc/meminfo here, and the two numbers come from different
// places: hw.memsize is the machine's total, while what is actually AVAILABLE
// has to be assembled from vm_stat's page counts.
//
// "Available" is deliberately free + inactive + purgeable + speculative rather
// than free alone. macOS keeps very little strictly free — it fills memory with
// cache it will surrender on demand — so free-only would report a healthy
// 64 GB machine as nearly full and the scheduler would refuse to place anything
// on it. This mirrors the reasoning behind Linux's MemAvailable.
func Probe() (Stats, error) {
	total, err := sysctlInt("hw.memsize")
	if err != nil {
		return Stats{}, fmt.Errorf("hw.memsize: %w", err)
	}
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return Stats{}, fmt.Errorf("vm_stat: %w", err)
	}
	avail, err := parseVMStatAvailable(string(out))
	if err != nil {
		return Stats{}, err
	}
	if avail > total {
		avail = total
	}
	return Stats{TotalBytes: total, AvailableBytes: avail, UsedBytes: total - avail}, nil
}

func sysctlInt(key string) (int64, error) {
	out, err := exec.Command("sysctl", "-n", key).Output()
	if err != nil {
		return 0, err
	}
	return parseInt(string(out))
}
