package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/iodesystems/corrallm/internal/agent"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/gpu"
	"github.com/iodesystems/corrallm/internal/proc"
)

// stubProbe makes probeLocal answer for a fixed machine.
func stubProbe(t *testing.T, c agent.Capacity, own proc.OwnMemory) {
	t.Helper()
	prev := probeLocal
	probeLocal = func(*Handlers) localProposal { return proposeLocal("box", c, own) }
	t.Cleanup(func() { probeLocal = prev })
}

// One 24 GB card and 64 GB of RAM, nothing else running.
var smallBox = agent.Capacity{
	GPU:        &agent.DeviceMem{TotalBytes: 24e9},
	Host:       &agent.DeviceMem{TotalBytes: 64e9},
	PerProcess: true,
}

// A fresh install has no server, so a spawned model had nowhere to go and the
// dashboard could not save one. Adding this machine fixes that, once.
func TestAddLocalServerLetsAFreshInstallSpawn(t *testing.T) {
	stubProbe(t, smallBox, proc.OwnMemory{})

	h := storeBackedHandlers(t, &config.Config{})
	ctx := context.Background()

	p, err := h.LocalServerProbe(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Body.Existing != "" || p.Body.Name != "box" || p.Body.Pools != "gpu0 24GB, system 64GB" {
		t.Fatalf("probe = %+v", p.Body)
	}
	if len(p.Body.PoolDetail) != 2 || p.Body.DevicePool != "gpu0" {
		t.Fatalf("probe detail = %+v", p.Body)
	}

	// No body at all: the proposal, reserve included.
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
	// 1 GiB card buffer → 2 GB; 10% of 64 GB (6.4 GB) beats 4 GiB → 7 GB.
	if srv.Reserve["gpu0"] != "2GB" || srv.Reserve["system"] != "7GB" {
		t.Fatalf("stored reserve = %v", srv.Reserve)
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

// An empty body is the same as none: the proposal is written.
func TestAddLocalServerEmptyBodyWritesProposal(t *testing.T) {
	stubProbe(t, smallBox, proc.OwnMemory{})
	h := storeBackedHandlers(t, &config.Config{})
	if _, err := h.AddLocalServer(context.Background(), &AddLocalServerInput{Body: &LocalServerEdit{}}); err != nil {
		t.Fatal(err)
	}
	srv := reloadStored(t, h, "after add").Servers["box"]
	if srv.Pools["system"] != "64GB" || srv.Reserve["system"] != "7GB" {
		t.Fatalf("stored = %+v", srv)
	}
}

// The operator's edits are what is written.
func TestAddLocalServerWritesTheEdit(t *testing.T) {
	stubProbe(t, smallBox, proc.OwnMemory{})
	h := storeBackedHandlers(t, &config.Config{})
	out, err := h.AddLocalServer(context.Background(), &AddLocalServerInput{Body: &LocalServerEdit{
		Name:    "desk",
		Pools:   map[string]string{"gpu0": "22GB", "system": "60GB"},
		Reserve: map[string]string{"gpu0": "4GB", "system": "12GB"},
		ECC:     map[string]bool{"system": true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Body.Server != "desk" {
		t.Fatalf("server = %q", out.Body.Server)
	}
	srv, ok := reloadStored(t, h, "after add").Servers["desk"]
	if !ok {
		t.Fatal("desk not stored")
	}
	if srv.Pools["gpu0"] != "22GB" || srv.Reserve["system"] != "12GB" || !srv.ECC["system"] {
		t.Fatalf("stored = %+v", srv)
	}
	// devicePool left empty keeps the probe's, which is still a pool.
	if srv.DevicePool != "gpu0" || srv.NoProcessMemory {
		t.Fatalf("devicePool = %q, noProcessMemory = %v", srv.DevicePool, srv.NoProcessMemory)
	}
}

func TestAddLocalServerRejectsBadEdits(t *testing.T) {
	stubProbe(t, smallBox, proc.OwnMemory{})
	pools := map[string]string{"gpu0": "24GB", "system": "64GB"}
	for _, tc := range []struct {
		name   string
		edit   LocalServerEdit
		status int
		want   string
	}{
		{"reserve over pool", LocalServerEdit{Pools: pools, Reserve: map[string]string{"gpu0": "25GB"}}, 422, "more than the pool"},
		{"zero pool", LocalServerEdit{Pools: map[string]string{"system": "0GB"}}, 422, "more than 0"},
		{"unparsable size", LocalServerEdit{Pools: map[string]string{"system": "lots"}}, 422, "system"},
		{"bad pool name", LocalServerEdit{Pools: map[string]string{"gpu 0": "24GB"}}, 422, "pool name"},
		{"reserve not a pool", LocalServerEdit{Pools: pools, Reserve: map[string]string{"gpu1": "1GB"}}, 422, "not one of the pools"},
		{"negative reserve", LocalServerEdit{Pools: pools, Reserve: map[string]string{"gpu0": "-1GB"}}, 422, "negative"},
		{"bad name", LocalServerEdit{Name: "my box"}, 422, "server name"},
		// Reaches config validation through mutateConfig.
		{"devices key not a pool", LocalServerEdit{Pools: pools, Devices: map[string]string{"gpu9": "GPU-x"}}, 400, "devices key"},
		{"agent of that name", LocalServerEdit{Name: "rack"}, 409, "another machine"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := storeBackedHandlers(t, &config.Config{Servers: map[string]config.Server{
				"rack": {Agent: &config.AgentBinding{Endpoints: []string{"http://y"}}},
			}})
			edit := tc.edit
			_, err := h.AddLocalServer(context.Background(), &AddLocalServerInput{Body: &edit})
			var se huma.StatusError
			if !errors.As(err, &se) || se.GetStatus() != tc.status || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %d containing %q", err, tc.status, tc.want)
			}
			if n := len(reloadStored(t, h, "after refusal").Servers); n != 1 {
				t.Fatalf("servers = %d, want 1 (nothing written)", n)
			}
		})
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

func poolOf(t *testing.T, p localProposal, pool string) LocalPool {
	t.Helper()
	for _, lp := range p.Pools {
		if lp.Pool == pool {
			return lp
		}
	}
	t.Fatalf("no pool %q in %+v", pool, p.Pools)
	return LocalPool{}
}

const mib = 1024 * 1024

// The reserve is what other processes use now plus a buffer, rounded up to a
// whole GB and never more than the pool. A card's memory held by corrallm's own
// backends is not "other processes".
func TestProposeLocalReserve(t *testing.T) {
	c := agent.Capacity{
		GPUs: []agent.GPUDevice{
			{UUID: "GPU-small", DeviceMem: agent.DeviceMem{Name: "small card", TotalBytes: 10 * 1024 * mib, UsedBytes: 9728 * mib}},
			{UUID: "GPU-big", ECC: true, DeviceMem: agent.DeviceMem{Name: "big card", TotalBytes: 32 * 1024 * mib, UsedBytes: 3 * 1024 * mib}},
		},
		Host:       &agent.DeviceMem{TotalBytes: 128e9, UsedBytes: 10e9},
		HostECC:    true,
		PerProcess: true,
	}
	c.GPU = &c.GPUs[0].DeviceMem
	p := proposeLocal("box", c, proc.OwnMemory{DeviceMiB: map[string]int{"GPU-big": 2048, "GPU-other": 4096}, AnonBytes: 4e9})

	big := poolOf(t, p, "gpu0")
	if big.UUID != "GPU-big" || big.Card != "big card" || !big.Bound || !big.ECC {
		t.Errorf("gpu0 = %+v", big)
	}
	// 3 GiB used − 2 GiB ours = 1 GiB, + 1 GiB buffer = 2.15 GB → 3 GB.
	if big.InUseBytes != 1024*mib || big.BufferBytes != 1024*mib || big.ReserveBytes != 3e9 {
		t.Errorf("gpu0 reserve = %+v", big)
	}
	if big.Reason != "in use now 1.1 GB + 1.1 GB buffer" {
		t.Errorf("gpu0 reason = %q", big.Reason)
	}

	small := poolOf(t, p, "gpu1")
	// 9.5 GiB + 1 GiB = 11.3 GB → 12 GB, capped at the 10 GB pool.
	if small.TotalBytes != 10e9 || small.ReserveBytes != 10e9 || small.ECC ||
		!strings.Contains(small.Reason, "capped") {
		t.Errorf("gpu1 = %+v", small)
	}

	sys := poolOf(t, p, "system")
	// 10 GB used − 4 GB our backends' anonymous memory + 12.8 GB (10% beats
	// 4 GiB) = 18.8 GB → 19 GB.
	if sys.InUseBytes != 6e9 || sys.BufferBytes != 12.8e9 || sys.ReserveBytes != 19e9 || !sys.ECC || sys.Card != "" {
		t.Errorf("system = %+v", sys)
	}
	if p.Server.Reserve["gpu0"] != "3GB" || p.Server.Reserve["gpu1"] != "10GB" || p.Server.Reserve["system"] != "19GB" {
		t.Errorf("server reserve = %v", p.Server.Reserve)
	}
}

// Small RAM: the 4 GiB floor beats 10%. One card is not bound, but still named.
func TestProposeLocalSmallHostBufferFloor(t *testing.T) {
	c := agent.Capacity{
		GPUs: []agent.GPUDevice{{UUID: "GPU-only", DeviceMem: agent.DeviceMem{Name: "only card",
			TotalBytes: 8 * 1024 * mib, UsedBytes: 512 * mib}}},
		Host: &agent.DeviceMem{TotalBytes: 16e9, UsedBytes: 2e9},
	}
	c.GPU = &c.GPUs[0].DeviceMem
	p := proposeLocal("box", c, proc.OwnMemory{})
	sys := poolOf(t, p, "system")
	// 2 GB + 4 GiB (4.29 GB) = 6.29 GB → 7 GB.
	if sys.BufferBytes != 4*1024*mib || sys.ReserveBytes != 7e9 {
		t.Errorf("system = %+v", sys)
	}
	g := poolOf(t, p, "gpu0")
	if g.Card != "only card" || g.UUID != "GPU-only" || g.Bound || p.Server.Devices != nil {
		t.Errorf("gpu0 = %+v, devices = %v", g, p.Server.Devices)
	}
	// 0.5 GiB + 1 GiB = 1.61 GB → 2 GB.
	if g.ReserveBytes != 2e9 {
		t.Errorf("gpu0 reserve = %d", g.ReserveBytes)
	}
}

// Unified memory keeps the wired-limit reserve, raised to the system rule when
// that asks for more.
func TestProposeLocalUnified(t *testing.T) {
	mac := func(used int64) agent.Capacity {
		return agent.Capacity{
			Unified: true,
			GPU:     &agent.DeviceMem{TotalBytes: 48 * 1024 * mib},
			Host:    &agent.DeviceMem{TotalBytes: 64 * 1024 * mib, UsedBytes: used},
		}
	}
	// Idle: 2 GB + 6.87 GB → 9 GB, below the 18 GB the GPU may not wire.
	idle := poolOf(t, proposeLocal("mac", mac(2e9), proc.OwnMemory{}), "system")
	if idle.ReserveBytes != 18e9 || !strings.Contains(idle.Reason, "may not wire") || idle.ECC {
		t.Errorf("idle = %+v", idle)
	}
	// Busy: 30 GB + 6.87 GB → 37 GB, above it.
	busy := proposeLocal("mac", mac(30e9), proc.OwnMemory{})
	if lp := poolOf(t, busy, "system"); lp.ReserveBytes != 37e9 || busy.Server.Reserve["system"] != "37GB" {
		t.Errorf("busy = %+v, reserve %v", lp, busy.Server.Reserve)
	}
}
