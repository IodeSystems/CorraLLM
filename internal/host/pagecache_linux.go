//go:build linux

package host

import (
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// dropPageCache asks the kernel to drop path's cached pages. evicted is the
// cached bytes before minus after, measured with mincore; measured is false
// when that measurement failed (the drop was still asked for).
func dropPageCache(path string) (evicted int64, measured bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return 0, false, err
	}
	before, errBefore := cachedBytes(f, st.Size())
	if err := unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED); err != nil {
		return 0, false, err
	}
	after, errAfter := cachedBytes(f, st.Size())
	if errBefore != nil || errAfter != nil {
		return 0, false, nil
	}
	return before - after, true, nil
}

// cachedBytes is how much of f is in the page cache: mincore over a read-only
// mapping of the whole file. Mapping does not read the file; only the
// residency vector is filled.
func cachedBytes(f *os.File, size int64) (int64, error) {
	if size <= 0 {
		return 0, nil
	}
	b, err := unix.Mmap(int(f.Fd()), 0, int(size), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return 0, err
	}
	defer func() { _ = unix.Munmap(b) }()
	page := int64(os.Getpagesize())
	vec := make([]byte, (size+page-1)/page)
	// x/sys has no Mincore wrapper on Linux; the syscall is stable.
	if _, _, errno := unix.Syscall(unix.SYS_MINCORE, uintptr(unsafe.Pointer(&b[0])),
		uintptr(len(b)), uintptr(unsafe.Pointer(&vec[0]))); errno != 0 {
		return 0, errno
	}
	var n int64
	for _, v := range vec {
		if v&1 != 0 {
			n += page
		}
	}
	return min(n, size), nil
}
