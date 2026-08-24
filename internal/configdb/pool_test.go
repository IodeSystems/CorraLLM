package configdb

import (
	"context"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

// `pool:` has NO COLUMN, by design, and this pins that the remainder carries it.
//
// Same reasoning as the tool pin: the schema is applied with CREATE TABLE IF NOT
// EXISTS, so a new column would never appear on the live database and every read
// would fail. It rides in the unprojected remainder instead.
//
// The failure this prevents is quiet and expensive. `pool:` is the only
// statement of WHICH card a model is on once ramUsage stops being declared. If
// it dropped on a config write, the model would fall back to the server-wide
// default pool — so a model running on gpu1 would be charged to gpu0, inflating
// one budget while leaving the other looking free, and the scheduler would place
// a second model onto memory that is already spoken for. Nothing would error.
func TestModelPoolSurvivesTheStore(t *testing.T) {
	ctx := context.Background()
	src := &Source{DB: openDB(t)}

	in := &config.Config{
		Servers: map[string]config.Server{"box1": {
			Pools:   map[string]string{"gpu0": "31654MiB", "gpu1": "9877MiB"},
			Devices: map[string]string{"gpu0": "GPU-ee90af07", "gpu1": "GPU-76a4c775"},
		}},
		Models: map[string]config.Model{
			"chandra-ocr-2": {Cmd: "llama-server", Server: "box1", Pool: "gpu1"},
		},
	}
	if err := src.Save(ctx, in); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := src.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if p := got.Models["chandra-ocr-2"].Pool; p != "gpu1" {
		t.Fatalf("pool lost on the round trip: %q — the model would be charged to the wrong card", p)
	}
}

// A placement's pool rides inside placements_json, which is a different path
// through the store than the model-level field. Two placements on two cards is
// exactly the case the model-level field cannot express, so losing this one
// costs more than losing the other.
func TestPlacementPoolSurvivesTheStore(t *testing.T) {
	ctx := context.Background()
	src := &Source{DB: openDB(t)}

	in := &config.Config{
		Servers: map[string]config.Server{"box1": {
			Pools:   map[string]string{"gpu0": "31654MiB", "gpu1": "9877MiB"},
			Devices: map[string]string{"gpu0": "GPU-ee90af07", "gpu1": "GPU-76a4c775"},
		}},
		Models: map[string]config.Model{
			"deepseek-v4-flash": {Placements: []config.Placement{
				{Name: "big", Server: "box1", Cmd: "llama-server -ngl 99", Pool: "gpu0"},
				{Name: "small", Server: "box1", Cmd: "llama-server -ngl 40", Pool: "gpu1"},
			}},
		},
	}
	if err := src.Save(ctx, in); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := src.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	ps := got.Models["deepseek-v4-flash"].PlacementList()
	if len(ps) != 2 {
		t.Fatalf("got %d placements, want 2", len(ps))
	}
	for _, p := range ps {
		want := map[string]string{"big": "gpu0", "small": "gpu1"}[p.Name]
		if p.Pool != want {
			t.Errorf("placement %q pool = %q, want %q", p.Name, p.Pool, want)
		}
	}
}

// ramUsage and pool must both survive TOGETHER. The split's whole premise is
// that they answer different questions, so a round trip that kept one and
// dropped the other would re-couple them silently — and the surviving field
// would look authoritative.
func TestPoolAndRAMUsageBothSurvive(t *testing.T) {
	ctx := context.Background()
	src := &Source{DB: openDB(t)}

	in := &config.Config{
		Servers: map[string]config.Server{"box1": {
			Pools:   map[string]string{"gpu0": "31654MiB", "gpu1": "9877MiB"},
			Devices: map[string]string{"gpu0": "GPU-ee90af07", "gpu1": "GPU-76a4c775"},
		}},
		Models: map[string]config.Model{
			"nomic-embed-text": {
				Cmd: "llama-server", Server: "box1", Pool: "gpu1",
				RAMUsage: map[string]string{"gpu1": "816MiB"},
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
	m := got.Models["nomic-embed-text"]
	if m.Pool != "gpu1" {
		t.Errorf("pool = %q, want gpu1", m.Pool)
	}
	if m.RAMUsage["gpu1"] != "816MiB" {
		t.Errorf("ramUsage = %v, want gpu1=816MiB", m.RAMUsage)
	}
}
