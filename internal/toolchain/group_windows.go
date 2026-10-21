//go:build windows

package toolchain

import "os/exec"

// killWithGroup is the default on Windows: cancelling kills the direct child
// only. Recipes are bash scripts and are not run on Windows today.
func killWithGroup(*exec.Cmd) {}
