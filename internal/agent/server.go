package agent

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
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
	// stateDir is where the agent notes which process groups it supervises and
	// keeps their log files, so a RESTARTED agent can adopt what the previous
	// one left running (or kill what it cannot adopt). Empty disables both
	// (tests, and any install with nowhere to write): output is then piped,
	// and a backend does not survive this agent.
	stateDir string
	// allowInstallDeps lets the toolchain surface install SYSTEM PACKAGES here.
	//
	// Off unless asked for. This agent already runs arbitrary shell by design,
	// so the flag is not much of a security boundary — what it buys is the
	// promise that corrallm does not touch a machine's packages unless somebody
	// enabled it on that machine deliberately.
	allowInstallDeps bool

	// jobsCtx ends every tool run when the agent shuts down. Jobs (builds,
	// probes) die with whoever started them; only model backends outlive the
	// agent. See toolRun. Guarded by jobsMu: a failed self-update re-arms it.
	jobsMu     sync.Mutex
	jobsCtx    context.Context
	jobsCancel context.CancelFunc
	jobs       sync.WaitGroup

	// identityDelays is when a new backend's identity is read; see
	// defaultIdentityDelays.
	identityDelays []time.Duration

	mu       sync.Mutex
	backends map[string]*supervised
	seq      int
}

type supervised struct {
	id        string
	key       string
	model     string
	cmd       string
	configCmd string
	port      int
	started   time.Time
	handle    host.Handle
	logs      *logRing
}

// SetAllowInstallDeps permits the toolchain surface to install system packages
// on this machine. Call before serving.
func (s *Server) SetAllowInstallDeps(v bool) { s.allowInstallDeps = v }

// SetStateDir enables restart bookkeeping, adopts the backends a previous
// agent left running and kills the ones it cannot adopt. Call before serving
// and before the first heartbeat, so the primary's first view of this machine
// already includes what was adopted.
func (s *Server) SetStateDir(dir string) {
	s.stateDir = dir
	adopt, killed := Recover(dir)
	if killed > 0 {
		slog.Warn("agent: killed backends left by a previous agent that could not be adopted", "count", killed)
	}
	keepLogs := map[string]bool{}
	for _, r := range adopt {
		if s.adopt(r) && r.Log != "" {
			keepLogs[filepath.Base(r.Log)] = true
			keepLogs[filepath.Base(r.Log)+".1"] = true
		}
	}
	// Log files of backends that are gone. Each is capped, but a crash loop
	// writes a new one per spawn, so nothing else would ever bound the count.
	if dir := s.logDir(); dir != "" {
		if ents, err := os.ReadDir(dir); err == nil {
			for _, e := range ents {
				if !keepLogs[e.Name()] {
					_ = os.Remove(filepath.Join(dir, e.Name()))
				}
			}
		}
	}
}

// adopt re-registers a backend a previous agent started, under the SAME id and
// key, so the primary — which addresses it by id and matches it by key — sees
// nothing change.
func (s *Server) adopt(r supervisedRec) bool {
	h, err := host.Adopt(r.PGID)
	if err != nil {
		slog.Warn("agent: could not adopt a recorded backend", "id", r.ID, "pgid", r.PGID, "err", err)
		ForgetSupervised(s.stateDir, r.PGID)
		return false
	}
	ring := newLogRing(500)
	b := &supervised{
		id: r.ID, key: r.Key, model: r.Model, cmd: r.Cmd, configCmd: r.ConfigCmd,
		port: r.Port, started: time.UnixMilli(r.Started), handle: h, logs: ring,
	}
	s.mu.Lock()
	s.backends[r.ID] = b
	if n := backendSeq(r.ID); n > s.seq {
		s.seq = n
	}
	s.mu.Unlock()
	if r.Log != "" {
		go s.tail(r.Log, ring, true, h.Done())
	}
	go func() { <-h.Done(); ForgetSupervised(s.stateDir, r.PGID) }()
	slog.Info("agent: adopted a backend left running by a previous agent",
		"id", r.ID, "key", r.Key, "model", r.Model, "pgid", r.PGID)
	return true
}

func (s *Server) logDir() string {
	if s.stateDir == "" {
		return ""
	}
	return filepath.Join(s.stateDir, "logs")
}

// tail feeds a backend's log file into its ring and the agent's own stdout,
// then removes the file once the backend has exited and the tail is drained.
// The ring keeps the last lines for the primary; the file was only ever the
// channel that does not die with the agent.
func (s *Server) tail(path string, ring *logRing, seed bool, done <-chan struct{}) {
	tailLog(path, io.MultiWriter(os.Stdout, ring), seed, done, logCapBytes)
	_ = os.Remove(path)
	_ = os.Remove(path + ".1")
}

// New builds an agent. An empty token means no authentication — only reachable
// via the explicit opt-out on the command, never by default.
func New(version, token string) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		version:    version,
		token:      token,
		booted:     time.Now(),
		host:       host.NewLocal("agent"),
		backends:   map[string]*supervised{},
		jobsCtx:    ctx,
		jobsCancel: cancel,

		identityDelays: defaultIdentityDelays,
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
	// Toolchain. Deliberately NOT under /backends — see tools.go for why a
	// build that registered as a backend would be reaped 60 seconds in.
	mux.HandleFunc("POST /agent/v1/tools/run", s.guard(s.toolRun))
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
	id := newBackendID(s.seq, s.booted)
	s.mu.Unlock()

	// Output goes to a file when there is somewhere to put one, so the backend
	// never depends on this process to keep reading a pipe (see logfile.go).
	// Tee to the agent's own stdout as well, either way: an operator watching
	// the agent should see what its backends are doing without the primary.
	out := io.Writer(io.MultiWriter(os.Stdout, ring))
	var logPath string
	if dir := s.logDir(); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err == nil {
			logPath = filepath.Join(dir, id+".log")
			if f, err := openBackendLog(logPath); err == nil {
				defer func() { _ = f.Close() }() // the child holds its own copy
				out = f
			} else {
				logPath = ""
			}
		}
		if logPath == "" {
			slog.Warn("agent: cannot write a backend log file; piping output instead (this backend will not survive an agent restart)",
				"dir", dir)
		}
	}

	// Detached: its own session, so a hangup on the terminal this agent was
	// started from does not reach it. It is meant to outlive this agent.
	started := time.Now()
	h, err := s.host.Start(host.Spec{
		Name:   req.Model,
		Key:    req.Key,
		Cmd:    req.Cmd,
		Port:   req.Port,
		Out:    out,
		Detach: true,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	b := &supervised{
		id: id, key: req.Key, model: req.Model, cmd: req.Cmd, configCmd: req.ConfigCmd,
		port: req.Port, started: started, handle: h, logs: ring,
	}
	s.mu.Lock()
	s.backends[id] = b
	s.mu.Unlock()
	if logPath != "" {
		go s.tail(logPath, ring, false, h.Done())
	}

	if pgid := pgidOf(h.ID()); pgid > 0 {
		recordSupervised(s.stateDir, supervisedRec{
			PGID: pgid, Cmd: req.Cmd, ID: id, Key: req.Key, Model: req.Model,
			ConfigCmd: req.ConfigCmd, Port: req.Port, Started: started.UnixMilli(), Log: logPath,
		})
		go observeIdentity(s.stateDir, pgid, h.Done(), s.identityDelays)
		// Drop the note when it exits, so startup only ever looks at things
		// that are genuinely still running.
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
	// Per card first: one vendor-tool read answers both questions, and the sum
	// of the split IS the group total MemoryMiB would read with a second call.
	mem := -1
	var byDev map[string]int
	if dm, ok := b.handle.(host.DeviceMemory); ok {
		if m, err := dm.MemoryByDeviceMiB(); err == nil {
			byDev, mem = m, 0
			for _, v := range m {
				mem += v
			}
		}
	}
	if byDev == nil {
		if v, err := b.handle.MemoryMiB(); err == nil {
			mem = v
		}
	}
	out := Backend{
		ID: b.id, Key: b.key, Model: b.model, Cmd: b.cmd, ConfigCmd: b.configCmd, Port: b.port,
		Alive: b.handle.Alive(), Exited: exited,
		Started: b.started.UnixMilli(), MemoryMiB: mem, MemoryByDevice: byDev,
		LastSeq: b.logs.LastSeq(),
	}
	if exited {
		if err := b.handle.Err(); err != nil {
			out.Err = err.Error()
		}
	}
	return out
}

// Shutdown ends this agent's JOBS and leaves its backends running.
//
// This is the reverse of what it used to do, deliberately. Every agent restart
// — a crash, start.sh's loop, a manual stop, a self-update, a systemd restart —
// used to stop every model on the machine and cost each one a cold load. The
// backends are now detached, write to files, and are recorded with enough
// identity for the NEXT agent to adopt them (see reap.go), so leaving them is
// safe: they stay tracked across the gap. A full stop is StopBackends, and the
// primary's explicit unload still signals them one by one.
//
// Tool runs are jobs, not backends, and die with the agent that started them:
// their context is cancelled here, which kills their process groups.
func (s *Server) Shutdown() {
	s.jobsMu.Lock()
	s.jobsCancel()
	s.jobsMu.Unlock()
	s.refreshIdentities()
	done := make(chan struct{})
	go func() { s.jobs.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		slog.Warn("agent: tool runs still finishing after 10s; exiting anyway")
	}
}

// refreshIdentities records every live backend's identity once more, right
// before this agent exits. The scheduled looks (identityDelays) leave a window
// after a spawn in which no identity is recorded yet, and a backend left in
// that window cannot be adopted — the next agent kills it. Observed in the
// sandbox: an agent stopped one second after a spawn killed that backend on
// restart.
func (s *Server) refreshIdentities() {
	if s.stateDir == "" {
		return
	}
	s.mu.Lock()
	bs := make([]*supervised, 0, len(s.backends))
	for _, b := range s.backends {
		bs = append(bs, b)
	}
	s.mu.Unlock()
	for _, b := range bs {
		pgid := pgidOf(b.handle.ID())
		if pgid <= 0 || !b.handle.Alive() {
			continue
		}
		if id, err := procIdentity(pgid); err == nil {
			updateIdentity(s.stateDir, pgid, id)
		}
	}
}

// beginJob registers a tool run and returns the context it is bound to, or
// false once the agent is shutting down. Add happens under jobsMu, before any
// cancel, so Shutdown's Wait never races a late Add.
func (s *Server) beginJob() (context.Context, bool) {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	if s.jobsCtx.Err() != nil {
		return nil, false
	}
	s.jobs.Add(1)
	return s.jobsCtx, true
}

// rearmJobs lets tool runs start again after a Shutdown that did not end the
// process — an exec into a new build that failed.
func (s *Server) rearmJobs() {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	s.jobsCtx, s.jobsCancel = context.WithCancel(context.Background())
}

// StopBackends stops everything this agent supervises: SIGTERM, then SIGKILL
// for anything still alive after 10 seconds. The explicit full stop.
func (s *Server) StopBackends() {
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
