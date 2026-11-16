package proxy

import (
	"fmt"
	"log/slog"

	"github.com/iodesystems/corrallm/internal/cacheattr"
	"github.com/iodesystems/corrallm/internal/config"
)

// DetectedCapacity is what the model's running llama-server backends report
// (cacheattr.Capacity): /props total_slots and the cache slot counts. ok
// false for a remote model, without a poller, or before any backend reported.
func (p *Proxy) DetectedCapacity(name string, mc config.Model) (cacheattr.Capacity, bool) {
	if p.cachePoller == nil || mc.Remote() || mc.Cmd == "" {
		return cacheattr.Capacity{}, false
	}
	return p.cachePoller.Capacity(name)
}

// AdmitSlots is the model's admission capacity. Config maxConcurrent wins when
// set: it is the operator's choice, and a backend reporting a different
// total_slots is logged (once per change), not obeyed. Unset, a llama-server
// backend's reported total_slots is used once one has reported; before that,
// and for every other backend, the default (config.Model.Slots, 1).
func (p *Proxy) AdmitSlots(name string, mc config.Model) int {
	c, ok := p.DetectedCapacity(name, mc)
	if !ok || c.Slots <= 0 {
		return mc.Slots()
	}
	if mc.MaxConcurrent <= 0 {
		return c.Slots
	}
	if c.Slots != mc.MaxConcurrent {
		p.warnSlotMismatch(name, mc.MaxConcurrent, c.Slots)
	}
	return mc.MaxConcurrent
}

func (p *Proxy) warnSlotMismatch(name string, configured, detected int) {
	p.slotWarnMu.Lock()
	defer p.slotWarnMu.Unlock()
	pair := [2]int{configured, detected}
	if p.slotWarn[name] == pair {
		return
	}
	if p.slotWarn == nil {
		p.slotWarn = map[string][2]int{}
	}
	p.slotWarn[name] = pair
	slog.Warn(fmt.Sprintf("maxConcurrent %d but backend reports total_slots %d; admitting maxConcurrent", configured, detected),
		"model", name)
}
