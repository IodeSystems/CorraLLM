package gpu

import (
	"errors"
	"testing"
)

// withRunCmd overrides the exec seam for the duration of a test.
func withRunCmd(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	orig := runCmd
	runCmd = fn
	t.Cleanup(func() { runCmd = orig })
}

// TestParseGPUCSVSingleLine: one GPU row parses into its Stats.
func TestParseGPUCSVSingleLine(t *testing.T) {
	out := []byte("0, NVIDIA GeForce RTX 5090, 32607, 17014, 15098\n")
	stats, err := parseGPUCSV(out)
	if err != nil {
		t.Fatalf("parseGPUCSV: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("want 1 GPU, got %d", len(stats))
	}
	want := Stats{Name: "NVIDIA GeForce RTX 5090", TotalMiB: 32607, UsedMiB: 17014, FreeMiB: 15098}
	if stats[0] != want {
		t.Errorf("stats = %+v, want %+v", stats[0], want)
	}
}

// TestParseGPUCSVMultiLine: multiple GPU rows all parse; Probe reports only
// the first (corrallm targets a single-GPU box).
func TestParseGPUCSVMultiLine(t *testing.T) {
	out := []byte("0, RTX 5090, 32607, 17014, 15098\n1, RTX 4090, 24564, 1000, 23564\n")
	stats, err := parseGPUCSV(out)
	if err != nil {
		t.Fatalf("parseGPUCSV: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("want 2 GPUs, got %d", len(stats))
	}
	if stats[0].Name != "RTX 5090" || stats[1].Name != "RTX 4090" {
		t.Errorf("stats = %+v", stats)
	}
}

// TestParseGPUCSVEmpty: empty output parses to no GPUs, not an error.
func TestParseGPUCSVEmpty(t *testing.T) {
	stats, err := parseGPUCSV([]byte(""))
	if err != nil {
		t.Fatalf("parseGPUCSV: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("want 0 GPUs, got %d", len(stats))
	}
}

// TestParseGPUCSVMalformed: a row with the wrong field count is an error, not
// a silent partial parse.
func TestParseGPUCSVMalformed(t *testing.T) {
	if _, err := parseGPUCSV([]byte("garbage, not, enough\n")); err == nil {
		t.Error("want error on malformed row")
	}
}

// TestProbeReturnsFirstGPU verifies Probe() uses the exec seam and reports
// only the first row of a multi-GPU response.
func TestProbeReturnsFirstGPU(t *testing.T) {
	withRunCmd(t, func(name string, args ...string) ([]byte, error) {
		return []byte("0, RTX 5090, 32607, 17014, 15098\n1, RTX 4090, 24564, 1000, 23564\n"), nil
	})
	s, err := Probe()
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if s.Name != "RTX 5090" || s.FreeMiB != 15098 {
		t.Errorf("Probe = %+v", s)
	}
}

// TestProbeErrorWhenCommandFails: nvidia-smi missing/erroring surfaces as an
// error — this is the fail-safe signal callers key off of.
func TestProbeErrorWhenCommandFails(t *testing.T) {
	withRunCmd(t, func(name string, args ...string) ([]byte, error) {
		return nil, errors.New("exec: \"nvidia-smi\": executable file not found in $PATH")
	})
	if _, err := Probe(); err == nil {
		t.Error("want error when nvidia-smi is unavailable")
	}
}

// TestProbeErrorWhenNoGPUsReported: a successful call with zero rows is still
// an error (nothing to report), not a zero-value Stats.
func TestProbeErrorWhenNoGPUsReported(t *testing.T) {
	withRunCmd(t, func(name string, args ...string) ([]byte, error) {
		return []byte(""), nil
	})
	if _, err := Probe(); err == nil {
		t.Error("want error when nvidia-smi reports no GPUs")
	}
}

// TestParseProcCSV: pid/used_memory rows parse into a pid→MiB map.
func TestParseProcCSV(t *testing.T) {
	out := []byte("1234, 8192\n5678, 4096\n")
	m, err := parseProcCSV(out)
	if err != nil {
		t.Fatalf("parseProcCSV: %v", err)
	}
	if len(m) != 2 || m[1234] != 8192 || m[5678] != 4096 {
		t.Errorf("m = %+v", m)
	}
}

// TestParseProcCSVEmpty: no compute apps running is a valid, error-free state.
func TestParseProcCSVEmpty(t *testing.T) {
	m, err := parseProcCSV([]byte(""))
	if err != nil {
		t.Fatalf("parseProcCSV: %v", err)
	}
	if len(m) != 0 {
		t.Errorf("want empty map, got %+v", m)
	}
}

// TestProcVRAMErrorWhenCommandFails mirrors Probe's fail-safe contract for
// the per-process query.
func TestProcVRAMErrorWhenCommandFails(t *testing.T) {
	withRunCmd(t, func(name string, args ...string) ([]byte, error) {
		return nil, errors.New("not found")
	})
	if _, err := ProcVRAM(); err == nil {
		t.Error("want error when nvidia-smi is unavailable")
	}
}
