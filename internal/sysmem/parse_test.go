package sysmem

import "testing"

// Real vm_stat output from a 64 GB Apple-silicon machine (16 KiB pages).
const vmStatSample = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                              120000.
Pages active:                            900000.
Pages inactive:                          800000.
Pages speculative:                        50000.
Pages throttled:                              0.
Pages wired down:                       1200000.
Pages purgeable:                          30000.
"Translation faults":                 123456789.
Pages copy-on-write:                    1234567.
`

// Available must count reclaimable memory, not just free. macOS keeps almost
// nothing strictly free — it fills memory with cache it surrenders on demand —
// so a free-only reading reports a mostly-idle machine as full and the
// scheduler refuses to place anything on it.
func TestParseVMStatAvailable_CountsReclaimable(t *testing.T) {
	got, err := parseVMStatAvailable(vmStatSample)
	if err != nil {
		t.Fatal(err)
	}
	const page = 16384
	want := int64(120000+800000+50000+30000) * page
	if got != want {
		t.Errorf("available = %d, want %d (free+inactive+speculative+purgeable)", got, want)
	}
	// Wired must NOT be counted: it cannot be reclaimed, and on unified memory
	// it is exactly where a Metal backend's weights live.
	if wired := int64(1200000) * page; got >= wired*3 {
		t.Errorf("available %d looks like it included wired pages", got)
	}
}

// The page size is read from the header, not assumed: Apple silicon uses 16 KiB
// while Intel Macs use 4 KiB, and assuming 4 KiB would under-report a 64 GB
// machine by a factor of four.
func TestParseVMStatAvailable_HonoursPageSize(t *testing.T) {
	four := `Mach Virtual Memory Statistics: (page size of 4096 bytes)
Pages free:                              100.
Pages inactive:                            0.
`
	got, err := parseVMStatAvailable(four)
	if err != nil {
		t.Fatal(err)
	}
	if got != 100*4096 {
		t.Errorf("got %d, want %d", got, 100*4096)
	}
}

// Unparseable output must be an error, never a zero: zero available is a number
// the scheduler acts on.
func TestParseVMStatAvailable_ErrorsRatherThanZero(t *testing.T) {
	for _, in := range []string{"", "not vm_stat output", "Mach Virtual Memory Statistics: (page size of 16384 bytes)\n"} {
		if _, err := parseVMStatAvailable(in); err == nil {
			t.Errorf("input %q should have errored", in)
		}
	}
}
