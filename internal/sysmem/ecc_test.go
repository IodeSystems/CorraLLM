package sysmem

import (
	"os"
	"path/filepath"
	"testing"
)

// EDAC present means a memory controller directory; an empty tree, a missing
// tree, or a stray file do not count.
func TestEDACPresent(t *testing.T) {
	root := t.TempDir()
	if edacPresent(root) {
		t.Error("empty edac tree reported ECC")
	}
	if edacPresent(filepath.Join(root, "absent")) {
		t.Error("missing edac tree reported ECC")
	}
	if err := os.WriteFile(filepath.Join(root, "mc"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if edacPresent(root) {
		t.Error("a file named like a controller reported ECC")
	}
	if err := os.Mkdir(filepath.Join(root, "mc0"), 0o700); err != nil {
		t.Fatal(err)
	}
	if !edacPresent(root) {
		t.Error("mc0 present but not reported as ECC")
	}
}
