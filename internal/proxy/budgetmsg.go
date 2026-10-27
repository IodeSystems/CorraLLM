package proxy

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

// A 429 for a spent budget used to say only "<model>: budget exhausted (hard
// limit); retry after 368 seconds" (owner 10-27, "getting booted by usage was
// fairly opaque"). The caller could not tell whose budget it was — the model's,
// shared by every key, or its own — how much had been spent, or that "71209
// seconds" meant "after midnight UTC". budgetNote names each full hard window,
// its spend against its limit, and when it has room again.

// BudgetNote is one full hard budget, as a refusal reports it.
type BudgetNote struct {
	Scope   string  `json:"scope"`             // ledger scope, e.g. "key:sk-x", "cred:fireworks/default"
	Owner   string  `json:"owner"`             // whose budget, in words
	Window  string  `json:"window"`            // e.g. "usd/calendar-day"
	Used    float64 `json:"used"`              // spent in the window
	Limit   float64 `json:"limit"`             // the window's limit
	RoomAt  string  `json:"room_at,omitempty"` // RFC 3339: when it admits again; empty = never (one-off)
	Summary string  `json:"summary"`           // the sentence the message uses
}

// scopeOwner says whose budget a ledger scope is, in words.
func scopeOwner(scope string) string {
	switch {
	case scope == config.GlobalScopeKey:
		return "the box-wide limit (every key)"
	case strings.HasPrefix(scope, "prov:"):
		return "the " + strings.TrimPrefix(scope, "prov:") + " provider's limit (every key)"
	case strings.HasPrefix(scope, "cred:"):
		return "the " + strings.TrimPrefix(scope, "cred:") + " account's limit (every key using it)"
	case strings.HasPrefix(scope, "keycred:"):
		k, pat, _ := strings.Cut(strings.TrimPrefix(scope, "keycred:"), ">")
		return "key " + k + "'s limit on accounts " + pat
	case strings.HasPrefix(scope, "key:"):
		k, model, share := strings.Cut(strings.TrimPrefix(scope, "key:"), "@")
		if share {
			return "key " + k + "'s share of " + model
		}
		return "key " + k + "'s own limit"
	}
	return "the model-wide limit on " + scope + " (every key)"
}

// windowWords renders a window label ("usd/calendar-day") and amounts.
func windowWords(label string, used, limit float64) string {
	dim, per, _ := strings.Cut(label, "/")
	amt := func(n float64) string {
		if dim == config.DimUSD {
			return fmt.Sprintf("$%.2f", n)
		}
		return fmt.Sprintf("%.0f %s", n, map[string]string{config.DimRequests: "requests", config.DimDwell: "s"}[dim])
	}
	pw := map[string]string{
		"calendar-day": "today (UTC)", "calendar-month": "this month (UTC)", "total": "in total",
		"minute": "in the last minute", "hour": "in the last hour", "day": "in the last 24 hours", "month": "in the last 30 days",
	}[per]
	if pw == "" {
		pw = "per " + per
	}
	return fmt.Sprintf("%s of %s spent %s", amt(used), amt(limit), pw)
}

// budgetNotes describes every full hard window among scopes.
func (p *Proxy) budgetNotes(scopes []string) []BudgetNote {
	if p.quota == nil {
		return nil
	}
	want := map[string]bool{}
	for _, s := range scopes {
		want[s] = true
	}
	now := time.Now()
	var out []BudgetNote
	for _, e := range p.quota.Snapshot() {
		if !want[e.Backend] {
			continue
		}
		_, wait := p.quota.HardBlocked(e.Backend)
		for _, w := range e.Windows {
			if !w.Hard || !w.Blocked {
				continue
			}
			n := BudgetNote{Scope: e.Backend, Owner: scopeOwner(e.Backend), Window: w.Label, Used: w.Used, Limit: w.Limit}
			room := "it does not refill"
			if !strings.HasSuffix(w.Label, "/total") && wait > 0 {
				at := now.Add(wait)
				n.RoomAt = at.UTC().Format(time.RFC3339)
				room = "room again in " + humanWait(wait) + " (" + at.UTC().Format("15:04 UTC") + ")"
			}
			n.Summary = fmt.Sprintf("%s: %s; %s", n.Owner, windowWords(w.Label, w.Used, w.Limit), room)
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Scope < out[j].Scope })
	return out
}

// keyScopes are a caller's own allocation scopes in the ledger: its key-wide
// ones and its per-model shares, and its parent's.
func (p *Proxy) keyScopes(key string) []string {
	if p.quota == nil {
		return nil
	}
	keys := []string{key}
	if parent, ok := p.children.get(key); ok && parent.Parent != "" {
		keys = append(keys, parent.Parent)
	}
	var out []string
	for _, e := range p.quota.Snapshot() {
		for _, k := range keys {
			if e.Backend == config.KeyScopeKey(k) || strings.HasPrefix(e.Backend, config.KeyScopeKey(k)+"@") ||
				strings.HasPrefix(e.Backend, "keycred:"+k+">") {
				out = append(out, e.Backend)
			}
		}
	}
	return out
}

// humanWait says a wait the way a person reads it.
func humanWait(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()+0.5))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()+0.5))
	case d < 48*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return fmt.Sprintf("%d h", h)
		}
		return fmt.Sprintf("%d h %d min", h, m)
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24+0.5))
}

func summaries(ns []BudgetNote) string {
	parts := make([]string, 0, len(ns))
	for _, n := range ns {
		parts = append(parts, n.Summary)
	}
	return strings.Join(parts, "; ")
}

// mustJSON is v as JSON, "[]" for nil or on error: a refusal body is always
// valid JSON.
func mustJSON(v []BudgetNote) string {
	if len(v) == 0 {
		return "[]"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}
