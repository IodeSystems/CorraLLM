package task

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// onePixelPNG is a minimal valid PNG so image extraction has a real file to
// read (the parser inlines bytes, so a non-existent path must fail loudly).
var onePixelPNG, _ = base64.StdEncoding.DecodeString(
	"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")

func writeProbe(t *testing.T, body string, files map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ProbeFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, b := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const visionProbe = `---
name: vision-red-circle
class: capability
requires: { modality: image }
---

# Vision: a solid red circle

This prose documents why the probe exists. It must never be sent to the model.

## Prompt

What shape and colour is in this image?

![a red circle](fixture/circle.png)

## Checks

- response_contains: red
- response_contains: circle
`

func TestLoadMarkdown_VisionProbe(t *testing.T) {
	dir := writeProbe(t, visionProbe, map[string][]byte{"fixture/circle.png": onePixelPNG})
	tk, err := LoadMarkdown(dir)
	if err != nil {
		t.Fatalf("LoadMarkdown: %v", err)
	}
	if tk.Name != "vision-red-circle" || tk.Class != "capability" {
		t.Errorf("frontmatter not applied: %+v", tk)
	}
	if tk.Requires.Modality != "image" {
		t.Errorf("requires.modality = %q", tk.Requires.Modality)
	}
	if len(tk.Stages) != 1 {
		t.Fatalf("want 1 stage, got %d", len(tk.Stages))
	}
	st := tk.Stages[0]
	if len(st.Checks) != 2 || st.Checks[0].Kind != "response_contains" || st.Checks[0].Text != "red" {
		t.Errorf("checks not decoded by the shared Check decoder: %+v", st.Checks)
	}
	// text part + one image part
	if len(st.Parts) != 2 || st.Parts[0].Type != "text" || st.Parts[1].Type != "image_url" {
		t.Fatalf("parts = %+v", st.Parts)
	}
	if !strings.HasPrefix(st.Parts[1].ImageURL.URL, "data:image/png;base64,") {
		t.Errorf("image not inlined as a data URI: %.40q", st.Parts[1].ImageURL.URL)
	}
	// The image syntax must be stripped from the prompt text — leaving
	// "![a red circle](fixture/circle.png)" in the prose would tell a
	// text-only reading of the transcript that the model was shown a path.
	if strings.Contains(st.Prompt, "![") || strings.Contains(st.Prompt, ".png") {
		t.Errorf("image markdown left in prompt: %q", st.Prompt)
	}
	if !strings.Contains(st.Prompt, "What shape") {
		t.Errorf("prompt text lost: %q", st.Prompt)
	}
	// Prose above the first ## is documentation, NOT a prompt. Sending it
	// would turn a comment into an instruction.
	if strings.Contains(st.Prompt, "must never be sent") {
		t.Errorf("description prose leaked into the prompt: %q", st.Prompt)
	}
	if !strings.Contains(tk.Description, "why the probe exists") {
		t.Errorf("description = %q", tk.Description)
	}
	if strings.Contains(tk.Description, "# Vision") {
		t.Errorf("H1 title should be stripped from the description: %q", tk.Description)
	}
}

// The whole point of the markdown format is that it is a second SYNTAX, not a
// second engine. A markdown probe and the equivalent task.yaml must produce
// identical Tasks apart from the format-only fields.
func TestLoadMarkdown_EquivalentToYAML(t *testing.T) {
	md := writeProbe(t, `---
name: twin
class: tooluse
workspace: fixture/
limits: { maxTurnsPerStage: 5, maxToolCallsPerStage: 9 }
---

## Prompt

Do the thing.

## Checks

- cmd_ok: "true"
- tool_called: { name: run, min: 2 }
`, map[string][]byte{"fixture/.keep": {}})

	yml := t.TempDir()
	if err := os.MkdirAll(filepath.Join(yml, "fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(yml, "task.yaml"), []byte(`name: twin
class: tooluse
workspace: fixture/
limits: { maxTurnsPerStage: 5, maxToolCallsPerStage: 9 }
stages:
  - prompt: |-
      Do the thing.
    checks:
      - cmd_ok: "true"
      - tool_called: { name: run, min: 2 }
`), 0o644); err != nil {
		t.Fatal(err)
	}

	a, err := LoadMarkdown(md)
	if err != nil {
		t.Fatalf("markdown: %v", err)
	}
	b, err := Load(yml)
	if err != nil {
		t.Fatalf("yaml: %v", err)
	}
	if a.Name != b.Name || a.Class != b.Class || a.Limits != b.Limits {
		t.Errorf("header mismatch:\n md=%+v\nyml=%+v", a, b)
	}
	if len(a.Stages) != len(b.Stages) {
		t.Fatalf("stage count %d vs %d", len(a.Stages), len(b.Stages))
	}
	if a.Stages[0].Prompt != b.Stages[0].Prompt {
		t.Errorf("prompt mismatch: %q vs %q", a.Stages[0].Prompt, b.Stages[0].Prompt)
	}
	if len(a.Stages[0].Checks) != len(b.Stages[0].Checks) {
		t.Fatalf("check count %d vs %d", len(a.Stages[0].Checks), len(b.Stages[0].Checks))
	}
	for i := range a.Stages[0].Checks {
		// Compare by value: Check.Min/Max are *int, so struct equality would
		// compare POINTERS and never match across two loads.
		if !sameCheck(a.Stages[0].Checks[i], b.Stages[0].Checks[i]) {
			t.Errorf("check[%d]: %+v vs %+v", i, a.Stages[0].Checks[i], b.Stages[0].Checks[i])
		}
	}
	// Defaults must come from the SAME code path or the formats drift.
	if a.Limits.MaxTurnsPerStage != b.Limits.MaxTurnsPerStage {
		t.Errorf("defaults drifted: %d vs %d", a.Limits.MaxTurnsPerStage, b.Limits.MaxTurnsPerStage)
	}
}

func TestLoadMarkdown_MultiStage(t *testing.T) {
	dir := writeProbe(t, `---
name: multi
class: tooluse
---

## Prompt

First.

## Checks

- cmd_ok: "true"

## Prompt

Second.

## Options

forceCompact: true

## Checks

- cmd_ok: "false"
`, nil)
	tk, err := LoadMarkdown(dir)
	if err != nil {
		t.Fatalf("LoadMarkdown: %v", err)
	}
	if len(tk.Stages) != 2 {
		t.Fatalf("want 2 stages, got %d", len(tk.Stages))
	}
	if tk.Stages[0].Prompt != "First." || tk.Stages[1].Prompt != "Second." {
		t.Errorf("stage prompts: %q / %q", tk.Stages[0].Prompt, tk.Stages[1].Prompt)
	}
	if !tk.Stages[1].ForceCompact {
		t.Error("## Options forceCompact not applied to its stage")
	}
	if tk.Stages[0].ForceCompact {
		t.Error("## Options leaked onto the previous stage")
	}
	// workspace is optional: a capability probe has no fixture.
	if tk.Workspace != "" {
		t.Errorf("workspace should be empty, got %q", tk.Workspace)
	}
}

func TestLoadMarkdown_Errors(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"no frontmatter", "## Prompt\n\nhi\n", "frontmatter"},
		{"no prompt section", "---\nname: x\nclass: tooluse\n---\n\njust prose\n", "## Prompt"},
		{"checks before prompt", "---\nname: x\nclass: tooluse\n---\n\n## Checks\n\n- cmd_ok: \"true\"\n", "before any"},
		{"empty checks list", "---\nname: x\nclass: tooluse\n---\n\n## Prompt\n\nhi\n\n## Checks\n\nnothing here\n", "no `- ` list items"},
		{"bad class", "---\nname: x\nclass: nonsense\n---\n\n## Prompt\n\nhi\n", "class"},
		{"missing image file", "---\nname: x\nclass: capability\n---\n\n## Prompt\n\nhi\n\n![](gone.png)\n", "gone.png"},
		{"unsupported image type", "---\nname: x\nclass: capability\n---\n\n## Prompt\n\nhi\n\n![](f.bmp)\n", "f.bmp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeProbe(t, tc.body, nil)
			_, err := LoadMarkdown(dir)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should mention %q", err, tc.want)
			}
		})
	}
}

// Unknown ## sections are prose, not errors — a probe doubles as documentation
// and "## Why" or "## Notes" must not break it.
func TestLoadMarkdown_UnknownSectionsAreProse(t *testing.T) {
	dir := writeProbe(t, `---
name: x
class: capability
---

## Prompt

hi

## Why this exists

Because a cold model drops the image.

## Checks

- response_contains: hello
`, nil)
	tk, err := LoadMarkdown(dir)
	if err != nil {
		t.Fatalf("unknown section broke the parse: %v", err)
	}
	if len(tk.Stages) != 1 || len(tk.Stages[0].Checks) != 1 {
		t.Errorf("stages/checks mangled by the unknown section: %+v", tk.Stages)
	}
}

// LoadDir dispatches on what is present, deterministically.
func TestLoadDir_Dispatch(t *testing.T) {
	md := writeProbe(t, "---\nname: m\nclass: capability\n---\n\n## Prompt\n\nhi\n", nil)
	if tk, err := LoadDir(md); err != nil || tk.Name != "m" {
		t.Errorf("markdown dir: %v %+v", err, tk)
	}
	empty := t.TempDir()
	if _, err := LoadDir(empty); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("empty dir should report ErrNotExist so discovery can skip it, got %v", err)
	}
}

// sameCheck compares two Checks by VALUE, dereferencing the *int bounds.
func sameCheck(a, b Check) bool {
	eq := func(x, y *int) bool {
		if x == nil || y == nil {
			return x == y
		}
		return *x == *y
	}
	return a.Kind == b.Kind && a.Cmd == b.Cmd && a.Path == b.Path && a.Text == b.Text &&
		a.Name == b.Name && a.ArgContains == b.ArgContains && a.N == b.N && eq(a.Min, b.Min) && eq(a.Max, b.Max)
}

// A multi-line block scalar inside a check list must survive the markdown
// parser. It did not: parseChecks kept only lines starting with "- ", so
// `- python: |` retained its first line and dropped the script — surfacing as
// "python: script is required" on a check whose script was plainly there.
func TestParseChecks_BlockScalarSurvives(t *testing.T) {
	dir := writeProbe(t, `---
name: scripted
class: capability
---

## Prompt

hi

## Checks

- response_contains: red
- python: |
    words = response.split()
    if len(words) < 2:
        fail("too short: %r" % response)

    if "red" not in response:
        fail("no red")
- response_contains: circle
`, nil)
	tk, err := LoadMarkdown(dir)
	if err != nil {
		t.Fatalf("LoadMarkdown: %v", err)
	}
	cks := tk.Stages[0].Checks
	if len(cks) != 3 {
		t.Fatalf("want 3 checks, got %d: %+v", len(cks), cks)
	}
	py := cks[1]
	if py.Kind != "python" {
		t.Fatalf("check[1] kind = %q", py.Kind)
	}
	for _, want := range []string{"words = response.split()", "too short", `if "red" not in response`} {
		if !strings.Contains(py.Text, want) {
			t.Errorf("script lost %q:\n%s", want, py.Text)
		}
	}
	// The blank line inside the scalar is content, not a terminator.
	if !strings.Contains(py.Text, "\n\n") {
		t.Error("blank line inside the block scalar was dropped")
	}
	// And the item AFTER the scalar must still parse.
	if cks[2].Kind != "response_contains" || cks[2].Text != "circle" {
		t.Errorf("check following a block scalar mangled: %+v", cks[2])
	}
}
