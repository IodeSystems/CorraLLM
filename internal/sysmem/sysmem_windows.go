//go:build windows

package sysmem

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// memoryStatusEx mirrors MEMORYSTATUSEX. x/sys/windows does not wrap
// GlobalMemoryStatusEx, so the struct and the call are declared here; field
// order and widths are the documented layout, and dwLength must be set to the
// struct size or the call rejects it.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

var (
	kernel32               = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalMemoryStatus = kernel32.NewProc("GlobalMemoryStatusEx")
)

// Probe reads this machine's physical memory.
//
// The Windows counterpart to /proc/meminfo, under the same fail-safe contract as
// the rest of the package: an error means "introspection unavailable", and a
// caller shows nothing rather than reporting a zero as though it were measured.
//
// AvailPhys is what is reported as available — physical memory obtainable
// without paging — because that is the closest analogue to Linux's
// MemAvailable, which the other platforms report. Comparing it against anything
// else would make the same dashboard column mean two things.
//
// UNVERIFIED: written without a Windows machine to run it on.
func Probe() (Stats, error) {
	m := memoryStatusEx{}
	m.Length = uint32(unsafe.Sizeof(m))
	r, _, err := procGlobalMemoryStatus.Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 {
		return Stats{}, fmt.Errorf("GlobalMemoryStatusEx: %w", err)
	}
	total := int64(m.TotalPhys)
	avail := int64(m.AvailPhys)
	return Stats{
		TotalBytes:     total,
		AvailableBytes: avail,
		UsedBytes:      total - avail,
	}, nil
}
