package proxy

import (
	"fmt"
	"net/http"
	"time"

	"github.com/iodesystems/corrallm/internal/store"
)

// /v1/keys/report: the per-child-key usage report, over a window, as JSON.
//
// A parent key reads its own direct traffic AND every child's, each as its own
// row, broken down by served model — the machine-readable counterpart to the
// `corrallm keys report` CLI. The scope is fixed server-side: the caller plus
// every key it minted (children.of). A key cannot be told to read someone
// else's children, and a child cannot read at all (children do not manage
// keys; the same rule that gates /v1/keys gates this).
//
//	GET /v1/keys/report?since=<RFC 3339>&until=<RFC 3339>
//
// Both are optional; the window defaults to the whole log.
//
// The response:
//
//	{"window": {"since": …, "until": …},
//	 "rows": [{"key": "ck-…", "label": "proj-a",
//	            "models": [{"served": "deepseek", "calls": 42, "errors": 1, …}]}]}
//
// The caller's own key leads the rows; each minted child follows in roster
// order. A key with no traffic in the window appears with an empty models
// list, so "this child made no calls" is distinguishable from "this child does
// not exist".

type keyReportModel struct {
	Served           string  `json:"served"`
	Calls            int64   `json:"calls"`
	Errors           int64   `json:"errors"`
	DwellMS          int64   `json:"dwellMs"`
	QueuedMS         int64   `json:"queuedMs"`
	PromptTokens     int64   `json:"promptTokens"`
	CachedTokens     int64   `json:"cachedTokens"`
	CompletionTokens int64   `json:"completionTokens"`
	ReasoningTokens  int64   `json:"reasoningTokens"`
	CostUSD          float64 `json:"costUsd"`
}

type keyReportRow struct {
	Key    string            `json:"key"`
	Label  string            `json:"label"`
	Models []keyReportModel  `json:"models"`
}

type keyReportResponse struct {
	Window keyReportWindow `json:"window"`
	Rows   []keyReportRow  `json:"rows"`
}

type keyReportWindow struct {
	Since string `json:"since,omitempty"`
	Until string `json:"until,omitempty"`
}

// handleKeyReport serves GET /v1/keys/report, scoped to the caller.
func (p *Proxy) handleKeyReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		jsonErr(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	caller := p.resolveCaller(callerKey(r))
	switch {
	case caller.Key == "":
		jsonErr(w, http.StatusUnauthorized, "send your corrallm key as Authorization: Bearer <key>")
		return
	case !caller.Recognized:
		jsonErr(w, http.StatusUnauthorized, "unknown key")
		return
	case p.revokedKey(caller) != "":
		jsonErr(w, http.StatusUnauthorized, p.revokedKey(caller))
		return
	case caller.Parent != "":
		jsonErr(w, http.StatusForbidden, "a child key cannot read the report; use the key that minted it")
		return
	}
	if p.store == nil {
		jsonErr(w, http.StatusServiceUnavailable, "the report needs the database")
		return
	}

	wnd, err := reportWindow(r)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// The scope is the caller and its children, decided from the roster — never
	// from the request. The caller leads so its direct traffic is a separate row.
	kids := p.children.of(caller.Key)
	keys := make([]string, 0, len(kids)+1)
	keys = append(keys, caller.Key)
	for _, k := range kids {
		keys = append(keys, k.Key)
	}
	rows, err := p.store.KeyReport(wnd, keys)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "report: "+err.Error())
		return
	}

	// Group the (key, served) rows by key, keeping roster order: caller first,
	// then each child in the order children.of returned.
	labels := map[string]string{caller.Key: ""}
	order := map[string]int{caller.Key: 0}
	for i, k := range kids {
		labels[k.Key] = k.Label
		order[k.Key] = i + 1
	}
	models := map[string][]keyReportModel{}
	for _, r := range rows {
		models[r.Key] = append(models[r.Key], keyReportModel{
			Served:           r.Served,
			Calls:            r.Calls,
			Errors:           r.Errors,
			DwellMS:          r.DwellMS,
			QueuedMS:         r.QueuedMS,
			PromptTokens:     r.PromptTokens,
			CachedTokens:     r.CachedTokens,
			CompletionTokens: r.CompletionTokens,
			ReasoningTokens:  r.ReasoningTokens,
			CostUSD:          r.CostUSD,
		})
	}
	out := make([]keyReportRow, 0, len(keys))
	for _, k := range keys {
		out = append(out, keyReportRow{Key: k, Label: labels[k], Models: models[k]})
	}
	// A key with no traffic gets an empty models list, not an omitted one.
	for i := range out {
		if out[i].Models == nil {
			out[i].Models = []keyReportModel{}
		}
	}

	jsonResp(w, http.StatusOK, keyReportResponse{
		Window: keyReportWindow{
			Since: fmtWindowTime(wnd.FromMS),
			Until: fmtWindowTime(wnd.ToMS),
		},
		Rows: out,
	})
}

// reportWindow parses since/until (RFC 3339) into a store.Window. An omitted
// bound is unbounded; a malformed one is an error.
func reportWindow(r *http.Request) (store.Window, error) {
	q := r.URL.Query()
	var w store.Window
	if s := q.Get("since"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return w, fmt.Errorf("since: want RFC 3339, e.g. 2026-11-01T00:00:00Z")
		}
		w.FromMS = t.UnixMilli()
	}
	if s := q.Get("until"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return w, fmt.Errorf("until: want RFC 3339, e.g. 2026-11-01T00:00:00Z")
		}
		w.ToMS = t.UnixMilli()
	}
	return w, nil
}

// fmtWindowTime renders a unix-milli bound as RFC 3339, or "" when unbounded.
func fmtWindowTime(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}
