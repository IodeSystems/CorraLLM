package run

import (
	"os"
	"path/filepath"
	"testing"
)

// TestToolsetCedeFileTools: a toolset value may be a bare server list
// (cedeFileTools=false) or an object with cedeFileTools:true; both parse and the
// flag is carried, preserving declaration order.
func TestToolsetCedeFileTools(t *testing.T) {
	y := `
llm: { baseURL: "http://x", apiKeyEnv: K }
models: [m]
toolsets:
  baseline: []
  mcpshell:
    - cmd: mcpshell
      args: ["mcp"]
  polylsp:
    cedeFileTools: true
    servers:
      - cmd: poly-lsp-mcp
        args: ["mcp", "--root", "{{workspace}}"]
`
	c, err := loadConfigBytes([]byte(y))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []struct {
		name string
		cede bool
		nsrv int
	}{{"baseline", false, 0}, {"mcpshell", false, 1}, {"polylsp", true, 1}}
	if len(c.Toolsets) != len(want) {
		t.Fatalf("got %d toolsets, want %d", len(c.Toolsets), len(want))
	}
	for i, w := range want {
		ts := c.Toolsets[i]
		if ts.Name != w.name || ts.CedeFileTools != w.cede || len(ts.Servers) != w.nsrv {
			t.Errorf("toolset %d = {%q cede=%v n=%d}, want {%q cede=%v n=%d}",
				i, ts.Name, ts.CedeFileTools, len(ts.Servers), w.name, w.cede, w.nsrv)
		}
	}
}

// probeDirs are directory REFERENCES: the box names where a tool keeps its own
// probes, once, and editing them there changes what runs here.
//
// Relative entries anchor to the CONFIG FILE's directory. The bench is spawned
// by a daemon whose cwd is its deployment directory, so a path resolved against
// the process's cwd means something different depending on who started the run
// — the exact defect that forced the probe library to be embedded.
func TestProbeDirsResolveAgainstTheConfigFile(t *testing.T) {
	home := t.TempDir()
	cfgPath := filepath.Join(home, "llm-bench.yaml")
	body := "llm:\n  baseURL: http://x\nmodels: [m]\ntoolsets:\n  baseline: []\nprobeDirs:\n  - ./local-probes\n  - /already/absolute\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	// Run from somewhere else entirely: the daemon's cwd is not the config's.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	defer func() { _ = os.Chdir(cwd) }()

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	want := []string{filepath.Join(home, "local-probes"), "/already/absolute"}
	if len(cfg.ProbeDirs) != len(want) {
		t.Fatalf("probeDirs = %v, want %v", cfg.ProbeDirs, want)
	}
	for i := range want {
		if cfg.ProbeDirs[i] != want[i] {
			t.Errorf("probeDirs[%d] = %q, want %q", i, cfg.ProbeDirs[i], want[i])
		}
	}
}

// No probeDirs is the built-in library, not the current directory.
func TestNoProbeDirsIsEmptyNotCwd(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "llm-bench.yaml")
	if err := os.WriteFile(p, []byte("llm:\n  baseURL: http://x\nmodels: [m]\ntoolsets:\n  baseline: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.ProbeDirs) != 0 {
		t.Errorf("probeDirs = %v, want none", cfg.ProbeDirs)
	}
}
