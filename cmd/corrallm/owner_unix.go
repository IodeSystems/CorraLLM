//go:build unix

package main

import (
	"os"
	"syscall"

	"github.com/iodesystems/corrallm/internal/host"
)

// selfIdentity is this process's pid and start stamp, written on every
// in-flight marker so a later sweep can tell whether the writer still runs.
func selfIdentity() (int, string) {
	pid := os.Getpid()
	id, err := host.ProcIdentity(pid)
	if err != nil {
		return pid, ""
	}
	return pid, id.Start
}

// processAlive reports whether the process that wrote a marker still runs:
// the pid exists AND has the same start stamp, so a pid reused by an
// unrelated process does not keep a dead writer's markers standing. With no
// stamp to compare, the pid alone decides.
func processAlive(pid int64, start string) bool {
	if pid <= 0 {
		return false
	}
	if start == "" {
		return syscall.Kill(int(pid), 0) == nil
	}
	id, err := host.ProcIdentity(int(pid))
	return err == nil && id.Start == start
}
