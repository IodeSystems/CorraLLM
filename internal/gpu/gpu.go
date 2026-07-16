// Package gpu reads live NVIDIA VRAM state via nvidia-smi. It is the sole I/O
// seam the slot auto-tuner (internal/tune) depends on: every call can fail
// (no GPU, no driver, nvidia-smi not on PATH) and callers MUST treat that as
// "introspection unavailable" and fall back to unmodified behavior — this
// package never papers over an error with a zero value.
package gpu

import (
	"fmt"
	"os"
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

// GroupVRAM sums the VRAM (MiB) of every compute process in process group pgid.
// corrallm spawns each backend via `sh -c` with Setpgid, so nvidia-smi reports
// the llama-server CHILD's pid, not the shell's — but the child shares the
// shell's PGID (== the spawned cmd's Pid). Attributing by the bare spawn pid
// misses entirely; we must sum the whole group. Linux /proc only; a pid that
// vanishes mid-scan is skipped, not fatal.
func GroupVRAM(pgid int) (int, error) {
	procs, err := ProcVRAM()
	if err != nil {
		return 0, err
	}
	total := 0
	for pid, mib := range procs {
		if g, err := PGIDFn(pid); err == nil && g == pgid {
			total += mib
		}
	}
	return total, nil
}

// PGIDFn resolves a pid's process-group id. It is a package var (like runCmd) so
// tests in other packages — which mock nvidia-smi with synthetic pids that have
// no /proc entry — can substitute a deterministic resolver. Production reads
// /proc/<pid>/stat.
var PGIDFn = procPGID

// procPGID returns the process-group id of pid from /proc/<pid>/stat. The comm
// field is parenthesized and may itself contain spaces/parens, so scan past the
// LAST ')' and take pgrp = the 3rd field after it (state, ppid, pgrp).
func procPGID(pid int) (int, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+1 >= len(s) {
		return 0, fmt.Errorf("proc %d: malformed stat", pid)
	}
	fields := strings.Fields(s[i+1:])
	if len(fields) < 3 {
		return 0, fmt.Errorf("proc %d: short stat", pid)
	}
	return strconv.Atoi(fields[2]) // state, ppid, pgrp
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
