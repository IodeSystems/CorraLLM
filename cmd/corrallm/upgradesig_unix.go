//go:build unix

package main

import (
	"os"
	"syscall"
)

// upgradeSignals hand the running daemon over to a new process. SIGUSR2,
// because SIGHUP is config reload and SIGTERM is stop.
func upgradeSignals() []os.Signal { return []os.Signal{syscall.SIGUSR2} }

// sendUpgrade signals pid to hand over.
func sendUpgrade(pid int) error { return syscall.Kill(pid, syscall.SIGUSR2) }
