package check

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.starlark.net/starlark"
	"go.starlark.net/syntax"

	"github.com/iodesystems/corrallm/internal/bench/journal"
	"github.com/iodesystems/corrallm/internal/bench/task"
)

// Scripted success criteria.
//
// The fixed check kinds (file_contains, tool_called, ...) cover the common
// assertions, but a real benchmark eventually needs a predicate they cannot
// express: "the answer names at least two of these three symbols", "the JSON
// parses and its `port` field is an int", "the model called search before edit".
// Encoding those as new DSL kinds means a new kind per question forever.
//
// So a check can be a SCRIPT. Python syntax specifically, because these get
// written by (and for) people working with LLMs, and an LLM asked to write a
// predicate produces Python far more reliably than a bespoke DSL.
//
// It is Starlark, not CPython. That is a deliberate trade and worth stating:
//
//   - Starlark is Python's syntax with a deterministic, sandboxed evaluator. No
//     imports, no filesystem, no network, no clock, bounded execution. A probe
//     is data that arrives from wherever probes come from; running it must not
//     be a way to execute arbitrary code inside the serving host.
//   - CPython would need CGO or a subprocess, and would hand a probe author the
//     whole machine. For a predicate over a response string that is a bad trade.
//   - What you give up: imports and the stdlib. `re`, `json`, `math` are not
//     available. The bindings below cover what checks actually reach for; if a
//     probe genuinely needs a regex, that is a signal to add a binding rather
//     than to embed a bigger language.
//
// The script fails the check by calling fail("why"), or by leaving `ok` False.
// Anything else passes, so the common case is one line.
const scriptTimeout = 5 * time.Second

// scriptEnv is what a check script can see.
type scriptEnv struct {
	response string
	journ    []journal.Entry
	m        Metrics
	dir      string
}

// runScript evaluates a Starlark predicate and reports the outcome.
func runScript(c task.Check, env scriptEnv) Result {
	r := Result{Kind: c.Kind, Desc: "python: " + firstLine(c.Text)}

	var failMsg string
	failed := false
	fail := func(th *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
		parts := make([]string, 0, len(args))
		for _, a := range args {
			if s, ok := starlark.AsString(a); ok {
				parts = append(parts, s)
			} else {
				parts = append(parts, a.String())
			}
		}
		failed = true
		failMsg = strings.Join(parts, " ")
		// Returning an error stops evaluation, which is what a failed assertion
		// should do — the rest of the script asserted nothing about a run that
		// is already known bad.
		return starlark.None, fmt.Errorf("%s", failMsg)
	}

	toolNames := starlark.NewList(nil)
	for _, e := range env.journ {
		_ = toolNames.Append(starlark.String(e.Tool))
	}

	// read_file(path) -> str, jailed to the workspace. Checks routinely need to
	// look at what the model WROTE, not just what it said; without this a
	// scripted predicate could only assert on the reply, which is the smaller
	// half of what a coding probe produces.
	readFile := func(th *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
		var rel string
		if err := starlark.UnpackPositionalArgs("read_file", args, kw, 1, &rel); err != nil {
			return nil, err
		}
		// Jail: a probe must not read outside its own scratch workspace, no
		// matter what path it constructs.
		full := filepath.Join(env.dir, filepath.Clean("/"+rel))
		data, err := os.ReadFile(full)
		if err != nil {
			return starlark.String(""), nil // absent reads as empty, not an error
		}
		return starlark.String(string(data)), nil
	}

	predeclared := starlark.StringDict{
		"read_file": starlark.NewBuiltin("read_file", readFile),
		// The model's visible reply — the thing most predicates are about.
		"response": starlark.String(env.response),
		// Every tool name called this stage, in order.
		"tools_called": toolNames,
		"compactions":  starlark.MakeInt(env.m.Compactions),
		"fail":         starlark.NewBuiltin("fail", fail),
		// Seeded False-if-set semantics: a script that never touches `ok` passes.
		"ok": starlark.Bool(true),
	}

	th := &starlark.Thread{Name: "check"}
	// Bounded work: a probe must not be able to hang the harness with a loop.
	done := time.AfterFunc(scriptTimeout, func() { th.Cancel("check script exceeded its time budget") })
	defer done.Stop()

	// TopLevelControl: Starlark forbids if/for at module top level by default,
	// which would force every one-line predicate into a function wrapper. A
	// check script IS a top-level predicate, so allow it. GlobalReassign lets a
	// script rebind `ok`, which is the documented way to report a verdict.
	opts := &syntax.FileOptions{
		TopLevelControl: true,
		GlobalReassign:  true,
		Recursion:       false, // a probe must not be able to recurse the harness to death
	}
	globals, err := starlark.ExecFileOptions(opts, th, "check.star", c.Text, predeclared)
	switch {
	case failed:
		r.Pass = false
		r.Detail = failMsg
		return r
	case err != nil:
		// A syntax or runtime error is the PROBE's bug, not the model's. Say so,
		// or an author will spend the afternoon blaming the model.
		r.Pass = false
		r.Detail = "check script error (this is a probe bug, not a model failure): " + err.Error()
		return r
	}
	if v, present := globals["ok"]; present {
		if b, isBool := v.(starlark.Bool); isBool && !bool(b) {
			r.Pass = false
			if d, has := globals["detail"]; has {
				if s, isStr := starlark.AsString(d); isStr {
					r.Detail = s
				}
			}
			if r.Detail == "" {
				r.Detail = "script set ok = False"
			}
			return r
		}
	}
	r.Pass = true
	return r
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}
