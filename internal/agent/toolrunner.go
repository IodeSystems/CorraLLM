package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/toolchain"
)

// ToolRunner runs recipe verbs on a remote host through its agent. It is the
// toolchain.Runner counterpart to RemoteHost.
type ToolRunner struct {
	rh *RemoteHost
	// Force is passed through to the recipe's build verb.
	Force bool
	// Progress, when set, receives the remote log AS IT HAPPENS. Setting it is
	// what asks the agent to stream.
	Progress io.Writer
}

// NewToolRunner binds a toolchain runner to an already-configured RemoteHost,
// reusing its endpoint selection — which endpoint of a laptop answers is a
// question that has one right answer per moment, and two independent guesses
// would diverge.
func NewToolRunner(rh *RemoteHost) *ToolRunner { return &ToolRunner{rh: rh} }

func (t *ToolRunner) Where() string { return t.rh.Name() }

// SetForce and SetProgress let the registry apply per-invocation settings
// without knowing this type.
func (t *ToolRunner) SetForce(v bool)         { t.Force = v }
func (t *ToolRunner) SetProgress(w io.Writer) { t.Progress = w }

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

	req := ToolRunRequest{Spec: spec, Verb: string(verb), Force: t.Force, Stream: t.Progress != nil}
	if t.Progress != nil {
		raw, err := t.stream(ctx, cli, endpoint, req)
		if err != nil && isNotFound(err) {
			return nil, tooOld(t.rh.Name())
		}
		return raw, err
	}

	var resp ToolRunResponse
	err = t.rh.callWith(ctx, cli, http.MethodPost, endpoint+"/agent/v1/tools/run", req, &resp)
	if err != nil {
		// A 404 here is the compatible failure the route was designed around:
		// this agent predates the toolchain surface. Say so plainly rather than
		// reporting a mystery HTTP error — it resolves itself once the agent
		// self-updates, and knowing that is the difference between waiting and
		// debugging.
		if isNotFound(err) {
			return nil, tooOld(t.rh.Name())
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

func tooOld(server string) error {
	return fmt.Errorf("host %q: its agent is too old for the toolchain surface (no /agent/v1/tools/run) — it will pick this up on its next self-update", server)
}

// stream posts the request and reads the agent's NDJSON reply, forwarding each
// log line as it lands.
//
// It also copes with an agent that does NOT know how to stream: such an agent
// ignores the unknown `stream` field and answers with one ordinary JSON object,
// so the content type decides which shape is being read rather than the
// primary assuming its own version is deployed everywhere.
func (t *ToolRunner) stream(ctx context.Context, cli *http.Client, endpoint string, req ToolRunRequest) (*toolchain.Raw, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		endpoint+"/agent/v1/tools/run", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set(ProtocolHeader, strconv.Itoa(Protocol))
	httpReq.Header.Set("Content-Type", "application/json")
	if tok := t.rh.token; tok != "" {
		httpReq.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := cli.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("agent %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}

	if !strings.Contains(resp.Header.Get("Content-Type"), "x-ndjson") {
		// An agent that cannot stream. Take the whole-response shape and hand
		// its log over in one go — late, but not lost.
		var one ToolRunResponse
		if err := json.NewDecoder(resp.Body).Decode(&one); err != nil {
			return nil, err
		}
		if one.Log != "" && t.Progress != nil {
			_, _ = io.WriteString(t.Progress, one.Log)
		}
		raw := &toolchain.Raw{JSON: one.JSON, Log: one.Log}
		if one.Error != "" {
			return raw, fmt.Errorf("%s", one.Error)
		}
		return raw, nil
	}

	dec := json.NewDecoder(resp.Body)
	// A build line can be long (nvcc template errors run to kilobytes), and the
	// decoder grows its buffer as needed, so no limit is imposed here.
	raw := &toolchain.Raw{}
	for {
		var f ToolRunFrame
		if err := dec.Decode(&f); err != nil {
			if err == io.EOF {
				// The stream ended without a terminal frame: the agent died, or
				// the connection dropped mid-build. Say so rather than
				// reporting an empty result as success.
				if len(raw.JSON) == 0 {
					return raw, fmt.Errorf("the log stream from %q ended before the build reported a result", t.rh.Name())
				}
				return raw, nil
			}
			return raw, err
		}
		if f.Done {
			raw.JSON = f.JSON
			if f.Err != "" {
				return raw, fmt.Errorf("%s", f.Err)
			}
			return raw, nil
		}
		if t.Progress != nil {
			_, _ = io.WriteString(t.Progress, f.Log+"\n")
		}
	}
}
