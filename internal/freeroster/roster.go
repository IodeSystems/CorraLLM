// Package freeroster keeps the set of models a provider currently offers for
// free, refreshed periodically (P16e). A provider's :free roster churns —
// OpenRouter adds and drops free models — so a statically-configured free model
// can silently go paid (402) or vanish (404). Error-spill catches that
// reactively; this catches it PROACTIVELY: a model that has left the roster is
// marked stale and the selector routes around it before a request hits it.
package freeroster

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Roster holds each provider's currently-free model ids. Safe for concurrent use.
type Roster struct {
	mu         sync.RWMutex
	byProvider map[string]*providerRoster
}

type providerRoster struct {
	free      map[string]bool
	fetchedAt time.Time
	err       string
}

// New builds an empty roster.
func New() *Roster { return &Roster{byProvider: map[string]*providerRoster{}} }

// Set records a provider's free set (or a fetch error, leaving the prior set in
// place so a transient fetch failure does not wrongly strand every model).
func (r *Roster) Set(provider string, freeIDs []string, err error, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pr := r.byProvider[provider]
	if pr == nil {
		pr = &providerRoster{free: map[string]bool{}}
		r.byProvider[provider] = pr
	}
	pr.fetchedAt = now
	if err != nil {
		pr.err = err.Error()
		return
	}
	pr.err = ""
	pr.free = make(map[string]bool, len(freeIDs))
	for _, id := range freeIDs {
		pr.free[id] = true
	}
}

// Has reports whether a model is free for a provider. known is false when the
// provider has never been fetched (or the last fetch errored and left no set),
// so a caller can distinguish "confirmed gone" from "don't know yet" and avoid
// stranding a model on missing data.
func (r *Roster) Has(provider, id string) (free, known bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	pr := r.byProvider[provider]
	if pr == nil || len(pr.free) == 0 {
		return false, false
	}
	return pr.free[id], true
}

// ProviderView is a provider's roster for the API.
type ProviderView struct {
	Provider  string   `json:"provider"`
	Free      []string `json:"free"`
	Count     int      `json:"count"`
	FetchedAt int64    `json:"fetchedAt,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// Snapshot returns each provider's roster for display, provider-sorted.
func (r *Roster) Snapshot() []ProviderView {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ProviderView, 0, len(r.byProvider))
	for name, pr := range r.byProvider {
		ids := make([]string, 0, len(pr.free))
		for id := range pr.free {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		v := ProviderView{Provider: name, Free: ids, Count: len(ids), Error: pr.err}
		if !pr.fetchedAt.IsZero() {
			v.FetchedAt = pr.fetchedAt.Unix()
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out
}

// FetchFree GETs an OpenAI-compatible /v1/models endpoint (base URL + auth
// header) and returns the ids that are free — either a ":free" id suffix or
// pricing prompt/completion both "0". The two signals normally agree; either is
// sufficient (see the OpenRouter research).
func FetchFree(ctx context.Context, hc *http.Client, modelsURL string, headers map[string]string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("models endpoint returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	return parseFree(body)
}

func parseFree(body []byte) ([]string, error) {
	var doc struct {
		Data []struct {
			ID      string `json:"id"`
			Pricing struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parse models: %w", err)
	}
	var free []string
	for _, m := range doc.Data {
		if hasFreeSuffix(m.ID) || (m.Pricing.Prompt == "0" && m.Pricing.Completion == "0") {
			free = append(free, m.ID)
		}
	}
	return free, nil
}

func hasFreeSuffix(id string) bool {
	const s = ":free"
	return len(id) >= len(s) && id[len(id)-len(s):] == s
}
