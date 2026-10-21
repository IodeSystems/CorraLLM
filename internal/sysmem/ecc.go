package sysmem

import (
	"os"
	"path/filepath"
	"runtime"
)

// edacRoot is where Linux's EDAC subsystem lists memory controllers. A var so
// tests can point it at a fake tree.
var edacRoot = "/sys/devices/system/edac/mc"

// HostECC reports whether system RAM is ECC-protected.
//
// Linux only, and only by the one signal that does not need root: an EDAC
// memory controller (mc0, mc1, …) is registered only when the platform reports
// error-correcting memory. Every other case — no controller, an unreadable
// tree, another OS — answers false. Conservative on purpose: a wrong "no ECC"
// costs a reload, a wrong "ECC" keeps flipped weights serving.
func HostECC() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	return edacPresent(edacRoot)
}

// edacPresent reports whether root holds at least one mc* controller.
func edacPresent(root string) bool {
	matches, err := filepath.Glob(filepath.Join(root, "mc*"))
	if err != nil {
		return false
	}
	for _, m := range matches {
		if st, err := os.Stat(m); err == nil && st.IsDir() {
			return true
		}
	}
	return false
}
