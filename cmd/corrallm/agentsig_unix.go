//go:build unix

package main

import (
	"os"
	"syscall"
)

// fullStopSignals asks a running agent to stop its backends and exit.
// SIGUSR1 because SIGTERM/SIGINT now mean "exit, leave the backends".
func fullStopSignals() []os.Signal { return []os.Signal{syscall.SIGUSR1} }
