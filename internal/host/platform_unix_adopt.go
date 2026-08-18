//go:build unix

package host

// adoptGroup and releaseGroup exist for Windows, where the process tree is a
// Job Object that has to be created, assigned and closed. On unix the kernel
// already groups the tree via Setpgid, so both are nothing.
func adoptGroup(int) error { return nil }
func releaseGroup(int)     {}
