package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"time"
)

// The caller-key roster: who is talking to this box, what lane they land in,
// and whether anybody decided that.
//
// Keys were the one part of the scheduling model with no management surface.
// Weights live on groups and groups are editable; the key→group map was
// hand-edited YAML and a restart. Worse, an UNASSIGNED key was invisible:
// ResolveGroup falls back silently, so a key nobody had ever thought about
// looked exactly like one deliberately placed in the default lane. You could
// only manage keys you already knew about, which is backwards on a box that
// mints them freely.
//
// So this joins two sources that were never joined: the configured map, and the
// keys actually observed in traffic. The second is what makes enrolment a click
// — a stranger shows up in the roster with recognized=false, and assigning it a
// group is a PUT to the config-entry editor.

// KeyHash is a stable short identifier for a caller key.
//
// Not a redaction. The operator asked for hashing as an IDENTIFIER, and was
// explicit that keys are not secret here — corrallm is the admin surface, and a
// roster that hid the thing you are trying to recognize would be useless. The
// hash exists so a UI has something fixed to key rows on and to diff across
// refreshes, while the key itself stays visible and greppable in logs.
func KeyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:12]
}

// KeyRow is one caller key: its lane, its effective weight, and whether that
// was a decision or a default.
type KeyRow struct {
	Key    string `json:"key"`
	Hash   string `json:"hash"`
	Group  string `json:"group"`
	Weight int    `json:"weight" doc:"The group's fairshare weight, which is what actually schedules this caller."`

	// Recognized is false for a key seen in traffic but absent from the config.
	// It is the whole point of the roster: such a caller is being served in the
	// fallback lane by omission rather than by decision, and until it is listed
	// nobody can tell those apart.
	Recognized bool `json:"recognized"`

	// Requests and LastSeen come from usage and are zero/empty for a configured
	// key that has never called. Both are needed, not just the count: a key
	// assigned a lane months ago and silent since is a different thing from one
	// hammering the box right now, and a request total alone cannot tell them
	// apart — a big number may be entirely historical.
	Requests int64  `json:"requests"`
	LastSeen string `json:"lastSeen,omitempty" doc:"RFC3339 timestamp of this key's most recent request; empty if never seen."`

	// CostUSD and DwellMS are what the key actually consumed, which is the other
	// half of deciding a lane. Request count says how noisy a caller is; cost
	// says how expensive, and they diverge — a few long generations outweigh
	// thousands of embeddings.
	CostUSD float64 `json:"costUSD"`
	DwellMS int64   `json:"dwellMS"`
}

// KeysInput scopes the observed half of the roster.
type KeysInput struct {
	WindowHours int `query:"windowHours" doc:"Look back this far for keys seen in traffic (0 = all recorded usage)."`
}

// KeysOutput is the roster plus the policy that governs strangers.
type KeysOutput struct {
	Body struct {
		Keys []KeyRow `json:"keys"`
		// UnknownAllowed / UnknownGroup report the standing policy, so the UI
		// can say what happens to a key nobody assigns instead of leaving the
		// reader to infer it from an absence.
		UnknownAllowed bool   `json:"unknownAllowed"`
		UnknownGroup   string `json:"unknownGroup"`
	}
}

// Keys lists every caller key this box knows about — configured or merely seen.
func (h *Handlers) Keys(_ context.Context, in *KeysInput) (*KeysOutput, error) {
	cfg := h.config()
	out := &KeysOutput{}
	out.Body.Keys = []KeyRow{}
	if cfg == nil {
		return out, nil
	}
	out.Body.UnknownAllowed = cfg.UnknownKeys.Allowed()
	out.Body.UnknownGroup = cfg.UnknownKeys.FallbackGroup()

	rows := map[string]*KeyRow{}
	add := func(key string) *KeyRow {
		if r, ok := rows[key]; ok {
			return r
		}
		group, g, recognized := cfg.ResolveGroupRecognized(key)
		r := &KeyRow{
			Key: key, Hash: KeyHash(key), Group: group,
			Weight: g.EffectiveWeight(), Recognized: recognized,
		}
		rows[key] = r
		return r
	}
	for key := range cfg.Keys {
		add(key)
	}

	// Observed traffic. A store failure degrades the roster to the configured
	// half rather than failing it: knowing who is assigned is still worth
	// serving, and an operator staring at an error learns nothing at all.
	if h.Store != nil {
		var sinceMS int64
		if in != nil && in.WindowHours > 0 {
			sinceMS = time.Now().Add(-time.Duration(in.WindowHours) * time.Hour).UnixMilli()
		}
		if used, err := h.Store.RollupByKey(sinceMS); err == nil {
			for _, u := range used {
				if u.Key == "" {
					continue // unkeyed traffic is not a key to enrol
				}
				r := add(u.Key)
				r.Requests = u.Requests
				r.CostUSD = u.CostUSD
				r.DwellMS = u.DwellMS
				if u.LastSeenMS > 0 {
					r.LastSeen = time.UnixMilli(u.LastSeenMS).UTC().Format(time.RFC3339)
				}
			}
		}
	}

	for _, r := range rows {
		out.Body.Keys = append(out.Body.Keys, *r)
	}
	sortKeyRows(out.Body.Keys)
	return out, nil
}

// sortKeyRows puts what needs a DECISION first: unrecognized keys, busiest
// among them. A stranger doing real traffic is the most urgent row on the page;
// a configured key behaving itself is the least.
func sortKeyRows(ks []KeyRow) {
	sort.SliceStable(ks, func(i, j int) bool {
		a, b := ks[i], ks[j]
		if a.Recognized != b.Recognized {
			return !a.Recognized
		}
		if a.Requests != b.Requests {
			return a.Requests > b.Requests
		}
		return a.Key < b.Key
	})
}
