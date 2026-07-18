// Command llm-bench is the benchmark runner CLI.
//
//	llm-bench run [--models a,b] [--toolsets baseline,..] [--tasks glob] [--out dir] [--config f] [--mcp-bin p] [--judge]
//	llm-bench validate [--tasks-dir probes]   — load + validate every task, nonzero exit if any is invalid
//	llm-bench judge -run out/<ts> [--config f] [--model override]   — P1 judge phase over a completed run
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/corrallm/internal/bench/judge"
	"github.com/iodesystems/corrallm/internal/bench/run"
	"github.com/iodesystems/corrallm/internal/bench/task"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		os.Exit(cmdRun(os.Args[2:]))
	case "validate":
		os.Exit(cmdValidate(os.Args[2:]))
	case "judge":
		os.Exit(cmdJudge(os.Args[2:]))
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: llm-bench <run|validate|judge> [flags]")
	fmt.Fprintln(os.Stderr, "  llm-bench run      [--config llm-bench.yaml] [--tasks-dir probes] [--models a,b] [--toolsets baseline,..] [--tasks glob] [--out out] [--mcp-bin path] [--tool-format json|tightc] [--judge]")
	fmt.Fprintln(os.Stderr, "  llm-bench validate [--tasks-dir probes]")
	fmt.Fprintln(os.Stderr, "  llm-bench judge    -run out/<ts> [--config llm-bench.yaml] [--model override]")
}

func cmdRun(argv []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	config := fs.String("config", "llm-bench.yaml", "path to llm-bench.yaml")
	tasksDir := fs.String("tasks-dir", "probes", "directory of task subdirs")
	models := fs.String("models", "", "comma-separated model filter (default: all in config)")
	toolsets := fs.String("toolsets", "", "comma-separated toolset filter (default: all in config)")
	tasksGlob := fs.String("tasks", "", "glob on task dir names (default: all)")
	out := fs.String("out", "out", "output root directory")
	mcpBin := fs.String("mcp-bin", "", "path to llm-bench-mcp (default: local/bin/llm-bench-mcp or $PATH)")
	doJudge := fs.Bool("judge", false, "run the P1 judge phase after candidates finish")
	toolFormat := fs.String("tool-format", "", "tool-result encoding: json|tightc (default: config toolResultFormat, else json)")
	_ = fs.Parse(argv)

	cfg, err := run.LoadConfig(*config)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	// Flag overrides config.
	if *toolFormat != "" {
		cfg.ToolResultFormat = *toolFormat
	}
	bin, err := resolveMcpBin(*mcpBin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	opts := run.Options{
		Config:    cfg,
		TasksDir:  *tasksDir,
		Out:       *out,
		Models:    splitCSV(*models),
		Toolsets:  splitCSV(*toolsets),
		TasksGlob: *tasksGlob,
		McpBin:    bin,
		BinDir:    resolveBinDir(),
		Judge:     *doJudge,
	}
	rows, outDir, err := run.Run(context.Background(), opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		return 1
	}
	passed := 0
	for _, r := range rows {
		if r.Pass {
			passed++
		}
	}
	fmt.Printf("wrote %s (%d stage rows, %d passed)\n", outDir, len(rows), passed)
	return 0
}

func cmdValidate(argv []string) int {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	tasksDir := fs.String("tasks-dir", "probes", "directory of task subdirs")
	_ = fs.Parse(argv)

	ents, err := os.ReadDir(*tasksDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "validate:", err)
		return 1
	}
	bad := 0
	n := 0
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(*tasksDir, e.Name())
		t, err := task.LoadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue // not a probe dir
		}
		n++
		_ = t
		if err != nil {
			fmt.Printf("INVALID %s: %v\n", e.Name(), err)
			bad++
		} else {
			fmt.Printf("ok      %s\n", e.Name())
		}
	}
	fmt.Printf("%d task(s), %d invalid\n", n, bad)
	if bad > 0 {
		return 1
	}
	return 0
}

func cmdJudge(argv []string) int {
	fs := flag.NewFlagSet("judge", flag.ExitOnError)
	config := fs.String("config", "llm-bench.yaml", "path to llm-bench.yaml")
	runDir := fs.String("run", "", "path to a completed run dir (out/<ts>)")
	model := fs.String("model", "", "judge model override (default: llm-bench.yaml judge.model)")
	_ = fs.Parse(argv)

	if *runDir == "" {
		fmt.Fprintln(os.Stderr, "judge: -run out/<ts> is required")
		return 2
	}
	cfg, err := run.LoadConfig(*config)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	jc := judge.Config{Model: cfg.Judge.Model, MaxTranscriptBytes: cfg.Judge.MaxTranscriptBytes}
	if *model != "" {
		jc.Model = *model
	}
	newRunner := func(m string) agent.LLMRunner {
		return llm.NewClient(cfg.LLM.BaseURL, os.Getenv(cfg.LLM.APIKeyEnv), m)
	}
	results, err := judge.Judge(context.Background(), *runDir, jc, newRunner)
	if err != nil {
		fmt.Fprintln(os.Stderr, "judge:", err)
		return 1
	}
	scored, failed := 0, 0
	for _, r := range results {
		if r.Err != "" {
			failed++
		} else {
			scored++
		}
	}
	fmt.Printf("judged %d combos (%d scored, %d errored) → %s/judge.jsonl\n", len(results), scored, failed, *runDir)
	return 0
}

// resolveBinDir returns the absolute ./local/bin dir when it exists (where
// bin/llm-bench installs llm-bench-mcp + the toolset servers), else "" ($PATH only).
func resolveBinDir() string {
	p := filepath.Join("local", "bin")
	if fi, err := os.Stat(p); err == nil && fi.IsDir() {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
	}
	return ""
}

// resolveMcpBin finds the llm-bench-mcp binary: explicit flag, then
// ./local/bin/llm-bench-mcp, then $PATH.
func resolveMcpBin(flagVal string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	if p := filepath.Join("local", "bin", "llm-bench-mcp"); fileExists(p) {
		return filepath.Abs(p)
	}
	if p, err := exec.LookPath("llm-bench-mcp"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("llm-bench-mcp not found: build it with bin/llm-bench or pass --mcp-bin")
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
