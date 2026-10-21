package api

import (
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/gpu"
)

// Defect 5: an agent-backed server's pools never tagged a card on the
// dashboard, so box1 moved under an agent beside its primary lost every
// per-card pool tag. A UUID selector names the same physical card wherever it
// is probed, so it tags; a bus-id selector names a slot every box has, so an
// agent's still does not.
func TestLocalDevicePools_AgentBesideThePrimary(t *testing.T) {
	const u5090 = "GPU-ee90af07-0882-d325-182e-87137ec6d47b"
	const u3080 = "GPU-76a4c775-a47f-61b9-3a9f-9c7d5edfc544"
	devs := []gpu.Stats{
		{Index: 0, UUID: u3080, PCIBusID: "00000000:03:00.0", Name: "NVIDIA GeForce RTX 3080"},
		{Index: 1, UUID: u5090, PCIBusID: "00000000:0A:00.0", Name: "NVIDIA GeForce RTX 5090"},
	}
	cfg := &config.Config{Servers: map[string]config.Server{
		"box1": {
			Pools:   map[string]string{"gpu0": "32607MiB", "gpu1": "10240MiB"},
			Devices: map[string]string{"gpu0": u5090, "gpu1": u3080},
			Agent:   &config.AgentBinding{Endpoints: []string{"http://127.0.0.1:6503"}},
		},
		"remote": {
			Pools:   map[string]string{"gpu9": "24GB"},
			Devices: map[string]string{"gpu9": "03:00.0"},
			Agent:   &config.AgentBinding{Endpoints: []string{"http://10.0.0.9:6503"}},
		},
	}}
	h := &Handlers{Cfg: cfg}
	got := h.localDevicePools(devs)
	if got[u5090] != "gpu0" || got[u3080] != "gpu1" {
		t.Errorf("pool tags = %v, want the 5090 → gpu0 and the 3080 → gpu1", got)
	}
	if len(got) != 2 {
		t.Errorf("a remote agent's bus-id selector claimed a local card: %v", got)
	}
}
