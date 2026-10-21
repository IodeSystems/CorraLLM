//go:build windows

package host

import "fmt"

// Adopt is unsupported on Windows. A backend there lives in a Job Object
// created with KILL_ON_JOB_CLOSE, so it dies with the process that held the
// job; there is never a surviving group to adopt.
func Adopt(pgid int) (Handle, error) {
	return nil, fmt.Errorf("adopting a running process group is not supported on windows")
}

// ProcIdentity is unsupported on Windows, for the same reason as Adopt.
func ProcIdentity(pid int) (Identity, error) {
	return Identity{}, fmt.Errorf("process identity is not supported on windows")
}
