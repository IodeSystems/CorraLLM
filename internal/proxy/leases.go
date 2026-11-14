package proxy

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
)

// Lease API (P78 §7): /api/v1/leases. Called by an extension's own process
// (the colmap runner), authenticated by that extension's lease.token — not
// the admin token, and not a caller key. Never listed in /v1/capabilities.
//
//	POST   /api/v1/leases            grant (201) | 429 lease_busy | 503 lease_never_fits | 400 | 409 (held: does not fit)
//	GET    /api/v1/leases/{id}?wait= long-poll: held | yield_requested; 404 unknown
//	DELETE /api/v1/leases/{id}       204; 404 unknown

// leaseExtension names the extension whose lease token r carries.
func (p *Proxy) leaseExtension(r *http.Request) (string, bool) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	tok = strings.TrimSpace(tok)
	if !ok || tok == "" {
		return "", false
	}
	cfg := p.config()
	names := make([]string, 0, len(cfg.Extensions))
	for n := range cfg.Extensions {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		l := cfg.Extensions[n].Lease
		if l == nil {
			continue
		}
		want := l.ExpandedToken()
		if want != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(want)) == 1 {
			return n, true
		}
	}
	return "", false
}

type leaseRequestBody struct {
	Extension string            `json:"extension"`
	Job       string            `json:"job"`
	Stage     string            `json:"stage"`
	Owner     string            `json:"owner"`
	Usage     map[string]string `json:"usage"`
	Yieldable bool              `json:"yieldable"`
	Held      bool              `json:"held"`
}

func leaseError(w http.ResponseWriter, status int, kind, msg string, extra map[string]any) {
	e := map[string]any{"type": kind, "message": msg}
	for k, v := range extra {
		e[k] = v
	}
	jsonResp(w, status, map[string]any{"error": e})
}

func (p *Proxy) handleLeases(w http.ResponseWriter, r *http.Request) {
	ext, ok := p.leaseExtension(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="corrallm-lease"`)
		leaseError(w, http.StatusUnauthorized, "auth", "lease token required", nil)
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/leases"), "/")
	switch {
	case id == "" && r.Method == http.MethodPost:
		p.grantLease(w, r, ext)
	case id != "" && !strings.Contains(id, "/") && (r.Method == http.MethodGet || r.Method == http.MethodDelete):
		// One extension's token never reaches another's lease; an unknown id
		// and a foreign one answer the same.
		if owner, ok := p.mgr.LeaseExtension(id); !ok || owner != ext {
			leaseError(w, http.StatusNotFound, "not_found", "no such lease", nil)
			return
		}
		if r.Method == http.MethodDelete {
			if err := p.mgr.ReleaseLease(id); err != nil {
				leaseError(w, http.StatusNotFound, "not_found", "no such lease", nil)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		p.leaseStatus(w, r, id)
	default:
		leaseError(w, http.StatusMethodNotAllowed, "invalid_request_error", r.Method+" "+r.URL.Path+" is not a lease call", nil)
	}
}

func (p *Proxy) grantLease(w http.ResponseWriter, r *http.Request, ext string) {
	var body leaseRequestBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		leaseError(w, http.StatusBadRequest, "invalid_request_error", "body: "+err.Error(), nil)
		return
	}
	if body.Extension == "" {
		body.Extension = ext
	}
	if body.Extension != ext {
		leaseError(w, http.StatusForbidden, "permission_error", "this token belongs to extension "+ext, nil)
		return
	}
	usage := make(map[string]int64, len(body.Usage))
	for pool, s := range body.Usage {
		n, err := config.ParseSize(s)
		if err != nil {
			leaseError(w, http.StatusBadRequest, "invalid_request_error", "usage."+pool+": "+err.Error(), nil)
			return
		}
		usage[pool] = n
	}
	g, err := p.mgr.AcquireLease(r.Context(), proc.LeaseRequest{
		Extension: ext, Job: body.Job, Stage: body.Stage, Owner: body.Owner,
		Usage: usage, Yieldable: body.Yieldable, Held: body.Held,
	})
	var le *proc.LeaseRequestError
	var ce *proc.CapacityError
	switch {
	case err == nil:
		jsonResp(w, http.StatusCreated, map[string]any{
			"id": g.ID, "granted_at": g.GrantedAt.Unix(), "protected_until": g.ProtectedUntil.Unix(),
		})
	case errors.As(err, &le):
		leaseError(w, http.StatusBadRequest, "invalid_request_error", le.Msg, nil)
	case errors.Is(err, proc.ErrLeaseConflict):
		leaseError(w, http.StatusConflict, "lease_conflict", err.Error(), nil)
	case errors.As(err, &ce) && ce.Permanent:
		leaseError(w, http.StatusServiceUnavailable, "lease_never_fits", ce.Error(), map[string]any{"blocking": nonNil(ce.Blocking)})
	case errors.As(err, &ce):
		secs := int((ce.RetryAfter + time.Second - 1) / time.Second)
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		leaseError(w, http.StatusTooManyRequests, "lease_busy", ce.Error(), map[string]any{"blocking": nonNil(ce.Blocking), "retry_after": secs})
	case errors.Is(err, proc.ErrDraining):
		leaseError(w, http.StatusServiceUnavailable, "restarting", "corrallm is handing over; re-register with the successor", nil)
	default:
		leaseError(w, http.StatusServiceUnavailable, "lease_unavailable", err.Error(), nil)
	}
}

func (p *Proxy) leaseStatus(w http.ResponseWriter, r *http.Request, id string) {
	var wait time.Duration
	if v := r.URL.Query().Get("wait"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			leaseError(w, http.StatusBadRequest, "invalid_request_error", "wait: a duration such as 30s", nil)
			return
		}
		wait = d
	}
	st, err := p.mgr.LeaseStatus(r.Context(), id, wait)
	if err != nil {
		leaseError(w, http.StatusNotFound, "not_found", "no such lease", nil)
		return
	}
	out := map[string]any{"id": st.ID, "state": st.State}
	if st.State == proc.LeaseYieldRequested {
		out["wanted_by"] = st.WantedBy
		out["deadline"] = st.Deadline.Unix()
	}
	jsonResp(w, http.StatusOK, out)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
