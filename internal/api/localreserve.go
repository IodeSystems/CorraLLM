package api

import (
	"fmt"
	"sort"

	"github.com/iodesystems/corrallm/internal/agent"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/gpu"
	"github.com/iodesystems/corrallm/internal/proc"
)

// The reserve a local server is proposed with.
//
// A pool sized at the whole card or the whole of RAM lets corrallm admit
// against memory the desktop, the OS and every other program already hold, and
// the first model that fills it runs the machine out. So the proposal keeps
// back what other processes use at probe time, plus a buffer for what they grow
// into, rounded UP to a whole GB. The operator sees the figure and its parts
// and can change it before anything is written.

const (
	oneGiB int64 = 1 << 30
	// cardBuffer is the headroom per GPU on top of what others use now: room
	// for a desktop compositor or a browser to allocate more.
	cardBuffer = oneGiB
	// systemBufferMin is the least headroom kept in system RAM; systemBufferPct
	// of the total applies when it is more.
	systemBufferMin = 4 * oneGiB
	systemBufferPct = 10
)

// LocalPool is one pool of the server adding this machine would write, with
// how its reserve was reached.
type LocalPool struct {
	Pool         string `json:"pool"`
	TotalBytes   int64  `json:"totalBytes" doc:"The pool's size as it would be written (whole GB, rounded down)."`
	ReserveBytes int64  `json:"reserveBytes" doc:"Memory kept free for everything that is not a corrallm backend (whole GB, rounded up, never more than totalBytes)."`
	InUseBytes   int64  `json:"inUseBytes" doc:"Memory other processes held when the probe ran: used, minus what corrallm's own backends hold (on a GPU, their memory there; in system RAM, their anonymous memory, Linux only)."`
	BufferBytes  int64  `json:"bufferBytes" doc:"Headroom added to inUseBytes: 1 GiB per GPU; for system RAM the larger of 4 GiB and 10% of the total."`
	Reason       string `json:"reason" doc:"The reserve in words, e.g. 'in use now 1.2 GB + 1.1 GB buffer'."`
	Card         string `json:"card" doc:"The GPU this pool budgets; empty for system RAM."`
	UUID         string `json:"uuid" doc:"The card's vendor UUID; empty for system RAM."`
	Bound        bool   `json:"bound" doc:"Whether the pool is bound to the card by UUID (devices). One card is not bound: there is no other to confuse it with."`
	ECC          bool   `json:"ecc" doc:"Whether the pool's memory was detected as ECC. An unreadable answer is false."`
}

// localProposal is the server adding this machine would write, and per pool
// what it was sized from.
type localProposal struct {
	Name   string
	Server config.Server
	Pools  []LocalPool // sorted by pool name
}

// proposeLocal sizes a server for this machine from its probe. own is the
// memory the backends corrallm itself runs here hold (proc.Manager.LocalMemory);
// it is not "in use by others", since corrallm already accounts for it.
func proposeLocal(name string, c agent.Capacity, own proc.OwnMemory) localProposal {
	srv := localServerFrom(c, statsOf(c.GPUs))
	sizes, _ := config.ParseSizes(srv.Pools) // written by bytesToGB: always parses
	wired, _ := config.ParseSizes(srv.Reserve)
	srv.Reserve = map[string]string{}

	byUUID := map[string]agent.GPUDevice{}
	for _, d := range c.GPUs {
		byUUID[d.UUID] = d
	}

	p := localProposal{Name: name, Server: srv}
	for _, pool := range sortedKeys(srv.Pools) {
		lp := LocalPool{Pool: pool, TotalBytes: sizes[pool]}
		if pool == "system" || c.Unified {
			lp.ECC = c.HostECC && !c.Unified // unified memory has no ECC
			if c.Host != nil {
				// Less only the ANONYMOUS memory of corrallm's own backends:
				// mmapped weights are page cache the kernel already reports as
				// available, so subtracting the whole resident set would
				// subtract those bytes twice. Off Linux nothing is subtracted.
				lp.InUseBytes = max(0, c.Host.UsedBytes-own.AnonBytes)
			}
			lp.BufferBytes = max(systemBufferMin, lp.TotalBytes*systemBufferPct/100)
		} else {
			d, ok := cardFor(c, srv, pool, byUUID)
			lp.Card, lp.UUID, lp.ECC = d.Name, d.UUID, d.ECC
			lp.Bound = srv.Devices[pool] != ""
			if ok {
				lp.InUseBytes = max(0, d.UsedBytes-int64(own.DeviceMiB[d.UUID])*1024*1024)
			}
			lp.BufferBytes = cardBuffer
		}
		lp.ReserveBytes = ceilGB(lp.InUseBytes + lp.BufferBytes)
		lp.Reason = fmt.Sprintf("in use now %s + %s buffer", fmtGB(lp.InUseBytes), fmtGB(lp.BufferBytes))
		// Unified memory: macOS will not let the GPU wire past its limit, and
		// sizeFrom turns the rest into reserve. Keep whichever is larger.
		if w := wired[pool]; w > lp.ReserveBytes {
			lp.ReserveBytes = w
			lp.Reason += fmt.Sprintf("; raised to %s, what the GPU may not wire", fmtGB(w))
		}
		if lp.ReserveBytes > lp.TotalBytes {
			lp.ReserveBytes = lp.TotalBytes
			lp.Reason += "; capped at the pool size"
		}
		if lp.ReserveBytes > 0 {
			srv.Reserve[pool] = bytesToGB(lp.ReserveBytes) // whole GB already
		}
		p.Pools = append(p.Pools, lp)
	}
	if len(srv.Reserve) == 0 {
		srv.Reserve = nil
	}
	p.Server = srv
	return p
}

// cardFor finds the card behind a device pool: by its devices binding, or for
// a single unbound pool the one card there is. ok is false when nothing was
// measured.
func cardFor(c agent.Capacity, srv config.Server, pool string, byUUID map[string]agent.GPUDevice) (agent.GPUDevice, bool) {
	if uuid := srv.Devices[pool]; uuid != "" {
		d, ok := byUUID[uuid]
		return d, ok
	}
	if len(c.GPUs) == 1 {
		return c.GPUs[0], true
	}
	if c.GPU != nil && len(c.GPUs) == 0 {
		return agent.GPUDevice{DeviceMem: *c.GPU}, true // an agent too old to list cards
	}
	return agent.GPUDevice{}, false
}

// statsOf turns the probe's card list into what localServerFrom sorts.
func statsOf(devs []agent.GPUDevice) []gpu.Stats {
	out := make([]gpu.Stats, 0, len(devs))
	for _, d := range devs {
		out = append(out, gpu.Stats{Index: d.Index, UUID: d.UUID, PCIBusID: d.PCIBusID, Name: d.Name,
			TotalMiB: int(d.TotalBytes / (1024 * 1024)), UsedMiB: int(d.UsedBytes / (1024 * 1024))})
	}
	return out
}

// ceilGB rounds up to a whole GB, as bytesToGBUp does.
func ceilGB(b int64) int64 { return (b + gb - 1) / gb * gb }

func fmtGB(b int64) string { return fmt.Sprintf("%.1f GB", float64(b)/gb) }

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
