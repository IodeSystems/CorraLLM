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
	"time"
)

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
func Key(caller string, body []byte) (string, bool) {
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
