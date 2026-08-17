package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A top-level field must survive Save, or the daemon deletes it.
//
// THIS IS A REGRESSION TEST FOR A REAL LOSS. `tools:` was added to the live
// config while a daemon built before the field was still running. Reading was
// safe — yaml.Unmarshal ignores unknown fields, and that older binary validated
// the file happily — so the change looked inert. It was not: the daemon also
// WRITES config, and Save marshals the in-memory Config, where a field the
// running binary has no member for has nowhere to live. The next autonomous
// write (a discovery refresh) silently dropped the entire block, and
// `corrallm tools list` went from three rows to "no tools declared".
//
// Nothing warned, and nothing could have: to the old binary those keys never
// existed. The lesson generalises to every future top-level section — deploy the
// binary that knows a field BEFORE writing that field into config — and this
// test at least catches the half that is checkable here: that a Config carrying
// tools round-trips through the writer intact.
func TestToolsSurviveSave(t *testing.T) {
	c := &Config{
		Servers: map[string]Server{
			"box1":            {Pools: map[string]string{"gpu0": "24GB"}},
			"carlsmacbookpro": {Pools: map[string]string{"system": "64GB"}},
		},
		Tools: map[string]Tool{
			"llama.cpp": {
				URL: "https://github.com/ggml-org/llama.cpp.git",
				Ref: "master",
				Bin: "llama-server",
				Hosts: map[string]ToolHost{
					"box1":            {},
					"carlsmacbookpro": {InstalledAt: "/Users/x/ml-kit/local/bin/llama.cpp"},
				},
			},
		},
	}

	path := filepath.Join(t.TempDir(), "config.yml")
	if err := Save(path, c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v\n%s", err, b)
	}
	tool, ok := got.Tools["llama.cpp"]
	if !ok {
		t.Fatalf("the tools block did not survive Save — this is the exact loss that hit the live config:\n%s", b)
	}
	if tool.Ref != "master" || tool.URL == "" || tool.Bin != "llama-server" {
		t.Errorf("tool fields lost in the round trip: %+v", tool)
	}
	if len(tool.Hosts) != 2 {
		t.Fatalf("hosts lost: %+v", tool.Hosts)
	}
	// The adopted/managed distinction is the one that decides whether a build
	// may write to a directory. Losing it silently would be worse than losing
	// the whole block.
	if tool.Hosts["carlsmacbookpro"].InstalledAt == "" {
		t.Error("installedAt lost — an adopted entry would come back as managed and become buildable")
	}
	if tool.Hosts["box1"].Adopted() {
		t.Error("a managed entry came back adopted")
	}
}
