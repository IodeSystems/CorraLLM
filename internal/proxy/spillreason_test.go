package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// A free-tier backend whose provider has stopped serving it answers 402/401/403.
// corrallm spills to the next candidate; when there is no next candidate the
// caller gets 503 "no backend available".
//
// That sentence is about corrallm, and it was the ONLY account of the failure a
// person could read: the real reason ("Cerebras answered 402") lived in one log
// line. On the live box that produced 8 rows across five days all claiming the
// box had nothing to offer, when the actual problem was an unpaid account.
func TestExhaustedAfterAHardFailRecordsWhatTheBackendSaid(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":"insufficient credit"}`))
	}))
	t.Cleanup(up.Close)

	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr := proc.NewManager(&config.Config{})
	t.Cleanup(mgr.Shutdown)

	broke := modelTo(t, up.URL, "chat")
	broke.FreeTier = &config.FreeTier{Provider: "somebody", Private: true}
	cfg := &config.Config{
		Models:         map[string]config.Model{"broke": broke},
		PriorityGroups: map[string]config.PriorityGroup{"default": {Weight: 1}},
		Keys:           map[string]config.KeyPolicy{"yscr": {Group: "default"}},
	}
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"broke"}`))
	req.Header.Set("X-Corrallm-Key", "yscr")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 once the only candidate hard-failed", rec.Code)
	}
	// The wire response is deliberately unchanged — callers parse it, and the
	// upstream's status is the operator's business.
	if !strings.Contains(rec.Body.String(), "no backend available") {
		t.Errorf("wire body changed: %q", rec.Body.String())
	}

	rows, err := st.RecentActivity(store.Window{}, 10, "", "", "")
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 activity row, got %d", len(rows))
	}
	if got := rows[0].Error; !strings.Contains(got, "402") || !strings.Contains(got, "broke") {
		t.Errorf("row error = %q, want it to name the backend and what it said", got)
	}
}

// The durable half. The ledger already counts consecutive hard failures, but in
// memory and cleared by any success — so it answers "how hard should I back off
// now" and forgets everything on a restart. That is exactly how
// cerebras-gpt-oss-120b refused for five days without a single screen saying so.
//
// This records when the refusals STARTED and survives the process, and a
// success wipes it, because the table is a list of what is broken now.
func TestARefusingBackendIsRecordedUntilItServesAgain(t *testing.T) {
	broken := true
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if broken {
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = w.Write([]byte(`{"error":"insufficient credit"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(up.Close)

	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr := proc.NewManager(&config.Config{})
	t.Cleanup(mgr.Shutdown)

	m := modelTo(t, up.URL, "chat")
	m.FreeTier = &config.FreeTier{Provider: "somebody", Private: true}
	cfg := &config.Config{
		Models:         map[string]config.Model{"rung": m},
		PriorityGroups: map[string]config.PriorityGroup{"default": {Weight: 1}},
		Keys:           map[string]config.KeyPolicy{"yscr": {Group: "default"}},
	}
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)

	send := func() int {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"rung"}`))
		req.Header.Set("X-Corrallm-Key", "yscr")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}

	send()
	send()
	rows, err := st.RefusingBackends()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].Backend != "rung" {
		t.Fatalf("want rung recorded as refusing, got %v", rows)
	}
	if rows[0].Count != 2 || rows[0].Status != 402 {
		t.Errorf("count/status = %d/%d, want 2/402", rows[0].Count, rows[0].Status)
	}

	// A NEW Proxy over the same store stands in for a restart: the streak has to
	// be picked back up, or the first success would not know to clear it.
	broken = false
	r2 := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r2)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"rung"}`))
	req.Header.Set("X-Corrallm-Key", "yscr")
	rec := httptest.NewRecorder()
	r2.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 once the backend recovered", rec.Code)
	}
	rows, err = st.RefusingBackends()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("want the streak cleared after a success across a restart, got %v", rows)
	}
}
