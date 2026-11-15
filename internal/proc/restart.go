package proc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Drain-then-restart of one local backend (`corrallm backend restart`).
//
// The use case is a changed launch: a new llama.cpp build activated, an env
// var added to the cmd, a slot count changed. A bare SIGTERM to the backend
// does that too, and cuts every request in flight and refuses every request
// that dials it until the respawn (aw4 11-14: 12 + 7 × 502 on two Swift
// restarts). This does it in order instead:
//
//  1. drain: the backend admits nothing new. A request that reaches it waits
//     for the replacement (the refresh hold in EnsureReady), behind the
//     scheduler's own queue, which answers 429 + Retry-After when it is full.
//  2. wait for the in-flight requests, up to DrainTimeout; past it the backend
//     is stopped anyway and those requests are cut.
//  3. stop the process group (SIGTERM, then SIGKILL after evictGrace).
//  4. load the model again from the CURRENT config: the cmd is read and
//     ${tool:x} is resolved at spawn, so a new build or a new env takes effect.
//  5. wait for health. The held requests are served by the new process.
//
// It reuses the non-ECC refresh's drain (refreshing + unloadKey), so a held
// request behaves the same in both.

// DefaultRestartDrain bounds the drain when the caller gives none. A long
// generation on box1 has taken 9 minutes; the handoff drain is 10 minutes too.
const DefaultRestartDrain = 10 * time.Minute

// BackendRestartRetryAfter is the Retry-After for a request refused because a
// local backend is stopping or not yet answering. Short on purpose: a retry
// either reaches the replacement or waits inside its load (EnsureReady holds
// a request while the model loads), so a long value only adds idle time.
const BackendRestartRetryAfter = 5 * time.Second

// restartRequester is what the model-load record shows as the cause.
const restartRequester = "backend restart"

// RestartOptions are the caller's choices for Restart.
type RestartOptions struct {
	// Reason is free text, recorded on the stop event ("restart: <reason>").
	Reason string
	// DrainTimeout bounds the wait for in-flight requests. 0 = DefaultRestartDrain.
	DrainTimeout time.Duration
	// Progress, when set, is told each stage as it starts: "draining",
	// "stopping", "loading". detail is a short human line.
	Progress func(stage, detail string)
}

// RestartResult says what Restart did.
type RestartResult struct {
	// Running is false when the model had no resident process: nothing was
	// restarted, and its next request starts it with the current config.
	Running bool
	// InFlight is how many requests were in flight when the drain began; Cut
	// is how many of them were still running when the drain timed out.
	InFlight, Cut int
	// OldID and NewID are the process handles' ids (a pid locally).
	OldID, NewID string
	// CmdChanged reports that the command run differs from the previous one
	// (after ${tool:x} expansion and slot tuning).
	CmdChanged bool
	// OldCmdHash and NewCmdHash identify the command each process ran (after
	// ${tool:x} expansion and slot tuning): CmdHash of it, so an operator can
	// match the respawn against the config they loaded.
	OldCmdHash, NewCmdHash string
	// StaleConfig reports that the config was reloaded after the new process
	// was spawned and its cmd differs: the replacement does not run what the
	// config holds now, and a second restart is needed to pick it up.
	StaleConfig bool
	// Drained and Loaded are how long each half took.
	Drained, Loaded time.Duration
}

// Restart drains, stops and respawns the resident backend of one local model.
// See the comment at the top of this file.
func (m *Manager) Restart(ctx context.Context, served string, o RestartOptions) (RestartResult, error) {
	var res RestartResult
	progress := func(stage, detail string) {
		slog.Info("backend restart: "+stage, "model", served, "detail", detail)
		if o.Progress != nil {
			o.Progress(stage, detail)
		}
	}
	drain := o.DrainTimeout
	if drain <= 0 {
		drain = DefaultRestartDrain
	}
	cfg := m.config()
	if cfg == nil {
		return res, fmt.Errorf("unknown model %q", served)
	}
	mdl, ok := cfg.Effective(served)
	if !ok {
		return res, fmt.Errorf("unknown model %q", served)
	}
	if mdl.Cmd == "" {
		return res, fmt.Errorf("model %q has no cmd (pure proxy): corrallm runs no process for it", served)
	}
	if m.draining.Load() {
		return res, errors.New("this corrallm process has handed over; run the restart against its successor")
	}
	if m.IsPaused(served) {
		return res, fmt.Errorf("model %q is paused; unpause it first (a paused model is not loaded)", served)
	}
	key := m.procKey(served)

	m.mu.Lock()
	p := m.procs[key]
	m.mu.Unlock()
	if p == nil {
		return res, nil
	}
	res.Running = true
	// The restart loads the model itself; a keepWarm respawn into the gap
	// between the stop and that load would race it (keepwarm.go).
	m.mu.Lock()
	if m.restarting == nil {
		m.restarting = map[string]bool{}
	}
	m.restarting[key] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.restarting, key)
		m.mu.Unlock()
	}()

	// A backend still loading is restarted once it is up: stopping a load
	// half way gains nothing, and the replacement would be the same load.
	select {
	case <-p.ready:
	case <-ctx.Done():
		return res, ctx.Err()
	}

	p.mu.Lock()
	switch {
	case p.draining:
		p.mu.Unlock()
		return res, fmt.Errorf("model %q is already draining", served)
	case p.state != StateReady:
		st := p.state
		p.mu.Unlock()
		return res, fmt.Errorf("model %q is %s, not ready", served, st)
	}
	oldCmd := p.spawnedCmd
	res.OldCmdHash = CmdHash(oldCmd)
	if p.handle != nil {
		res.OldID = p.handle.ID()
	}
	res.InFlight = p.refs
	p.refreshing = true // a request that reaches it now waits for the replacement
	p.mu.Unlock()

	reason := "restart"
	if o.Reason != "" {
		reason += ": " + o.Reason
	}
	drainStart := time.Now()
	progress("draining", fmt.Sprintf("%d in flight, waiting up to %s", res.InFlight, drain))
	// force: a pinned model restarts too; the operator asked for it by name.
	if _, err := m.unloadKey(key, served, true, reason); err != nil {
		p.mu.Lock()
		p.refreshing = false
		p.mu.Unlock()
		return res, err
	}

	dctx, cancel := context.WithTimeout(ctx, drain)
	err := m.awaitGone(dctx, key, p)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			// The caller gave up; the drain goes on, and the next request
			// loads the model as after any unload.
			return res, ctx.Err()
		}
		m.mu.Lock()
		if m.procs[key] == p {
			p.mu.Lock()
			res.Cut = p.refs
			p.mu.Unlock()
			slog.Warn("backend restart: drain timed out, stopping with requests in flight",
				"model", served, "cut", res.Cut, "waited", drain)
			m.evictLocked(p)
		}
		m.mu.Unlock()
	}
	res.Drained = time.Since(drainStart)

	progress("stopping", "waiting for the old process to exit")
	if err := m.awaitStop(ctx, key); err != nil {
		return res, err
	}

	progress("loading", "starting it from the current config")
	loadStart := time.Now()
	// EnsureReady resolves the model from the config current at this moment,
	// not from mdl (read before the drain): a reload during the drain, or a
	// held request carrying a pre-reload copy that wins the respawn, still
	// spawns the newest cmd.
	np, release, _, err := m.EnsureReady(WithRequester(ctx, restartRequester), served, mdl, nil, nil)
	if err != nil {
		return res, fmt.Errorf("restart: the old process stopped, the new one did not come up: %w", err)
	}
	release()
	res.Loaded = time.Since(loadStart)
	np.mu.Lock()
	if np.handle != nil {
		res.NewID = np.handle.ID()
	}
	res.CmdChanged = oldCmd != "" && np.spawnedCmd != oldCmd
	res.NewCmdHash = CmdHash(np.spawnedCmd)
	configCmd, placement := np.configCmd, np.placement
	np.mu.Unlock()
	res.StaleConfig = configCmd != "" && m.configCmdNow(served, placement) != configCmd
	return res, nil
}

// configCmdNow is the configured cmd the current config gives a served model
// at one placement (by name), or "" when it has none.
func (m *Manager) configCmdNow(served, placement string) string {
	cfg := m.config()
	if cfg == nil {
		return ""
	}
	mdl, ok := cfg.Effective(served)
	if !ok {
		return ""
	}
	for _, pl := range mdl.PlacementList() {
		if pl.Name == placement {
			return mdl.ForPlacement(pl).Cmd
		}
	}
	return mdl.Cmd
}

// CmdHash is a short, stable id of a spawned command line: the first 12 hex
// digits of its SHA-256. "" for no command.
func CmdHash(cmd string) string {
	if cmd == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(cmd))
	return hex.EncodeToString(sum[:])[:12]
}

// Stopping reports that corrallm has ordered this process to stop: it is
// draining, or it was evicted. A request that fails on it failed because of
// that order, not because the backend is broken.
func (p *Process) Stopping() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.draining || p.stopped
}
