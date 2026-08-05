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
//
// Index/UUID/PCIBusID identify WHICH card this describes. They exist because
// "the GPU" stopped being a well-defined phrase the moment a box had two, and
// because the two orderings a host offers DISAGREE: nvidia-smi enumerates by
// PCI bus id, while CUDA (and therefore llama.cpp) defaults to FASTEST_FIRST.
// Adding a slower second card to a box renumbered nvidia-smi's index 0 onto the
// new card while llama.cpp kept the old one as CUDA0 — so an index is a fine
// label and a terrible identity. Bind a pool to a card by UUID or bus id, never
// by position.
type Stats struct {
	Index                      int
	UUID                       string
	PCIBusID                   string
	Name                       string
	TotalMiB, UsedMiB, FreeMiB int
}

// Matches reports whether selector names this device. A selector is a UUID (or
// a unique prefix of one) or a PCI bus id, matched case-insensitively; bus ids
// compare on their suffix so the operator may write "0a:00.0" for the
// "00000000:0A:00.0" the vendor tool prints.
//
// Position is deliberately NOT accepted: an index that means a different card
// after a hardware change is exactly the failure this identity exists to
// prevent.
func (s Stats) Matches(selector string) bool {
	sel := strings.ToLower(strings.TrimSpace(selector))
	if sel == "" {
		return false
	}
	if u := strings.ToLower(s.UUID); u != "" && strings.HasPrefix(u, sel) {
		return true
	}
	if b := strings.ToLower(s.PCIBusID); b != "" {
		// ":"+sel rather than a bare suffix so "0.0" cannot match every card:
		// a partial bus id must still start at a domain/bus boundary.
		if b == sel || strings.HasSuffix(b, ":"+sel) {
			return true
		}
	}
	return false
}

// Select finds the one device a selector names. It is an error for a selector
// to match none — silently falling back to "the first card" is how a budget
// ends up describing hardware that is not there.
//
// Ambiguity is an error too: a prefix short enough to match two cards would
// otherwise resolve by map/slice order, which is the positional identity this
// package refuses to offer.
func Select(devs []Stats, selector string) (Stats, error) {
	var hit Stats
	found := 0
	for _, d := range devs {
		if d.Matches(selector) {
			hit, found = d, found+1
		}
	}
	switch found {
	case 0:
		return Stats{}, fmt.Errorf("gpu: no device matches %q", selector)
	case 1:
		return hit, nil
	default:
		return Stats{}, fmt.Errorf("gpu: %q is ambiguous — it matches %d devices", selector, found)
	}
}

// Prober reads GPU memory state from a vendor backend. Implementations wrap a
// tool (nvidia-smi, rocm-smi, …); any method may return an error, which callers
// treat as "introspection unavailable".
type Prober interface {
	// Name identifies the backend (for logs/introspect output).
	Name() string
	// Probe returns the first GPU's memory snapshot.
	//
	// "First" is a position, and on a multi-GPU host it is not an identity —
	// see Stats. Anything that must know WHICH card it is talking about calls
	// ProbeAll and selects; Probe remains only for the genuinely
	// device-agnostic callers (a startup log line, a single-GPU enrollment).
	Probe() (Stats, error)
	// ProbeAll returns every GPU the backend can see, in the backend's own
	// enumeration order. Callers bind to a card by UUID or bus id, not by the
	// position in this slice.
	ProbeAll() ([]Stats, error)
	// ProcVRAM maps each compute process's pid → VRAM MiB.
	ProcVRAM() (map[int]int, error)
	// Unified reports whether Probe describes memory the GPU SHARES with the
	// host rather than a device's own.
	//
	// It cannot be inferred from GOOS by whoever consumes the reading. An Intel
	// Mac with a discrete AMD card is darwin and genuinely has two pools; an
	// Apple-silicon Mac is darwin and has one. Only the prober knows which
	// thing it just measured, so it has to say.
	//
	// A consumer that gets this wrong declares the same bytes twice — see
	// api.sizeFrom, where doing exactly that gave a 64 GiB Mac a 119 GB budget.
	Unified() bool
}

// Default is the prober used for this host. NVIDIA is the default because its
// absence self-fails (nvidia-smi not on PATH → an error → fail-safe), so no
// explicit detection is needed for the common case; swap it for another backend
// (or add PATH-based detection) to support non-NVIDIA hardware.
var Default Prober = NVIDIA{}

// PerProcessAvailable reports whether this host can attribute memory to one
// process group at all — by vendor tool, or by resident set on unified memory.
//
// It is what decides whether a model's declared size is a hint or the only
// number anyone will ever have, so it must answer for the WHOLE mechanism
// rather than for nvidia-smi alone.
func PerProcessAvailable() bool {
	if _, err := Default.ProcVRAM(); err == nil {
		return true
	}
	return Default.Unified() && perProcessAvailable()
}

// Probe / ProbeAll / ProcVRAM / Unified delegate to Default so callers stay
// backend-agnostic.
func Probe() (Stats, error)          { return Default.Probe() }
func ProbeAll() ([]Stats, error)     { return Default.ProbeAll() }
func ProcVRAM() (map[int]int, error) { return Default.ProcVRAM() }
func Unified() bool                  { return Default.Unified() }

// DeviceProcProber is the OPTIONAL half of the Prober contract: a backend that
// can say which card a process's memory came from, not just how much it holds.
//
// Optional rather than required because it is genuinely unavailable on some
// hosts — unified memory has one device and macOS will not attribute it to a
// process at all — and forcing those backends to fake a device key would put an
// invented identity into the one place identity has to be real.
type DeviceProcProber interface {
	ProcVRAMByDevice() (map[string]map[int]int, error)
}

// ProcVRAMOn returns pid → MiB for the compute processes holding memory on ONE
// card. A backend that cannot attribute per device yields an error rather than
// the whole-host figure: answering a per-card question with a cross-card sum
// would over-charge a pool by exactly the memory some other card gave up.
func ProcVRAMOn(uuid string) (map[int]int, error) {
	dp, ok := Default.(DeviceProcProber)
	if !ok {
		return nil, fmt.Errorf("gpu: %s cannot attribute memory per device", Default.Name())
	}
	byDev, err := dp.ProcVRAMByDevice()
	if err != nil {
		return nil, err
	}
	procs, ok := byDev[uuid]
	if !ok {
		// No compute processes on that card is a real, empty answer — not a
		// failure. A missing key and an idle card are the same state here.
		return map[int]int{}, nil
	}
	return procs, nil
}

// GroupVRAM sums the VRAM (MiB) of every compute process in process group pgid.
// corrallm spawns each backend via `sh -c` with Setpgid, so the vendor tool
// reports the llama-server CHILD's pid, not the shell's — but the child shares
// the shell's PGID (== the spawned cmd's Pid). Attributing by the bare spawn pid
// misses entirely; we must sum the whole group. Linux /proc only; a pid that
// vanishes mid-scan is skipped, not fatal.
func GroupVRAM(pgid int) (int, error) {
	procs, err := Default.ProcVRAM()
	if err != nil {
		// No vendor tool. On UNIFIED memory that is not the end of the story:
		// there is no separate VRAM, so the process group's resident set is the
		// device footprint, and the OS will say what it is. Treating the missing
		// vendor tool as "unmeasurable" is what made a knowable number into a
		// field humans had to type — and then never learn they had typed wrong.
		if Default.Unified() {
			return groupResidentMiB(pgid)
		}
		return 0, err
	}
	return sumGroup(procs, pgid), nil
}

// GroupVRAMOn is GroupVRAM restricted to ONE card — the memory process group
// pgid holds on the device named by uuid.
//
// It exists because a pool is a card's ledger, not the host's. Once a box has
// two GPUs, charging a whole-host process total to one pool double-counts every
// byte the process placed on the other card, and the scheduler then refuses to
// admit against capacity that is genuinely free.
//
// Unified memory has one device, so the whole-group figure IS the per-device
// one; the fallback path is shared with GroupVRAM deliberately.
func GroupVRAMOn(pgid int, uuid string) (int, error) {
	procs, err := ProcVRAMOn(uuid)
	if err != nil {
		if Default.Unified() {
			return groupResidentMiB(pgid)
		}
		return 0, err
	}
	return sumGroup(procs, pgid), nil
}

// sumGroup totals the pids in procs that belong to process group pgid. A pid
// whose group cannot be resolved (it exited mid-scan) is skipped, not fatal.
func sumGroup(procs map[int]int, pgid int) int {
	total := 0
	for pid, mib := range procs {
		if g, err := PGIDFn(pid); err == nil && g == pgid {
			total += mib
		}
	}
	return total
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

// Unified is false: a discrete card's VRAM is its own, separate from host RAM,
// so the two are independent budgets.
func (NVIDIA) Unified() bool { return false }

// ProbeAll reads every GPU's memory stats, in nvidia-smi's own order — which is
// by PCI bus id, and is NOT the order CUDA (or llama.cpp) uses. Each row carries
// its UUID and bus id so callers can name a card rather than count to it.
func (NVIDIA) ProbeAll() ([]Stats, error) {
	out, err := runCmd("nvidia-smi", "--query-gpu=index,uuid,pci.bus_id,name,memory.total,memory.used,memory.free", "--format=csv,noheader,nounits")
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi: %w", err)
	}
	return parseGPUCSV(out)
}

// Probe reads the FIRST GPU's memory stats — nvidia-smi's index 0, which on a
// multi-GPU box is whichever card happens to sit on the lowest PCI bus. Callers
// that care which card they measured must use ProbeAll and Select.
func (n NVIDIA) Probe() (Stats, error) {
	all, err := n.ProbeAll()
	if err != nil {
		return Stats{}, err
	}
	if len(all) == 0 {
		return Stats{}, fmt.Errorf("nvidia-smi: no GPUs reported")
	}
	return all[0], nil
}

// ProcVRAM reads per-process VRAM usage (pid → MiB), SUMMED across every GPU.
// corrallm spawns every model process itself, so this gives an EXACT per-model
// footprint. An empty result (no compute apps running) is not an error.
//
// Summing is right for "how much VRAM does this process hold" and wrong for
// "how much of THIS card does it hold" — a process split across two GPUs
// answers both differently. Use ProcVRAMByDevice for the second question.
func (n NVIDIA) ProcVRAM() (map[int]int, error) {
	byDev, err := n.ProcVRAMByDevice()
	if err != nil {
		return nil, err
	}
	m := map[int]int{}
	for _, procs := range byDev {
		for pid, mib := range procs {
			m[pid] += mib
		}
	}
	return m, nil
}

// ProcVRAMByDevice reads per-process VRAM usage attributed to the card holding
// it: gpu uuid → pid → MiB. This is what lets a footprint be charged to the
// pool that actually gave up the bytes.
func (NVIDIA) ProcVRAMByDevice() (map[string]map[int]int, error) {
	out, err := runCmd("nvidia-smi", "--query-compute-apps=gpu_uuid,pid,used_memory", "--format=csv,noheader,nounits")
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi: %w", err)
	}
	return parseProcByDeviceCSV(out)
}

// parseGPUCSV parses `nvidia-smi
// --query-gpu=index,uuid,pci.bus_id,name,memory.total,memory.used,memory.free
// --format=csv,noheader,nounits` output, e.g.
//
//	0, GPU-76a4c775-…, 00000000:03:00.0, NVIDIA GeForce RTX 3080, 10240, 4, 10236
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
		if len(fields) != 7 {
			return nil, fmt.Errorf("nvidia-smi: unexpected --query-gpu row %q", line)
		}
		nums := make([]int, 0, 4)
		for _, f := range []struct {
			label string
			idx   int
		}{{"index", 0}, {"memory.total", 4}, {"memory.used", 5}, {"memory.free", 6}} {
			n, err := strconv.Atoi(strings.TrimSpace(fields[f.idx]))
			if err != nil {
				return nil, fmt.Errorf("nvidia-smi: %s %q: %w", f.label, fields[f.idx], err)
			}
			nums = append(nums, n)
		}
		stats = append(stats, Stats{
			Index:    nums[0],
			UUID:     strings.TrimSpace(fields[1]),
			PCIBusID: strings.TrimSpace(fields[2]),
			Name:     strings.TrimSpace(fields[3]),
			TotalMiB: nums[1], UsedMiB: nums[2], FreeMiB: nums[3],
		})
	}
	return stats, nil
}

// parseProcByDeviceCSV parses `nvidia-smi --query-compute-apps=gpu_uuid,pid,used_memory
// --format=csv,noheader,nounits` output, e.g.
//
//	GPU-76a4c775-…, 1234, 8192
func parseProcByDeviceCSV(out []byte) (map[string]map[int]int, error) {
	m := map[string]map[int]int{}
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
		if len(fields) != 3 {
			return nil, fmt.Errorf("nvidia-smi: unexpected --query-compute-apps row %q", line)
		}
		uuid := strings.TrimSpace(fields[0])
		pid, err := strconv.Atoi(strings.TrimSpace(fields[1]))
		if err != nil {
			return nil, fmt.Errorf("nvidia-smi: pid %q: %w", fields[1], err)
		}
		mib, err := strconv.Atoi(strings.TrimSpace(fields[2]))
		if err != nil {
			return nil, fmt.Errorf("nvidia-smi: used_memory %q: %w", fields[2], err)
		}
		if m[uuid] == nil {
			m[uuid] = map[int]int{}
		}
		m[uuid][pid] += mib
	}
	return m, nil
}
