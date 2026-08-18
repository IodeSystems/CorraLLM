//go:build unix

// These tests parse /proc/meminfo, which only exists on Linux. The Windows
// probe calls GlobalMemoryStatusEx and has no fixture to parse.
package sysmem

import (
	"os"
	"path/filepath"
	"testing"
)

func writeMeminfo(t *testing.T, body string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "meminfo")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	old := meminfoPath
	meminfoPath = p
	t.Cleanup(func() { meminfoPath = old })
}

func TestProbeReadsTotalAndAvailable(t *testing.T) {
	writeMeminfo(t, `MemTotal:       65780392 kB
MemFree:         1234567 kB
MemAvailable:   40000000 kB
Buffers:          123456 kB
`)
	s, err := Probe()
	if err != nil {
		t.Fatal(err)
	}
	if s.TotalBytes != 65780392*1024 {
		t.Errorf("total = %d", s.TotalBytes)
	}
	if s.AvailableBytes != 40000000*1024 {
		t.Errorf("available = %d", s.AvailableBytes)
	}
	// Used is derived from AVAILABLE, not MemFree — reclaimable page cache is
	// not "in use" for the purpose of fitting another model.
	if want := int64(65780392-40000000) * 1024; s.UsedBytes != want {
		t.Errorf("used = %d, want %d", s.UsedBytes, want)
	}
}

// A probe that cannot read the file must FAIL rather than report a zeroed
// machine — a 0/0 bar would read as "no memory at all", which is a lie.
func TestProbeErrorsWhenUnreadable(t *testing.T) {
	old := meminfoPath
	meminfoPath = filepath.Join(t.TempDir(), "nope")
	t.Cleanup(func() { meminfoPath = old })
	if _, err := Probe(); err == nil {
		t.Fatal("expected an error for a missing meminfo")
	}
}

func TestProbeErrorsWithoutMemTotal(t *testing.T) {
	writeMeminfo(t, "MemFree: 100 kB\n")
	if _, err := Probe(); err == nil {
		t.Fatal("expected an error when MemTotal is absent")
	}
}
