package tune

import (
	"path/filepath"
	"testing"
	"time"
)

// Keyed by PLACEMENT, not by server or model. Two placements of one model are
// the case the whole design exists for, and a coarser key would have the second
// silently overwrite the first.
func TestCapabilitiesAreKeyedPerPlacement(t *testing.T) {
	c, err := New(filepath.Join(t.TempDir(), "vram-profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	c.PutCapabilities("box1", "qwen", Capabilities{
		ContextLength: 180000, Slots: 4, Modalities: []string{"text", "image"}, Tools: true})
	c.PutCapabilities("mac1", "qwen", Capabilities{
		ContextLength: 65536, Slots: 1, Modalities: []string{"text"}})

	box, ok := c.CapabilitiesFor("box1", "qwen")
	if !ok || box.ContextLength != 180000 || !box.Supports("image") {
		t.Errorf("box1 record wrong: %+v", box)
	}
	mac, ok := c.CapabilitiesFor("mac1", "qwen")
	if !ok || mac.ContextLength != 65536 {
		t.Errorf("mac1 record wrong: %+v", mac)
	}
	// The one that matters: the same weights, different cmd, different answer.
	if mac.Supports("image") {
		t.Error("mac1 inherited box1's vision — placements must not share a record")
	}
	if _, ok := c.CapabilitiesFor("nowhere", "qwen"); ok {
		t.Error("an unprobed placement reported capabilities")
	}
}

// A record describes the COMMAND it was taken from. Add --mmproj and vision
// appears; drop it and vision goes, with nothing about the model name changing.
func TestCapabilitiesGoStaleWhenTheCommandChanges(t *testing.T) {
	caps := Capabilities{ProbedCmd: "llama-server -m x.gguf --mmproj p.gguf"}
	if caps.StaleFor("llama-server -m x.gguf --mmproj p.gguf") {
		t.Error("identical command reported stale")
	}
	if !caps.StaleFor("llama-server -m x.gguf") {
		t.Error("a command that dropped the projector must be reported stale — " +
			"the recorded vision capability no longer describes what runs")
	}
	// Never probed: nothing to be stale about, and claiming staleness would
	// make an unprobed placement look probed-and-outdated.
	if (Capabilities{}).StaleFor("anything") {
		t.Error("an unprobed record claimed staleness")
	}
}

// Capabilities must survive a restart, or every daemon bounce silently
// un-probes the fleet and models stop advertising what they can do.
func TestCapabilitiesPersistAcrossReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vram-profile.json")
	c, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	c.PutCapabilities("mac1", "qwen", Capabilities{
		Modalities: []string{"text", "image"}, Tools: true, ProbedAt: time.Now()})

	// Reopened by a different Cache, as a restart would.
	again, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := again.CapabilitiesFor("mac1", "qwen")
	if !ok || !got.Supports("image") || !got.Tools {
		t.Errorf("capabilities did not survive reload: %+v (found=%v)", got, ok)
	}
}

// The profile file keeps its existing flat shape; capabilities live beside it.
// Nesting them in would make every profile file on disk unparseable.
func TestCapabilitiesDoNotDisturbTheProfileFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vram-profile.json")
	c, _ := New(path)
	c.Update("Apple M1 Max", "qwen", Profile{PeakMiB: 34431})
	c.PutCapabilities("mac1", "qwen", Capabilities{Modalities: []string{"text"}})

	again, err := New(path)
	if err != nil {
		t.Fatalf("profile file became unreadable: %v", err)
	}
	if p, ok := again.Get("Apple M1 Max", "qwen"); !ok || p.PeakMiB != 34431 {
		t.Errorf("profile lost: %+v (found=%v)", p, ok)
	}
}
