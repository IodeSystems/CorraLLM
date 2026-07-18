package run

import "testing"

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
