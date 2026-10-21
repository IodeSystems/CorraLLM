package gpu

import (
	"fmt"
	"strings"
	"sync"
)

// ECC: whether a card's memory corrects bit flips.
//
// It decides how long weights may sit in memory. Without ECC a flipped bit in
// loaded weights stays until the model reloads, so a model on a non-ECC card is
// reloaded on a schedule (see proc's non-ECC refresh). The answer errs toward
// "no ECC": a wrong "no" costs one reload, a wrong "yes" keeps corrupt weights
// serving.

// ECCProber is the OPTIONAL half of the Prober contract: a backend that can say
// whether each card has ECC enabled. A backend without it (Apple unified
// memory) is treated as non-ECC by every caller.
type ECCProber interface {
	// ECCByUUID maps device uuid → ECC enabled now.
	ECCByUUID() (map[string]bool, error)
}

// ECCByUUID reads `ecc.mode.current` for every card. "Enabled" is ECC;
// "Disabled", "[N/A]" (consumer cards report this) and anything else are not.
func (NVIDIA) ECCByUUID() (map[string]bool, error) {
	out, err := runCmd("nvidia-smi", "--query-gpu=uuid,ecc.mode.current", "--format=csv,noheader")
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi: %w", err)
	}
	return parseECCCSV(out)
}

// parseECCCSV parses `nvidia-smi --query-gpu=uuid,ecc.mode.current
// --format=csv,noheader` output, e.g.
//
//	GPU-76a4c775-…, [N/A]
//	GPU-1f2e3d4c-…, Enabled
func parseECCCSV(out []byte) (map[string]bool, error) {
	m := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		uuid, mode, ok := strings.Cut(line, ",")
		uuid = strings.TrimSpace(uuid)
		if !ok || uuid == "" {
			return nil, fmt.Errorf("nvidia-smi: unexpected ecc row %q", line)
		}
		m[uuid] = strings.EqualFold(strings.TrimSpace(mode), "Enabled")
	}
	return m, nil
}

// eccCache holds the first successful read. ECC mode changes only take effect
// after a reboot, which restarts this process too, so one read is enough; a
// failed read is not cached, so the next call tries again.
var eccCache struct {
	sync.Mutex
	m map[string]bool
}

// ECC returns uuid → ECC enabled for this host's cards, from Default. An error
// when the backend cannot tell; callers treat that as "no ECC".
func ECC() (map[string]bool, error) {
	eccCache.Lock()
	defer eccCache.Unlock()
	if eccCache.m != nil {
		return eccCache.m, nil
	}
	ep, ok := Default.(ECCProber)
	if !ok {
		return nil, fmt.Errorf("gpu: %s cannot report ECC", Default.Name())
	}
	m, err := ep.ECCByUUID()
	if err != nil {
		return nil, err
	}
	eccCache.m = m
	return m, nil
}
