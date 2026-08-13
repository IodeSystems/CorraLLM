package proc

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
)

// pauseSweepInterval is how often expired pauses are retired in the background.
//
// Every read path expires a pause lazily (see lookupPause), so the sweeper is
// not what makes a resume correct — it is what makes a resume HAPPEN for
// something nothing asks for. A persistent model is loaded by Preload and then
// never requested by name, so without a sweep a "pause until 09:00" on a pinned
// embedder would sit unloaded until the next restart or the next unrelated
// request. 30s is well under the granularity anyone picks in a datetime field.
const pauseSweepInterval = 30 * time.Second

// extPrefix marks a pause (and a process key) that addresses an extension
// rather than a single model. Same namespace as config.Model.ProcKey, and
// collision-free for the same reason: a model may not be named after an
// extension, and a served name containing a colon is not addressable.
const extPrefix = "extension:"

// repinAttempts bounds the re-warm of a pinned process after a resume. The
// backoff doubles from 1s, so five attempts span ~31s — comfortably past
// evictGrace (15s), which is the window the retry exists to cover. See repin.
const repinAttempts = 5

// Pause is an operator's "do not run this" order, optionally expiring.
//
// It is deliberately NOT config: pausing is operational state (a GPU is needed
// elsewhere, a backend is misbehaving), not declared intent, and writing it into
// the YAML would churn the user's file for something that is often undone within
// the hour. It is persisted to the store instead, so it survives a restart —
// the failure mode of an in-memory pause is the worst one available: the
// operator pauses a model, corrallm restarts, and the model they were keeping
// off the box quietly loads itself again.
//
// A pause addresses a PROCESS, not a name. That is what gives it the same blast
// radius as an unload: an extension hosts several models in one process, so
// pausing any of them keys on "extension:<name>" and takes all of them out of
// service together. Pausing three of four models and leaving the process up for
// the fourth would be a pause that frees nothing, which is the opposite of why
// anyone reaches for it.
type Pause struct {
	// Key is the process this pause applies to: a served model name, or
	// "extension:<name>". Identical to config.Model.ProcKey for that model.
	Key    string    `json:"key"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
	// ResumeAt is when the pause lifts on its own. Zero means indefinite —
	// only an explicit unpause clears it.
	ResumeAt time.Time `json:"resumeAt"`
}

// Extension returns the extension this pause addresses, and whether it is an
// extension pause at all.
func (p Pause) Extension() (string, bool) {
	name, ok := strings.CutPrefix(p.Key, extPrefix)
	return name, ok
}

// Label is the human name of what was paused — the extension, or the model.
func (p Pause) Label() string {
	if name, ok := p.Extension(); ok {
		return name
	}
	return p.Key
}

// Expired reports whether a timed pause has run out as of now.
func (p Pause) Expired(now time.Time) bool {
	return !p.ResumeAt.IsZero() && !now.Before(p.ResumeAt)
}

// PersistedPause is one stored pause row, in the wire shape the store speaks.
type PersistedPause struct {
	Target     string // process key: a model name, or "extension:<name>"
	Reason     string
	AtMS       int64
	ResumeAtMS int64 // 0 = indefinite
}

// PauseStore persists pauses so they survive a restart. Kept as an interface
// here (rather than importing the store package) for the same reason
// quota.CounterStore is: the process manager has no business knowing about
// SQLite, and tests want a memory-only manager.
type PauseStore interface {
	LoadPauses() ([]PersistedPause, error)
	SavePause(target, reason string, atMS, resumeAtMS int64) error
	DeletePause(target string) error
}

// UsePauseStore attaches durable storage and restores any live pauses.
//
// Call it BEFORE Preload, or a paused persistent model loads at boot and is
// only unloaded once something notices — which for a pinned model is never.
// Pauses that already expired while corrallm was down are dropped here rather
// than resurrected. A load error leaves the manager unpaused rather than
// failing boot: refusing to start because the pause table is unreadable trades
// a small correctness loss for a total outage.
func (m *Manager) UsePauseStore(s PauseStore) {
	if s == nil {
		return
	}
	rows, err := s.LoadPauses()
	if err != nil {
		slog.Warn("pause: restore failed, starting unpaused", "err", err)
		rows = nil
	}
	now := time.Now()

	m.mu.Lock()
	m.pauseStore = s
	if m.paused == nil {
		m.paused = map[string]Pause{}
	}
	var stale []string
	for _, r := range rows {
		p := Pause{Key: r.Target, Reason: r.Reason, At: time.UnixMilli(r.AtMS)}
		if r.ResumeAtMS > 0 {
			p.ResumeAt = time.UnixMilli(r.ResumeAtMS)
		}
		if p.Expired(now) {
			stale = append(stale, r.Target)
			continue
		}
		m.paused[r.Target] = p
	}
	restored := len(m.paused)
	m.mu.Unlock()

	for _, key := range stale {
		if err := s.DeletePause(key); err != nil {
			slog.Warn("pause: dropping expired pause failed", "target", key, "err", err)
		}
	}
	if restored > 0 {
		slog.Info("pause: restored", "targets", restored)
	}
}

// pauseError is the refusal EnsureReady returns for a paused process.
//
// Permanent, so a lane's walk spills to the next member and an exhausted walk
// answers 503 rather than 429 + Retry-After. A pause is an operator decision,
// not congestion: telling a client to retry in 30s against a model that is off
// until tomorrow would be a lie, and against an indefinite pause it would be an
// invitation to hammer.
func pauseError(name string, p Pause) *CapacityError {
	var reason string
	if ext, ok := p.Extension(); ok {
		reason = fmt.Sprintf("model %q is paused: its extension %q is paused", name, ext)
	} else {
		reason = fmt.Sprintf("model %q is paused", name)
	}
	if p.Reason != "" {
		reason += ": " + p.Reason
	}
	if !p.ResumeAt.IsZero() {
		reason += fmt.Sprintf(" (until %s)", p.ResumeAt.Format(time.RFC3339))
	}
	return &CapacityError{Permanent: true, Reason: reason}
}

// lookupPause returns a live pause for a process key, retiring it in place if
// it expired. Caller holds m.mu. The bool reports whether a store delete is
// owed (done by the caller after unlocking — that is a database write, not a
// map edit).
func (m *Manager) lookupPause(key string, now time.Time) (Pause, bool, bool) {
	p, ok := m.paused[key]
	if !ok {
		return Pause{}, false, false
	}
	if p.Expired(now) {
		delete(m.paused, key)
		return Pause{}, false, true
	}
	return p, true, false
}

// dropPersisted removes a pause row, logging rather than propagating: the
// in-memory state is already correct and the row is a durability detail.
func (m *Manager) dropPersisted(key string) {
	m.mu.Lock()
	s := m.pauseStore
	m.mu.Unlock()
	if s == nil {
		return
	}
	if err := s.DeletePause(key); err != nil {
		slog.Warn("pause: delete failed", "target", key, "err", err)
	}
}

// pauseByKey is the one lookup every read goes through. An expired pause is
// retired here, which is what makes a resume time take effect without a timer.
func (m *Manager) pauseByKey(key string) (Pause, bool) {
	m.mu.Lock()
	p, ok, owed := m.lookupPause(key, time.Now())
	m.mu.Unlock()
	if owed {
		m.dropPersisted(key)
	}
	return p, ok
}

// PauseOf returns the pause keeping a served model out of service, or false.
// For an extension-hosted model that is its EXTENSION's pause — one process,
// one pause, so every sibling reports the same thing.
func (m *Manager) PauseOf(model string) (Pause, bool) {
	return m.pauseByKey(m.procKey(model))
}

// PauseOfExtension returns an extension's pause, or false.
func (m *Manager) PauseOfExtension(ext string) (Pause, bool) {
	return m.pauseByKey(extPrefix + ext)
}

// IsPaused is the hot-path predicate behind candidate filtering.
func (m *Manager) IsPaused(model string) bool {
	_, ok := m.PauseOf(model)
	return ok
}

// Pauses lists every active pause, sorted by key.
func (m *Manager) Pauses() []Pause {
	now := time.Now()
	m.mu.Lock()
	out := make([]Pause, 0, len(m.paused))
	var owed []string
	for key, p := range m.paused {
		if p.Expired(now) {
			delete(m.paused, key)
			owed = append(owed, key)
			continue
		}
		out = append(out, p)
	}
	m.mu.Unlock()
	for _, key := range owed {
		m.dropPersisted(key)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// PauseResult reports what a pause actually did to the running process.
type PauseResult struct {
	// Target is the process key that was paused — "extension:<name>" when the
	// model asked for is hosted, so the caller can tell the operator that more
	// than the model they named went out of service.
	Target string
	// Affected lists every served model this pause took out of service. One
	// entry for an ordinary model; all of them for an extension.
	Affected []string
	// Evicted is how many processes went immediately (0 if nothing was
	// resident, or if it is draining in-flight work instead).
	Evicted int
	// Draining is true when in-flight requests are being finished before the
	// process goes.
	Draining bool
	// Skipped explains why the process is still up despite the pause. Empty
	// when there was nothing in the way.
	Skipped string
}

// PauseModel takes a model out of service: it will not be spawned by a request,
// a lane fall-through, an explicit load, or boot preload until it is unpaused or
// resumeAt passes. Its running process is unloaded — draining in-flight work
// rather than killing it, exactly like UnloadModel.
//
// An extension-hosted model pauses its whole EXTENSION, taking every sibling
// with it, because they are one process (see Pause). The result names what was
// actually affected so the caller can say so.
//
// resumeAt must be in the future; zero means indefinite.
func (m *Manager) PauseModel(model, reason string, resumeAt time.Time) (PauseResult, error) {
	cfg := m.config()
	if cfg == nil {
		return PauseResult{}, fmt.Errorf("unknown model %q", model)
	}
	if _, ok := cfg.Models[model]; !ok {
		return PauseResult{}, fmt.Errorf("unknown model %q", model)
	}
	return m.pauseKey(m.procKey(model), reason, resumeAt)
}

// PauseExtension takes an extension out of service, addressed by its own name
// rather than by one of the models it happens to provide. Every model it
// provides is refused for as long as it holds.
func (m *Manager) PauseExtension(ext, reason string, resumeAt time.Time) (PauseResult, error) {
	cfg := m.config()
	if cfg == nil {
		return PauseResult{}, fmt.Errorf("unknown extension %q", ext)
	}
	if _, ok := cfg.Extensions[ext]; !ok {
		return PauseResult{}, fmt.Errorf("unknown extension %q", ext)
	}
	return m.pauseKey(extPrefix+ext, reason, resumeAt)
}

// pauseKey is the shared body: record the order, persist it, unload the process.
//
// Unlike UnloadModel this does NOT refuse a persistent (pinned) process. A pin
// says "keep this warm under memory pressure", which is a scheduling
// preference; a pause is an explicit operator order, and honouring the pin
// would make pausing a pinned model a no-op — the one case where an operator
// most needs it to work. (oidio is pinned, so without this, pausing any audio
// model would do nothing at all.)
func (m *Manager) pauseKey(key, reason string, resumeAt time.Time) (PauseResult, error) {
	now := time.Now()
	if !resumeAt.IsZero() && !resumeAt.After(now) {
		return PauseResult{}, fmt.Errorf("resume time %s is not in the future", resumeAt.Format(time.RFC3339))
	}

	p := Pause{Key: key, Reason: reason, At: now, ResumeAt: resumeAt}
	m.mu.Lock()
	if m.paused == nil {
		m.paused = map[string]Pause{}
	}
	m.paused[key] = p
	s := m.pauseStore
	m.mu.Unlock()

	if s != nil {
		var resumeMS int64
		if !resumeAt.IsZero() {
			resumeMS = resumeAt.UnixMilli()
		}
		if err := s.SavePause(key, reason, now.UnixMilli(), resumeMS); err != nil {
			// The pause is already in effect in memory; a failed write costs
			// durability across a restart, not correctness now.
			slog.Warn("pause: persist failed", "target", key, "err", err)
		}
	}

	res := PauseResult{Target: key, Affected: m.ServedBy(key)}
	slog.Info("pause: paused", "target", key, "models", res.Affected, "reason", reason, "until", resumeAt)

	n, err := m.unloadKey(key, p.Label(), true)
	if err != nil {
		// unloadKey's only remaining error with force is "already draining"; a
		// pause must not fail because an unload was already underway.
		res.Skipped = err.Error()
		return res, nil
	}
	if n == 0 {
		// Either it was not resident, or it went to draining. Distinguish so the
		// operator knows whether the GPU is free yet.
		m.mu.Lock()
		proc := m.procs[key]
		m.mu.Unlock()
		if proc != nil && proc.Draining() {
			res.Draining = true
		}
	}
	res.Evicted = n
	return res, nil
}

// ServedBy lists the served models a process key covers, sorted. One entry for
// an ordinary model; every provided model for an extension.
func (m *Manager) ServedBy(key string) []string {
	if ext, ok := strings.CutPrefix(key, extPrefix); ok {
		if cfg := m.config(); cfg != nil {
			return cfg.ExtensionModels(ext)
		}
		return nil
	}
	return []string{key}
}

// UnpauseModel lifts the pause covering a model — which for a hosted model is
// its extension's, so every sibling comes back with it. It reports whether
// anything was actually paused.
func (m *Manager) UnpauseModel(ctx context.Context, model string) (bool, error) {
	cfg := m.config()
	if cfg == nil {
		return false, fmt.Errorf("unknown model %q", model)
	}
	if _, ok := cfg.Models[model]; !ok {
		return false, fmt.Errorf("unknown model %q", model)
	}
	return m.unpauseKey(ctx, m.procKey(model)), nil
}

// UnpauseExtension returns an extension to service, addressed by its own name.
func (m *Manager) UnpauseExtension(ctx context.Context, ext string) (bool, error) {
	cfg := m.config()
	if cfg == nil {
		return false, fmt.Errorf("unknown extension %q", ext)
	}
	if _, ok := cfg.Extensions[ext]; !ok {
		return false, fmt.Errorf("unknown extension %q", ext)
	}
	return m.unpauseKey(ctx, extPrefix+ext), nil
}

// unpauseKey clears a pause and re-warms the process if it is pinned, since
// nothing else will ever ask for a pinned process by name — Preload is the only
// thing that loads one, and it already ran at boot.
func (m *Manager) unpauseKey(ctx context.Context, key string) bool {
	m.mu.Lock()
	_, was := m.paused[key]
	delete(m.paused, key)
	m.mu.Unlock()
	if !was {
		return false
	}
	m.dropPersisted(key)
	slog.Info("pause: resumed", "target", key)
	m.repin(ctx, key)
	return true
}

// repin re-warms a pinned process whose pause just lifted. Best-effort and
// asynchronous: a failure is logged exactly as Preload's is, and must not make
// the unpause itself fail.
func (m *Manager) repin(ctx context.Context, key string) {
	cfg := m.config()
	if cfg == nil {
		return
	}
	// Any model of an extension reaches the same process, and Effective()
	// overlays the extension's cmd/persistence onto it — so one served name is
	// enough to warm either kind of process.
	served := m.ServedBy(key)
	if len(served) == 0 {
		return
	}
	name := served[0]
	mdl, ok := cfg.Effective(name)
	if !ok || !mdl.Persistent || mdl.Cmd == "" {
		return
	}
	// Detach from the caller's context. An unpause arrives on an HTTP request
	// whose context is canceled the instant the response is written, and this
	// re-warm outlives it by design — a cold load takes seconds to minutes.
	// Observed live: the spawn completed anyway (EnsureReady had already
	// started the load goroutine) but the wait was canceled, so every resume of
	// a pinned process logged a failure it had not actually suffered. Values
	// are kept, cancellation is not; Shutdown still reaps the process group.
	ctx = context.WithoutCancel(ctx)
	go func() {
		// Retry, because the first attempt races the teardown of the process
		// this pause just evicted.
		//
		// Eviction is asynchronous (SIGTERM, then up to evictGrace before
		// SIGKILL), so a resume issued seconds after the pause can reach the
		// spawn while the old process still holds a resource the new one needs
		// — a port, or a fixed systemd unit name. Observed live: a backend
		// spawned with `systemd-run --unit=oidio` failed with "Unit oidio.scope
		// was already loaded", exited 1, and the pinned extension stayed down
		// with nothing to retry it, since nothing ever requests a pinned
		// process by name.
		//
		// The window is bounded by evictGrace, so the backoff is sized to
		// outlast it. A model that genuinely cannot start still gives up, and
		// says so once rather than looping.
		delay := time.Second
		for attempt := 1; ; attempt++ {
			_, done, _, err := m.EnsureReady(ctx, name, mdl, nil, nil)
			if err == nil {
				done()
				slog.Info("pause: pinned process reloaded after resume", "target", key, "attempts", attempt)
				return
			}
			// A pause set again while this was retrying wins: the operator's
			// newer decision must not be undone by an older resume's retry.
			if _, paused := m.pauseByKey(key); paused {
				slog.Info("pause: abandoning reload, paused again", "target", key)
				return
			}
			if attempt >= repinAttempts {
				slog.Warn("pause: reload of pinned process after resume failed",
					"target", key, "attempts", attempt, "err", err)
				return
			}
			slog.Info("pause: reload after resume failed, retrying",
				"target", key, "attempt", attempt, "in", delay, "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			delay *= 2
		}
	}()
}

// StartPauseSweeper retires expired pauses in the background until ctx is done.
//
// Reads expire pauses lazily already; this exists so a process that nothing
// requests still comes back at its resume time — see pauseSweepInterval.
func (m *Manager) StartPauseSweeper(ctx context.Context) {
	go func() {
		t := time.NewTicker(pauseSweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.sweepPauses(ctx)
			}
		}
	}()
}

// sweepPauses expires timed pauses that have come due and re-warms any pinned
// processes among them.
func (m *Manager) sweepPauses(ctx context.Context) {
	now := time.Now()
	m.mu.Lock()
	var due []string
	for key, p := range m.paused {
		if p.Expired(now) {
			delete(m.paused, key)
			due = append(due, key)
		}
	}
	m.mu.Unlock()
	for _, key := range due {
		m.dropPersisted(key)
		slog.Info("pause: expired, resumed", "target", key)
		m.repin(ctx, key)
	}
}
