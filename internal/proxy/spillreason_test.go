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
