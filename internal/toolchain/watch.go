package toolchain

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

// Watcher is the scheduled drift check.
//
// Everything else in this package answers a question somebody asked. Without
// this, a pin only rots in public: llama.cpp ships several builds a day, and
// "how far behind are we" was answered exclusively by a human opening the
// Tooling panel or running `corrallm tools list`. A pin nobody looks at is a
// pin nobody knows is stale.
//
// CHECKING IS ON BY DEFAULT AND BUILDING IS NOT, and the asymmetry is the whole
// design. A check is one `git ls-remote` per tool — cheap enough that doing it
// unasked is uncontroversial. A build is ten to twenty minutes of every core and
// the GPU, and it REPLACES the binary that resident models are running on. That
// stays a decision somebody makes, per tool, with `rebuild: true`.
type Watcher struct {
	// Reg answers what is installed where. Required.
	Reg *Registry
	// Builder runs an opted-in rebuild. Nil means checks still run and drift is
	// still reported, but nothing is ever built — which is what a daemon with no
	// build slot should do, rather than refusing to check at all.
	Builder *Builder

	// Tick is how often the loop wakes to see what has come due. It is NOT the
	// check interval: that is per tool, from `check:`. Default watchTick.
	Tick time.Duration
	// Now is injectable so a test can drive a 6h cadence without waiting 6h.
	Now func() time.Time

	mu   sync.Mutex
	last map[string]time.Time // tool -> when it was last checked
}

// watchTick is how often the loop looks for due tools.
//
// A minute, against check intervals measured in hours: the tick only decides
// how late a check can be, and waking once a minute to compare a few timestamps
// costs nothing. Making it the check interval instead would mean a config
// reload that shortens `check:` does not take effect until the old, longer
// interval has elapsed.
const watchTick = time.Minute

func (w *Watcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Watcher) tick() time.Duration {
	if w.Tick > 0 {
		return w.Tick
	}
	return watchTick
}

// Run drives the check loop until ctx is cancelled.
//
// The first pass happens one tick in, not immediately at boot. Agents are still
// connecting in the first seconds of a restart, and a check that runs then
// reports "unreachable" for hosts that are merely not up yet — which is a false
// drift alarm in the log every time the daemon restarts.
func (w *Watcher) Run(ctx context.Context) {
	if w.Reg == nil {
		return
	}
	t := time.NewTicker(w.tick())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		w.CheckDue(ctx)
	}
}

// CheckDue runs one pass: every tool whose cadence has elapsed. Exported so a
// test drives it directly rather than racing a ticker.
func (w *Watcher) CheckDue(ctx context.Context) {
	cfg := w.Reg.Cfg()
	if cfg == nil {
		return
	}
	names := make([]string, 0, len(cfg.Tools))
	for name := range cfg.Tools {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		t := cfg.Tools[name]
		every, on := config.CheckIntervalOf(t)
		if !on {
			continue
		}
		if !w.due(name, every) {
			continue
		}
		// Stamped BEFORE the survey, not after. A host behind a VPN can take the
		// probe timeout to answer; stamping afterwards would measure the cadence
		// from the end of a slow check and quietly stretch it.
		w.stamp(name)
		w.checkTool(ctx, name, t)
	}
}

func (w *Watcher) due(tool string, every time.Duration) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	last, seen := w.last[tool]
	if !seen {
		// Never checked in this process. Due now — a restart is exactly when
		// somebody wants to know what drifted while it was down.
		return true
	}
	return w.now().Sub(last) >= every
}

func (w *Watcher) stamp(tool string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.last == nil {
		w.last = map[string]time.Time{}
	}
	w.last[tool] = w.now()
}

// LastChecked reports when each tool was last checked in this process.
func (w *Watcher) LastChecked() map[string]time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]time.Time, len(w.last))
	for k, v := range w.last {
		out[k] = v
	}
	return out
}

// checkTool surveys one tool across its declared hosts and acts on drift.
//
// Concurrent across hosts for the same reason SurveyAll is: a survey is
// dominated by waiting on machines that may be asleep, and doing them in
// sequence makes one unreachable laptop delay every other host's check.
func (w *Watcher) checkTool(ctx context.Context, name string, t config.Tool) {
	hosts := make([]string, 0, len(t.Hosts))
	for h := range t.Hosts {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)

	states := make([]State, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func(i int, h string) {
			defer wg.Done()
			states[i] = w.Reg.Survey(ctx, name, h)
		}(i, h)
	}
	wg.Wait()

	for _, st := range states {
		w.report(ctx, name, t, st)
	}
}

// report logs what a survey found and starts a rebuild when one is warranted.
func (w *Watcher) report(ctx context.Context, name string, t config.Tool, st State) {
	switch {
	case st.Error != "":
		// A failure to ASK, not a statement about the tool. Logged at info: a
		// laptop that is closed is the common case, and warning about it every
		// six hours trains people to ignore the log.
		slog.Info("tool check could not reach a host",
			"tool", name, "host", st.Host, "err", st.Error)
		return
	case st.Probe != nil && !st.Probe.Present:
		slog.Info("tool is declared but not installed", "tool", name, "host", st.Host)
		return
	case st.Drift == nil:
		return
	case st.Drift.Error != "":
		slog.Info("tool drift check failed", "tool", name, "host", st.Host, "err", st.Drift.Error)
		return
	case !st.Drift.Behind:
		return
	}

	// Drift, demonstrated. Upstream.Behind is false whenever either side is
	// unknown, so reaching here means both revisions were actually established.
	slog.Warn("tool is BEHIND its pin",
		"tool", name, "host", st.Host, "ref", st.Drift.Ref,
		"local", st.Drift.Local, "upstream", st.Drift.RemoteHead,
		"rebuild", t.Rebuild)

	if !t.Rebuild {
		return
	}
	if st.Adopted {
		// An adopted entry points at an install corrallm does not own — ml-kit's
		// tree, a Homebrew prefix, somebody's checkout. Building there would
		// `git clean -xdf` a directory this daemon was never given.
		slog.Warn("not rebuilding an ADOPTED tool; drift is reported only",
			"tool", name, "host", st.Host)
		return
	}
	if w.Builder == nil {
		return
	}
	if ctx.Err() != nil {
		return
	}
	j, err := w.Builder.Start(name, st.Host, false)
	if err != nil {
		// Almost always "a build is already running". Not an error worth
		// escalating: the slot is busy and this tool comes due again.
		slog.Info("scheduled rebuild not started", "tool", name, "host", st.Host, "err", err)
		return
	}
	slog.Warn("scheduled rebuild STARTED", "tool", name, "host", st.Host, "job", j.ID)
}
