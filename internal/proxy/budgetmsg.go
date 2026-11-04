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
	}
	if k, sel, ok := keyBudgetOf(scope); ok {
		return "key " + k + "'s budget " + selectorWords(sel)
	}
	return "the model-wide limit on " + scope + " (every key)"
}

// labelAmount renders an amount in a window label's dimension.
func labelAmount(label string, n float64) string {
	dim, _, _ := strings.Cut(label, "/")
	return amountWords(dim, n)
}

// budgetWords says a budget with a start and an end.
func budgetWords(label string, amount float64, start, end time.Time) string {
	return fmt.Sprintf("budget of %s created on %s, expires on %s", labelAmount(label, amount),
		start.UTC().Format("2006-01-02"), end.UTC().Format("2006-01-02 15:04 UTC"))
}

// timedBudgetWords adds a provider's unexpired timed budgets to its refusal: they
// are spent, or this refusal would not have happened.
func (p *Proxy) timedBudgetWords(scope string, now time.Time) string {
	if !config.IsProviderScope(scope) {
		return ""
	}
	var b strings.Builder
	for _, g := range p.grants.of(scope) {
		if g.ExpiresMS > now.UnixMilli() {
			fmt.Fprintf(&b, "; timed %s; $%.2f spent", budgetWords("usd/", g.USD,
				time.UnixMilli(g.CreatedMS), time.UnixMilli(g.ExpiresMS)), g.Spent)
		}
	}
	return b.String()
}

// windowWords renders a window with no period: a falloff one or a total.
func windowWords(label string, used, limit float64) string {
	_, per, _ := strings.Cut(label, "/")
	pw := map[string]string{
		"total":  "in total",
		"minute": "in the last minute", "hour": "in the last hour", "day": "in the last 24 hours", "month": "in the last 30 days",
	}[per]
	if pw == "" {
		pw = "per " + per
	}
	return fmt.Sprintf("%s of %s spent %s", labelAmount(label, used), labelAmount(label, limit), pw)
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
			if w.Paced {
				n.Summary = fmt.Sprintf("%s: today's share of the month, %s, is spent ($%.2f); it is what is left of the month over the days to its renewal; %s",
					n.Owner, labelAmount(w.Label, w.Limit), w.Used, room)
			} else if !w.StartedAt.IsZero() {
				n.Summary = fmt.Sprintf("%s: %s; %s spent", n.Owner,
					budgetWords(w.Label, w.Limit, w.StartedAt, w.ResetsAt), labelAmount(w.Label, w.Used))
			}
			n.Summary += p.timedBudgetWords(e.Backend, now)
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Scope < out[j].Scope })
	return out
}

// keyBudgetOf reads a key budget scope (config.KeyBudgetScope) back into the
// key and the selector.
func keyBudgetOf(scope string) (key, sel string, ok bool) {
	switch {
	case strings.HasPrefix(scope, "keycred:"):
		k, pat, _ := strings.Cut(strings.TrimPrefix(scope, "keycred:"), ">")
		return k, pat, true
	case strings.HasPrefix(scope, "key:"):
		rest := strings.TrimPrefix(scope, "key:")
		if k, s, found := strings.Cut(rest, "|"); found {
			return k, s, true
		}
		if k, m, found := strings.Cut(rest, "@"); found {
			return k, "model:" + m, true
		}
		return rest, config.SelectAll, true
	}
	return "", "", false
}

// selectorWords is a budget's selector as a refusal says it.
func selectorWords(sel string) string {
	if sel == config.SelectAll {
		return "on everything"
	}
	return "on " + sel
}

// budgetNotesFor is budgetNotes worded for the caller: its parent's budgets
// say "parent key".
func (p *Proxy) budgetNotesFor(c config.Caller, scopes []string) []BudgetNote {
	notes := p.budgetNotes(scopes)
	for i, n := range notes {
		if k, sel, ok := keyBudgetOf(n.Scope); ok && c.Parent != "" && k == c.Parent {
			owner := "parent key " + k + "'s budget " + selectorWords(sel)
			notes[i].Summary = owner + strings.TrimPrefix(n.Summary, n.Owner)
			notes[i].Owner = owner
		}
	}
	return notes
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
