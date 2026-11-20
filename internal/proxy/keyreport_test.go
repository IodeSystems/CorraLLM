package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/store"
)

// getReport runs GET /v1/keys/report with key and query, decodes the body.
func getReport(t *testing.T, k *childKeyRig, key, query string) (int, keyReportResponse, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/keys/report?"+query, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	k.r.ServeHTTP(rec, req)
	var body keyReportResponse
	if rec.Code == http.StatusOK {
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
	}
	return rec.Code, body, rec.Body.String()
}

// mintFor mints a child under the named configured parent (k.mint hardcodes "parent").
func (k *childKeyRig) mintFor(parent, body string) childKeyBody {
	k.t.Helper()
	rec := k.do(http.MethodPost, "/v1/keys", parent, body)
	if rec.Code != http.StatusCreated {
		k.t.Fatalf("mint for %s: %d %s", parent, rec.Code, rec.Body.String())
	}
	var out childKeyBody
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		k.t.Fatalf("mint body: %v %s", err, rec.Body.String())
	}
	return out
}

func reportRow(rows []keyReportRow, key string) (keyReportRow, bool) {
	for _, r := range rows {
		if r.Key == key {
			return r, true
		}
	}
	return keyReportRow{}, false
}

// A parent reads its own direct traffic and every child's, each as a row
// broken down by served model, with all the report fields present.
func TestKeyReportParentSeesSelfAndChildren(t *testing.T) {
	k := newChildKeyRig(t, nil)
	a := k.mint(`{"label":"proj-a"}`)
	b := k.mint(`{"label":"proj-b"}`)

	// Direct traffic from the parent, and traffic from each child.
	if c := k.chat("parent", "mock"); c != http.StatusOK {
		t.Fatalf("parent chat: %d", c)
	}
	if c := k.chat(a.Key, "mock"); c != http.StatusOK {
		t.Fatalf("child a chat: %d", c)
	}
	if c := k.chat(a.Key, "other"); c != http.StatusOK {
		t.Fatalf("child a chat other: %d", c)
	}
	if c := k.chat(b.Key, "mock"); c != http.StatusOK {
		t.Fatalf("child b chat: %d", c)
	}

	code, body, raw := getReport(t, k, "parent", "")
	if code != http.StatusOK {
		t.Fatalf("report: %d %s", code, raw)
	}
	if len(body.Rows) != 3 {
		t.Fatalf("want 3 rows (parent + 2 children), got %d: %+v", len(body.Rows), body.Rows)
	}

	// The caller leads.
	if body.Rows[0].Key != "parent" {
		t.Fatalf("first row = %q, want the caller", body.Rows[0].Key)
	}

	// The parent's direct row: one model (mock), one call.
	pr, ok := reportRow(body.Rows, "parent")
	if !ok {
		t.Fatal("no row for the parent")
	}
	if len(pr.Models) != 1 || pr.Models[0].Served != "mock" || pr.Models[0].Calls != 1 {
		t.Errorf("parent row = %+v, want one mock call", pr)
	}

	// Child a: two models (mock, other), one call each, labelled.
	ra, ok := reportRow(body.Rows, a.Key)
	if !ok {
		t.Fatal("no row for child a")
	}
	if ra.Label != "proj-a" {
		t.Errorf("child a label = %q, want proj-a", ra.Label)
	}
	if len(ra.Models) != 2 {
		t.Fatalf("child a models = %+v, want two", ra.Models)
	}
	byModel := map[string]keyReportModel{}
	for _, m := range ra.Models {
		byModel[m.Served] = m
	}
	if m, ok := byModel["mock"]; !ok || m.Calls != 1 {
		t.Errorf("child a mock = %+v, want 1 call", m)
	}
	if m, ok := byModel["other"]; !ok || m.Calls != 1 {
		t.Errorf("child a other = %+v, want 1 call", m)
	}

	// Child b: one model, one call.
	rb, ok := reportRow(body.Rows, b.Key)
	if !ok {
		t.Fatal("no row for child b")
	}
	if rb.Label != "proj-b" || len(rb.Models) != 1 || rb.Models[0].Calls != 1 {
		t.Errorf("child b row = %+v, want proj-b with one call", rb)
	}
}

// A key cannot read another parent's children: the scope is fixed server-side
// from the roster, not from the request.
func TestKeyReportCannotReadAnotherParent(t *testing.T) {
	k := newChildKeyRig(t, nil)
	// "peer" mints a child of ITS OWN; "parent" has none.
	peerChild := k.mintFor("peer", `{"label":"peer-a"}`)
	// Give parent its own child too, so the report is not trivially empty.
	parentChild := k.mint(`{"label":"parent-a"}`)

	if c := k.chat(peerChild.Key, "mock"); c != http.StatusOK {
		t.Fatalf("peer child chat: %d", c)
	}
	if c := k.chat(parentChild.Key, "mock"); c != http.StatusOK {
		t.Fatalf("parent child chat: %d", c)
	}

	code, body, raw := getReport(t, k, "parent", "")
	if code != http.StatusOK {
		t.Fatalf("report: %d %s", code, raw)
	}
	// Only parent + parent's own child; peer's child must not appear.
	if len(body.Rows) != 2 {
		t.Fatalf("want 2 rows, got %d: %+v", len(body.Rows), body.Rows)
	}
	if _, ok := reportRow(body.Rows, peerChild.Key); ok {
		t.Errorf("parent's report contains peer's child %q", peerChild.Key)
	}
	if _, ok := reportRow(body.Rows, parentChild.Key); !ok {
		t.Errorf("parent's report is missing its own child %q", parentChild.Key)
	}
}

// A child key cannot read the report at all (same rule as /v1/keys management).
func TestKeyReportChildForbidden(t *testing.T) {
	k := newChildKeyRig(t, nil)
	a := k.mint(`{"label":"proj-a"}`)
	if code, _, raw := getReport(t, k, a.Key, ""); code != http.StatusForbidden {
		t.Fatalf("child report: %d %s, want 403", code, raw)
	}
}

// Unauthenticated and unknown keys are refused.
func TestKeyReportAuth(t *testing.T) {
	k := newChildKeyRig(t, nil)
	if code, _, raw := getReport(t, k, "", ""); code != http.StatusUnauthorized {
		t.Fatalf("no key: %d %s, want 401", code, raw)
	}
	if code, _, raw := getReport(t, k, "not-a-key", ""); code != http.StatusUnauthorized {
		t.Fatalf("unknown key: %d %s, want 401", code, raw)
	}
}

// The window narrows the report: since/until are RFC 3339 and the store's
// Window does the filtering.
func TestKeyReportWindow(t *testing.T) {
	k := newChildKeyRig(t, nil)
	a := k.mint(`{"label":"proj-a"}`)
	if c := k.chat(a.Key, "mock"); c != http.StatusOK {
		t.Fatalf("chat: %d", c)
	}

	// A window that excludes everything (far in the past) is empty of traffic.
	code, body, raw := getReport(t, k, "parent", "since=2000-01-01T00:00:00Z&until=2000-01-02T00:00:00Z")
	if code != http.StatusOK {
		t.Fatalf("report: %d %s", code, raw)
	}
	// The child row is present but carries no model rows in that window.
	ra, ok := reportRow(body.Rows, a.Key)
	if !ok {
		t.Fatal("no child row")
	}
	if len(ra.Models) != 0 {
		t.Errorf("child models in an empty window = %+v, want none", ra.Models)
	}
	// The window is echoed back.
	if body.Window.Since == "" || body.Window.Until == "" {
		t.Errorf("window not echoed: %+v", body.Window)
	}
}

// A malformed since is a 400.
func TestKeyReportBadWindow(t *testing.T) {
	k := newChildKeyRig(t, nil)
	if code, _, raw := getReport(t, k, "parent", "since=not-a-time"); code != http.StatusBadRequest {
		t.Fatalf("bad since: %d %s, want 400", code, raw)
	}
}

// The store query is what the handler feeds: guard against a key the caller
// does not own leaking in through the handler even if the roster changed.
func TestKeyReportHandlerFeedsOwnedKeysOnly(t *testing.T) {
	k := newChildKeyRig(t, nil)
	// Seed activity for a key that belongs to NOBODY in parent's scope.
	if err := k.st.InsertActivity(store.Activity{TS: 1, Served: "mock", Key: "stranger", Status: 200}); err != nil {
		t.Fatal(err)
	}
	_, _, raw := getReport(t, k, "parent", "")
	if strings.Contains(raw, "stranger") {
		t.Errorf("stranger key leaked into the report: %s", raw)
	}
}
