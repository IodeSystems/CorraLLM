//go:build unix

package proc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/agent"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/tune"
)

// box1's local nvidia-smi: index 0 is the 3080, index 1 the 5090. The agent
// runs on this same machine, so this is what a fallback to the local probe
// would read.
func fakeBox1NvidiaSMI(t *testing.T) {
	t.Helper()
	fakeNvidiaSMIDevices(t,
		"0, "+box1UUID3080+", 00000000:03:00.0, "+name3080+", 10240, 1000, 9240\n"+
			"1, "+box1UUID5090+", 00000000:04:00.0, "+name5090+", 32607, 29000, 3607",
		"")
}

// An agent-backed server whose agent has not reported yet has no knowable
// device. Before the fix this fell through to the local probe: index 0, the
// 3080, for a model whose pool is bound to the 5090.
func TestAgentDeviceName_NoReportYetIsNotTheLocalProbe(t *testing.T) {
	fakeBox1NvidiaSMI(t)
	m := NewManager(box1AgentCfg(t, "http://127.0.0.1:1"))
	m.SetLiveness(agent.NewLiveness()) // a successor: no heartbeat yet
	big := m.config().Models["big"]
	if got := m.deviceNameFor("box1", big.Pool, big.RAMUsage); got != "" {
		t.Errorf("no agent report: device = %q, want \"\" (not this machine's index 0)", got)
	}
	if got := m.DeviceNameForModel("big"); got != "" {
		t.Errorf("DeviceNameForModel = %q, want \"\"", got)
	}
}

// adoptAgent answers status polls for every backend with a fixed footprint.
func adoptAgent(t *testing.T, memMiB int) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agent/v1/hello", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(agent.Hello{Protocol: agent.Protocol})
	})
	mux.HandleFunc("GET /agent/v1/backends/{id}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(agent.Backend{ID: r.PathValue("id"), Alive: true, MemoryMiB: memMiB})
	})
	mux.HandleFunc("GET /agent/v1/backends/{id}/logs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"lines":[],"next":1}`))
	})
	mux.HandleFunc("POST /agent/v1/backends/{id}/signal", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// The handoff path, reproduced: a successor adopts box1's gpu0 model from the
// agent's list (primeAgents) BEFORE the agent's first heartbeat. The adopted
// footprint must be filed under the 5090 the pool names — once the agent has
// reported — and never under the local probe's index 0 (box1 2026-10-21
// 01:43:31: Swift's 29202 MiB filed under the 3080).
func TestAdoptedBackend_MeasuredUnderThePoolsCard(t *testing.T) {
	fakeBox1NvidiaSMI(t)
	defer func(d, w, p time.Duration) { adoptMeasureDelay, adoptMeasureWait, devicesKnownPoll = d, w, p }(
		adoptMeasureDelay, adoptMeasureWait, devicesKnownPoll)
	adoptMeasureDelay, adoptMeasureWait, devicesKnownPoll = 50*time.Millisecond, 10*time.Second, 20*time.Millisecond

	agentURL := adoptAgent(t, 29202)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(backend.Close)
	u, _ := url.Parse(backend.URL)
	var port int
	_, _ = fmt.Sscanf(u.Port(), "%d", &port)
	cmd := fmt.Sprintf("exec llama-server --port %d", port)
	cfg, err := config.LoadBytesForTest([]byte(fmt.Sprintf(`
servers:
  box1:
    pools: { gpu0: 32607MiB, gpu1: 10240MiB, system: 125GB }
    devices: { gpu0: %s, gpu1: %s }
    agent: { endpoints: [%q], token: t }
models:
  swift:
    cmd: %q
    server: box1
    pool: gpu0
    ramUsage: { gpu0: 31000MiB }
    proxy: %d
`, box1UUID5090, box1UUID3080, agentURL, cmd, port)))
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(cfg)
	live := agent.NewLiveness()
	m.SetLiveness(live)
	cache, err := tune.New(filepath.Join(t.TempDir(), "vram-profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	// The junk entry the defect left behind: a gpu0 model's profile under the
	// 3080. It must neither steer the adopted model's charge nor be read as
	// this model's profile.
	cache.Update(name3080, "swift", tune.Profile{BaseMiB: 29202, PeakMiB: 29202, MeasuredSlots: 1, Source: tune.SourceServing})
	m.SetTuneCache(cache)

	a, r, v := m.ReconcileAgent(context.Background(), "box1", []agent.Backend{{
		ID: "b1-x", Key: "swift", Model: "swift", Alive: true, Started: old(),
		Cmd: cmd, ConfigCmd: cmd, Port: port,
	}})
	if a != 1 || r != 0 || v != 0 {
		t.Fatalf("adopted=%d reaped=%d vanished=%d; want 1/0/0", a, r, v)
	}
	m.mu.Lock()
	p := m.procs["swift"]
	m.mu.Unlock()
	if p == nil {
		t.Fatal("not adopted")
	}
	defer stopWatching(p.handle)
	if got := p.usage["gpu0"]; got != 31000*miB {
		t.Errorf("adopted charge on gpu0 = %d MiB, want the configured 31000 (not the 3080's junk profile)", got/miB)
	}

	// Well past the delay with no agent report: nothing may be measured.
	time.Sleep(300 * time.Millisecond)
	if got, _ := cache.Get(name3080, "swift"); got.PeakMiB != 29202 || len(got.Samples) != 0 {
		t.Errorf("3080 entry changed before the agent reported: %+v", got)
	}
	if _, ok := cache.Get(name5090, "swift"); ok {
		t.Fatal("measured before the agent reported its cards")
	}

	// The agent's first heartbeat.
	live.RecordCapacity("box1", box1Capacity(true))
	deadline := time.Now().Add(5 * time.Second)
	for {
		if prof, ok := cache.Get(name5090, "swift"); ok {
			if prof.PeakMiB != 29202 {
				t.Errorf("5090 profile = %+v, want PeakMiB 29202", prof)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("adopted backend never measured under the 5090")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got, _ := cache.Get(name3080, "swift"); len(got.Samples) != 0 {
		t.Errorf("adopted measurement also filed under the 3080: %+v", got)
	}

	// The stale 3080 entry is not this model's profile.
	if prof, ok := m.TuneProfileForModel("swift"); !ok || len(prof.Samples) == 0 {
		t.Errorf("TuneProfileForModel = %+v ok=%v, want the 5090 measurement", prof, ok)
	}
}

// No report within the bound: the adopted backend is left unmeasured.
func TestAdoptedBackend_NoReportNoMeasurement(t *testing.T) {
	m := NewManager(box1AgentCfg(t, "http://127.0.0.1:1"))
	m.SetLiveness(agent.NewLiveness())
	defer func(p time.Duration) { devicesKnownPoll = p }(devicesKnownPoll)
	devicesKnownPoll = 10 * time.Millisecond
	if m.waitDevicesKnown("box1", nil, 50*time.Millisecond) {
		t.Error("devices known with no agent report")
	}
	done := make(chan struct{})
	close(done)
	if m.waitDevicesKnown("box1", done, time.Minute) {
		t.Error("devices known after the backend exited")
	}
}
