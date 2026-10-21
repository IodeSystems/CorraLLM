//go:build windows

package main

import "os"

// fullStopSignals is empty on Windows: there is no SIGUSR1, and backends there
// die with the agent anyway (their Job Object closes with it).
func fullStopSignals() []os.Signal { return nil }
