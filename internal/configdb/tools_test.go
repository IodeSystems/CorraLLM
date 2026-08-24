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

// A PIN must survive being stored, and this is the assertion with history
// behind it: `pin` has no column, so it rides in the unprojected remainder.
// That is the mechanism working as designed — a field added later needs no
// migration — but a pin that silently dropped on the next config write would
// un-hold a tool nobody touched, which is precisely the failure ("I had to hold
// llama.cpp back — for weeks") that pinning exists to end.
//
// Adding a column instead would need a migration, and this schema is applied
// with CREATE TABLE IF NOT EXISTS: on an existing database the column would
// never appear and every read would fail. The remainder is the safe home.
func TestToolPinSurvivesTheStore(t *testing.T) {
	ctx := context.Background()
	src := &Source{DB: openDB(t)}

	const held = "0b1bad14f0b1bad14f0b1bad14f0b1bad14f0b1b"
	in := &config.Config{
		Servers: map[string]config.Server{"box1": {Pools: map[string]string{"gpu0": "24GB"}}},
		Tools: map[string]config.Tool{
			"llama.cpp": {
				URL:   "https://github.com/ggml-org/llama.cpp.git",
				Ref:   "master",
				Pin:   held,
				Bin:   "llama-server",
				Hosts: map[string]config.ToolHost{"box1": {}},
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
	tool := got.Tools["llama.cpp"]
	if tool.Pin != held {
		t.Fatalf("pin lost on the round trip: %q — the tool would quietly resume tracking %s", tool.Pin, tool.Ref)
	}
	// Ref survives ALONGSIDE it. Pinning holds a tool; it does not retarget it,
	// and losing the branch would make un-pinning an act of memory.
	if tool.Ref != "master" {
		t.Errorf("ref lost while pinned: %q", tool.Ref)
	}
}
