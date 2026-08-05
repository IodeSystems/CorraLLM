package gpu

import (
	"errors"
	"testing"
)

// Two real cards from the box that motivated per-device identity. Note the
// ordering trap they encode: nvidia-smi lists the SLOWER 3080 first because its
// PCI bus id is lower (it sits behind the chipset), while CUDA — and so
// llama.cpp — calls the 5090 device 0. Position means opposite things in the
// two tools, which is why nothing here binds to one.
const (
	uuid3080 = "GPU-76a4c775-a47f-61b9-3a9f-9c7d5edfc544"
	uuid5090 = "GPU-ee90af07-0882-d325-182e-87137ec6d47b"

	rowsTwoGPUs = "0, " + uuid3080 + ", 00000000:03:00.0, NVIDIA GeForce RTX 3080, 10240, 4, 10236\n" +
		"1, " + uuid5090 + ", 00000000:0A:00.0, NVIDIA GeForce RTX 5090, 32607, 24, 32583\n"
)

// withRunCmd overrides the exec seam for the duration of a test.
func withRunCmd(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	orig := runCmd
	runCmd = fn
	t.Cleanup(func() { runCmd = orig })
}

// TestParseGPUCSVSingleLine: one GPU row parses into its Stats, identity included.
func TestParseGPUCSVSingleLine(t *testing.T) {
	out := []byte("1, " + uuid5090 + ", 00000000:0A:00.0, NVIDIA GeForce RTX 5090, 32607, 17014, 15098\n")
	stats, err := parseGPUCSV(out)
	if err != nil {
		t.Fatalf("parseGPUCSV: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("want 1 GPU, got %d", len(stats))
	}
	want := Stats{
		Index: 1, UUID: uuid5090, PCIBusID: "00000000:0A:00.0",
		Name: "NVIDIA GeForce RTX 5090", TotalMiB: 32607, UsedMiB: 17014, FreeMiB: 15098,
	}
	if stats[0] != want {
		t.Errorf("stats = %+v, want %+v", stats[0], want)
	}
}

// TestParseGPUCSVMultiLine: every row parses, in the tool's own order.
func TestParseGPUCSVMultiLine(t *testing.T) {
	stats, err := parseGPUCSV([]byte(rowsTwoGPUs))
	if err != nil {
		t.Fatalf("parseGPUCSV: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("want 2 GPUs, got %d", len(stats))
	}
	if stats[0].UUID != uuid3080 || stats[1].UUID != uuid5090 {
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

// TestProbeAllReportsEveryGPU: the whole point of the widened contract.
func TestProbeAllReportsEveryGPU(t *testing.T) {
	withRunCmd(t, func(name string, args ...string) ([]byte, error) {
		return []byte(rowsTwoGPUs), nil
	})
	all, err := ProbeAll()
	if err != nil {
		t.Fatalf("ProbeAll: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 GPUs, got %d", len(all))
	}
	if all[1].TotalMiB != 32607 {
		t.Errorf("second GPU = %+v", all[1])
	}
}

// TestProbeReturnsFirstGPU pins Probe's remaining, narrower contract: it is
// nvidia-smi's index 0 and nothing more. On this fixture that is the 3080 — the
// card a caller assuming "the GPU" would wrongly measure.
func TestProbeReturnsFirstGPU(t *testing.T) {
	withRunCmd(t, func(name string, args ...string) ([]byte, error) {
		return []byte(rowsTwoGPUs), nil
	})
	s, err := Probe()
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if s.UUID != uuid3080 || s.TotalMiB != 10240 {
		t.Errorf("Probe = %+v", s)
	}
}

// TestSelectByUUIDAndBusID: a pool binds to a card by either identity, and a
// short UUID prefix is enough as long as it stays unique.
func TestSelectByUUIDAndBusID(t *testing.T) {
	devs, err := parseGPUCSV([]byte(rowsTwoGPUs))
	if err != nil {
		t.Fatalf("parseGPUCSV: %v", err)
	}
	for _, tc := range []struct{ selector, wantUUID string }{
		{uuid5090, uuid5090},
		{"GPU-ee90af07", uuid5090},
		{"00000000:0A:00.0", uuid5090},
		{"0a:00.0", uuid5090}, // operator shorthand
		{"0A:00.0", uuid5090}, // case-insensitive
		{"03:00.0", uuid3080}, // the other card
		{"GPU-76a4c775", uuid3080},
	} {
		got, err := Select(devs, tc.selector)
		if err != nil {
			t.Errorf("Select(%q): %v", tc.selector, err)
			continue
		}
		if got.UUID != tc.wantUUID {
			t.Errorf("Select(%q) = %s, want %s", tc.selector, got.UUID, tc.wantUUID)
		}
	}
}

// TestSelectRejectsUnmatchedAndAmbiguous: both failures must be errors. Falling
// back to "the first card" is the bug this package exists to prevent, and
// resolving an ambiguous prefix by slice order reintroduces positional identity
// through the back door.
func TestSelectRejectsUnmatchedAndAmbiguous(t *testing.T) {
	devs, err := parseGPUCSV([]byte(rowsTwoGPUs))
	if err != nil {
		t.Fatalf("parseGPUCSV: %v", err)
	}
	if _, err := Select(devs, "GPU-deadbeef"); err == nil {
		t.Error("want error when no device matches")
	}
	if _, err := Select(devs, "GPU-"); err == nil {
		t.Error("want error when a prefix matches both devices")
	}
	if _, err := Select(devs, ""); err == nil {
		t.Error("want error on an empty selector")
	}
}

// TestSelectRejectsBareIndex: "0" must not resolve to a card. It is the one
// selector that looks reasonable and means a different GPU after a hardware
// change — which is exactly what happened here.
func TestSelectRejectsBareIndex(t *testing.T) {
	devs, err := parseGPUCSV([]byte(rowsTwoGPUs))
	if err != nil {
		t.Fatalf("parseGPUCSV: %v", err)
	}
	for _, sel := range []string{"0", "1", "gpu0", "gpu1"} {
		if _, err := Select(devs, sel); err == nil {
			t.Errorf("Select(%q) resolved; positional selectors must be rejected", sel)
		}
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

// TestParseProcByDeviceCSV: rows parse into uuid → pid → MiB, which is what
// lets a footprint be charged to the card that actually gave up the bytes.
func TestParseProcByDeviceCSV(t *testing.T) {
	out := []byte(uuid5090 + ", 1234, 8192\n" + uuid3080 + ", 5678, 4096\n")
	m, err := parseProcByDeviceCSV(out)
	if err != nil {
		t.Fatalf("parseProcByDeviceCSV: %v", err)
	}
	if len(m) != 2 || m[uuid5090][1234] != 8192 || m[uuid3080][5678] != 4096 {
		t.Errorf("m = %+v", m)
	}
}

// TestParseProcByDeviceCSVSplitProcess: one process holding memory on BOTH
// cards — llama.cpp's default split — stays attributed per card rather than
// collapsing into a single number that belongs to neither pool.
func TestParseProcByDeviceCSVSplitProcess(t *testing.T) {
	out := []byte(uuid5090 + ", 1234, 20000\n" + uuid3080 + ", 1234, 9000\n")
	m, err := parseProcByDeviceCSV(out)
	if err != nil {
		t.Fatalf("parseProcByDeviceCSV: %v", err)
	}
	if m[uuid5090][1234] != 20000 || m[uuid3080][1234] != 9000 {
		t.Errorf("m = %+v", m)
	}
}

// TestParseProcByDeviceCSVEmpty: no compute apps running is a valid, error-free
// state.
func TestParseProcByDeviceCSVEmpty(t *testing.T) {
	m, err := parseProcByDeviceCSV([]byte(""))
	if err != nil {
		t.Fatalf("parseProcByDeviceCSV: %v", err)
	}
	if len(m) != 0 {
		t.Errorf("want empty map, got %+v", m)
	}
}

// TestProcVRAMSumsAcrossDevices: the whole-host question still gets a
// whole-host answer, so a split process reports everything it holds.
func TestProcVRAMSumsAcrossDevices(t *testing.T) {
	withRunCmd(t, func(name string, args ...string) ([]byte, error) {
		return []byte(uuid5090 + ", 1234, 20000\n" + uuid3080 + ", 1234, 9000\n"), nil
	})
	m, err := ProcVRAM()
	if err != nil {
		t.Fatalf("ProcVRAM: %v", err)
	}
	if m[1234] != 29000 {
		t.Errorf("ProcVRAM = %+v, want pid 1234 at 29000", m)
	}
}

// TestProcVRAMOnIsPerCard: the same fixture, asked per card, must NOT report
// the cross-card sum — charging that to one pool over-commits it by exactly the
// memory the other card gave up.
func TestProcVRAMOnIsPerCard(t *testing.T) {
	withRunCmd(t, func(name string, args ...string) ([]byte, error) {
		return []byte(uuid5090 + ", 1234, 20000\n" + uuid3080 + ", 1234, 9000\n"), nil
	})
	on5090, err := ProcVRAMOn(uuid5090)
	if err != nil {
		t.Fatalf("ProcVRAMOn: %v", err)
	}
	if on5090[1234] != 20000 {
		t.Errorf("on 5090 = %+v, want 20000", on5090)
	}
}

// TestProcVRAMOnIdleCard: a card with no compute processes is an empty answer,
// not a failure — an idle GPU and an unreadable one must not look alike.
func TestProcVRAMOnIdleCard(t *testing.T) {
	withRunCmd(t, func(name string, args ...string) ([]byte, error) {
		return []byte(uuid5090 + ", 1234, 20000\n"), nil
	})
	m, err := ProcVRAMOn(uuid3080)
	if err != nil {
		t.Fatalf("ProcVRAMOn: %v", err)
	}
	if len(m) != 0 {
		t.Errorf("want empty map for an idle card, got %+v", m)
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
