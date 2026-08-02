package report

import (
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sampleRows() []Row {
	return []Row{
		{
			TS: "t", Model: "m1", Toolset: "baseline", Task: "fix", Class: "coding", Stage: 0,
			StageMetrics: StageMetrics{Turns: 2, ToolCalls: 3, PromptTokens: 30, CompletionTokens: 15, Tokens: 45, JSONErrors: 1, TokPerSec: 12.5, WallMs: 3600},
			ChecksPassed: 1, ChecksTotal: 1, Pass: true,
		},
		{
			TS: "t", Model: "m1", Toolset: "baseline", Task: "fix", Class: "coding", Stage: 1,
			StageMetrics: StageMetrics{Turns: 3, ToolCalls: 4, PromptTokens: 40, CompletionTokens: 20, Tokens: 60, BaitCalls: 0, WallMs: 5000},
			ChecksPassed: 2, ChecksTotal: 2, Pass: true,
		},
	}
}

// TestRowFlatJSON confirms every numeric metric is a TOP-LEVEL scalar field —
// no metric lives only inside a nested object — and judge_quality is present + null.
func TestRowFlatJSON(t *testing.T) {
	b, err := json.Marshal(sampleRows()[0])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, nested := m["metrics"]; nested {
		t.Error("metrics should be inlined, not nested under a 'metrics' key")
	}
	for _, k := range []string{
		"turns", "toolCalls", "promptTokens", "completionTokens", "tokens",
		"invalidArgRetries", "jsonErrors", "repeatedCalls", "baitCalls", "retries429",
		"tokPerSec", "wallMs", "checksPassed", "checksTotal", "judge", "judge_quality",
	} {
		if _, ok := m[k]; !ok {
			t.Errorf("runs.jsonl row missing top-level field %q", k)
		}
	}
	if string(m["judge_quality"]) != "null" {
		t.Errorf("judge_quality should be null in P0, got %s", m["judge_quality"])
	}
}

func TestWriteAll(t *testing.T) {
	dir := t.TempDir()
	if err := WriteAll(dir, "ts", sampleRows()); err != nil {
		t.Fatal(err)
	}
	// summary.csv: one aggregated row per model×toolset×task, with judge_quality reserved.
	f, err := os.Open(filepath.Join(dir, "summary.csv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 { // header + 1 aggregated row
		t.Fatalf("summary.csv want header+1 row, got %d rows", len(recs))
	}
	header := recs[0]
	col := func(name string) int {
		for i, h := range header {
			if h == name {
				return i
			}
		}
		return -1
	}
	if col("judge_quality") < 0 {
		t.Fatal("summary.csv missing judge_quality column")
	}
	row := recs[1]
	if got := row[col("tokens")]; got != "105" { // 45 + 60
		t.Errorf("tokens sum = %s, want 105", got)
	}
	if got := row[col("prompt_tokens")]; got != "70" { // 30 + 40
		t.Errorf("prompt_tokens sum = %s, want 70", got)
	}
	if got := row[col("json_errors")]; got != "1" {
		t.Errorf("json_errors sum = %s, want 1", got)
	}
	if got := row[col("pass_rate")]; !strings.HasPrefix(got, "1") {
		t.Errorf("pass_rate = %s, want ~1.0", got)
	}
	if row[col("judge_quality")] != "" {
		t.Errorf("judge_quality should be empty in P0, got %q", row[col("judge_quality")])
	}

	if _, err := os.Stat(filepath.Join(dir, "runs.jsonl")); err != nil {
		t.Errorf("runs.jsonl: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "report.md")); err != nil {
		t.Errorf("report.md: %v", err)
	}
}

// A `run: both` probe emits a cold pass and a warm pass. They must stay SEPARATE
// rows in summary.csv: merging them averages a model that works warm with the
// same model failing cold into a meaningless ~50% and hides the disagreement
// that is the whole reason both passes ran.
func TestWriteSummaryCSV_SplitsRunModes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "summary.csv")
	rows := []Row{
		{Model: "m", Toolset: "baseline", Task: "vision", RunMode: "warm", Class: "capability", Pass: true, ChecksTotal: 1, ChecksPassed: 1},
		{Model: "m", Toolset: "baseline", Task: "vision", RunMode: "cold", Class: "capability", Pass: false, ChecksTotal: 1, ChecksPassed: 0},
	}
	if err := WriteSummaryCSV(path, rows, nil); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	if !strings.Contains(out, "run_mode") {
		t.Error("summary.csv must carry a run_mode column or the passes are indistinguishable")
	}
	lines := 0
	for _, ln := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(ln, "m,baseline,vision,") {
			lines++
		}
	}
	if lines != 2 {
		t.Errorf("want 2 rows (cold + warm), got %d:\n%s", lines, out)
	}
}
