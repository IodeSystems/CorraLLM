package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/iodesystems/corrallm/internal/toolchain"
)

// The agent's toolchain surface: run one recipe verb on this machine.
//
// WHY THIS IS NOT A BACKEND. The obvious implementation is to hand a build to
// the existing /agent/v1/backends route — it already spawns a command, rings its
// output and reaps it. It would also be killed. proc.ReconcileAgent reaps any
// backend an agent reports whose key no primary Process claims, after a
// 60-second adoption grace, and a fifteen-minute CUDA compile is exactly that:
// something the agent is running that the residency ledger has never heard of.
// It would die just after it started, every time, and surface as a build that
// mysteriously fails around the one-minute mark.
//
// Registering builds in the ledger instead (the way proc.Trial does) would work
// and is the wrong shape: a compile is not a model, has no pools, cannot be
// evicted and admits nobody. So tools get their own table, and reconciliation
// never sees them.
//
// WHY THE PROTOCOL DOES NOT BUMP. Adding routes is backwards-compatible in the
// direction that matters: an agent too old to know them answers 404, and the
// primary reports "this host's agent is too old to build" — true, specific, and
// self-correcting the moment its heartbeat pulls the new binary. Bumping
// Protocol would instead make every not-yet-updated agent reject ALL requests
// from the upgraded primary, taking the fleet out of service until each one
// updated. The compatible failure is the small one.

// ToolRunRequest asks this agent to run one recipe verb.
//
// The spec travels with the request and is never stored. The agent holds no
// copy of `tools:` — the primary is the only place a tool is declared, so there
// is no second registry to drift out of agreement with config.
type ToolRunRequest struct {
	Spec toolchain.Spec `json:"spec"`
	Verb string         `json:"verb"`
	// Force rebuilds even when the build stamp already matches.
	Force bool `json:"force,omitempty"`
	// Stream asks for the log AS IT HAPPENS, as newline-delimited JSON, instead
	// of one object at the end.
	//
	// A remote build was silent for its whole duration and then produced twenty
	// thousand lines at once, which is indistinguishable from a hang for the ten
	// minutes it takes. An agent that predates this field ignores it and answers
	// the old way, which is why the primary decides by the response's
	// content type rather than by assuming.
	Stream bool `json:"stream,omitempty"`
}

// ToolRunFrame is one line of a streamed response.
//
// Either a log line or the terminal result — never both, because the log has
// already been sent line by line and repeating it in the final frame would
// double every build's output.
type ToolRunFrame struct {
	Log  string          `json:"log,omitempty"`
	Done bool            `json:"done,omitempty"`
	JSON json.RawMessage `json:"json,omitempty"`
	Err  string          `json:"error,omitempty"`
}

// ToolRunResponse is the recipe's JSON result plus what it printed getting there.
type ToolRunResponse struct {
	JSON json.RawMessage `json:"json"`
	Log  string          `json:"log"`
	// Error is a failure to RUN the recipe. A recipe that ran and answered "no"
	// reports that inside JSON instead — the distinction is what lets the
	// primary tell "ninfer cannot build here" from "I could not ask".
	Error string `json:"error,omitempty"`
}

// toolRun executes one verb and returns whatever the recipe said.
func (s *Server) toolRun(w http.ResponseWriter, r *http.Request) {
	var req ToolRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request: "+err.Error())
		return
	}
	if req.Spec.Name == "" {
		writeErr(w, http.StatusBadRequest, "spec.name is required")
		return
	}
	verb := toolchain.Verb(req.Verb)
	switch verb {
	case toolchain.VerbProbe, toolchain.VerbUpstream, toolchain.VerbPreflight,
		toolchain.VerbInstallDeps, toolchain.VerbBuild:
	default:
		writeErr(w, http.StatusBadRequest, "unknown verb "+req.Verb)
		return
	}

	if req.Stream {
		s.toolRunStreaming(w, r, req, verb)
		return
	}

	runner := &toolchain.Local{
		Dir:              s.recipeDir(),
		AllowInstallDeps: s.allowInstallDeps,
		Server:           s.hello().Hostname,
		Force:            req.Force,
		// No Progress writer: this response is not streamed, so a remote build's
		// log arrives when it finishes. Acceptable for now — the same first-cut
		// shape proc.Trial took, and streaming can be added without changing the
		// contract.
	}
	raw, err := runner.Run(r.Context(), req.Spec, verb)
	resp := ToolRunResponse{}
	if raw != nil {
		resp.JSON = raw.JSON
		resp.Log = raw.Log
	}
	if err != nil {
		resp.Error = err.Error()
	}
	// Always 200 with a body: the interesting failures here are answers, not
	// transport faults, and an HTTP error would lose the log that explains them.
	writeJSON(w, resp)
}

// recipeDir is where this agent extracts recipes. Under the state dir when it
// has one so the files outlive nothing but the agent itself; otherwise the
// Local runner picks a temp directory.
func (s *Server) recipeDir() string {
	if s.stateDir == "" {
		return ""
	}
	return s.stateDir + "/recipes"
}

// toolRunStreaming runs a verb and emits its output as it happens.
//
// One NDJSON frame per line, flushed immediately. Flushing per line is the
// whole point: buffered, the operator sees nothing until the build ends, which
// is the behaviour this replaces.
func (s *Server) toolRunStreaming(w http.ResponseWriter, r *http.Request, req ToolRunRequest, verb toolchain.Verb) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	// Headers go out before the first line so the client knows which shape it
	// is reading, even if the build takes a minute to say anything.
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	enc := json.NewEncoder(w)
	var mu sync.Mutex
	emit := func(f ToolRunFrame) {
		mu.Lock()
		defer mu.Unlock()
		_ = enc.Encode(f)
		if flusher != nil {
			flusher.Flush()
		}
	}

	lw := &lineWriter{emit: func(line string) { emit(ToolRunFrame{Log: line}) }}
	runner := &toolchain.Local{
		Dir:              s.recipeDir(),
		AllowInstallDeps: s.allowInstallDeps,
		Server:           s.hello().Hostname,
		Force:            req.Force,
		Progress:         lw,
	}

	raw, err := runner.Run(r.Context(), req.Spec, verb)
	lw.flush()

	f := ToolRunFrame{Done: true}
	if raw != nil {
		f.JSON = raw.JSON
	}
	if err != nil {
		f.Err = err.Error()
	}
	emit(f)
}

// lineWriter turns a byte stream into whole lines.
//
// cmd output arrives in arbitrary chunks — a write can hold half a line, or six
// lines and a fragment — so emitting per Write would produce frames that split
// mid-word and a UI that reassembles them wrongly.
type lineWriter struct {
	mu   sync.Mutex
	buf  []byte
	emit func(string)
}

func (l *lineWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(l.buf[:i]), "\r")
		l.buf = l.buf[i+1:]
		l.emit(line)
	}
	return len(p), nil
}

// flush emits a trailing fragment, so a build whose last line has no newline
// does not lose it — which is often the line that says what went wrong.
func (l *lineWriter) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buf) > 0 {
		l.emit(strings.TrimRight(string(l.buf), "\r"))
		l.buf = nil
	}
}
