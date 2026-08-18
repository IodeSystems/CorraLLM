//go:build windows

package host

import "unsafe"

// Small wrappers so platform_windows.go can pass struct pointers to the job
// object APIs without importing unsafe directly — the calls need a uintptr and
// a size, and keeping the conversions in one named place makes them auditable
// rather than scattered through the logic.
func unsafePointer[T any](v *T) unsafe.Pointer { return unsafe.Pointer(v) }
func unsafeSizeof[T any](v T) uintptr          { return unsafe.Sizeof(v) }
