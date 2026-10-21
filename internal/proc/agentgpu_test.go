package proc

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/agent"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/tune"
)

// box1 moved under an agent beside its primary: two cards, pools bound by
// UUID, and nvidia-smi's index 0 is the SMALL card.
const (
	box1UUID3080 = "GPU-76a4c775-a47f-61b9-3a9f-9c7d5edfc544"
	box1UUID5090 = "GPU-ee90af07-0882-d325-182e-87137ec6d47b"
	name3080     = "NVIDIA GeForce RTX 3080"
	name5090     = "NVIDIA GeForce RTX 5090"
	miB          = int64(1024 * 1024)
)

func box1AgentCfg(t *testing.T, endpoint string) *config.Config {
	t.Helper()
	cfg, err := config.LoadBytesForTest([]byte(`
servers:
  box1:
    pools: { gpu0: 32607MiB, gpu1: 10240MiB, system: 125GB }
    devices: { gpu0: ` + box1UUID5090 + `, gpu1: ` + box1UUID3080 + ` }
    agent: { endpoints: ["` + endpoint + `"], token: t }
models:
  big:
    cmd: "exec llama-server --port 5800"
    server: box1
    pool: gpu0
    proxy: 5800
  small:
    cmd: "exec llama-server --port 5801"
    server: box1
    pool: gpu1
    proxy: 5801
  split:
    cmd: "exec llama-server --port 5802 -ts 30,6"
    server: box1
    ramUsage: { gpu0: 30GB, gpu1: 7GB }
    proxy: 5802
`))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// box1Capacity is what the agent on box1 reports: the single GPU field is the
// 3080 (index 0), the list carries both cards.
func box1Capacity(perCard bool) agent.Capacity {
	c := agent.Capacity{
		GPU:        &agent.DeviceMem{Name: name3080, TotalBytes: 10240 * miB, UsedBytes: 1000 * miB},
		Host:       &agent.DeviceMem{Name: "system", TotalBytes: 125e9, UsedBytes: 40e9},
		PerProcess: true,
	}
	if perCard {
		c.GPUs = []agent.GPUDevice{
			{DeviceMem: agent.DeviceMem{Name: name3080, TotalBytes: 10240 * miB, UsedBytes: 1000 * miB}, Index: 0, UUID: box1UUID3080},
			{DeviceMem: agent.DeviceMem{Name: name5090, TotalBytes: 32607 * miB, UsedBytes: 29000 * miB}, Index: 1, UUID: box1UUID5090},
		}
	}
	return c
}

func box1Mgr(t *testing.T, endpoint string, perCard bool) *Manager {
	t.Helper()
	// This machine's own probe must not be what answers: fail it, so any
	// card named below can only have come from the agent's report.
	fakeNvidiaSMIFailing(t)
	m := NewManager(box1AgentCfg(t, endpoint))
	live := agent.NewLiveness()
	live.Beat("box1", time.Now())
	live.RecordCapacity("box1", box1Capacity(perCard))
	m.SetLiveness(live)
	return m
}

// Defect 3: every box1 profile was filed under the agent's single device name
// (the 3080), while tuneCmd read under the pool's card. Both now resolve to
// the card the model's pool names, from the agent's per-card list.
func TestAgentDeviceName_IsThePoolsCard(t *testing.T) {
	m := box1Mgr(t, "http://127.0.0.1:1", true)
	big := m.config().Models["big"]
	if got := m.deviceNameFor("box1", big.Pool, big.RAMUsage); got != name5090 {
		t.Errorf("write key for a gpu0 model = %q, want %q", got, name5090)
	}
	st, err := m.modelDevice("big")
	if err != nil || st.Name != name5090 || st.TotalMiB != 32607 {
		t.Errorf("read key for a gpu0 model = %+v (%v), want the 5090", st, err)
	}
	small := m.config().Models["small"]
	if got := m.deviceNameFor("box1", small.Pool, small.RAMUsage); got != name3080 {
		t.Errorf("write key for a gpu1 model = %q, want %q", got, name3080)
	}
}

// An agent older than the per-card list keeps the name it always gave.
func TestAgentDeviceName_OlderAgentUnchanged(t *testing.T) {
	m := box1Mgr(t, "http://127.0.0.1:1", false)
	big := m.config().Models["big"]
	if got := m.deviceNameFor("box1", big.Pool, big.RAMUsage); got != name3080 {
		t.Errorf("older agent: %q, want its single reported name %q", got, name3080)
	}
}

// Defect 4: the 3080's usage was charged as foreign usage on the 32 GB gpu0
// pool, and gpu1 got no correction. Each bound pool now reads its own card.
func TestAgentForeignUsage_PerCard(t *testing.T) {
	m := box1Mgr(t, "http://127.0.0.1:1", true)
	m.mu.Lock()
	defer m.mu.Unlock()
	if got := m.foreignUsedLocked("box1", "gpu0"); got != 29000*miB {
		t.Errorf("gpu0 foreign = %d MiB, want the 5090's 29000", got/miB)
	}
	if got := m.foreignUsedLocked("box1", "gpu1"); got != 1000*miB {
		t.Errorf("gpu1 foreign = %d MiB, want the 3080's 1000", got/miB)
	}
	if got := m.foreignUsedLocked("box1", "system"); got != 0 {
		t.Errorf("system foreign = %d, want 0 (not the device pool)", got)
	}

	// What corrallm placed on a card is not foreign.
	m.procs["big"] = &Process{Name: "big", key: "big", server: "box1",
		usage: map[string]int64{"gpu0": 20000 * miB}, state: StateReady, ready: make(chan struct{})}
	if got := m.foreignUsedLocked("box1", "gpu0"); got != 9000*miB {
		t.Errorf("gpu0 foreign with 20000 MiB placed = %d MiB, want 9000", got/miB)
	}
	if got := m.foreignUsedLocked("box1", "gpu1"); got != 1000*miB {
		t.Errorf("gpu1 foreign moved with a gpu0 placement: %d MiB", got/miB)
	}
}

// An older agent keeps the single-GPU arithmetic exactly.
func TestAgentForeignUsage_OlderAgentUnchanged(t *testing.T) {
	m := box1Mgr(t, "http://127.0.0.1:1", false)
	m.mu.Lock()
	defer m.mu.Unlock()
	if got := m.foreignUsedLocked("box1", "gpu0"); got != 1000*miB {
		t.Errorf("gpu0 = %d MiB, want the single reading (1000) as before", got/miB)
	}
	if got := m.foreignUsedLocked("box1", "gpu1"); got != 0 {
		t.Errorf("gpu1 = %d, want 0 as before", got)
	}
}

// Several cards and a device pool bound to none: no reading, not the first
// card's.
func TestAgentForeignUsage_UnboundPoolOnAMultiCardAgent(t *testing.T) {
	cfg, err := config.LoadBytesForTest([]byte(`
servers:
  box1:
    pools: { gpu0: 32607MiB, gpu1: 10240MiB }
    devices: { gpu1: ` + box1UUID3080 + ` }
    agent: { endpoints: ["http://127.0.0.1:1"], token: t }
`))
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(cfg)
	live := agent.NewLiveness()
	live.RecordCapacity("box1", box1Capacity(true))
	m.SetLiveness(live)
	m.mu.Lock()
	defer m.mu.Unlock()
	if got := m.foreignUsedLocked("box1", "gpu0"); got != 0 {
		t.Errorf("unbound gpu0 = %d MiB, want 0", got/miB)
	}
	if got := m.foreignUsedLocked("box1", "gpu1"); got != 1000*miB {
		t.Errorf("bound gpu1 = %d MiB, want 1000", got/miB)
	}
}

// statusOnlyAgent answers status polls for one backend with a fixed per-card
// split.
func statusOnlyAgent(t *testing.T, backend agent.Backend) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agent/v1/hello", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(agent.Hello{Protocol: agent.Protocol})
	})
	mux.HandleFunc("GET /agent/v1/backends/{id}", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(backend)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// Defect 2: measurePerDevice never ran for an agent backend, so a model split
// across box1's cards was filed as one whole-host total. Through a real
// RemoteHost handle it now records each card's share under that card.
func TestAgentMeasure_PerCard(t *testing.T) {
	url := statusOnlyAgent(t, agent.Backend{ID: "b1", Alive: true, MemoryMiB: 36000,
		MemoryByDevice: map[string]int{box1UUID5090: 30000, box1UUID3080: 6000}})
	m := box1Mgr(t, url, true)
	cache, err := tune.New(filepath.Join(t.TempDir(), "vram-profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	m.SetTuneCache(cache)

	h, err := agent.NewRemoteHost("box1", []string{url}, "t").Attach("b1", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.(interface{ Detach() }).Detach()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := h.MemoryOnMiB(box1UUID5090); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no status poll answered")
		}
		time.Sleep(20 * time.Millisecond)
	}

	m.measure("split", m.config().Models["split"], &Process{logs: newLogBuffer(10)}, h)
	if p, ok := cache.Get(name5090, "split"); !ok || p.PeakMiB != 30000 {
		t.Errorf("5090 profile = %+v ok=%v, want PeakMiB 30000", p, ok)
	}
	if p, ok := cache.Get(name3080, "split"); !ok || p.PeakMiB != 6000 {
		t.Errorf("3080 profile = %+v ok=%v, want PeakMiB 6000", p, ok)
	}

	// A one-card model on the agent: the whole footprint, filed under the
	// card its pool names — the key tuneCmd reads.
	m.measure("big", m.config().Models["big"], &Process{logs: newLogBuffer(10)}, h)
	if p, ok := cache.Get(name5090, "big"); !ok || p.PeakMiB != 36000 {
		t.Errorf("gpu0 model profile = %+v ok=%v, want PeakMiB 36000 under the 5090", p, ok)
	}
	if _, ok := cache.Get(name3080, "big"); ok {
		t.Error("a gpu0 model was filed under the 3080")
	}
}
