package api

import (
	"context"
	"testing"

	"github.com/iodesystems/corrallm/internal/agent"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/gpu"
)

// A fresh install has no server, so a spawned model had nowhere to go and the
// dashboard could not save one. Adding this machine fixes that, once.
func TestAddLocalServerLetsAFreshInstallSpawn(t *testing.T) {
	prev := probeLocal
	probeLocal = func() (string, config.Server) {
		return "box", localServerFrom(agent.Capacity{
			GPU:        &agent.DeviceMem{TotalBytes: 24e9},
			Host:       &agent.DeviceMem{TotalBytes: 64e9},
			PerProcess: true,
		}, nil)
	}
	t.Cleanup(func() { probeLocal = prev })

	h := storeBackedHandlers(t, &config.Config{})
	ctx := context.Background()

	p, err := h.LocalServerProbe(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Body.Existing != "" || p.Body.Name != "box" || p.Body.Pools != "gpu0 24GB, system 64GB" {
		t.Fatalf("probe = %+v", p.Body)
	}

	out, err := h.AddLocalServer(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Body.Server != "box" {
		t.Fatalf("server = %q", out.Body.Server)
	}
	srv := reloadStored(t, h, "after add").Servers["box"]
	if srv.Pools["gpu0"] != "24GB" || srv.DevicePool != "gpu0" || srv.Agent != nil {
		t.Fatalf("stored server = %+v", srv)
	}

	// A second add returns the same server and writes nothing new.
	again, err := h.AddLocalServer(ctx, nil)
	if err != nil || again.Body.Server != "box" {
		t.Fatalf("second add = %+v, %v", again, err)
	}
	if n := len(reloadStored(t, h, "after second add").Servers); n != 1 {
		t.Fatalf("servers = %d, want 1", n)
	}

	// The dashboard's own call: the first model under a `local` provider that
	// does not exist yet.
	in := &UpsertModelInput{Name: "local-tiny", Provider: "local"}
	in.Body = ModelSpec{Name: "local-tiny", Cmd: "llama-server -m /m.gguf --port 5899",
		Server: "box", Proxy: "5899", Type: "chat"}
	if _, err := h.UpsertModel(ctx, in); err != nil {
		t.Fatalf("first local model refused: %v", err)
	}
	if m, ok := reloadStored(t, h, "after model").Providers["local"].Models["tiny"]; !ok || m.Server != "box" {
		t.Fatalf("model not stored under the local provider on box: %+v", m)
	}
}

// With two cards, each gets a pool bound to its UUID, the larger as gpu0.
func TestLocalServerFromBindsEveryCard(t *testing.T) {
	srv := localServerFrom(agent.Capacity{
		GPU:  &agent.DeviceMem{TotalBytes: 10e9},
		Host: &agent.DeviceMem{TotalBytes: 128e9},
	}, []gpu.Stats{
		{UUID: "GPU-small", TotalMiB: 9537},
		{UUID: "GPU-big", TotalMiB: 30518},
	})
	if srv.Pools["gpu0"] != "32GB" || srv.Pools["gpu1"] != "10GB" || srv.Pools["system"] != "128GB" {
		t.Errorf("pools = %v", srv.Pools)
	}
	if srv.Devices["gpu0"] != "GPU-big" || srv.Devices["gpu1"] != "GPU-small" {
		t.Errorf("devices = %v", srv.Devices)
	}
	if !srv.NoProcessMemory {
		t.Errorf("PerProcess false must mark NoProcessMemory")
	}
}
