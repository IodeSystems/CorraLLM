package tune

import (
	"sort"
	"strings"
	"time"
)

// Capabilities are what a PLACEMENT turned out to be able to do, as opposed to
// what someone wrote down about the model.
//
// They live here, beside the measured profile, because they have the same shape
// and the same reason. A profile is keyed by (device, model) because the same
// weights cost different amounts on different hardware. Capabilities are keyed
// by (placement, model) because the same weights EXPOSE different things
// depending on the command that loaded them: whether an mmproj came along
// decides vision, the context flag decides the window, the slot count decides
// concurrency. A Q4 on a laptop and a Q6 on a 5090 are not the same backend and
// there is no honest way to describe them with one record.
//
// Storing them here rather than in config is the point. A declaration is
// something a person had to think to write, and corrallm treated its absence as
// a negative: a model that did not declare vision was skipped for vision probes,
// so the one mechanism that could have proven the capability was gated on
// somebody having already claimed it. Observed on a model that reads text out
// of PNGs perfectly well.
type Capabilities struct {
	// ContextLength is the window the backend ACTUALLY got, which is not
	// necessarily the one the command asked for.
	ContextLength int `json:"contextLength,omitempty"`
	// Slots is concurrency as the backend reports it — the real maxConcurrent.
	Slots int `json:"slots,omitempty"`
	// Modalities are corrallm's vocabulary (text|image|audio), already mapped
	// from whatever the backend called them.
	Modalities []string `json:"modalities,omitempty"`
	// Tools records whether the chat template can emit tool calls.
	Tools bool `json:"tools,omitempty"`
	// Upstream is the id the backend answers to, when it differs from the
	// served name.
	Upstream string `json:"upstream,omitempty"`
	// HasUI records whether the backend serves a web UI at its root.
	HasUI bool `json:"hasUI,omitempty"`

	// ProbedAt is when this was established. Kept because a capability record
	// describes a cmd that may since have changed, and an operator comparing a
	// stale record against a new command needs to know which is older.
	ProbedAt time.Time `json:"probedAt,omitempty"`
	// ProbedCmd is the command that produced this. Capabilities belong to the
	// COMMAND as much as the box; if it changes, this record describes
	// something that is no longer running.
	ProbedCmd string `json:"probedCmd,omitempty"`
}

// Supports reports whether this placement handles a modality.
func (c Capabilities) Supports(modality string) bool {
	for _, m := range c.Modalities {
		if strings.EqualFold(m, modality) {
			return true
		}
	}
	return false
}

// StaleFor reports whether this record describes a different command than the
// one about to run.
//
// A capability record is only as good as the command it was taken from: add
// --mmproj and vision appears, drop it and vision goes, and nothing about the
// model name changes either way. Callers use this to say "probed, but for a
// different cmd" rather than presenting a stale answer as current.
func (c Capabilities) StaleFor(cmd string) bool {
	return c.ProbedCmd != "" && strings.TrimSpace(c.ProbedCmd) != strings.TrimSpace(cmd)
}

// PutCapabilities records what a placement was found to do.
//
// Keyed by placement rather than by server: two placements on ONE box are the
// case the whole design exists for, and keying by server would have the second
// silently overwrite the first.
func (c *Cache) PutCapabilities(placement, model string, caps Capabilities) {
	if c == nil || placement == "" || model == "" {
		return
	}
	sort.Strings(caps.Modalities) // stable on disk, so a re-probe is a real diff
	c.mu.Lock()
	if c.caps == nil {
		c.caps = map[string]Capabilities{}
	}
	c.caps[key(placement, model)] = caps
	c.mu.Unlock()
	c.persist()
}

// CapabilitiesFor returns what a placement was found to do, if it has ever been
// probed.
func (c *Cache) CapabilitiesFor(placement, model string) (Capabilities, bool) {
	if c == nil {
		return Capabilities{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	got, ok := c.caps[key(placement, model)]
	return got, ok
}
