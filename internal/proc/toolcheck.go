package proc

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/iodesystems/corrallm/internal/config"
)

// CheckTool answers "does a model start on the build of this tool that is
// active now?" by trialling one configured model on it. The toolchain calls it
// after a build switches bin/, and switches back when it fails.
//
// It is a Trial, so it inherits a trial's rules: it takes only free capacity
// and evicts nothing, it waits for /health, and it is torn down after.
//
// ran reports whether a model was actually spawned. ran=false with an error
// means the build could not be checked (no idle model uses the tool here, or no
// free room), which says nothing about the build. ran=true with an error means
// the build was tried and failed.
func (m *Manager) CheckTool(ctx context.Context, tool, server string, log io.Writer) (ran bool, err error) {
	name, mdl, ok := m.toolCheckModel(tool, server)
	if !ok {
		return false, fmt.Errorf("no idle model on %s runs ${tool:%s}", server, tool)
	}
	if m.ExpandCmd == nil {
		return false, fmt.Errorf("no toolchain resolver is wired")
	}
	cmd, err := m.ExpandCmd(ctx, mdl.Cmd, server)
	if err != nil {
		// bin/ was just switched; a reference that no longer resolves is the
		// new build's fault.
		return true, fmt.Errorf("%s: %w", name, err)
	}
	mdl.Cmd = cmd
	_, _ = fmt.Fprintf(log, "  checking with %s\n", name)
	// Trial events arrive from more than one goroutine (the backend's output
	// copier and the trial itself), and log is a plain writer.
	var logMu sync.Mutex
	res, err := m.Trial(ctx, "tool-check-"+name, mdl, func(e TrialEvent) {
		if e.Msg != "" {
			logMu.Lock()
			_, _ = fmt.Fprintf(log, "  [%s] %s\n", e.Stage, e.Msg)
			logMu.Unlock()
		}
	})
	if err == nil {
		return true, nil
	}
	switch TrialStage(res.Failed) {
	case TrialResolve, TrialAdmit:
		return false, fmt.Errorf("%s: %w", name, err)
	}
	return true, fmt.Errorf("%s failed at %s: %w", name, res.Failed, err)
}

// toolCheckModel picks the model to check a tool with: a placement on server
// whose cmd references the tool and whose own process is not running — a
// running one holds the port the trial would bind. Smallest measured peak
// first, so the check costs as little as it can; unmeasured last; then by name
// so the choice is stable.
func (m *Manager) toolCheckModel(tool, server string) (string, config.Model, bool) {
	cfg := m.config()
	if cfg == nil {
		return "", config.Model{}, false
	}
	ref := "${tool:" + tool + "}"
	type cand struct {
		name string
		mdl  config.Model
		peak int
	}
	var cs []cand
	for name, mdl := range cfg.Models {
		if mdl.Disabled {
			continue
		}
		for _, p := range mdl.PlacementList() {
			if p.Server != server || !strings.Contains(p.Cmd, ref) {
				continue
			}
			if m.StateOf(mdl.PlacementProcKey(name, p)) != StateAbsent {
				continue
			}
			cs = append(cs, cand{name, mdl.ForPlacement(p), m.PlacementPeak(name, p)})
		}
	}
	if len(cs) == 0 {
		return "", config.Model{}, false
	}
	sort.Slice(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		if (a.peak == 0) != (b.peak == 0) {
			return b.peak == 0
		}
		if a.peak != b.peak {
			return a.peak < b.peak
		}
		return a.name < b.name
	})
	return cs[0].name, cs[0].mdl, true
}
