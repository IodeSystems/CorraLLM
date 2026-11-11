package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/iodesystems/corrallm/internal/cacheattr"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
)

// SLOT AFFINITY (P75 N9): send a chat turn back to the llama.cpp slot that
// served the conversation's previous turn, so its KV is still there.
//
// llama.cpp picks a slot by prompt similarity, then least recently used; with
// --parallel 2 a conversation's next turn often lands on the other slot and
// re-prefills (11-11, 12.5 h on Swift: 48.6% of re-prefilled tokens, most of
// them under a minute after the previous turn). `id_slot` in the body pins
// the slot. A pinned slot that is BUSY defers the request until it is free
// (server-context.cpp), so a pin is only ever sent to a slot corrallm sees
// free.
//
// "Free" is corrallm's own count of what it sent: every request to a model
// with the flag on takes a lease here, on a known slot when corrallm chose or
// the caller named one, or as "untracked" when llama.cpp chooses. An
// untracked request sits on a slot corrallm cannot name, so while one is in
// flight nothing is pinned. A slot count that is not known (no --parallel in
// the cmd) turns the whole thing into a no-op.

// Outcomes, stored on activity_cache.affinity.
const (
	affPinned   = "pinned"   // the previous turn's slot, free
	affMoved    = "moved"    // the previous turn's slot busy; another free one
	affAssigned = "assigned" // no previous slot known; a free one
	affUnpinned = "unpinned" // no slot known free: llama.cpp picks
	affClient   = "client"   // the caller sent id_slot: passed through
	affUnknown  = "unknown"  // slot count or instance not known: nothing done
)

// slotAssigner keeps, per served model, the slot occupancy of its current
// backend instance. The zero value is ready. One mutex, held for a few map
// and slice operations, never across I/O.
type slotAssigner struct {
	mu sync.Mutex
	by map[string]*slotState
}

type slotState struct {
	instance  string
	busy      []int    // in-flight requests on each slot that corrallm knows
	used      []uint64 // assignment sequence per slot, for least recently assigned
	seq       uint64
	untracked int // in flight on a slot corrallm does not know
}

// acquire takes a lease for one request. n is the backend's slot count (≤ 0:
// unknown), last the slot of the conversation's previous turn (-1: none),
// client the id_slot the caller sent when sent is true, and pin false for a
// request whose body cannot carry id_slot (it is counted, never pinned).
//
// slot is the id_slot to write into the body, -1 for none. release is safe to
// call more than once; after an instance change it touches only the old
// instance's state, which nothing reads any more.
func (a *slotAssigner) acquire(served, instance string, n, last, client int, sent, pin bool) (slot int, outcome string, release func()) {
	if instance == "" || n <= 0 {
		return -1, affUnknown, func() {}
	}
	a.mu.Lock()
	if a.by == nil {
		a.by = map[string]*slotState{}
	}
	st := a.by[served]
	if st == nil || st.instance != instance || len(st.busy) != n {
		// A new process (reload, crash, handoff to a new backend) starts with
		// every slot empty, and requests on the old one are not on this one.
		st = &slotState{instance: instance, busy: make([]int, n), used: make([]uint64, n)}
		a.by[served] = st
	}
	take := -1
	switch {
	case sent:
		outcome = affClient
		if client >= 0 && client < n {
			take = client
		}
	case !pin || st.untracked > 0:
		outcome = affUnpinned
	case last >= 0 && last < n && st.busy[last] == 0:
		take, outcome = last, affPinned
	default:
		for i := range st.busy {
			if st.busy[i] == 0 && (take < 0 || st.used[i] < st.used[take]) {
				take = i
			}
		}
		switch {
		case take < 0:
			outcome = affUnpinned
		case last >= 0 && last < n:
			outcome = affMoved
		default:
			outcome = affAssigned
		}
	}
	if take >= 0 {
		st.busy[take]++
		st.seq++
		st.used[take] = st.seq
	} else {
		st.untracked++
	}
	a.mu.Unlock()

	var once sync.Once
	release = func() {
		once.Do(func() {
			a.mu.Lock()
			if take >= 0 {
				st.busy[take]--
			} else {
				st.untracked--
			}
			a.mu.Unlock()
		})
	}
	if outcome == affClient {
		return -1, outcome, release
	}
	return take, outcome, release
}

// assignSlot applies slot affinity to one request on a candidate backend:
// the body to send, what was done ("" when the model's flag is off), and the
// lease to release when the backend has answered. Off, or not a local
// backend: the body is returned as it came, byte for byte.
//
// chatOut reports that the body going out is an OpenAI chat body to an
// OpenAI-compatible backend (chat completions, or /v1/messages converted).
func (p *Proxy) assignSlot(r *http.Request, pr *proc.Process, served string, backend config.Model, body []byte, chatOut bool) ([]byte, string, func()) {
	if !backend.SlotAffinity || backend.Cmd == "" || pr == nil {
		return body, "", func() {}
	}
	inst := pr.Instance()
	n := cacheattr.Parallel(backend.Cmd)
	client, sent := -1, false
	last := -1
	if chatOut {
		client, sent = clientIDSlot(body)
		if c, ok := r.Context().Value(conversationCtxKey{}).(conversation); ok && !sent {
			last = p.cacheTracker.LastSlot(c.id, inst, time.Now().UnixMilli())
		}
	}
	slot, outcome, release := p.slotAff.acquire(served, inst, n, last, client, sent, chatOut)
	if slot >= 0 {
		if nb, ok := withIDSlot(body, slot); ok {
			body = nb
		}
	}
	return body, outcome, release
}

// clientIDSlot reads a top-level id_slot the caller sent. Decoded only when
// the bytes appear at all, so the common body costs one scan.
func clientIDSlot(body []byte) (slot int, sent bool) {
	if !bytes.Contains(body, []byte(`"id_slot"`)) {
		return -1, false
	}
	var probe struct {
		IDSlot json.RawMessage `json:"id_slot"`
	}
	if json.Unmarshal(body, &probe) != nil || probe.IDSlot == nil {
		return -1, false
	}
	n, err := strconv.Atoi(string(probe.IDSlot))
	if err != nil {
		return -1, true // sent, but not a slot number: passed through, not counted
	}
	return n, true
}

// withIDSlot writes "id_slot": slot as the body's first member. A splice
// rather than a re-encode: the rest of the body is the caller's bytes.
func withIDSlot(body []byte, slot int) ([]byte, bool) {
	i := skipSpace(body, 0)
	if i >= len(body) || body[i] != '{' {
		return body, false
	}
	field := `"id_slot":` + strconv.Itoa(slot)
	out := make([]byte, 0, len(body)+len(field)+1)
	out = append(out, body[:i+1]...)
	out = append(out, field...)
	if j := skipSpace(body, i+1); j < len(body) && body[j] != '}' {
		out = append(out, ',')
	}
	return append(out, body[i+1:]...), true
}

func skipSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}
