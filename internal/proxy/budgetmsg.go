package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
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
func (p *Proxy) scopeOwner(scope string) string {
	switch {
	case scope == config.GlobalScopeKey:
		return "the box-wide limit (every key)"
	case strings.HasPrefix(scope, "prov:"):
		return "the " + strings.TrimPrefix(scope, "prov:") + " provider's limit (every key)"
	case strings.HasPrefix(scope, "cred:"):
		return "the " + strings.TrimPrefix(scope, "cred:") + " account's limit (every key using it)"
	}
	if k, sel, ok := keyBudgetOf(scope); ok {
		return "key " + p.keyName(k) + "'s budget " + selectorWords(sel)
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
			n := BudgetNote{Scope: e.Backend, Owner: p.scopeOwner(e.Backend), Window: w.Label, Used: w.Used, Limit: w.Limit}
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
			owner := "parent key " + p.keyName(k) + "'s budget " + selectorWords(sel)
			notes[i].Summary = owner + strings.TrimPrefix(n.Summary, n.Owner)
			notes[i].Owner = owner
			// The scope embeds the parent's secret; a child must not read it.
			notes[i].Scope = "parent:" + sel
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

// refusalStatus picks a budget refusal's status and sets its headers. A budget that
// refills at a reset time (a calendar day or month, a total) is out of money until
// then: 402, with no Retry-After, because clients retry a 429 with backoff and this
// wait is hours or days. A rolling window refills as it leaks: 429 with Retry-After.
// error.type and retry_after in the body are the same either way.
func refusalStatus(notes []BudgetNote, secs int, w http.ResponseWriter) int {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	for _, n := range notes {
		if _, per, _ := strings.Cut(n.Window, "/"); per == "total" || strings.HasPrefix(per, "calendar-") {
			return http.StatusPaymentRequired
		}
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	return http.StatusTooManyRequests
}

// errorContract is what a refusal's status means and what a client MUST do with
// it, served in /v1/capabilities. The status codes are corrallm's contract: a
// client library may handle them where it can, and an integrator has to.
var errorContract = map[string]any{
	"shape": `{"error":{"message","type","param"?,"code"?,"retry_after"?,"provider"?,"upstream_status"?,"budgets"?:[{scope,owner,window,used,limit,room_at,summary}]}}`,
	"statuses": []map[string]any{
		{"status": 402, "types": []string{"budget_exhausted", "allocation_exhausted", "allocation_spent"},
			"means": "a budget that refills at a reset time (calendar day, paced day, calendar month, total) is spent; no Retry-After",
			"do":    "do not retry; fall back to another model or fail now; retry_after and budgets[].room_at say when it reopens (allocation_spent never does)"},
		{"status": 402, "types": []string{"upstream_funds_exhausted"},
			"means": "not your balance: the remote account corrallm pays with is out of funds or over its billing quota, and no other candidate could serve; the body adds provider and upstream_status",
			"do":    "do not retry; fall back to another model; the owner has been alerted"},
		{"status": 429, "types": []string{"budget_exhausted", "allocation_exhausted", "paused", "backpressure", ""},
			"means": "a rolling budget is full, a model is paused, the scheduler is busy, or a backend is draining for a restart or unload; Retry-After is set",
			"do":    "wait Retry-After seconds and retry, or fall back"},
		{"status": 503, "types": []string{"backend_restarting"},
			"means": "a local backend is stopping, restarting or not yet listening (connection refused, or the connection closed before any reply); no part of a reply was sent; Retry-After is set (5 s)",
			"do":    "wait Retry-After seconds and retry; the request is safe to resend. A request that arrives while corrallm restarts a backend on purpose (corrallm backend restart) is held until the new process is ready instead"},
		{"status": 503, "means": "no backend is available, or corrallm is restarting",
			"do": "on \"restarting\" retry in a few seconds; otherwise fall back"},
		{"status": 502, "means": "a backend's reply was broken (not HTTP, or not the shape the route needs), a remote upstream failed, or a required second backend call failed (the message names it). A stream that already started is cut instead: no status can change after the first byte",
			"do": "retry once, then fall back"},
		{"status": 401, "types": []string{"invalid_request_error"}, "codes": []string{"invalid_api_key"},
			"means": "the key is switched off, expired or deleted, or (only when the operator set unknownKeys.allow false) there is no key or corrallm does not know it; the body is {error:{message,type,code}}",
			"do":    "fail"},
		{"status": 403, "means": "this key may not use this model", "do": "fail, or use a model the key may use"},
		{"status": 400, "means": "the request is malformed", "do": "fail"},
		{"status": 400, "types": []string{"invalid_request_error"}, "codes": []string{"unsupported_parameter"},
			"means": "a field corrallm documents but this backend cannot honour (e.g. rotation priors on /v1/reconstructions); param names it",
			"do":    "drop the field and resend"},
		{"status": 404, "types": []string{"invalid_request_error"},
			"means": "a file, upload, reconstruction or content variant that does not exist for this key (another key's id answers the same), or an unknown model",
			"do":    "fail; do not retry"},
		{"status": 409, "types": []string{"invalid_request_error"},
			"means": "the object's state does not allow the call: delete a running job, complete a finished upload, fetch content before completion, delete a file a job uses",
			"do":    "read the object's state and act on it"},
		{"status": 413, "types": []string{"request_too_large"},
			"means": "the body is over the route's cap (32 MiB on /v1/images/segmentations; 64 MiB on audio uploads; 512 MiB per /v1/files file; 64 MiB per /v1/uploads part; 8 GiB per upload; 1 MiB per JSON reconstruction create); the message names the cap",
			"do":    "shrink the request or split it through /v1/uploads; do not retry as is"},
		{"status": 507, "types": []string{"insufficient_storage"}, "codes": []string{"storage_quota_exceeded", "storage_full"},
			"means": "your key's file quota, or the extension's disk, is full",
			"do":    "delete files or finished jobs, then retry"},
	},
}
