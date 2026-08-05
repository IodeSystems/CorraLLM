package agent

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/corrallm/internal/host"
)

// Server supervises backends on this machine for a remote primary.
type Server struct {
	version string
	token   string
	booted  time.Time
	host    host.Host
	// stateDir is where the agent notes which process groups it supervises, so
	// a RESTARTED agent can kill what the previous one left running. Empty
	// disables it (tests, and any install with nowhere to write).
	stateDir string

	mu       sync.Mutex
	backends map[string]*supervised
	seq      int
}

type supervised struct {
	id      string
	key     string
	model   string
	cmd     string
	started time.Time
	handle  host.Handle
	logs    *logRing
}

// SetStateDir enables crash-recovery bookkeeping and reaps anything a previous
// agent left behind. Call before serving.
func (s *Server) SetStateDir(dir string) {
	s.stateDir = dir
	if n := ReapStale(dir); n > 0 {
		slog.Warn("agent: reaped backends left by a previous agent", "count", n)
	}
}

// New builds an agent. An empty token means no authentication — only reachable
// via the explicit opt-out on the command, never by default.
func New(version, token string) *Server {
	return &Server{
		version:  version,
		token:    token,
		booted:   time.Now(),
		host:     host.NewLocal("agent"),
		backends: map[string]*supervised{},
	}
}

// Routes returns the agent's HTTP surface.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agent/v1/hello", s.guard(s.helloRoute))
	mux.HandleFunc("GET /agent/v1/capacity", s.guard(s.capacity))
	mux.HandleFunc("GET /agent/v1/backends", s.guard(s.list))
	mux.HandleFunc("POST /agent/v1/backends", s.guard(s.start))
	mux.HandleFunc("GET /agent/v1/backends/{id}", s.guard(s.status))
	mux.HandleFunc("POST /agent/v1/backends/{id}/signal", s.guard(s.signal))
	mux.HandleFunc("GET /agent/v1/backends/{id}/logs", s.guard(s.logs))
	// Data plane. No method filter: this carries whatever the client sent —
	// POST completions, GET /v1/models, a websocket upgrade for realtime audio.
	mux.HandleFunc(proxyPrefix+"{port}/", s.guard(s.proxyBackend))
	return mux
}

// guard enforces the token and the protocol version on every call.
//
// The protocol check is not ceremony. A primary that speaks a version this
// agent does not understand could ask for a backend the agent cannot later
// describe or reap — and an unaccounted-for process holding tens of GB is the
// worst state in the system. Refusing up front is cheaper than discovering it
// after the spawn.
func (s *Server) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
				writeErr(w, http.StatusUnauthorized, "agent token required")
				return
			}
		}
		if v := r.Header.Get(ProtocolHeader); v != "" && v != strconv.Itoa(Protocol) {
			writeErr(w, http.StatusBadRequest,
				fmt.Sprintf("protocol %s not supported; this agent speaks %d", v, Protocol))
			return
		}
		h(w, r)
	}
}

// hello is the agent's identity, shared by the HTTP route and the heartbeat.
func (s *Server) hello() Hello {
	hn, _ := os.Hostname()
	return Hello{
		Protocol: Protocol,
		Version:  s.version,
		Hostname: hn,
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		Booted:   s.booted.UnixMilli(),
		BuildID:  OwnBuildID(),
	}
}

// snapshot is every supervised backend, for the heartbeat's reconciliation half.
func (s *Server) snapshot() []Backend {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Backend, 0, len(s.backends))
	for _, b := range s.backends {
		out = append(out, s.view(b))
	}
	return out
}

func (s *Server) helloRoute(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.hello())
}

// capacity answers the same measurement the heartbeat carries.
//
// It delegates to Probe rather than rebuilding it: the inline copy that used to
// live here omitted PerProcess and Unified entirely, so this route reported a
// unified-memory Mac as unable to attribute per-process memory while the
// heartbeat — from the same binary, seconds apart — reported the opposite.
func (s *Server) capacity(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, Probe())
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	var req StartRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Cmd) == "" {
		writeErr(w, http.StatusBadRequest, "cmd is required")
		return
	}

	ring := newLogRing(500)
	s.mu.Lock()
	s.seq++
	id := fmt.Sprintf("b%d", s.seq)
	s.mu.Unlock()

	// Tee to the agent's own stdout as well: an operator watching the agent
	// should see what its backends are doing without going through the primary.
	h, err := s.host.Start(host.Spec{
		Name: req.Model,
		Cmd:  req.Cmd,
		Out:  io.MultiWriter(os.Stdout, ring),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	b := &supervised{
		id: id, key: req.Key, model: req.Model, cmd: req.Cmd,
		started: time.Now(), handle: h, logs: ring,
	}
	s.mu.Lock()
	s.backends[id] = b
	s.mu.Unlock()

	if pgid := pgidOf(h.ID()); pgid > 0 {
		RecordSupervised(s.stateDir, pgid, req.Cmd)
		// Drop the note when it exits, so startup only ever kills things that
		// are genuinely still running.
		go func() { <-h.Done(); ForgetSupervised(s.stateDir, pgid) }()
	}
	slog.Info("agent: backend started", "id", id, "key", req.Key, "model", req.Model, "handle", h.ID())
	writeJSON(w, s.view(b))
}

func (s *Server) list(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	out := make([]Backend, 0, len(s.backends))
	for _, b := range s.backends {
		out = append(out, s.view(b))
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, map[string]any{"backends": out})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	b, ok := s.get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown backend")
		return
	}
	writeJSON(w, s.view(b))
}

func (s *Server) signal(w http.ResponseWriter, r *http.Request) {
	b, ok := s.get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown backend")
		return
	}
	var req SignalRequest
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req)
	sig := host.SigTerm
	if strings.EqualFold(req.Sig, "kill") {
		sig = host.SigKill
	}
	if err := b.handle.Signal(sig); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	slog.Info("agent: signalled backend", "id", b.id, "sig", req.Sig)
	writeJSON(w, s.view(b))
}

// logs returns retained output at or after `from`.
//
// Not a follow stream yet: a polling primary with `from` gets the same
// completeness guarantee without an open connection to lose, and the streaming
// version can be added without changing the contract.
func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	b, ok := s.get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown backend")
		return
	}
	from, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
	lines, next := b.logs.Since(from)
	writeJSON(w, map[string]any{"lines": lines, "next": next})
}

func (s *Server) get(id string) (*supervised, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.backends[id]
	return b, ok
}

// view snapshots a backend. MemoryMiB is -1 rather than 0 when this machine
// cannot attribute memory per process, so the primary can tell "nothing" from
// "cannot know" — the difference decides whether ramUsage stays advisory.
func (s *Server) view(b *supervised) Backend {
	exited := false
	select {
	case <-b.handle.Done():
		exited = true
	default:
	}
	mem := -1
	if v, err := b.handle.MemoryMiB(); err == nil {
		mem = v
	}
	out := Backend{
		ID: b.id, Key: b.key, Model: b.model, Cmd: b.cmd,
		Alive: b.handle.Alive(), Exited: exited,
		Started: b.started.UnixMilli(), MemoryMiB: mem,
		LastSeq: b.logs.LastSeq(),
	}
	if exited {
		if err := b.handle.Err(); err != nil {
			out.Err = err.Error()
		}
	}
	return out
}

// Shutdown stops everything this agent spawned.
//
// An agent that exits leaving backends running strands them: the primary's
// handles are gone, nothing will reap them, and the next agent starts against a
// machine that is mysteriously full. This is the same reasoning as the
// primary's own Shutdown.
//
// It does NOT cover the agent being killed abruptly or losing its primary while
// staying up — that needs a lease, which is a deliberate open decision (see
// plan.md), not an oversight.
func (s *Server) Shutdown() {
	s.mu.Lock()
	bs := make([]*supervised, 0, len(s.backends))
	for _, b := range s.backends {
		bs = append(bs, b)
	}
	s.mu.Unlock()
	for _, b := range bs {
		slog.Info("agent: stopping backend on shutdown", "id", b.id, "handle", b.handle.ID())
		_ = b.handle.Signal(host.SigTerm)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		remaining := bs[:0:0]
		for _, b := range bs {
			if b.handle.Alive() {
				remaining = append(remaining, b)
			}
		}
		if len(remaining) == 0 {
			return
		}
		bs = remaining
		time.Sleep(100 * time.Millisecond)
	}
	for _, b := range bs {
		if !b.handle.Alive() {
			continue
		}
		slog.Warn("agent: backend ignored SIGTERM; sending SIGKILL", "id", b.id)
		_ = b.handle.Signal(host.SigKill)
	}
}

// pgidOf parses host.Local's opaque handle id ("pgid:12345"). It is opaque by
// contract, so a parse failure just disables the bookkeeping rather than
// guessing — the id format is allowed to change.
func pgidOf(id string) int {
	rest, ok := strings.CutPrefix(id, "pgid:")
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return 0
	}
	return n
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": msg}})
}
