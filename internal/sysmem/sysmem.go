// Package sysmem reads live HOST memory state (system RAM), the counterpart to
// internal/gpu's VRAM probe.
//
// Same fail-safe contract as internal/gpu: every read may fail (unsupported OS,
// unreadable /proc), and a failure means "introspection unavailable" — callers
// must fall back to showing nothing rather than reporting a zero as if it were
// measured. corrallm never depends on this for scheduling; it exists so the
// dashboard can show what the box is ACTUALLY holding next to what the
// scheduler has accounted for.
package sysmem

import "fmt"

// errUnavailable is what a platform returns when it cannot measure at all.
//
// An error, never a zero: a zero that means "could not measure" is
// indistinguishable from a zero that means "nothing left", and the second is a
// number the scheduler would happily admit against.
var errUnavailable = fmt.Errorf("system memory is not measurable on this platform")

// Stats is the host's memory snapshot in bytes.
type Stats struct {
	TotalBytes     int64
	AvailableBytes int64
	UsedBytes      int64 // total - available (i.e. what a new allocation cannot have)
}

