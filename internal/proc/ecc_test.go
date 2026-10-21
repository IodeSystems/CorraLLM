package proc

import (
	"errors"
	"testing"

	"github.com/iodesystems/corrallm/internal/agent"
	"github.com/iodesystems/corrallm/internal/config"
)

const (
	eccUUIDA = "GPU-aaaa1111-0000-0000-0000-000000000000"
	eccUUIDB = "GPU-bbbb2222-0000-0000-0000-000000000000"
)

// fakeLocalECC replaces this machine's ECC probes for one test.
func fakeLocalECC(t *testing.T, cards map[string]bool, cardErr error, host, unified bool) {
	t.Helper()
	g, h, u := localGPUECC, localHostECC, localUnified
	t.Cleanup(func() { localGPUECC, localHostECC, localUnified = g, h, u })
	localGPUECC = func() (map[string]bool, error) { return cards, cardErr }
	localHostECC = func() bool { return host }
	localUnified = func() bool { return unified }
}

func eccCfg(t *testing.T, yaml string) *config.Config {
	t.Helper()
	cfg, err := config.LoadBytesForTest([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

type eccCase struct {
	pool   string
	ecc    bool
	source string
}

func checkECC(t *testing.T, m *Manager, server string, cases ...eccCase) {
	t.Helper()
	for _, c := range cases {
		ecc, src := m.PoolECC(server, c.pool)
		if ecc != c.ecc || src != c.source {
			t.Errorf("%s/%s: got (%v, %s), want (%v, %s)", server, c.pool, ecc, src, c.ecc, c.source)
		}
	}
}

// A local server: device pools follow their bound card, system follows EDAC,
// and the config override wins over both.
func TestPoolECC_Local(t *testing.T) {
	fakeLocalECC(t, map[string]bool{eccUUIDA: true, eccUUIDB: false}, nil, false, false)
	m := NewManager(eccCfg(t, `
servers:
  box1:
    pools: { gpu0: 24GB, gpu1: 10GB, system: 64GB }
    devices: { gpu0: `+eccUUIDA+`, gpu1: GPU-bbbb }
  box2:
    pools: { gpu0: 24GB, system: 64GB }
    ecc: { gpu0: true, system: true }
`))
	checkECC(t, m, "box1",
		eccCase{"gpu0", true, ECCFromDetected},
		eccCase{"gpu1", false, ECCFromDetected}, // a uuid PREFIX selector
		eccCase{"system", false, ECCFromDetected},
	)
	// box2 has no `devices:`; its gpu0 would be "not every card is ECC" —
	// the override says otherwise, and wins.
	checkECC(t, m, "box2",
		eccCase{"gpu0", true, ECCFromConfig},
		eccCase{"system", true, ECCFromConfig},
	)
	checkECC(t, m, "nope", eccCase{"gpu0", false, ECCUnknown})
}

// No `devices:` binding: ECC only when every card is. A failed probe is no ECC.
func TestPoolECC_LocalUnboundAndProbeFailure(t *testing.T) {
	cfg := eccCfg(t, `
servers:
  box1:
    pools: { gpu0: 24GB, system: 64GB }
`)
	fakeLocalECC(t, map[string]bool{eccUUIDA: true, eccUUIDB: true}, nil, true, false)
	checkECC(t, NewManager(cfg), "box1", eccCase{"gpu0", true, ECCFromDetected}, eccCase{"system", true, ECCFromDetected})

	fakeLocalECC(t, map[string]bool{eccUUIDA: true, eccUUIDB: false}, nil, true, false)
	checkECC(t, NewManager(cfg), "box1", eccCase{"gpu0", false, ECCFromDetected})

	fakeLocalECC(t, nil, errors.New("no nvidia-smi"), true, false)
	checkECC(t, NewManager(cfg), "box1", eccCase{"gpu0", false, ECCUnknown})

	// Unified memory (Apple): no ECC anywhere, whatever else says so.
	fakeLocalECC(t, map[string]bool{eccUUIDA: true}, nil, true, true)
	checkECC(t, NewManager(cfg), "box1", eccCase{"gpu0", false, ECCFromDetected}, eccCase{"system", false, ECCFromDetected})
}

// An agent-backed server: the agent's report decides, mapped onto pools by
// the `devices:` selectors (uuid or bus id). An agent older than the ECC
// fields reports none, which reads as no ECC; one not heard from yet is
// unknown, also no ECC.
func TestPoolECC_Agent(t *testing.T) {
	// This machine says everything is ECC; none of that may leak into an
	// agent's answer.
	fakeLocalECC(t, map[string]bool{eccUUIDA: true, eccUUIDB: true}, nil, true, false)
	m := NewManager(eccCfg(t, `
servers:
  mac:
    pools: { gpu0: 24GB, gpu1: 10GB, system: 64GB }
    devices: { gpu0: `+eccUUIDA+`, gpu1: "0a:00.0" }
    agent: { endpoints: ["http://127.0.0.1:1"], token: t }
`))
	live := agent.NewLiveness()
	m.SetLiveness(live)

	checkECC(t, m, "mac", eccCase{"gpu0", false, ECCUnknown}) // not reported yet

	gpus := func(eccA, eccB bool) []agent.GPUDevice {
		return []agent.GPUDevice{
			{DeviceMem: agent.DeviceMem{Name: "a"}, Index: 0, UUID: eccUUIDA, PCIBusID: "00000000:03:00.0", ECC: eccA},
			{DeviceMem: agent.DeviceMem{Name: "b"}, Index: 1, UUID: eccUUIDB, PCIBusID: "00000000:0A:00.0", ECC: eccB},
		}
	}
	live.RecordCapacity("mac", agent.Capacity{GPUs: gpus(true, false), HostECC: true})
	checkECC(t, m, "mac",
		eccCase{"gpu0", true, ECCFromDetected},
		eccCase{"gpu1", false, ECCFromDetected}, // by bus id
		eccCase{"system", true, ECCFromDetected},
	)

	live.RecordCapacity("mac", agent.Capacity{GPUs: gpus(false, true)})
	checkECC(t, m, "mac", eccCase{"gpu0", false, ECCFromDetected}, eccCase{"gpu1", true, ECCFromDetected})

	// An old agent: per-card list without ECC, no hostEcc → no ECC.
	live.RecordCapacity("mac", agent.Capacity{GPUs: gpus(false, false)})
	checkECC(t, m, "mac", eccCase{"gpu0", false, ECCFromDetected}, eccCase{"system", false, ECCFromDetected})

	// Older still: no per-card list at all.
	live.RecordCapacity("mac", agent.Capacity{GPU: &agent.DeviceMem{Name: "a"}})
	checkECC(t, m, "mac", eccCase{"gpu0", false, ECCUnknown})

	// Apple unified memory: no ECC even if a card claimed it.
	live.RecordCapacity("mac", agent.Capacity{GPUs: gpus(true, true), HostECC: true, Unified: true})
	checkECC(t, m, "mac", eccCase{"gpu0", false, ECCFromDetected}, eccCase{"system", false, ECCFromDetected})
}
