// Package gpu reads live GPU VRAM state for the slot auto-tuner (internal/tune).
//
// It is deliberately NOT hard-wired to NVIDIA. A Prober abstracts the vendor
// tool (nvidia-smi today; rocm-smi / Metal / etc. can be added as new Prober
// implementations), and Default selects the one for this host. Every method may
// fail (no GPU, no driver, tool not on PATH), and callers MUST treat failure as
// "introspection unavailable" and fall back to unmodified behavior — GPU
// introspection is never a hard requirement, and this package never papers over
// an error with a zero value.
package gpu

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Stats is one GPU's memory snapshot.
type Stats struct {
	Name                       string
	TotalMiB, UsedMiB, FreeMiB int
}

// Prober reads GPU memory state from a vendor backend. Implementations wrap a
// tool (nvidia-smi, rocm-smi, …); any method may return an error, which callers
// treat as "introspection unavailable".
type Prober interface {
	// Name identifies the backend (for logs/introspect output).
	Name() string
	// Probe returns the first GPU's memory snapshot.
	Probe() (Stats, error)
	// ProcVRAM maps each compute process's pid → VRAM MiB.
	ProcVRAM() (map[int]int, error)
}

// Default is the prober used for this host. NVIDIA is the default because its
// absence self-fails (nvidia-smi not on PATH → an error → fail-safe), so no
// explicit detection is needed for the common case; swap it for another backend
// (or add PATH-based detection) to support non-NVIDIA hardware.
var Default Prober = NVIDIA{}

// Probe / ProcVRAM delegate to Default so callers stay backend-agnostic.
func Probe() (Stats, error)          { return Default.Probe() }
func ProcVRAM() (map[int]int, error) { return Default.ProcVRAM() }

// GroupVRAM sums the VRAM (MiB) of every compute process in process group pgid.
// corrallm spawns each backend via `sh -c` with Setpgid, so the vendor tool
// reports the llama-server CHILD's pid, not the shell's — but the child shares
// the shell's PGID (== the spawned cmd's Pid). Attributing by the bare spawn pid
// misses entirely; we must sum the whole group. Linux /proc only; a pid that
// vanishes mid-scan is skipped, not fatal.
func GroupVRAM(pgid int) (int, error) {
	procs, err := Default.ProcVRAM()
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
// tests in other packages — which mock the vendor tool with synthetic pids that
// have no /proc entry — can substitute a deterministic resolver. Production reads
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

// runCmd is the process-execution seam: tests override it to feed canned CSV
// without shelling out to a real vendor tool.
var runCmd = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

// NVIDIA is the nvidia-smi Prober. Its absence self-fails, which is the fail-safe
// contract for every caller.
type NVIDIA struct{}

func (NVIDIA) Name() string { return "nvidia-smi" }

// Probe reads the first GPU's memory stats. corrallm targets a single-GPU box;
// multi-GPU CSV rows parse fine (see parseGPUCSV) but only the first is reported.
func (NVIDIA) Probe() (Stats, error) {
	out, err := runCmd("nvidia-smi", "--query-gpu=index,name,memory.total,memory.used,memory.free", "--format=csv,noheader,nounits")
	if err != nil {
		return Stats{}, fmt.Errorf("nvidia-smi: %w", err)
	}
	all, err := parseGPUCSV(out)
	if err != nil {
		return Stats{}, err
	}
	if len(all) == 0 {
		return Stats{}, fmt.Errorf("nvidia-smi: no GPUs reported")
	}
	return all[0], nil
}

// ProcVRAM reads per-process VRAM usage (pid → MiB). corrallm spawns every model
// process itself, so this gives an EXACT per-model footprint. An empty result
// (no compute apps running) is not an error.
func (NVIDIA) ProcVRAM() (map[int]int, error) {
	out, err := runCmd("nvidia-smi", "--query-compute-apps=pid,used_memory", "--format=csv,noheader,nounits")
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi: %w", err)
	}
	return parseProcCSV(out)
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
