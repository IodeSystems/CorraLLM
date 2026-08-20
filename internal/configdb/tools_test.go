package configdb

import (
	"context"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

// A tools block must survive being stored.
//
// This was a FILE round-trip test, and it existed because a `tools:` block once
// vanished on a write — the daemon marshalled a struct it had no member for, so
// the section had nowhere to live. The file writer is gone; the guarantee is
// not, so the test moved to where saving actually happens.
//
// installedAt gets its own assertion: it is the difference between an install
// corrallm may build into and one it must never touch, so losing it silently
// would make somebody else's checkout buildable.
func TestToolsSurviveTheStore(t *testing.T) {
	ctx := context.Background()
	src := &Source{DB: openDB(t)}

	in := &config.Config{
		Servers: map[string]config.Server{
			"box1":            {Pools: map[string]string{"gpu0": "24GB"}},
			"carlsmacbookpro": {Pools: map[string]string{"system": "64GB"}},
		},
		Tools: map[string]config.Tool{
			"llama.cpp": {
				URL: "https://github.com/ggml-org/llama.cpp.git",
				Ref: "master",
				Bin: "llama-server",
				Hosts: map[string]config.ToolHost{
					"box1":            {},
					"carlsmacbookpro": {InstalledAt: "/Users/x/ml-kit/local/bin/llama.cpp"},
				},
			},
		},
	}
	if err := src.Save(ctx, in); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := src.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	tool, ok := got.Tools["llama.cpp"]
	if !ok {
		t.Fatal("the tools block did not survive — this is the exact loss the port was built to end")
	}
	if tool.Ref != "master" || tool.Bin != "llama-server" || tool.URL == "" {
		t.Errorf("tool fields lost: %+v", tool)
	}
	if len(tool.Hosts) != 2 {
		t.Fatalf("hosts lost: %+v", tool.Hosts)
	}
	if tool.Hosts["carlsmacbookpro"].InstalledAt == "" {
		t.Error("installedAt lost — an adopted entry would come back as managed, and become buildable")
	}
	if tool.Hosts["box1"].Adopted() {
		t.Error("a managed entry came back adopted")
	}
}
