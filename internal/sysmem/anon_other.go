//go:build !linux

package sysmem

import "fmt"

// GroupAnonBytes needs /proc. Elsewhere a group's memory is not subtracted
// from "in use", which reserves it as if another process held it: the safe
// direction.
func GroupAnonBytes(int) (int64, error) {
	return 0, fmt.Errorf("anonymous memory per process group is only read on Linux")
}
