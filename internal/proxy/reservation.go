package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/events"
)

// handleReservations serves /v1/reservations — a caller (identified by its key →
// lane) can reserve slots on a model so batch work backs off and interactive work
// gets an already-free slot. The lease is short and must be renewed (heartbeat
// re-POST); it auto-expires so a dead client can't starve batch.
//
//	POST   /v1/reservations   {model, slots?, ttl?}   → create or renew
//	DELETE /v1/reservations?model=…                     → release
//	GET    /v1/reservations                             → list live reservations
func (p *Proxy) handleReservations(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		p.reserveCreate(w, r)
	case http.MethodDelete:
		p.reserveRelease(w, r)
	case http.MethodGet:
		p.reserveList(w, r)
	default:
		http.Error(w, `{"error":{"message":"method not allowed"}}`, http.StatusMethodNotAllowed)
	}
}

// primaryBackend is the scheduler backend name a reservation targets: the model's
// first (top-quality) backend, the one interactive requests hit first.
func primaryBackend(model string) string { return fmt.Sprintf("%s#0", model) }

func (p *Proxy) reserveCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model string `json:"model"`
		Slots int    `json:"slots"`
		TTL   string `json:"ttl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	m, ok := p.cfg.Models[body.Model]
	if !ok || len(m.Backends) == 0 {
		jsonErr(w, http.StatusNotFound, fmt.Sprintf("unknown model %q", body.Model))
		return
	}
	slots := body.Slots
	if slots < 1 {
		slots = 1
	}
	if capSlots := m.Backends[0].Slots(); slots > capSlots {
		jsonErr(w, http.StatusBadRequest, fmt.Sprintf("slots=%d exceeds backend capacity %d", slots, capSlots))
		return
	}
	ttl := p.sched.MaxReservationTTL()
	if body.TTL != "" {
		if d, err := time.ParseDuration(body.TTL); err == nil && d > 0 {
			ttl = d // scheduler caps it at the max
		}
	}
	lane, _ := p.cfg.ResolveGroup(callerKey(r))
	exp := p.sched.Reserve(primaryBackend(body.Model), lane, slots, ttl)
	p.publish(events.Event{Type: "changed"})
	jsonResp(w, http.StatusOK, map[string]any{
		"model": body.Model, "lane": lane, "slots": slots,
		"expires_at": exp.UTC().Format(time.RFC3339), "renew_within_seconds": int(time.Until(exp).Seconds()),
	})
}

func (p *Proxy) reserveRelease(w http.ResponseWriter, r *http.Request) {
	model := r.URL.Query().Get("model")
	if model == "" {
		jsonErr(w, http.StatusBadRequest, "missing ?model=")
		return
	}
	lane, _ := p.cfg.ResolveGroup(callerKey(r))
	p.sched.Release(primaryBackend(model), lane)
	p.publish(events.Event{Type: "changed"})
	jsonResp(w, http.StatusOK, map[string]any{"released": true, "model": model, "lane": lane})
}

func (p *Proxy) reserveList(w http.ResponseWriter, _ *http.Request) {
	var out []map[string]any
	for _, res := range p.sched.Reservations() {
		out = append(out, map[string]any{
			"model": strings.TrimSuffix(res.Backend, "#0"), "backend": res.Backend,
			"lane": res.Lane, "slots": res.Slots, "expires_at": res.ExpiresAt.UTC().Format(time.RFC3339),
		})
	}
	jsonResp(w, http.StatusOK, map[string]any{"reservations": out})
}

func jsonResp(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, status int, msg string) {
	jsonResp(w, status, map[string]any{"error": map[string]any{"message": msg}})
}
