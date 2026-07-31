package ui

import (
	"io/fs"
	"testing"
)

// //go:embed is a COMPILE error when its pattern matches nothing, so this test
// exists mostly to state the contract: the ui/dist directory must always hold
// at least the tracked .gitkeep, or `go build` breaks on a clean checkout.
func TestDistFSOpens(t *testing.T) {
	entries, err := fs.ReadDir(DistFS(), ".")
	if err != nil {
		t.Fatalf("read embedded dist: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("embedded dist is empty; the //go:embed pattern matched nothing")
	}
}

// A release must embed a real UI, not the placeholder. Skipped rather than
// failed when only the placeholder is present: a bare `go build` legitimately
// has no UI, and `make dist` runs pnpm first.
func TestDistFSCarriesBuiltUI(t *testing.T) {
	f, err := DistFS().Open("index.html")
	if err != nil {
		t.Skip("ui/dist holds only the placeholder; run `make ui-build` to embed the real dashboard")
	}
	_ = f.Close()
}
