package run

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// copyDir recursively copies the contents of src into dst (which must exist),
// preserving file modes. Symlinks are skipped.
// ProbeGoMod is what a fixture's go.mod is called AT REST, and copyDir renames
// it back when seeding a workspace.
//
// The indirection is forced by `go:embed`, which refuses to descend into a
// directory containing a go.mod ("cannot embed directory X: in different
// module") — verified, and not something a pattern can work around. Since the
// built-in probe library is embedded so the binary works from any directory,
// fixtures cannot carry a literal go.mod on disk.
//
// The fixture directory is also `_`-prefixed, which makes the go tool skip it
// for package discovery. That is deliberate and independent: several fixtures
// contain code that is broken ON PURPOSE (a failing test is the point of
// fix-failing-test), and without the underscore `go build ./...` on this repo
// would try to compile it.
//
// Both renames are undone here, at the single point where a workspace is
// seeded, so the model always sees an ordinary Go module.
const ProbeGoMod = "gomod.probe"

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if filepath.Base(rel) == ProbeGoMod {
			rel = filepath.Join(filepath.Dir(rel), "go.mod")
		}
		target := filepath.Join(dst, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case info.Mode().IsRegular():
			return copyFile(path, target, info.Mode().Perm())
		default:
			return nil // skip symlinks, devices, etc.
		}
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// gitInit initializes a git repo in dir and commits the seed, best-effort.
// A checkout that lacks git (or the commit failing) is non-fatal — the
// workspace still works for filesystem + run checks.
func gitInit(dir string) {
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=llm-bench", "GIT_AUTHOR_EMAIL=llm-bench@localhost",
			"GIT_COMMITTER_NAME=llm-bench", "GIT_COMMITTER_EMAIL=llm-bench@localhost")
		_ = cmd.Run()
	}
	run("init", "-q")
	run("add", "-A")
	run("commit", "-q", "-m", "llm-bench: seed workspace")
}
