package gpu

import "testing"

const vmStat = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                              120000.
Pages wired down:                       1200000.
`

func TestParseWiredBytes(t *testing.T) {
	got, err := parseWiredBytes(vmStat)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(1200000) * 16384; got != want {
		t.Errorf("wired = %d, want %d", got, want)
	}
	if _, err := parseWiredBytes("no wired line here"); err == nil {
		t.Error("missing wired count must error, not report zero")
	}
}

// iogpu.wired_limit_mb reads 0 when unset, meaning "system default" — a
// FRACTION of physical memory, not none. Treating the literal 0 as the limit
// would declare a 64 GB machine as having no GPU memory and nothing would ever
// be placed on it.
func TestDefaultWiredLimit_IsAFractionNotZero(t *testing.T) {
	const gb = int64(1) << 30
	for _, tc := range []struct{ mem, min int64 }{
		{64 * gb, 40 * gb},
		{16 * gb, 8 * gb},
	} {
		got := defaultWiredLimitBytes(tc.mem)
		if got <= 0 {
			t.Fatalf("mem=%d gave a limit of %d", tc.mem, got)
		}
		if got < tc.min || got >= tc.mem {
			t.Errorf("mem=%dGB → limit %dGB; want a sane fraction below total", tc.mem/gb, got/gb)
		}
	}
}

// Per-process attribution is impossible on macOS. It must ERROR: an empty map
// reads as "this process uses nothing", and a footprint of zero is a number the
// scheduler places another model against.
func TestApple_ProcVRAMIsAnErrorNotAnEmptyMap(t *testing.T) {
	m, err := Apple{}.ProcVRAM()
	if err == nil {
		t.Fatal("want an error, so callers treat it as unmeasurable")
	}
	if m != nil {
		t.Error("want a nil map alongside the error")
	}
}
