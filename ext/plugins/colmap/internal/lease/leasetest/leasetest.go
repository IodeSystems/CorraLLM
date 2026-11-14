// Package leasetest is a fake of corrallm's lease API (the shape of
// internal/proxy/leases.go): POST grants, GET ?wait= long-polls, DELETE
// releases. Tests drive it: make grants busy (429), request a yield, or
// forget every lease (a corrallm restart).
package leasetest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// Event is one call the server saw.
type Event struct {
	Kind  string // grant | busy | release | yield | held-grant | conflict
	ID    string
	Stage string
	Held  bool
	Usage map[string]string
}

type lease struct {
	id, stage string
	yield     bool
	deadline  time.Time
	changed   chan struct{}
}

// Server is the fake.
type Server struct {
	*httptest.Server
	Token string

	mu       sync.Mutex
	leases   map[string]*lease
	n        int
	busy     int // answer this many POSTs with 429
	conflict bool
	events   []Event
	// OnGrant, when set, runs on each grant (outside the lock).
	OnGrant func(id, stage string)
}

// New starts a fake with token.
func New(token string) *Server {
	s := &Server{Token: token, leases: map[string]*lease{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Busy makes the next n grants answer 429 with Retry-After: 1.
func (s *Server) Busy(n int) {
	s.mu.Lock()
	s.busy = n
	s.mu.Unlock()
}

// Conflict makes held re-registrations answer 409.
func (s *Server) Conflict(v bool) {
	s.mu.Lock()
	s.conflict = v
	s.mu.Unlock()
}

// Events returns a copy of what happened.
func (s *Server) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.events...)
}

// Held lists the ids of live leases.
func (s *Server) Held() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for id := range s.leases {
		out = append(out, id)
	}
	return out
}

// RequestYield asks every live lease (of stage, or all if "") to yield with
// the given grace. It returns how many it asked.
func (s *Server) RequestYield(stage string, grace time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, l := range s.leases {
		if (stage == "" || l.stage == stage) && !l.yield {
			l.yield, l.deadline = true, time.Now().Add(grace)
			close(l.changed)
			l.changed = make(chan struct{})
			s.events = append(s.events, Event{Kind: "yield", ID: l.id, Stage: l.stage})
			n++
		}
	}
	return n
}

// Forget drops every lease, as a corrallm restart does (leases are memory-only).
func (s *Server) Forget() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, l := range s.leases {
		close(l.changed)
		delete(s.leases, id)
	}
}

func jsonResp(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func errResp(w http.ResponseWriter, code int, typ string) {
	jsonResp(w, code, map[string]any{"error": map[string]any{"type": typ, "message": typ}})
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+s.Token {
		errResp(w, http.StatusUnauthorized, "auth")
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/leases"), "/")
	switch {
	case id == "" && r.Method == http.MethodPost:
		var body struct {
			Stage string            `json:"stage"`
			Usage map[string]string `json:"usage"`
			Held  bool              `json:"held"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		if body.Held && s.conflict {
			s.events = append(s.events, Event{Kind: "conflict", Stage: body.Stage, Held: true})
			s.mu.Unlock()
			errResp(w, http.StatusConflict, "lease_conflict")
			return
		}
		if s.busy > 0 && !body.Held {
			s.busy--
			s.events = append(s.events, Event{Kind: "busy", Stage: body.Stage})
			s.mu.Unlock()
			w.Header().Set("Retry-After", "1")
			jsonResp(w, http.StatusTooManyRequests, map[string]any{"error": map[string]any{"type": "lease_busy", "message": "busy", "blocking": []string{"guard"}, "retry_after": 1}})
			return
		}
		s.n++
		l := &lease{id: fmt.Sprintf("lease_%d", s.n), stage: body.Stage, changed: make(chan struct{})}
		s.leases[l.id] = l
		kind := "grant"
		if body.Held {
			kind = "held-grant"
		}
		s.events = append(s.events, Event{Kind: kind, ID: l.id, Stage: body.Stage, Held: body.Held, Usage: body.Usage})
		cb := s.OnGrant
		s.mu.Unlock()
		if cb != nil {
			cb(l.id, body.Stage)
		}
		now := time.Now().Unix()
		jsonResp(w, http.StatusCreated, map[string]any{"id": l.id, "granted_at": now, "protected_until": now + 10})
	case id != "" && r.Method == http.MethodDelete:
		s.mu.Lock()
		l, ok := s.leases[id]
		if ok {
			delete(s.leases, id)
			close(l.changed)
			s.events = append(s.events, Event{Kind: "release", ID: id, Stage: l.stage})
		}
		s.mu.Unlock()
		if !ok {
			errResp(w, http.StatusNotFound, "not_found")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case id != "" && r.Method == http.MethodGet:
		wait, _ := time.ParseDuration(r.URL.Query().Get("wait"))
		deadline := time.Now().Add(wait)
		for {
			s.mu.Lock()
			l, ok := s.leases[id]
			if !ok {
				s.mu.Unlock()
				errResp(w, http.StatusNotFound, "not_found")
				return
			}
			if l.yield {
				out := map[string]any{"id": id, "state": "yield_requested", "wanted_by": "guard", "deadline": l.deadline.Unix()}
				s.mu.Unlock()
				jsonResp(w, http.StatusOK, out)
				return
			}
			ch := l.changed
			s.mu.Unlock()
			left := time.Until(deadline)
			if left <= 0 {
				jsonResp(w, http.StatusOK, map[string]any{"id": id, "state": "held"})
				return
			}
			select {
			case <-ch:
			case <-time.After(left):
			case <-r.Context().Done():
				return
			}
		}
	default:
		errResp(w, http.StatusMethodNotAllowed, "invalid_request_error")
	}
}
