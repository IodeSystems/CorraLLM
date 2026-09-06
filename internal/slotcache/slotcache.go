// Package slotcache trades disk for VRAM.
//
// A llama.cpp backend keeps one conversation's KV cache per slot. With
// `--parallel 1` — which is what a 27B model on a 32 GB card can afford — two
// callers interleaving different conversations evict each other's prefix, and
// every switch re-reads the incoming prompt from scratch. Measured on box1 on
// 2026-09-05: reuse fell from 96% to 57% over four hours, requests went from
// re-reading ~1,500 prompt tokens to ~17,000, and mean time answering went 4.5 s
// → 10.8 s. Nothing was broken; the box was just doing the same work repeatedly.
//
// More slots is the textbook fix and costs KV memory the card does not have.
// This is the other lever: llama.cpp can write a slot's KV to disk and read it
// back, so a conversation that leaves the slot can return without being
// reprocessed. Measured against the production backend, same day:
//
//	save     19,639 tokens → 740 MB in 643 ms
//	restore  740 MB back in 331 ms
//	proof    the resent prompt reported 13,274 of 13,278 tokens cached
//
// A switch therefore costs ~0.97 s of backend time against ~10.9 s to reprocess
// the same prefix — 11x — and the state is 36.8 KB per token, so a 50k-token
// session is 1.9 GB. Disk is the cheap resource here; that is the whole idea.
//
// WHAT THIS PACKAGE DOES NOT DO: decide when to switch. It is the mechanism —
// name a conversation, save one, restore one, and keep the disk from filling.
// The policy lives with whoever owns the request path.
package slotcache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ConversationHeader lets a caller NAME the conversation a request belongs to,
// which is strictly better than corrallm guessing.
//
// The guess below reads the conversation's head — the system block and the first
// turn — because those do not change as it grows. Except in exactly the case
// this whole package exists for: a client that PRUNES the head of a long
// conversation to stay inside the context window changes those very messages, so
// the inferred key changes, the saved state is orphaned, and the request is
// reprocessed. The mechanism would miss the case it was built for.
//
// A declared id has none of that: it survives pruning, summarisation, and a
// changed tool list. It is still namespaced by the caller, so naming somebody
// else's conversation gets you your own state and not theirs.
const ConversationHeader = "X-Corrallm-Conversation"

// Key identifies one conversation's state, and getting it wrong has two
// different costs.
//
// Too broad and two conversations share a file: the restore puts somebody
// else's context in the slot, which is both wrong and a LEAK — a KV cache is
// the other conversation's content in another form. So the caller's identity is
// part of the key, always, and a state is never restored for a caller who did
// not create it.
//
// Too narrow and it changes every turn, which is the same as having no cache at
// all: the point is that turn N+1 of a conversation finds what turn N left.
// So the key is the conversation's HEAD — the parts that do not change as it
// grows — not the whole prompt.
func Key(caller, declared string, body []byte) (string, bool) {
	// What the caller says it is, when it says. Hashed with the caller for the
	// same reason the inferred key is: one caller's states are not another's to
	// address, however they spell the id.
	if declared = strings.TrimSpace(declared); declared != "" {
		h := sha256.New()
		_, _ = io.WriteString(h, caller)
		_, _ = h.Write([]byte{0})
		_, _ = io.WriteString(h, "declared:")
		_, _ = io.WriteString(h, declared)
		return hex.EncodeToString(h.Sum(nil))[:32], true
	}
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Messages) == 0 {
		return "", false
	}
	h := sha256.New()
	// The caller, so one caller's state is never handed to another.
	_, _ = io.WriteString(h, caller)
	_, _ = h.Write([]byte{0})
	// The head of the conversation: every leading system message, plus the first
	// non-system message. A coding agent sends the same system prompt for every
	// task, so the system block alone would collide across tasks; the first user
	// turn is what makes them different, and it is also the one message that
	// never changes as the conversation grows.
	seenFirstTurn := false
	for _, m := range req.Messages {
		if m.Role == "system" && !seenFirstTurn {
			_, _ = io.WriteString(h, m.Role)
			_, _ = h.Write(m.Content)
			continue
		}
		if !seenFirstTurn {
			_, _ = io.WriteString(h, m.Role)
			_, _ = h.Write(m.Content)
			seenFirstTurn = true
			break
		}
	}
	if !seenFirstTurn {
		// System messages only: not a conversation yet, and two callers priming
		// with the same system block would collide on an empty tail.
		return "", false
	}
	return hex.EncodeToString(h.Sum(nil))[:32], true
}

// Client talks to one llama.cpp backend's slot endpoints.
type Client struct {
	// BaseURL is the backend's own address, e.g. http://127.0.0.1:5802.
	BaseURL string
	HTTP    *http.Client
}

type slotResult struct {
	NSaved    int64 `json:"n_saved"`
	NRestored int64 `json:"n_restored"`
	NWritten  int64 `json:"n_written"`
	NRead     int64 `json:"n_read"`
	Timings   struct {
		SaveMS    float64 `json:"save_ms"`
		RestoreMS float64 `json:"restore_ms"`
	} `json:"timings"`
}

func (c *Client) do(ctx context.Context, slot int, action, filename string) (*slotResult, error) {
	if c.BaseURL == "" {
		return nil, fmt.Errorf("slotcache: no backend address")
	}
	body, _ := json.Marshal(map[string]string{"filename": filename})
	url := fmt.Sprintf("%s/slots/%d?action=%s", strings.TrimRight(c.BaseURL, "/"), slot, action)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 2 * time.Minute}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		// The backend says why — "slot is busy", "file not found", or the flag
		// was never passed. Pass it through: a caller that cannot tell those
		// apart will retry the one that can never work.
		return nil, fmt.Errorf("slotcache: %s returned %d: %s", action, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out slotResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("slotcache: %s: unreadable reply: %w", action, err)
	}
	return &out, nil
}

// Save writes the slot's current KV to the store under key. Returns the tokens
// and bytes written.
func (c *Client) Save(ctx context.Context, slot int, key string) (tokens, written int64, err error) {
	r, err := c.do(ctx, slot, "save", key+".bin")
	if err != nil {
		return 0, 0, err
	}
	return r.NSaved, r.NWritten, nil
}

// Restore reads a saved state back into the slot.
func (c *Client) Restore(ctx context.Context, slot int, key string) (tokens, read int64, err error) {
	r, err := c.do(ctx, slot, "restore", key+".bin")
	if err != nil {
		return 0, 0, err
	}
	return r.NRestored, r.NRead, nil
}

// Store is the directory of saved states, and the thing that keeps them from
// eating the disk.
//
// NAMESPACED BY BACKEND IDENTITY, and that is not tidiness. A saved state is
// KV for one model at one context size built by one command; restoring it into
// a different backend is meaningless at best. The namespace is a hash of what
// the backend IS, so a re-quantised model or a changed cmd simply finds no
// files rather than finding wrong ones.
type Store struct {
	Dir string
	// MaxBytes caps the whole store. Zero means 20 GB — roughly ten 50k-token
	// sessions at the measured 36.8 KB per token.
	MaxBytes int64
}

const defaultMaxBytes = 20 << 30

// Namespace derives the per-backend subdirectory from what the backend is.
func Namespace(model, cmd string, ctxSize int) string {
	h := sha256.Sum256([]byte(model + "\x00" + cmd + "\x00" + fmt.Sprint(ctxSize)))
	return hex.EncodeToString(h[:])[:16]
}

// Path is where a key's file lives for one backend.
func (s *Store) Path(namespace, key string) string {
	return filepath.Join(s.Dir, namespace, key+".bin")
}

// Has reports whether a state exists, and touches it so eviction sees it as
// recently used. A read that does not update recency evicts the file somebody
// is about to ask for.
func (s *Store) Has(namespace, key string) bool {
	p := s.Path(namespace, key)
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return false
	}
	// BOTH times, and mtime is the one that counts: Evict sorts on ModTime
	// because Go's FileInfo does not expose atime portably. Touching only atime
	// left recency unchanged and the store evicted exactly the state somebody was
	// about to ask for — caught by the test below, not in production.
	now := time.Now()
	_ = os.Chtimes(p, now, now)
	return true
}

// Prepare makes the namespace directory. llama.cpp writes into --slot-save-path
// itself, so the directory has to exist before a save is asked for.
func (s *Store) Prepare(namespace string) error {
	return os.MkdirAll(filepath.Join(s.Dir, namespace), 0o700)
}

type entry struct {
	path  string
	size  int64
	atime time.Time
}

// Evict deletes least-recently-used states until the store is under its cap.
// Returns how many files went and how many bytes that freed.
func (s *Store) Evict() (removed int, freed int64, err error) {
	max := s.MaxBytes
	if max <= 0 {
		max = defaultMaxBytes
	}
	var all []entry
	var total int64
	err = filepath.Walk(s.Dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || !strings.HasSuffix(p, ".bin") {
			return nil //nolint:nilerr // a vanished file is not this walk's problem
		}
		all = append(all, entry{path: p, size: fi.Size(), atime: fi.ModTime()})
		total += fi.Size()
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	if total <= max {
		return 0, 0, nil
	}
	sort.Slice(all, func(i, j int) bool { return all[i].atime.Before(all[j].atime) })
	for _, e := range all {
		if total <= max {
			break
		}
		if err := os.Remove(e.path); err != nil {
			continue
		}
		total -= e.size
		freed += e.size
		removed++
	}
	return removed, freed, nil
}

// DropNamespace removes every state for one backend — what to call when a
// backend goes away, because its states can never be restored into anything
// else and would otherwise sit there until eviction gets to them.
func (s *Store) DropNamespace(namespace string) error {
	return os.RemoveAll(filepath.Join(s.Dir, namespace))
}

// Manager decides nothing about scheduling and everything about safety.
//
// One backend, one slot, many conversations: it remembers which conversation is
// resident, and when a different one arrives it saves what is there and restores
// what is coming — under a lock, so two requests can never be doing that to the
// same slot at once.
//
// EVERY FAILURE IS SURVIVABLE BY DESIGN. A save that fails costs the outgoing
// conversation its cache; a restore that fails costs the incoming one a reprocess.
// Both are what happens today, every time, without this. So nothing here returns
// an error to the request path: it reports what it did, and the request proceeds
// either way. The one thing it must never do is leave the slot holding a
// conversation it will later claim is a different one.
type Manager struct {
	Store *Store
	// Slot is the llama.cpp slot id. One, because this exists for --parallel 1.
	Slot int
	// Timeout bounds a save or a restore. Measured at 0.6 s and 0.3 s for a
	// 740 MB state; a bound well above that turns a hung backend into a slow
	// request rather than a stuck one.
	Timeout time.Duration

	mu          sync.Mutex
	resident    map[string]string // backend id -> conversation key
	unsupported map[string]bool   // backend id -> "asked once, it cannot do this"
}

// Swap makes key the resident conversation on a backend, saving whatever was
// there. It returns a short description of what happened, for the log, and
// whether the incoming conversation's cache was restored.
func (m *Manager) Swap(ctx context.Context, c *Client, backendID, namespace, key string) (what string, restored bool) {
	if m == nil || m.Store == nil || key == "" {
		return "", false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.unsupported[backendID] {
		return "", false
	}
	if m.resident == nil {
		m.resident = map[string]string{}
		m.unsupported = map[string]bool{}
	}
	if m.resident[backendID] == key {
		return "", false // already here; the backend's own prefix cache handles it
	}
	if err := m.Store.Prepare(namespace); err != nil {
		return "store unavailable: " + err.Error(), false
	}

	timeout := m.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	// Save the outgoing conversation FIRST. Doing it after the restore would
	// write the state we just loaded under the previous conversation's name — the
	// one bug in here that corrupts rather than merely slows.
	if prev := m.resident[backendID]; prev != "" {
		sctx, cancel := context.WithTimeout(ctx, timeout)
		_, _, err := c.Save(sctx, m.Slot, filepath.Join(namespace, prev))
		cancel()
		if err != nil {
			// A backend without --slot-save-path says so on the first attempt.
			// Ask once, then stop asking: a per-request error on a backend that
			// can never do this is noise in every log line.
			if isUnsupported(err) {
				m.unsupported[backendID] = true
				return "backend cannot save slots", false
			}
			what = "save failed: " + err.Error()
		}
	}

	// The slot no longer holds what it held, whether or not the save worked.
	m.resident[backendID] = key

	if !m.Store.Has(namespace, key) {
		return what, false
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	_, _, err := c.Restore(rctx, m.Slot, filepath.Join(namespace, key))
	cancel()
	if err != nil {
		if isUnsupported(err) {
			m.unsupported[backendID] = true
			return "backend cannot restore slots", false
		}
		return "restore failed: " + err.Error(), false
	}
	if _, _, err := m.Store.Evict(); err != nil {
		return "restored, but eviction failed: " + err.Error(), true
	}
	return "restored", true
}

// Forget drops a backend's resident marker — call it when a backend stops, so
// the next request does not try to save a slot that belongs to a process which
// no longer exists.
func (m *Manager) Forget(backendID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.resident, backendID)
	delete(m.unsupported, backendID)
}

// isUnsupported spots a backend that was started without --slot-save-path.
// llama.cpp answers 501 for that, and there is no point asking it again.
func isUnsupported(err error) bool {
	s := err.Error()
	return strings.Contains(s, "returned 501") || strings.Contains(s, "slot save is not supported")
}
