package agent

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/toolchain"
)

// ToolRunner runs recipe verbs on a remote host through its agent. It is the
// toolchain.Runner counterpart to RemoteHost.
type ToolRunner struct {
	rh *RemoteHost
}

// NewToolRunner binds a toolchain runner to an already-configured RemoteHost,
// reusing its endpoint selection — which endpoint of a laptop answers is a
// question that has one right answer per moment, and two independent guesses
// would diverge.
func NewToolRunner(rh *RemoteHost) *ToolRunner { return &ToolRunner{rh: rh} }

func (t *ToolRunner) Where() string { return t.rh.Name() }

// Run posts one verb to the agent.
func (t *ToolRunner) Run(ctx context.Context, spec toolchain.Spec, verb toolchain.Verb) (*toolchain.Raw, error) {
	endpoint, err := t.rh.pick(ctx)
	if err != nil {
		return nil, err
	}

	// A dedicated client per call, budgeted by the verb. RemoteHost's own client
	// is fixed at 20 seconds — right for a spawn, and far too short for a
	// package install, which is minutes by nature.
	cli := &http.Client{Timeout: toolchain.Timeout(verb) + 30*time.Second}

	req := ToolRunRequest{Spec: spec, Verb: string(verb)}
	var resp ToolRunResponse
	err = t.rh.callWith(ctx, cli, http.MethodPost, endpoint+"/agent/v1/tools/run", req, &resp)
	if err != nil {
		// A 404 here is the compatible failure the route was designed around:
		// this agent predates the toolchain surface. Say so plainly rather than
		// reporting a mystery HTTP error — it resolves itself once the agent
		// self-updates, and knowing that is the difference between waiting and
		// debugging.
		if isNotFound(err) {
			return nil, fmt.Errorf("host %q: its agent is too old for the toolchain surface (no /agent/v1/tools/run) — it will pick this up on its next self-update", t.rh.Name())
		}
		return nil, err
	}
	raw := &toolchain.Raw{JSON: resp.JSON, Log: resp.Log}
	if resp.Error != "" {
		return raw, fmt.Errorf("%s", resp.Error)
	}
	if len(raw.JSON) == 0 {
		return raw, fmt.Errorf("host %q returned no result for %s %s", t.rh.Name(), spec.Name, verb)
	}
	return raw, nil
}

// isNotFound spots the "this agent predates the route" case.
//
// callWith renders a non-2xx as "agent <status>: <body>", and Go's mux answers
// an unknown route with a bare 404, so the status text is what there is to match
// on. A string match is unlovely; the alternative is threading a status code out
// of callWith, which every other caller would then have to ignore.
func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "404")
}
