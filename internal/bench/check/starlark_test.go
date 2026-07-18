package check

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/bench/journal"
	"github.com/iodesystems/corrallm/internal/bench/task"
)

func runPy(t *testing.T, script, response string, journ []journal.Entry, ws string) Result {
	t.Helper()
	if ws == "" {
		ws = t.TempDir()
	}
	return Evaluate(context.Background(), task.Check{Kind: "python", Text: script},
		ws, journ, Metrics{Response: response})
}

// The point of scripted checks: predicates the fixed kinds cannot express.
func TestPython_PassAndFail(t *testing.T) {
	if r := runPy(t, `if "red" not in response.lower(): fail("expected red")`, "A RED circle", nil, ""); !r.Pass {
		t.Errorf("should pass: %+v", r)
	}
	r := runPy(t, `if "red" not in response.lower(): fail("expected red, got: " + response)`, "a blue square", nil, "")
	if r.Pass {
		t.Error("should fail")
	}
	if !strings.Contains(r.Detail, "blue square") {
		t.Errorf("fail() message should reach the detail: %q", r.Detail)
	}
}

// A script that asserts nothing passes — the common case is one line, and
// requiring ceremony to express "this is fine" invites copy-paste noise.
func TestPython_SilentScriptPasses(t *testing.T) {
	if r := runPy(t, `x = 1 + 1`, "anything", nil, ""); !r.Pass {
		t.Errorf("a script that never fails should pass: %+v", r)
	}
}

// `ok = False` is the alternative to fail(), for scripts that compute a verdict.
func TestPython_OkVariable(t *testing.T) {
	r := runPy(t, `
n = len([w for w in response.split() if w.startswith("a")])
ok = n >= 2
detail = "found %d a-words" % n
`, "apple banana avocado", nil, "")
	if !r.Pass {
		t.Errorf("2 a-words should pass: %+v", r)
	}
	r = runPy(t, `
n = len([w for w in response.split() if w.startswith("z")])
ok = n >= 2
detail = "found %d z-words" % n
`, "apple banana", nil, "")
	if r.Pass {
		t.Error("0 z-words should fail")
	}
	if !strings.Contains(r.Detail, "found 0") {
		t.Errorf("detail variable should surface: %q", r.Detail)
	}
}

// Predicates over tool use — the other half of what a probe observes.
func TestPython_ToolsCalled(t *testing.T) {
	j := []journal.Entry{{Tool: "search"}, {Tool: "write_file"}}
	r := runPy(t, `
if "search" not in tools_called: fail("model never searched")
if tools_called.index("search") > tools_called.index("write_file"):
    fail("searched only after writing")
`, "", j, "")
	if !r.Pass {
		t.Errorf("should pass: %+v", r)
	}
	r = runPy(t, `if "search" not in tools_called: fail("model never searched")`, "",
		[]journal.Entry{{Tool: "write_file"}}, "")
	if r.Pass || !strings.Contains(r.Detail, "never searched") {
		t.Errorf("should fail with the message: %+v", r)
	}
}

// Checks must be able to inspect what the model WROTE, not only what it said.
func TestPython_ReadFile(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "answers.txt"), []byte("port 7443\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := runPy(t, `if "7443" not in read_file("answers.txt"): fail("wrong port")`, "", nil, ws); !r.Pass {
		t.Errorf("should pass: %+v", r)
	}
	// A missing file reads as empty rather than exploding, so a probe can say
	// "if it wasn't written, that's the failure" in its own words.
	if r := runPy(t, `if read_file("nope.txt") != "": fail("expected empty")`, "", nil, ws); !r.Pass {
		t.Errorf("absent file should read empty: %+v", r)
	}
}

// A probe is data. It must not be able to read outside its scratch workspace.
func TestPython_ReadFileIsJailed(t *testing.T) {
	ws := t.TempDir()
	secret := filepath.Join(filepath.Dir(ws), "secret.txt")
	_ = os.WriteFile(secret, []byte("TOPSECRET"), 0o644)
	defer os.Remove(secret)
	r := runPy(t, `
leaked = read_file("../secret.txt")
if "TOPSECRET" in leaked: fail("escaped the workspace")
`, "", nil, ws)
	if !r.Pass {
		t.Errorf("path traversal should not reach outside the workspace: %+v", r)
	}
}

// The sandbox is the reason this is Starlark and not CPython: a probe arriving
// from anywhere must not be able to touch the host.
func TestPython_NoImports(t *testing.T) {
	r := runPy(t, `import os`, "", nil, "")
	if r.Pass {
		t.Error("imports must not be available")
	}
	if !strings.Contains(r.Detail, "probe bug") {
		t.Errorf("a script error is the PROBE's bug and should say so: %q", r.Detail)
	}
}

// A broken script must not read as a model failure — otherwise an author blames
// the model for their own typo.
func TestPython_ScriptErrorIsLabelledAsProbeBug(t *testing.T) {
	r := runPy(t, `this is not valid syntax(((`, "", nil, "")
	if r.Pass {
		t.Error("a syntax error must fail the check")
	}
	if !strings.Contains(r.Detail, "probe bug, not a model failure") {
		t.Errorf("detail must attribute the error correctly: %q", r.Detail)
	}
}
