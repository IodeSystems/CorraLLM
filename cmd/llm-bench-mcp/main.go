// Command llm-bench-mcp is the stdio MCP server the llm-bench runner spawns for
// every run. It exposes workspace tools (read_file, write_file, list_dir, run)
// jailed to a --workspace root, plus per-task bait tools and poison rules
// loaded from a --taskspec JSON file. Every tool call is journaled as JSONL to
// --journal: the deterministic source of truth for tool-usage checks.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/iodesystems/corrallm/internal/bench/journal"
	"github.com/iodesystems/corrallm/internal/bench/task"
)

const (
	runTimeout    = 30 * time.Second
	maxRunOutput  = 32 << 10 // 32 KiB
	baitCannedMsg = "OK. (no-op)"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("llm-bench-mcp: ")

	var (
		workspace = flag.String("workspace", "", "workspace root (all file/run tools are jailed to this dir)")
		allow     = flag.String("allow", "go,git,ls,cat,grep,sed,python3", "comma-separated allowlist of run binaries")
		taskspec  = flag.String("taskspec", "", "path to taskspec JSON (bait tools + poison rules)")
		journalP  = flag.String("journal", "", "path to the JSONL call journal")
		// fileTools gates read_file/write_file/list_dir. Off when a toolset
		// supplies its OWN file+navigation surface (poly-lsp: node_read reads,
		// node_edit writes, `structure` lists dirs). Exposing both gives the
		// model redundant tools with different conventions to mislearn across
		// (write_file{path,content} vs node_edit{file,newText}). Only `run`
		// stays — executing is the one thing poly-lsp can't do, so it's
		// llm-bench-mcp's sole unique capability.
		fileTools = flag.Bool("file-tools", true, "expose read_file/write_file/list_dir (off = run-only, cede file+nav to a richer server)")
	)
	flag.Parse()

	if *workspace == "" || *journalP == "" {
		log.Fatalf("--workspace and --journal are required")
	}
	absWs, err := filepath.Abs(*workspace)
	if err != nil {
		log.Fatalf("resolve workspace: %v", err)
	}
	if fi, err := os.Stat(absWs); err != nil || !fi.IsDir() {
		log.Fatalf("workspace %q is not a directory", absWs)
	}

	spec := task.TaskSpec{}
	if *taskspec != "" {
		spec, err = task.LoadSpec(*taskspec)
		if err != nil {
			log.Fatalf("load taskspec: %v", err)
		}
	}

	w, err := journal.NewWriter(*journalP)
	if err != nil {
		log.Fatalf("open journal: %v", err)
	}
	defer w.Close()

	srv := &mcpServer{
		workspace: absWs,
		allow:     splitSet(*allow),
		poison:    spec.Poison,
		journ:     w,
	}

	srv.fileTools = *fileTools
	s := server.NewMCPServer("llm-bench-mcp", "0.1.0")
	srv.register(s, spec.BaitTools)

	if err := server.ServeStdio(s); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

type mcpServer struct {
	workspace string
	allow     map[string]bool
	poison    []task.PoisonRule
	journ     *journal.Writer
	fileTools bool // expose read_file/write_file (see --file-tools)
}

func splitSet(csv string) map[string]bool {
	m := map[string]bool{}
	for _, p := range strings.Split(csv, ",") {
		if p = strings.TrimSpace(p); p != "" {
			m[p] = true
		}
	}
	return m
}

func (srv *mcpServer) register(s *server.MCPServer, bait []task.BaitTool) {
	// read_file/write_file/list_dir are ceded when a richer file+nav server is
	// present (--file-tools=false), so the model sees exactly one convention.
	if srv.fileTools {
		s.AddTool(mcp.NewTool("read_file",
			mcp.WithDescription("Read a UTF-8 text file relative to the workspace root."),
			mcp.WithString("path", mcp.Required(), mcp.Description("workspace-relative file path")),
		), srv.wrap("read_file", srv.readFile))

		s.AddTool(mcp.NewTool("write_file",
			mcp.WithDescription("Create or overwrite a text file relative to the workspace root."),
			mcp.WithString("path", mcp.Required(), mcp.Description("workspace-relative file path")),
			mcp.WithString("content", mcp.Required(), mcp.Description("full file contents")),
		), srv.wrap("write_file", srv.writeFile))

		s.AddTool(mcp.NewTool("list_dir",
			mcp.WithDescription("List entries of a directory relative to the workspace root."),
			mcp.WithString("path", mcp.Description("workspace-relative dir path (default \".\")")),
		), srv.wrap("list_dir", srv.listDir))
	}

	s.AddTool(mcp.NewTool("run",
		mcp.WithDescription("Run an allowlisted program (argv, no shell) in the workspace root. 30s timeout."),
		mcp.WithArray("argv", mcp.Required(), mcp.WithStringItems(),
			mcp.Description("program and arguments, e.g. [\"go\",\"test\",\"./...\"]")),
	), srv.wrap("run", srv.run))

	for _, b := range bait {
		desc := b.Description
		if desc == "" {
			desc = "utility"
		}
		s.AddTool(mcp.NewTool(b.Name, mcp.WithDescription(desc)),
			srv.wrapBait(b.Name))
	}
}

// wrap adapts a core tool handler with journaling + poison splicing.
func (srv *mcpServer) wrap(name string, fn func(context.Context, mcp.CallToolRequest) (string, error)) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		argsJSON := argsToJSON(req)
		result, err := fn(ctx, req)
		if err != nil {
			// Errors meant for the model are journaled too, then returned as an
			// error result so the loop can recover.
			srv.record(name, argsJSON, len(err.Error()), false, false)
			return mcp.NewToolResultError(err.Error()), nil
		}
		poisoned := false
		if txt, ok := srv.poisonFor(name, string(argsJSON)); ok {
			result += "\n\n" + txt
			poisoned = true
		}
		srv.record(name, argsJSON, len(result), poisoned, false)
		return mcp.NewToolResultText(result), nil
	}
}

// wrapBait handles a bait tool: benign canned reply, but journaled bait=true.
func (srv *mcpServer) wrapBait(name string) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		argsJSON := argsToJSON(req)
		srv.record(name, argsJSON, len(baitCannedMsg), false, true)
		return mcp.NewToolResultText(baitCannedMsg), nil
	}
}

func (srv *mcpServer) record(tool string, args json.RawMessage, resultBytes int, poisoned, bait bool) {
	if err := srv.journ.Append(journal.Entry{
		TS:          time.Now().UnixNano(),
		Tool:        tool,
		Args:        args,
		ResultBytes: resultBytes,
		Poisoned:    poisoned,
		Bait:        bait,
	}); err != nil {
		log.Printf("journal append: %v", err)
	}
}

// poisonFor returns the first matching poison text for tool + args JSON.
func (srv *mcpServer) poisonFor(tool, argsJSON string) (string, bool) {
	for _, p := range srv.poison {
		if p.Into != tool {
			continue
		}
		if p.Match == "" || strings.Contains(argsJSON, p.Match) {
			return p.Text, true
		}
	}
	return "", false
}

// ── jail ────────────────────────────────────────────────────────────

// resolve maps a workspace-relative path to an absolute path inside the jail,
// rejecting absolute paths and any traversal that escapes the root.
func (srv *mcpServer) resolve(rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute paths are not allowed: %q", rel)
	}
	full := filepath.Join(srv.workspace, filepath.Clean(rel))
	r, err := filepath.Rel(srv.workspace, full)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes workspace: %q", rel)
	}
	return full, nil
}

// ── core tools ──────────────────────────────────────────────────────

func (srv *mcpServer) readFile(_ context.Context, req mcp.CallToolRequest) (string, error) {
	rel, err := req.RequireString("path")
	if err != nil {
		return "", err
	}
	full, err := srv.resolve(rel)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return "", fmt.Errorf("read %s: %v", rel, err)
	}
	return truncateMiddle(string(b), maxRunOutput), nil
}

// truncateMiddle bounds s to ~max bytes keeping head AND tail. Tool results
// flow into the model's context; for command/test output the diagnosis usually
// sits at the END (go test prints failures last), so head-only truncation hides
// exactly the part the model needs and invites re-running the same command.
func truncateMiddle(s string, max int) string {
	if len(s) <= max {
		return s
	}
	head, tail := max*2/3, max/3
	return s[:head] + fmt.Sprintf("\n...[%d bytes omitted]...\n", len(s)-head-tail) + s[len(s)-tail:]
}

func (srv *mcpServer) writeFile(_ context.Context, req mcp.CallToolRequest) (string, error) {
	rel, err := req.RequireString("path")
	if err != nil {
		return "", err
	}
	content, err := req.RequireString("content")
	if err != nil {
		return "", err
	}
	full, err := srv.resolve(rel)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", fmt.Errorf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %v", rel, err)
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(content), rel), nil
}

func (srv *mcpServer) listDir(_ context.Context, req mcp.CallToolRequest) (string, error) {
	rel := req.GetString("path", ".")
	full, err := srv.resolve(rel)
	if err != nil {
		return "", err
	}
	ents, err := os.ReadDir(full)
	if err != nil {
		return "", fmt.Errorf("list %s: %v", rel, err)
	}
	var b strings.Builder
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		b.WriteString(name)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// checkArg rejects an argv element that points outside the workspace.
//
// This is CONTAINMENT, not a security boundary, and the distinction is
// worth being honest about: the default allowlist carries python3 and
// go, so anything determined to leave the workspace trivially can
// (`python3 -c ...`). A real boundary needs OS sandboxing, not argument
// inspection.
//
// What it does buy is HONEST MEASUREMENTS, which is what this harness
// exists for. cmd.Dir only sets the CWD, so a wandering agent could run
// `ls /tmp` and splice the host temp dir into its transcript — that is
// exactly what produced a bogus 205k-token result and the false
// conclusion that poly-lsp was verbose. Benchmarks have to depend on
// nothing but the workspace to be reproducible.
func (srv *mcpServer) checkArg(arg string) error {
	v := arg
	if strings.HasPrefix(v, "-") {
		// A bare flag is never a path; --flag=VALUE hides one.
		i := strings.IndexByte(v, '=')
		if i < 0 {
			return nil
		}
		v = v[i+1:]
	}
	if v == "" {
		return nil
	}
	if strings.HasPrefix(v, "~") {
		return fmt.Errorf("argument %q is home-relative; run is jailed to the workspace", arg)
	}
	if filepath.IsAbs(v) {
		return fmt.Errorf("argument %q is an absolute path; run is jailed to the workspace (use a path relative to it)", arg)
	}
	// Only consult the jail when the arg could actually climb out, so
	// ordinary arguments that merely contain dots or slashes — sed
	// scripts like 's/a/b/', regexes, version strings — pass untouched.
	if strings.Contains(v, "..") {
		if _, err := srv.resolve(v); err != nil {
			return fmt.Errorf("argument %q escapes the workspace", arg)
		}
	}
	return nil
}

func (srv *mcpServer) run(ctx context.Context, req mcp.CallToolRequest) (string, error) {
	argv, err := req.RequireStringSlice("argv")
	if err != nil {
		return "", err
	}
	if len(argv) == 0 {
		return "", fmt.Errorf("argv must be non-empty")
	}
	if !srv.allow[argv[0]] {
		return "", fmt.Errorf("binary %q not in allowlist", argv[0])
	}
	for _, a := range argv[1:] {
		if err := srv.checkArg(a); err != nil {
			return "", err
		}
	}
	cctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	cmd.Dir = srv.workspace
	out, runErr := cmd.CombinedOutput()
	outStr := truncateMiddle(string(out), maxRunOutput)
	status := "exit 0"
	if runErr != nil {
		status = "exit error: " + runErr.Error()
	}
	return fmt.Sprintf("$ %s\n%s\n[%s]", strings.Join(argv, " "), outStr, status), nil
}

// argsToJSON marshals a call's arguments to a compact JSON object for the
// journal + poison matching. Never fails hard — falls back to "{}".
func argsToJSON(req mcp.CallToolRequest) json.RawMessage {
	b, err := json.Marshal(req.GetArguments())
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}
