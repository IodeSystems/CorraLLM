package agent

import (
	"encoding/json"
	"net/http"

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

	runner := &toolchain.Local{
		Dir:              s.recipeDir(),
		AllowInstallDeps: s.allowInstallDeps,
		Server:           s.hello().Hostname,
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
