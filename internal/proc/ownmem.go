package proc

import "github.com/iodesystems/corrallm/internal/host"

// OwnMemory is the memory held by the backends this process runs on THIS
// machine.
type OwnMemory struct {
	DeviceMiB map[string]int // device uuid → MiB
	AnonBytes int64          // anonymous system RAM (sysmem.GroupAnonBytes)
}

// LocalMemory reports OwnMemory.
//
// It answers "how much of the memory in use is ours", which a probe of the
// machine needs before it calls the rest "in use by other processes". A
// backend whose memory cannot be attributed is left out, so both figures are
// floors: the caller then treats that memory as someone else's and reserves
// it, which is the safe direction.
//
// Agent-run backends are left out too: they hold memory on another machine,
// or, beside the primary, on cards a local server does not describe.
func (m *Manager) LocalMemory() OwnMemory {
	m.mu.Lock()
	var handles []host.Handle
	for _, p := range m.procs {
		p.mu.Lock()
		h := p.handle
		p.mu.Unlock()
		if h != nil && !onAgent(h) {
			handles = append(handles, h)
		}
	}
	m.mu.Unlock()

	// One vendor-tool read and one /proc scan per backend, outside the lock:
	// they take tens of milliseconds and need none of the manager's state.
	out := OwnMemory{DeviceMiB: map[string]int{}}
	for _, h := range handles {
		if dm, ok := h.(host.DeviceMemory); ok {
			if byDev, err := dm.MemoryByDeviceMiB(); err == nil {
				for uuid, mib := range byDev {
					out.DeviceMiB[uuid] += mib
				}
			}
		}
		if hm, ok := h.(host.HostMemory); ok {
			if b, err := hm.AnonBytes(); err == nil {
				out.AnonBytes += b
			}
		}
	}
	return out
}
