//go:build linux

package sysmem

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGroupAnonBytesSumsTheGroup(t *testing.T) {
	root := t.TempDir()
	write := func(pid, stat, status string) {
		t.Helper()
		dir := filepath.Join(root, pid)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{"stat": stat, "status": status} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	// comm with a space and a paren: fields are counted after the last ')'.
	write("100", "100 (sh) S 1 100 100 0", "Name:\tsh\nRssAnon:\t    1024 kB\nRssFile:\t 9999 kB\n")
	write("101", "101 (llama (server)) S 100 100 100 0", "RssAnon:\t 2048 kB\n")
	write("200", "200 (other) S 1 200 200 0", "RssAnon:\t 4096 kB\n")
	if err := os.MkdirAll(filepath.Join(root, "self"), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = prev })

	got, err := GroupAnonBytes(100)
	if err != nil || got != 3072*1024 {
		t.Fatalf("GroupAnonBytes(100) = %d, %v; want %d", got, err, 3072*1024)
	}
	if _, err := GroupAnonBytes(300); err == nil {
		t.Fatal("an empty group must be an error, not 0")
	}
}
