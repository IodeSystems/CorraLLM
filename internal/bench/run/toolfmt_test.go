package run

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// TestEncoderFor covers format → encoder selection (the flag/config resolution):
// json is the baseline (nil = passthrough), each named format returns a working
// encoder, and an unknown format errors.
func TestEncoderFor(t *testing.T) {
	// json / "" → nil encoder (passthrough).
	for _, f := range []string{"", "json"} {
		enc, err := EncoderFor(f)
		if err != nil {
			t.Fatalf("EncoderFor(%q) err: %v", f, err)
		}
		if enc != nil {
			t.Errorf("EncoderFor(%q) should be nil (baseline passthrough)", f)
		}
	}

	// The non-baseline format returns a non-nil encoder.
	enc, err := EncoderFor("tightc")
	if err != nil {
		t.Fatalf("EncoderFor(tightc) err: %v", err)
	}
	if enc == nil {
		t.Fatalf("EncoderFor(tightc) unexpectedly nil")
	}

	// tightc actually transforms a JSON tool result (this is the exact encoder
	// wired to sess.EncodeToolResult): a uniform array → count-anchored table.
	const rawJSON = `[{"sym":"S.Start","class":"method"},{"sym":"main","class":"func"}]`
	got := enc(rawJSON)
	if got == rawJSON {
		t.Fatal("tightc encoder did not transform JSON")
	}
	if !strings.Contains(got, "[2]sym,class") || !strings.Contains(got, "S.Start,method") {
		t.Errorf("tightc encoder output not a count-anchored table:\n%s", got)
	}

	// Unknown format → error.
	if _, err := EncoderFor("bogus"); err == nil {
		t.Error("unknown format should error")
	}
}

func TestEffectiveToolResultFormat(t *testing.T) {
	if got := (Config{}).EffectiveToolResultFormat(); got != "json" {
		t.Errorf("empty config should default to json, got %q", got)
	}
	if got := (Config{ToolResultFormat: "tightc"}).EffectiveToolResultFormat(); got != "tightc" {
		t.Errorf("configured format not honored, got %q", got)
	}
}

// TestRunUnknownToolFormatErrors proves an unknown tool-format is rejected at
// STARTUP (before any combo runs), like a missing toolset binary.
func TestRunUnknownToolFormatErrors(t *testing.T) {
	tasksDir := writeSmokeTask(t)
	out := t.TempDir()
	opts := Options{
		Config: Config{
			LLM:              LLMConfig{BaseURL: "http://unused.invalid"},
			Models:           []string{"fake"},
			Toolsets:         OrderedToolsets{{Name: "baseline"}},
			ToolResultFormat: "bogus",
		},
		TasksDir:  tasksDir,
		Out:       out,
		McpBin:    "/does/not/matter",
		NewRunner: func(string) agent.LLMRunner { return &fakeRunner{} },
	}
	_, _, err := Run(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "unknown tool-result format") {
		t.Fatalf("expected unknown-format error, got %v", err)
	}
	// Fail-fast: no run dir created.
	if ents, _ := os.ReadDir(out); len(ents) != 0 {
		t.Errorf("startup failure should not create a run dir, found %d entries", len(ents))
	}
}

// TestRunToolFormatRecordedOnRows runs a full combo with --tool-format tightc and
// asserts every row carries toolFormat="tightc" and summary.csv gets the column +
// value. This also exercises the sess.EncodeToolResult wiring (tightc path).
func TestRunToolFormatRecordedOnRows(t *testing.T) {
	mcpBin := buildMcp(t)
	tasksDir := writeSmokeTask(t)

	fake := &fakeRunner{resp: []scriptedResp{
		{calls: []llm.ToolCall{toolCall("c0", "run", map[string]any{"argv": []string{"go", "test", "./..."}})}},
		{content: "The test fails."},
		{calls: []llm.ToolCall{toolCall("c1", "write_file", map[string]any{"path": "mathx.go", "content": fixedMathx})}},
		{calls: []llm.ToolCall{toolCall("c2", "run", map[string]any{"argv": []string{"go", "test", "./..."}})}},
		{content: "Fixed."},
	}}

	out := t.TempDir()
	opts := Options{
		Config: Config{
			LLM:              LLMConfig{BaseURL: "http://unused.invalid"},
			Models:           []string{"fake"},
			Toolsets:         OrderedToolsets{{Name: "baseline"}},
			ToolResultFormat: "tightc",
		},
		TasksDir:  tasksDir,
		Out:       out,
		McpBin:    mcpBin,
		NewRunner: func(string) agent.LLMRunner { return fake },
	}
	rows, outDir, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no rows produced")
	}
	for i, r := range rows {
		if r.ToolFormat != "tightc" {
			t.Errorf("row %d ToolFormat = %q; want tightc", i, r.ToolFormat)
		}
	}
	csvBytes, err := os.ReadFile(filepath.Join(outDir, "summary.csv"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitN(string(csvBytes), "\n", 3)
	if !strings.Contains(lines[0], "tool_format") {
		t.Errorf("summary.csv header missing tool_format column:\n%s", lines[0])
	}
	if len(lines) < 2 || !strings.Contains(lines[1], "tightc") {
		t.Errorf("summary.csv data row missing tightc value:\n%s", string(csvBytes))
	}
}
