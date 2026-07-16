// Package gpu reads live NVIDIA VRAM state via nvidia-smi. It is the sole I/O
// seam the slot auto-tuner (internal/tune) depends on: every call can fail
// (no GPU, no driver, nvidia-smi not on PATH) and callers MUST treat that as
// "introspection unavailable" and fall back to unmodified behavior — this
// package never papers over an error with a zero value.
package gpu

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// runCmd is the process-execution seam: tests override it to feed canned CSV
// without shelling out to a real nvidia-smi.
var runCmd = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

// Stats is one GPU's memory snapshot (nvidia-smi --query-gpu).
type Stats struct {
	Name                       string
	TotalMiB, UsedMiB, FreeMiB int
}

// Probe reads the first GPU's memory stats. corrallm targets a single-GPU
// box; multi-GPU CSV rows parse fine (see parseGPUCSV) but only the first is
// reported. Any failure (nvidia-smi missing, non-zero exit, unparseable
// output) is returned as an error — the fail-safe contract for every caller.
func Probe() (Stats, error) {
	all, err := probeAll()
	if err != nil {
		return Stats{}, err
	}
	if len(all) == 0 {
		return Stats{}, fmt.Errorf("nvidia-smi: no GPUs reported")
	}
	return all[0], nil
}

func probeAll() ([]Stats, error) {
	out, err := runCmd("nvidia-smi", "--query-gpu=index,name,memory.total,memory.used,memory.free", "--format=csv,noheader,nounits")
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi: %w", err)
	}
	return parseGPUCSV(out)
}

// parseGPUCSV parses `nvidia-smi --query-gpu=index,name,memory.total,memory.used,memory.free
// --format=csv,noheader,nounits` output, e.g.
//
//	0, NVIDIA GeForce RTX 5090, 32607, 17014, 15098
func parseGPUCSV(out []byte) ([]Stats, error) {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil, nil
	}
	var stats []Stats
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) != 5 {
			return nil, fmt.Errorf("nvidia-smi: unexpected --query-gpu row %q", line)
		}
		total, err := strconv.Atoi(strings.TrimSpace(fields[2]))
		if err != nil {
			return nil, fmt.Errorf("nvidia-smi: memory.total %q: %w", fields[2], err)
		}
		used, err := strconv.Atoi(strings.TrimSpace(fields[3]))
		if err != nil {
			return nil, fmt.Errorf("nvidia-smi: memory.used %q: %w", fields[3], err)
		}
		free, err := strconv.Atoi(strings.TrimSpace(fields[4]))
		if err != nil {
			return nil, fmt.Errorf("nvidia-smi: memory.free %q: %w", fields[4], err)
		}
		stats = append(stats, Stats{
			Name:     strings.TrimSpace(fields[1]),
			TotalMiB: total, UsedMiB: used, FreeMiB: free,
		})
	}
	return stats, nil
}

// ProcVRAM reads per-process VRAM usage (pid → MiB) via `nvidia-smi
// --query-compute-apps`. corrallm spawns every model process itself, so this
// gives an EXACT per-model footprint (no guessing at "used minus everyone
// else") — the empirical input the auto-tuner measures from. An empty result
// (no compute apps running) is not an error.
func ProcVRAM() (map[int]int, error) {
	out, err := runCmd("nvidia-smi", "--query-compute-apps=pid,used_memory", "--format=csv,noheader,nounits")
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi: %w", err)
	}
	return parseProcCSV(out)
}

// parseProcCSV parses `nvidia-smi --query-compute-apps=pid,used_memory
// --format=csv,noheader,nounits` output, e.g.
//
//	1234, 8192
func parseProcCSV(out []byte) (map[int]int, error) {
	m := map[int]int{}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return m, nil
	}
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) != 2 {
			return nil, fmt.Errorf("nvidia-smi: unexpected --query-compute-apps row %q", line)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil {
			return nil, fmt.Errorf("nvidia-smi: pid %q: %w", fields[0], err)
		}
		mib, err := strconv.Atoi(strings.TrimSpace(fields[1]))
		if err != nil {
			return nil, fmt.Errorf("nvidia-smi: used_memory %q: %w", fields[1], err)
		}
		m[pid] = mib
	}
	return m, nil
}
