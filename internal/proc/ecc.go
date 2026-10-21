package proc

import (
	"slices"

	"github.com/iodesystems/corrallm/internal/gpu"
	"github.com/iodesystems/corrallm/internal/sysmem"
)

// Which pools hold memory with ECC.
//
// Every "cannot tell" answers NO ECC. The answer decides whether a model is
// reloaded on a schedule (refresh.go): a wrong "no" costs one reload, a wrong
// "yes" keeps flipped weights serving until something else reloads them.

// ECC sources, as reported with each answer.
const (
	ECCFromConfig   = "config"   // servers.<name>.ecc override
	ECCFromDetected = "detected" // nvidia-smi / EDAC / an agent's report
	ECCUnknown      = "unknown"  // nothing to go on: treated as no ECC
)

// localGPUECC and localHostECC read THIS machine. Vars so tests need neither a
// GPU nor an EDAC tree.
var (
	localGPUECC  = gpu.ECC
	localHostECC = sysmem.HostECC
	localUnified = gpu.Unified
)

// PoolECC reports whether pool on server is ECC memory, and where that answer
// came from.
//
// In order: the server's `ecc:` override; for a DEVICE pool, the card it is
// bound to by `devices:` (or, unbound, every card of the machine agreeing);
// for any other pool, system RAM. "The machine" is this one for a local
// server and the agent's last report for an agent-backed one — the same split
// devicesOn makes. Unified memory (Apple) has no ECC.
func (m *Manager) PoolECC(server, pool string) (bool, string) {
	cfg := m.config()
	if cfg == nil {
		return false, ECCUnknown
	}
	srv, ok := cfg.Servers[server]
	if !ok {
		return false, ECCUnknown
	}
	if v, ok := srv.ECC[pool]; ok {
		return v, ECCFromConfig
	}

	var unified, hostECC bool
	var cardECC map[string]bool
	if m.agentBacked(server) {
		m.mu.Lock()
		live := m.live
		m.mu.Unlock()
		c, ok := live.Capacity(server) // nil-safe
		if !ok {
			return false, ECCUnknown // not reported yet
		}
		// An agent older than these fields sends neither: every card and
		// the host read as non-ECC, which is the safe reading.
		unified, hostECC, cardECC = c.Unified, c.HostECC, c.ECCByUUID()
	} else {
		unified, hostECC = localUnified(), localHostECC()
		if !unified {
			cardECC, _ = localGPUECC() // an error leaves it nil: no ECC
		}
	}
	if unified {
		return false, ECCFromDetected
	}

	if !slices.Contains(cfg.DevicePoolsFor(server), pool) {
		return hostECC, ECCFromDetected
	}
	if len(cardECC) == 0 {
		return false, ECCUnknown
	}
	if sel := cfg.DeviceSelectorFor(server, pool); sel != "" {
		// A uuid selector resolves against the ECC answer's own keys, without
		// another device probe; a bus id needs the full device list.
		known := make([]gpu.Stats, 0, len(cardECC))
		for uuid := range cardECC {
			known = append(known, gpu.Stats{UUID: uuid})
		}
		st, err := gpu.SelectByUUID(known, sel)
		if err != nil {
			devs, derr := m.devicesOn(server)
			if derr != nil {
				return false, ECCUnknown
			}
			st, err = gpu.Select(devs, sel)
		}
		if err != nil {
			return false, ECCUnknown
		}
		v, ok := cardECC[st.UUID]
		if !ok {
			return false, ECCUnknown
		}
		return v, ECCFromDetected
	}
	// No binding: which card holds the pool is not known, so it is ECC only if
	// every card on the machine is.
	for _, v := range cardECC {
		if !v {
			return false, ECCFromDetected
		}
	}
	return true, ECCFromDetected
}
