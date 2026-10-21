package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/host"
)

// box1's two cards in nvidia-smi's order: the 3080 sits on the lower PCI bus,
// so it is index 0 although the 5090 is the card most pools budget.
const (
	uuid3080 = "GPU-76a4c775-a47f-61b9-3a9f-9c7d5edfc544"
	uuid5090 = "GPU-ee90af07-0882-d325-182e-87137ec6d47b"
)

// fakeSMI puts an nvidia-smi on PATH that prints box1's two cards.
func fakeSMI(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	rows := "0, " + uuid3080 + ", 00000000:03:00.0, NVIDIA GeForce RTX 3080, 10240, 1000, 9240\n" +
		"1, " + uuid5090 + ", 00000000:0A:00.0, NVIDIA GeForce RTX 5090, 32607, 29000, 3607"
	script := "#!/bin/sh\ncase \"$1\" in\n  --query-gpu=*) echo '" + rows + "' ;;\n  --query-compute-apps=*) ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "nvidia-smi"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// Defect: the agent reported ONE GPU, nvidia-smi's index 0 — the 3080 on box1.
// It now reports every card with its identity, and keeps the single field
// unchanged for primaries that only read that.
func TestProbe_ReportsEveryCard(t *testing.T) {
	fakeSMI(t)
	c := Probe()
	if c.GPU == nil || c.GPU.Name != "NVIDIA GeForce RTX 3080" {
		t.Fatalf("GPU = %+v, want the first card (the 3080) for older primaries", c.GPU)
	}
	if len(c.GPUs) != 2 {
		t.Fatalf("GPUs = %+v, want both cards", c.GPUs)
	}
	devs := c.DeviceStats()
	if devs[1].UUID != uuid5090 || devs[1].Name != "NVIDIA GeForce RTX 5090" ||
		devs[1].TotalMiB != 32607 || devs[1].UsedMiB != 29000 || devs[1].PCIBusID != "00000000:0A:00.0" {
		t.Errorf("5090 = %+v", devs[1])
	}
	if devs[0].UUID != uuid3080 || devs[0].UsedMiB != 1000 {
		t.Errorf("3080 = %+v", devs[0])
	}

	// The wire is additive: an older primary decodes the same `gpu` it did.
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var old struct {
		GPU *DeviceMem `json:"gpu"`
	}
	if err := json.Unmarshal(b, &old); err != nil || old.GPU == nil || old.GPU.Name != "NVIDIA GeForce RTX 3080" {
		t.Errorf("older primary reads gpu = %+v (%v)", old.GPU, err)
	}
}

// An agent older than the per-card list sends none; DeviceStats says so with
// nil rather than inventing a card from the single reading.
func TestDeviceStats_AbsentFromAnOlderAgent(t *testing.T) {
	var c Capacity
	if err := json.Unmarshal([]byte(`{"gpu":{"name":"x","totalBytes":1}}`), &c); err != nil {
		t.Fatal(err)
	}
	if d := c.DeviceStats(); d != nil {
		t.Errorf("DeviceStats = %+v, want nil", d)
	}
}

// memHandle is a handle with a fixed per-card footprint, or none.
type memHandle struct {
	byDev map[string]int
	err   error
}

func (memHandle) ID() string                      { return "pgid:1" }
func (memHandle) Done() <-chan struct{}           { return make(chan struct{}) }
func (memHandle) Err() error                      { return nil }
func (memHandle) Signal(host.Sig) error           { return nil }
func (memHandle) Alive() bool                     { return true }
func (memHandle) MemoryMiB() (int, error)         { return 777, nil }
func (memHandle) MemoryOnMiB(string) (int, error) { return 0, nil }
func (h memHandle) MemoryByDeviceMiB() (map[string]int, error) {
	return h.byDev, h.err
}

// Defect: a backend's status carried one aggregate, so the primary could not
// measure a two-card model per card. The status now carries the split, and the
// total is its sum.
func TestView_ReportsMemoryPerCard(t *testing.T) {
	s := New("test", "")
	b := &supervised{id: "b1", started: time.Now(), logs: newLogRing(4),
		handle: memHandle{byDev: map[string]int{uuid5090: 29000, uuid3080: 6000}}}
	v := s.view(b)
	if v.MemoryByDevice[uuid5090] != 29000 || v.MemoryByDevice[uuid3080] != 6000 {
		t.Errorf("MemoryByDevice = %v", v.MemoryByDevice)
	}
	if v.MemoryMiB != 35000 {
		t.Errorf("MemoryMiB = %d, want the split's sum 35000", v.MemoryMiB)
	}

	// Attributable but empty is an empty object, not null: "nothing on any
	// card" is a real answer.
	b.handle = memHandle{byDev: map[string]int{}}
	raw, _ := json.Marshal(s.view(b))
	if !strings.Contains(string(raw), `"memoryByDevice":{}`) {
		t.Errorf("empty split encoded as %s", raw)
	}

	// Cannot split: null, and the aggregate path as before.
	b.handle = memHandle{err: os.ErrNotExist}
	v = s.view(b)
	raw, _ = json.Marshal(v)
	if v.MemoryByDevice != nil || !strings.Contains(string(raw), `"memoryByDevice":null`) || v.MemoryMiB != 777 {
		t.Errorf("unsplittable handle: %s", raw)
	}
}

// statusAgent answers a backend status poll with body.
func statusAgent(t *testing.T, body string) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agent/v1/hello", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(Hello{Protocol: Protocol})
	})
	mux.HandleFunc("GET /agent/v1/backends/{id}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func awaitMemory(t *testing.T, h host.Handle) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := h.MemoryMiB(); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no status poll answered")
}

// Defect: RemoteHost's handle answered every per-card question with an error,
// so measurePerDevice never ran for an agent backend. It now answers from the
// agent's split — and still errors for an agent that sends none.
func TestRemoteHandle_MemoryOnMiB(t *testing.T) {
	url := statusAgent(t, `{"id":"b1","alive":true,"memoryMiB":35000,"memoryByDevice":{"`+uuid5090+`":29000,"`+uuid3080+`":6000}}`)
	h, err := NewRemoteHost("box1", []string{url}, "").Attach("b1", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.(*remoteHandle).Detach()
	awaitMemory(t, h)
	if v, err := h.MemoryOnMiB(uuid5090); err != nil || v != 29000 {
		t.Errorf("5090 = %d, %v; want 29000", v, err)
	}
	if v, err := h.MemoryOnMiB(uuid3080); err != nil || v != 6000 {
		t.Errorf("3080 = %d, %v; want 6000", v, err)
	}
	if v, err := h.MemoryOnMiB("GPU-not-here"); err != nil || v != 0 {
		t.Errorf("a card holding nothing = %d, %v; want 0, nil", v, err)
	}

	// An older agent: one aggregate, no split.
	url = statusAgent(t, `{"id":"b2","alive":true,"memoryMiB":35000}`)
	h2, err := NewRemoteHost("box1", []string{url}, "").Attach("b2", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h2.(*remoteHandle).Detach()
	awaitMemory(t, h2)
	if _, err := h2.MemoryOnMiB(uuid5090); err == nil {
		t.Error("an agent with no per-card split answered a per-card question")
	}
}
