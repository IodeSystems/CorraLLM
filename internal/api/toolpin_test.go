package api

import (
	"context"
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

// Pinning a tool from the dashboard.
//
// Before this, `tools:` was the one part of config with no write surface at
// all — holding llama.cpp back meant exporting the config, editing YAML and
// loading it again, which is why it was done by not-rebuilding and remembering
// why instead. The daemon now owns the decision, which is what makes it survive
// a scheduled rebuild.

const heldSha = "0b1bad14f0b1bad14f0b1bad14f0b1bad14f0b1b"

func pinnable(t *testing.T) *Handlers {
	t.Helper()
	return storeBackedHandlers(t, &config.Config{
		Servers: map[string]config.Server{"box1": {Pools: map[string]string{"gpu0": "24GB"}}},
		Tools: map[string]config.Tool{
			"llama.cpp": {
				URL:   "https://github.com/ggml-org/llama.cpp.git",
				Ref:   "master",
				Bin:   "llama-server",
				Hosts: map[string]config.ToolHost{"box1": {}},
			},
		},
	})
}

func pin(t *testing.T, h *Handlers, tool, sha string) (*ToolPinOutput, error) {
	t.Helper()
	in := &ToolPinInput{}
	in.Body.Tool, in.Body.Pin = tool, sha
	return h.ToolPin(context.Background(), in)
}

func TestPinIsPersistedAndLeavesTheTrackedRefAlone(t *testing.T) {
	h := pinnable(t)

	out, err := pin(t, h, "llama.cpp", heldSha)
	if err != nil {
		t.Fatalf("pin: %v", err)
	}
	if out.Body.Pin != heldSha || out.Body.Ref != "master" {
		t.Fatalf("pin reported back wrong: %+v", out.Body)
	}

	saved := reloadStored(t, h, "after pinning a tool")
	tool := saved.Tools["llama.cpp"]
	if tool.Pin != heldSha {
		t.Fatalf("pin did not persist: %q", tool.Pin)
	}
	// The branch survives the pin. Overwriting `ref` with the sha would work
	// for the hold and make un-pinning an act of memory — and would silently
	// break the drift check, which cannot follow a commit anywhere.
	if tool.Ref != "master" {
		t.Fatalf("pinning retargeted the tool: ref = %q", tool.Ref)
	}
}

// Un-pinning is the same call with nothing in it, and it must actually clear —
// a pin that lingers is a tool that never moves again.
func TestUnpinReleasesTheTool(t *testing.T) {
	h := pinnable(t)
	if _, err := pin(t, h, "llama.cpp", heldSha); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if _, err := pin(t, h, "llama.cpp", ""); err != nil {
		t.Fatalf("unpin: %v", err)
	}
	if got := reloadStored(t, h, "after un-pinning").Tools["llama.cpp"].Pin; got != "" {
		t.Fatalf("pin survived un-pinning: %q", got)
	}
}

// A sha pasted in whatever case it was copied in must not create a second
// spelling of the same commit: the drift check compares strings, so an
// uppercase pin against git's lowercase output would report a tool as forever
// behind a pin it is already sitting on.
func TestPinIsNormalizedToLowercase(t *testing.T) {
	h := pinnable(t)
	if _, err := pin(t, h, "llama.cpp", strings.ToUpper(heldSha)); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if got := reloadStored(t, h, "after pinning").Tools["llama.cpp"].Pin; got != heldSha {
		t.Fatalf("pin not normalized: %q", got)
	}
}

// Refused at the edge rather than at the next build, twenty minutes in on a
// machine nobody is watching.
func TestAbbreviatedPinIsRefused(t *testing.T) {
	h := pinnable(t)
	if _, err := pin(t, h, "llama.cpp", "0b1bad14f"); err == nil {
		t.Fatal("an abbreviated sha was accepted as a pin")
	}
	if got := reloadStored(t, h, "after a refused pin").Tools["llama.cpp"].Pin; got != "" {
		t.Fatalf("a refused pin was written anyway: %q", got)
	}
}

func TestPinningAnUndeclaredToolIsRefused(t *testing.T) {
	h := pinnable(t)
	if _, err := pin(t, h, "ninfer", heldSha); err == nil {
		t.Fatal("pinning a tool that is not declared was accepted")
	}
}

// The YAML editor covers tools now, so everything else on a tool — url, hosts,
// check cadence, notes — is editable from the same place every other kind is.
// Tools were the one config kind the editor did not know about.
func TestToolIsEditableAsYAML(t *testing.T) {
	h := pinnable(t)

	out, err := h.EntryYAML(context.Background(), &EntryYAMLInput{Kind: "tool", Name: "llama.cpp"})
	if err != nil {
		t.Fatalf("read tool yaml: %v", err)
	}
	if !strings.Contains(out.Body.YAML, "ref: master") {
		t.Fatalf("tool yaml does not look like a tool: %q", out.Body.YAML)
	}

	edited := strings.Replace(out.Body.YAML, "ref: master", "ref: master\npin: "+heldSha, 1)
	if err := putYAML(t, h, "tool", "llama.cpp", edited); err != nil {
		t.Fatalf("save tool yaml: %v", err)
	}
	if got := reloadStored(t, h, "after editing a tool").Tools["llama.cpp"].Pin; got != heldSha {
		t.Fatalf("edited pin did not persist: %q", got)
	}
}

// Deleting a tool a model's cmd depends on must NAME the models. ${tool:x}
// refuses rather than falling back to PATH, so the cost of getting this wrong
// is a model that cannot spawn — discovered at the next load, by which time the
// deletion is not the obvious cause.
func TestDeletingAReferencedToolIsRefused(t *testing.T) {
	h := pinnable(t)
	// Authored through the editor so `proxy` gets its real parse — it accepts a
	// bare port, a host:port or an object, and hand-building the node here
	// would be testing the construction rather than the config.
	if err := putYAML(t, h, "model", "qwen",
		"cmd: \"${tool:llama.cpp}/llama-server -m x.gguf\"\nserver: box1\nproxy: 5800\ntype: chat\n"); err != nil {
		t.Fatalf("author the model: %v", err)
	}

	_, err := h.DeleteEntry(context.Background(), &DeleteEntryInput{Kind: "tool", Name: "llama.cpp"})
	if err == nil {
		t.Fatal("deleted a tool a model spawns through")
	}
	if !strings.Contains(err.Error(), "qwen") {
		t.Fatalf("the refusal should name the model that breaks, got: %v", err)
	}
}
