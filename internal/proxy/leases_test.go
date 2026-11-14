package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
)

func leaseMount(t *testing.T) (*chi.Mux, *proc.Manager) {
	t.Helper()
	t.Setenv("LEASE_TOKEN_TEST", "tok-colmap")
	body := `
servers:
  box1:
    pools: { gpu1: 10GB, system: 100GB }
extensions:
  colmap:
    cmd: "exec colmap-svc"
    server: box1
    proxy: 5822
    lease: { token: "${LEASE_TOKEN_TEST}", max: { gpu1: 8GB, system: 40GB } }
    provides:
      sfm: { type: reconstruction }
  other:
    cmd: "exec other"
    server: box1
    proxy: 5823
    lease: { token: tok-other, max: { gpu1: 1GB } }
    provides:
      x: { type: reconstruction }
`
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	mgr := proc.NewManager(cfg)
	t.Cleanup(mgr.Shutdown)
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), nil).Mount(r)
	return r, mgr
}

func leaseCall(r http.Handler, method, path, tok, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// The wire B4 implements against: POST → 201 {id, granted_at,
// protected_until}; GET → {id, state}; DELETE → 204; then 404.
func TestLeaseHandlersWire(t *testing.T) {
	r, mgr := leaseMount(t)
	req := `{"extension":"colmap","job":"recon_1","stage":"match","owner":"o1","usage":{"gpu1":"4GB","system":"6GB"},"yieldable":true}`
	if rec := leaseCall(r, http.MethodPost, "/api/v1/leases", "", req); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", rec.Code)
	}
	if rec := leaseCall(r, http.MethodPost, "/api/v1/leases", "wrong", req); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad token: %d", rec.Code)
	}
	if rec := leaseCall(r, http.MethodPost, "/api/v1/leases", "tok-other", req); rec.Code != http.StatusForbidden {
		t.Errorf("other extension's token: %d", rec.Code)
	}
	rec := leaseCall(r, http.MethodPost, "/api/v1/leases", "tok-colmap", req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("grant: %d %s", rec.Code, rec.Body.String())
	}
	var g struct {
		ID             string `json:"id"`
		GrantedAt      int64  `json:"granted_at"`
		ProtectedUntil int64  `json:"protected_until"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &g)
	if !strings.HasPrefix(g.ID, "lease_") || g.GrantedAt == 0 || g.ProtectedUntil < g.GrantedAt {
		t.Fatalf("grant body = %s", rec.Body.String())
	}
	if l := mgr.Snapshot().Leases; len(l) != 1 || l[0].Owner != "o1" || l[0].Name != "lease:colmap/recon_1/match" {
		t.Errorf("snapshot = %+v", l)
	}
	rec = leaseCall(r, http.MethodGet, "/api/v1/leases/"+g.ID+"?wait=0s", "tok-colmap", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"state":"held"`) {
		t.Errorf("status: %d %s", rec.Code, rec.Body.String())
	}
	if rec := leaseCall(r, http.MethodGet, "/api/v1/leases/"+g.ID, "tok-other", ""); rec.Code != http.StatusNotFound {
		t.Errorf("other extension reads the lease: %d", rec.Code)
	}
	if rec := leaseCall(r, http.MethodDelete, "/api/v1/leases/"+g.ID, "tok-other", ""); rec.Code != http.StatusNotFound {
		t.Errorf("other extension releases the lease: %d", rec.Code)
	}
	if rec := leaseCall(r, http.MethodDelete, "/api/v1/leases/"+g.ID, "tok-colmap", ""); rec.Code != http.StatusNoContent {
		t.Errorf("release: %d", rec.Code)
	}
	if rec := leaseCall(r, http.MethodGet, "/api/v1/leases/"+g.ID, "tok-colmap", ""); rec.Code != http.StatusNotFound {
		t.Errorf("after release: %d", rec.Code)
	}
}

func TestLeaseHandlersRefusals(t *testing.T) {
	r, _ := leaseMount(t)
	for _, c := range []struct {
		body string
		want int
		typ  string
	}{
		{`{"usage":{"gpu1":"9GB"},"yieldable":true}`, http.StatusBadRequest, "invalid_request_error"}, // over max
		{`{"usage":{"gpu7":"1GB"}}`, http.StatusBadRequest, "invalid_request_error"},                  // unknown pool
		{`{"usage":{"gpu1":"lots"}}`, http.StatusBadRequest, "invalid_request_error"},
		{`not json`, http.StatusBadRequest, "invalid_request_error"},
	} {
		rec := leaseCall(r, http.MethodPost, "/api/v1/leases", "tok-colmap", c.body)
		if rec.Code != c.want || !strings.Contains(rec.Body.String(), c.typ) {
			t.Errorf("%s: %d %s", c.body, rec.Code, rec.Body.String())
		}
	}
	// 8 GB held, then 8 GB more (gpu1 is 10 GB): busy → 429 + Retry-After,
	// blocking names the first lease. A held re-registration that does not
	// fit is 409.
	if rec := leaseCall(r, http.MethodPost, "/api/v1/leases", "tok-colmap", `{"job":"a","stage":"match","usage":{"gpu1":"8GB"}}`); rec.Code != http.StatusCreated {
		t.Fatalf("first: %d %s", rec.Code, rec.Body.String())
	}
	rec := leaseCall(r, http.MethodPost, "/api/v1/leases", "tok-colmap", `{"job":"b","stage":"match","usage":{"gpu1":"8GB"},"yieldable":true}`)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" ||
		!strings.Contains(rec.Body.String(), "lease_busy") || !strings.Contains(rec.Body.String(), "lease:colmap/a/match") {
		t.Errorf("busy: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}
	rec = leaseCall(r, http.MethodPost, "/api/v1/leases", "tok-colmap", `{"job":"b","stage":"match","usage":{"gpu1":"8GB"},"held":true}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("held over capacity: %d %s", rec.Code, rec.Body.String())
	}
	if rec := leaseCall(r, http.MethodPut, "/api/v1/leases", "tok-colmap", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT: %d", rec.Code)
	}
}
