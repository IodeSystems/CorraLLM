package proc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/corrallm/internal/agent"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/host"
	"github.com/iodesystems/corrallm/internal/tune"
)

// A trial is a model spawned to be LOOKED AT, then thrown away.
//
// Authoring a backend command is iterative — a flag is wrong, a path is wrong,
// the context size does not fit — and the only way to find out has been to
// write the model into config, load it, read the logs, and edit again. Every
// iteration mutated the live config, and a half-written model sitting in
// `models:` is one a lane can reference and a request can land on.
//
// So a trial commits to nothing. It takes a command string that exists nowhere,
// runs it on the chosen server, reports what happened at each step, and tears it
// down. What it learns is the point: the measured footprint IS the ramUsage to
// write down, and the banner's slot count IS the maxConcurrent — numbers that
// were previously typed from guesswork and only corrected after an OOM.

// TrialStage names one step. Ordered as they run; a failure ends the run at
// whichever stage failed, except teardown, which always runs.
type TrialStage string

const (
	TrialResolve  TrialStage = "resolve"  // where would this actually forward?
	TrialAdmit    TrialStage = "admit"    // is there room, without evicting anyone?
	TrialSpawn    TrialStage = "spawn"    // start the group
	TrialLog      TrialStage = "log"      // one line of backend output
	TrialHealth   TrialStage = "health"   // /health answers 2xx
	TrialProbe    TrialStage = "probe"    // what model id does it actually serve?
	TrialMeasure  TrialStage = "measure"  // what did it really cost?
	TrialTeardown TrialStage = "teardown" // and it is gone
)

// TrialEvent is one thing that happened, streamed as it happens.
//
// Log lines are events too, on the same channel and in order relative to the
// stages around them. That ordering is the feature: "health timed out" three
// lines after "failed to load model: no such file" is a diagnosis, whereas the
// same two facts in separate panes is a puzzle.
type TrialEvent struct {
	Stage TrialStage     `json:"stage"`
	OK    bool           `json:"ok"`
	Msg   string         `json:"msg,omitempty"`
	Data  map[string]any `json:"data,omitempty"`
}

// TrialResult is what the run learned, for prefilling the model form.
//
// Every field is optional because every field can legitimately be unknowable:
// MemoryMiB is absent on a host that cannot attribute memory per process, and
// Upstream is absent from a backend that serves no /v1/models.
type TrialResult struct {
	OK        bool   `json:"ok"`
	Upstream  string `json:"upstream,omitempty"`
	MemoryMiB int    `json:"memoryMiB,omitempty"`
	HasUI     bool   `json:"hasUI,omitempty"`
	Failed    string `json:"failedStage,omitempty"`

	// What the backend says about ITSELF. Every one of these was previously
	// something the operator had to know before they could save a model — the
	// context it ended up with, how many concurrent requests it will take, what
	// inputs it accepts. The backend knows all of it and will say so if asked,
	// which makes asking the operator a design mistake rather than a limitation.
	ContextLength int      `json:"contextLength,omitempty"`
	Slots         int      `json:"slots,omitempty"`
	Modalities    []string `json:"modalities,omitempty"`
	SupportsTools bool     `json:"supportsTools,omitempty"`
}

// trialTTL bounds a trial that nobody is watching any more.
//
// A trial holds a real reservation and a real process. The operator closing the
// browser tab must not be the difference between that being returned and it
// being held until the daemon restarts, so the run is bounded on its own clock
// as well as by the caller's context.
// Long enough to sit through a first-time model download. `-hf` fetches on the
// first spawn, and tens of GB at LAN speed is tens of minutes — during which
// the backend is working exactly as intended and has simply not bound its port
// yet. A short budget here reports "unhealthy" for a model that is fine, which
// is the least useful answer a probe can give.
//
// It is not the real safety net: waitHealthy watches the process and returns
// the moment it EXITS, so a genuinely broken command fails in seconds rather
// than waiting this out. This only bounds the case where nothing is wrong and
// nothing is finished.
const trialTTL = 90 * time.Minute

// trialGrace is how long a trial's process group gets to exit on SIGTERM before
// it is killed. Shorter than eviction's: a trial has no in-flight requests to
// protect, and the operator is watching a spinner.
const trialGrace = 10 * time.Second

// TrialKeyPrefix marks a Process as a trial rather than a configured model.
//
// It must be a real key in Manager.procs, not a side table. ReconcileAgent reaps
// any backend on an agent that no live Process claims (reconcile.go), so a trial
// tracked anywhere else would be killed as an orphan 60 seconds in — during the
// slow cold load that is the whole reason for trialling it.
const TrialKeyPrefix = "trial:"

// IsTrial reports whether a process key belongs to a trial, so read surfaces can
// label it rather than presenting an experiment as a configured model.
func IsTrial(key string) bool { return strings.HasPrefix(key, TrialKeyPrefix) }

// Trial spawns an uncommitted command, reports what happened, and removes it.
//
// emit is called from this goroutine, in order, and must not block for long.
// The error return is the run's outcome; every stage is also reported through
// emit, so a caller streaming to a UI needs nothing else.
func (m *Manager) Trial(ctx context.Context, id string, mdl config.Model, emit func(TrialEvent)) (TrialResult, error) {
	var res TrialResult
	fail := func(stage TrialStage, err error) (TrialResult, error) {
		res.Failed = string(stage)
		emit(TrialEvent{Stage: stage, Msg: err.Error()})
		return res, err
	}

	ctx, cancel := context.WithTimeout(ctx, trialTTL)
	defer cancel()

	// --- resolve -----------------------------------------------------------
	// TargetFor, not ProxyTarget: on an agent-bound server `proxy: 5800` means
	// "the port MY backend listens on", which resolves to loopback — the
	// primary. Health-checking that would probe whatever local process happens
	// to hold the port and report a stranger's health as the trial's.
	cfg := m.config()
	if cfg == nil {
		return fail(TrialResolve, fmt.Errorf("no config loaded"))
	}
	target, err := cfg.TargetFor(id, mdl)
	if err != nil {
		return fail(TrialResolve, err)
	}
	if target == nil || target.URL == nil {
		return fail(TrialResolve, fmt.Errorf("proxy did not resolve to a URL"))
	}
	emit(TrialEvent{Stage: TrialResolve, OK: true, Msg: target.BaseURLString(),
		Data: map[string]any{"url": target.BaseURLString(), "server": mdl.Server}})

	// --- admit -------------------------------------------------------------
	key := TrialKeyPrefix + id
	p, err := m.admitTrial(key, id, mdl, target)
	if err != nil {
		return fail(TrialAdmit, err)
	}
	// From here every exit path must release the reservation and the process.
	defer func() {
		emit(TrialEvent{Stage: TrialTeardown, OK: true, Msg: m.endTrial(key, p)})
	}()
	emit(TrialEvent{Stage: TrialAdmit, OK: true, Msg: admitMsg(p.usage),
		Data: map[string]any{"reserved": p.usage}})

	// --- spawn -------------------------------------------------------------
	h, err := m.hostFor(mdl.Server).Start(host.Spec{
		// Key, not just Name: reconciliation matches on it, and a mismatch gets
		// the backend reaped as an orphan while this process is still starting.
		Name: id, Key: key, Cmd: mdl.Cmd, Out: &trialSink{emit: emit},
	})
	if err != nil {
		return fail(TrialSpawn, err)
	}
	p.mu.Lock()
	p.handle = h
	p.spawnedCmd = mdl.Cmd
	p.state = StateLoading
	p.mu.Unlock()
	emit(TrialEvent{Stage: TrialSpawn, OK: true, Msg: h.ID()})

	// --- health ------------------------------------------------------------
	// Listening is not ready: llama-server binds early and answers 503 "Loading
	// model" until the weights and KV cache are in. waitHealthy already knows
	// that, and watching Done() means a backend that dies during startup is
	// reported as the crash it is rather than as a timeout.
	// Report progress while waiting. A first-time `-hf` fetch can be tens of
	// minutes of apparent silence, and an operator watching a spinner cannot
	// tell "downloading 30 GB" from "hung". waitHealthyFor returns immediately
	// if the process exits, so a broken command still fails fast.
	healthErr := make(chan error, 1)
	go func() { healthErr <- m.waitHealthyFor(target, h.Done(), trialTTL) }()
	waited := 0
	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()
waiting:
	for {
		select {
		case err := <-healthErr:
			if err != nil {
				return fail(TrialHealth, err)
			}
			break waiting
		case <-tick.C:
			waited += 20
			emit(TrialEvent{Stage: TrialHealth, OK: true,
				Msg:  fmt.Sprintf("still starting (%ds) — a first run downloads the model before it binds its port", waited),
				Data: map[string]any{"waitedSeconds": waited, "state": "loading"}})
		case <-ctx.Done():
			return fail(TrialHealth, ctx.Err())
		}
	}
	p.mu.Lock()
	p.state = StateReady
	p.readyAt = time.Now()
	p.mu.Unlock()
	emit(TrialEvent{Stage: TrialHealth, OK: true, Msg: "/health answered 2xx"})

	// --- probe -------------------------------------------------------------
	// A backend's own id for a model is often not the name it is served under
	// (a path, a repo id, a quantisation suffix). Asking beats guessing, and a
	// wrong `upstream` is a 404 at request time with nothing to point at.
	if up, err := m.probeUpstream(ctx, target); err != nil {
		emit(TrialEvent{Stage: TrialProbe, Msg: err.Error()})
	} else {
		res.Upstream = up
		emit(TrialEvent{Stage: TrialProbe, OK: true, Msg: up,
			Data: map[string]any{"upstream": up}})
	}
	m.probeUI(p)
	res.HasUI = p.hasUI.Load() == 1

	// Ask the backend what it became. `-c 200000` is a REQUEST; the number it
	// actually got is what matters for contextPerRequest, and slot count is
	// maxConcurrent. Best-effort: a backend without /props is still a usable
	// model, it just cannot describe itself.
	if pr, err := m.probeProps(ctx, target); err != nil {
		emit(TrialEvent{Stage: TrialProbe, Msg: "no /props: " + err.Error()})
	} else {
		res.ContextLength, res.Slots = pr.NCtx, pr.Slots
		res.Modalities, res.SupportsTools = pr.Modalities, pr.Tools
		emit(TrialEvent{Stage: TrialProbe, OK: true,
			Msg: fmt.Sprintf("context %d, %d slot(s), modalities %v, tools %v",
				pr.NCtx, pr.Slots, pr.Modalities, pr.Tools),
			Data: map[string]any{"contextLength": pr.NCtx, "slots": pr.Slots,
				"modalities": pr.Modalities, "supportsTools": pr.Tools}})
	}

	// --- measure -----------------------------------------------------------
	// The payoff. On a host with per-process accounting this number is the
	// ramUsage to write down — measured, for this exact command, instead of
	// typed and discovered wrong later. Where it is unavailable, saying so is
	// the honest answer and is precisely why that host requires a declared one.
	if mib, err := h.MemoryMiB(); err != nil {
		emit(TrialEvent{Stage: TrialMeasure,
			Msg: "cannot attribute memory per process on this host — declare ramUsage by hand"})
	} else {
		res.MemoryMiB = mib
		emit(TrialEvent{Stage: TrialMeasure, OK: true, Msg: fmt.Sprintf("%d MiB", mib),
			Data: map[string]any{"memoryMiB": mib, "ramUsage": fmt.Sprintf("%dMiB", mib)}})
	}

	res.OK = true
	return res, nil
}

// admitTrial reserves pools for a trial, or refuses.
//
// It reserves only what is already FREE — makeRoomLocked is deliberately not
// called. A trial is an experiment, and an experiment that can evict a warm
// production backend to run is worse than one that has to wait for room.
//
// The Process it registers is marked persistent, which reads backwards until
// you pair it with the above: having taken only free memory, a trial displaces
// nothing by holding it, and being unevictable means the operator's run is not
// killed halfway through by unrelated traffic. trialTTL is what bounds it.
func (m *Manager) admitTrial(key, id string, mdl config.Model, target *config.ProxyTarget) (*Process, error) {
	if mdl.Cmd == "" {
		return nil, fmt.Errorf("a trial needs a cmd — there is nothing to spawn")
	}
	if mdl.Server == "" {
		return nil, fmt.Errorf("a trial needs a server — it must run somewhere")
	}
	if m.live != nil && !m.live.Reachable(mdl.Server, time.Now()) {
		return nil, fmt.Errorf("server %q is down: its agent has not reported in for over %s",
			mdl.Server, agent.MissWindow)
	}

	// A trial has no measured profile to prefer — it is not a configured model,
	// so nothing has ever measured it. Whatever the operator declared is all
	// there is, and an empty declaration is honestly unknown rather than free.
	usage, err := config.ParseSizes(mdl.RAMUsage)
	if err != nil {
		return nil, fmt.Errorf("ramUsage: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// No one-trial-per-server rule. It was justified by two experiments racing a
	// port, but the operator chooses the port and capacity already gates whether
	// a second run has room — so the rule blocked legitimate parallel work to
	// prevent a collision its victim had already avoided.
	if _, taken := m.procs[key]; taken {
		return nil, fmt.Errorf("trial %q is already running", id)
	}
	// An unsized probe takes WHAT IS FREE, not the whole pool.
	//
	// unknownIfEmpty reserves the entire pool for an unknown model, which is the
	// right call for a production spawn: it evicts everything, runs alone, and
	// the measurement governs from then on. For a probe it is a dead end — a
	// probe never evicts, so "reserve everything" means it can only ever run on
	// an empty machine, and the first thing anyone wants to probe is a second
	// model on a box that already has one.
	//
	// Reserving the free remainder keeps the property that matters (nothing else
	// is admitted while an unmeasured thing is running) without inventing a
	// per-model number. If it needs more than the machine has, that IS the
	// probe's answer.
	if len(usage) == 0 {
		usage = m.freeRemainderLocked(mdl.Server)
		if len(usage) == 0 {
			return nil, fmt.Errorf("no free capacity on %q to probe into — unload something first",
				mdl.Server)
		}
	}
	if !m.fitsLocked(mdl.Server, usage, nil) {
		return nil, fmt.Errorf("not enough free capacity on %q for %s — unload something first "+
			"(a probe never evicts a running model)", mdl.Server, admitMsg(usage))
	}
	m.reserveLocked(mdl.Server, usage)

	p := &Process{
		Name: id, ModelName: id, key: key, Target: target,
		server: mdl.Server, usage: usage,
		persistent: true, // see the doc comment: it took only free memory
		logs:       newLogBuffer(500),
		state:      StateAbsent,
		ready:      make(chan struct{}),
	}
	close(p.ready) // nothing waits on a trial the way requests wait on a model
	m.procs[key] = p
	return p, nil
}

// freeRemainderLocked is what is actually unspoken-for on server right now:
// budget, minus corrallm's own reservations, minus what other tenants are
// using. Caller holds m.mu.
func (m *Manager) freeRemainderLocked(server string) map[string]int64 {
	out := map[string]int64{}
	for pool, budget := range m.budget[server] {
		free := budget - m.used[server][pool] - m.foreignUsedLocked(server, pool)
		if free > 0 {
			out[pool] = free
		}
	}
	return out
}

// endTrial stops the process group and returns the reservation, whatever
// happened upstream. Reports what it did, for the teardown event.
func (m *Manager) endTrial(key string, p *Process) string {
	p.mu.Lock()
	h := p.handle
	p.state = StateEvicting
	p.mu.Unlock()

	msg := "released; nothing was spawned"
	if h != nil {
		msg = "stopped"
		_ = h.Signal(host.SigTerm)
		select {
		case <-h.Done():
		case <-time.After(trialGrace):
			// A backend in CUDA teardown can ignore SIGTERM for a long time, and
			// an untracked group holding tens of GB is the worst state there is.
			_ = h.Signal(host.SigKill)
			msg = "stopped (killed after " + trialGrace.String() + ")"
		}
	}

	m.mu.Lock()
	if m.procs[key] == p {
		delete(m.procs, key)
	}
	m.freeLocked(p.server, p.usage)
	m.mu.Unlock()
	return msg
}

// probeUpstream asks the backend what model id it actually serves.
func (m *Manager) probeUpstream(ctx context.Context, t *config.ProxyTarget) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.BaseURLString()+"/v1/models", nil)
	if err != nil {
		return "", err
	}
	for k, v := range t.Headers {
		req.Header.Set(k, v)
	}
	resp, err := m.healthCli.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("/v1/models returned %d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", err
	}
	if len(body.Data) == 0 || body.Data[0].ID == "" {
		return "", fmt.Errorf("/v1/models listed nothing")
	}
	return body.Data[0].ID, nil
}

// recordCapabilities files what a probe found against the placement that was
// probed, so nothing has to be declared for it to be known.
//
// Modalities are translated here rather than at the edge: llama.cpp says
// vision/video/audio, corrallm's vocabulary is text/image/audio, and storing
// the backend's spelling would push that mismatch onto every reader.
func (m *Manager) recordCapabilities(name string, mdl config.Model, res TrialResult) {
	if m.tuneCache == nil {
		return
	}
	pl, ok := mdl.PlacementOn(mdl.Server)
	if !ok {
		if ps := mdl.PlacementList(); len(ps) > 0 {
			pl = ps[0]
		} else {
			return // a pure proxy: nothing placed, nothing to key against
		}
	}
	mods := []string{"text"}
	for _, r := range res.Modalities {
		switch r {
		case "vision", "video", "image":
			if !containsFold(mods, "image") {
				mods = append(mods, "image")
			}
		case "audio":
			if !containsFold(mods, "audio") {
				mods = append(mods, "audio")
			}
		}
	}
	m.tuneCache.PutCapabilities(pl.Name, name, tune.Capabilities{
		ContextLength: res.ContextLength, Slots: res.Slots,
		Modalities: mods, Tools: res.SupportsTools,
		Upstream: res.Upstream, HasUI: res.HasUI,
		ProbedAt: time.Now(), ProbedCmd: pl.Cmd,
	})
}

func containsFold(all []string, want string) bool {
	for _, s := range all {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}

// InstallProbedModalities makes probed capabilities the answer everything gets
// when it asks what a model accepts.
//
// It is a hook rather than a lookup at each site because the question is asked
// from the proxy's catalog, the bench planner and the overview, and every one
// of them wants the same answer. The catalog is the important one: llm-bench
// reads modalities from /v1/models, so pointing that at probed data makes bench
// gate on what a backend DOES rather than on what someone declared — without
// bench changing at all.
//
// Falls through to the declaration when a model has never been probed, so
// nothing changes for a fleet that has not adopted probing.
func (m *Manager) InstallProbedModalities() {
	config.ProbedModalities = func(served string) (map[string]config.ModalitySpec, bool) {
		cfg := m.config()
		if cfg == nil || m.tuneCache == nil {
			return nil, false
		}
		mdl, ok := cfg.Models[served]
		if !ok {
			return nil, false
		}
		// Union across placements: a model is capable of a thing if ANY way of
		// serving it is. Which placement a given request lands on is the
		// scheduler's business, and a catalog that changed shape depending on
		// what happened to be warm would be worse than useless.
		out := map[string]config.ModalitySpec{}
		found := false
		for _, pl := range mdl.PlacementList() {
			caps, ok := m.tuneCache.CapabilitiesFor(pl.Name, served)
			if !ok {
				continue
			}
			found = true
			for _, mod := range caps.Modalities {
				out[mod] = config.ModalitySpec{}
			}
		}
		if !found || len(out) == 0 {
			return nil, false
		}
		return out, true
	}
}

// Capabilities returns what a placement was probed to do.
func (m *Manager) Capabilities(placement, model string) (tune.Capabilities, bool) {
	if m.tuneCache == nil {
		return tune.Capabilities{}, false
	}
	return m.tuneCache.CapabilitiesFor(placement, model)
}

// backendProps is the subset of llama.cpp's /props worth acting on.
type backendProps struct {
	NCtx       int
	Slots      int
	Modalities []string
	Tools      bool
}

// probeProps asks the backend to describe itself.
//
// The chat template is inspected rather than trusted from a flag: whether a
// model can call tools is a property of the template it was built with, and a
// model configured as tool-capable that silently never emits a tool call is a
// failure that surfaces as bad answers rather than as an error.
func (m *Manager) probeProps(ctx context.Context, t *config.ProxyTarget) (backendProps, error) {
	var out backendProps
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.BaseURLString()+"/props", nil)
	if err != nil {
		return out, err
	}
	for k, v := range t.Headers {
		req.Header.Set(k, v)
	}
	resp, err := m.healthCli.Do(req)
	if err != nil {
		return out, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, fmt.Errorf("/props returned %d", resp.StatusCode)
	}
	var body struct {
		TotalSlots int `json:"total_slots"`
		NCtx       int `json:"n_ctx"`
		DefaultGen struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
		Modalities   map[string]bool `json:"modalities"`
		ChatTemplate string          `json:"chat_template"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return out, err
	}
	out.NCtx = body.NCtx
	if out.NCtx == 0 {
		out.NCtx = body.DefaultGen.NCtx
	}
	out.Slots = body.TotalSlots
	for k, on := range body.Modalities {
		if on {
			out.Modalities = append(out.Modalities, k)
		}
	}
	sort.Strings(out.Modalities) // map order is random; a shuffling list reads as a change
	out.Tools = strings.Contains(strings.ToLower(body.ChatTemplate), "tool_call")
	return out, nil
}

func admitMsg(usage map[string]int64) string {
	if len(usage) == 0 {
		return "no pools reserved"
	}
	parts := make([]string, 0, len(usage))
	for pool, b := range usage {
		parts = append(parts, fmt.Sprintf("%s: %d MiB", pool, b/(1024*1024)))
	}
	return strings.Join(parts, ", ")
}

// trialSink turns the backend's output into log events, one per line.
//
// Writes arrive in arbitrary chunks, so partial lines are held until their
// newline: llama.cpp's banner is what the operator is reading, and a line torn
// across two events is one they have to reassemble by eye.
type trialSink struct {
	mu   sync.Mutex
	buf  []byte
	emit func(TrialEvent)
}

func (s *trialSink) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, b...)
	for {
		i := bytes.IndexByte(s.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(s.buf[:i]), "\r")
		s.buf = s.buf[i+1:]
		if line != "" {
			s.emit(TrialEvent{Stage: TrialLog, OK: true, Msg: line})
		}
	}
	return len(b), nil
}

// Probe interrogates a model that ALREADY EXISTS, as opposed to Trial, which
// spawns an ephemeral one that exists nowhere.
//
// The distinction is the whole difference in behaviour. A trial is an
// experiment: it is admitted only into free memory, never evicts, and is torn
// down whatever happens, because nothing is supposed to survive it. A probe is
// a question about a declared model, so it goes through the ordinary load path
// — normal admission, normal eviction, normal residency — and LEAVES THE MODEL
// AS IT FOUND IT. If it was already warm it stays warm; if this call loaded it,
// it stays loaded under its own sticky/TTL rules rather than being killed by
// the act of asking about it.
//
// What it reports is identical, and deliberately so: capability discovery
// should not depend on whether the thing has been written down yet.
func (m *Manager) Probe(ctx context.Context, name string, emit func(TrialEvent)) (TrialResult, error) {
	var res TrialResult
	fail := func(stage TrialStage, err error) (TrialResult, error) {
		res.Failed = string(stage)
		emit(TrialEvent{Stage: stage, Msg: err.Error()})
		return res, err
	}

	cfg := m.config()
	if cfg == nil {
		return fail(TrialResolve, fmt.Errorf("no config loaded"))
	}
	mdl, ok := cfg.Models[name]
	if !ok {
		return fail(TrialResolve, fmt.Errorf("no model %q — to try a command that is not "+
			"configured yet, use a trial instead", name))
	}
	target, err := cfg.TargetFor(name, mdl)
	if err != nil {
		return fail(TrialResolve, err)
	}
	emit(TrialEvent{Stage: TrialResolve, OK: true, Msg: target.BaseURLString(),
		Data: map[string]any{"url": target.BaseURLString(), "server": mdl.Server}})

	// The ordinary door. Everything a real request would face — pause, capacity,
	// eviction, a down agent — applies here too, so a probe reports the model as
	// it actually is rather than as a private copy would be.
	p, release, loaded, err := m.EnsureReady(ctx, name, mdl, nil, nil)
	if err != nil {
		return fail(TrialHealth, err)
	}
	defer release()
	emit(TrialEvent{Stage: TrialHealth, OK: true,
		Msg:  map[bool]string{true: "loaded for this probe", false: "already resident"}[loaded],
		Data: map[string]any{"triggeredLoad": loaded}})

	if up, err := m.probeUpstream(ctx, target); err == nil {
		res.Upstream = up
		emit(TrialEvent{Stage: TrialProbe, OK: true, Msg: up})
	}
	if pr, err := m.probeProps(ctx, target); err != nil {
		emit(TrialEvent{Stage: TrialProbe, Msg: "no /props: " + err.Error()})
	} else {
		res.ContextLength, res.Slots = pr.NCtx, pr.Slots
		res.Modalities, res.SupportsTools = pr.Modalities, pr.Tools
		emit(TrialEvent{Stage: TrialProbe, OK: true,
			Msg: fmt.Sprintf("context %d, %d slot(s), modalities %v, tools %v",
				pr.NCtx, pr.Slots, pr.Modalities, pr.Tools),
			Data: map[string]any{"contextLength": pr.NCtx, "slots": pr.Slots,
				"modalities": pr.Modalities, "supportsTools": pr.Tools}})
	}
	res.HasUI = p.hasUI.Load() == 1
	// Record against the PLACEMENT that was probed. Two placements of one model
	// are the case this exists for, so a per-model record would have the second
	// silently overwrite the first.
	m.recordCapabilities(name, mdl, res)

	p.mu.Lock()
	h := p.handle
	p.mu.Unlock()
	if h == nil {
		// A pure proxy has no local process, so there is nothing to weigh.
		emit(TrialEvent{Stage: TrialMeasure, Msg: "no local process: this model is proxied, not spawned"})
	} else {
		// The MAXIMUM ever observed, not a spot reading.
		//
		// A live sample is whatever the process happens to hold at this instant,
		// which is misleading in both directions: a backend that has just
		// started has faulted in almost nothing (observed: 16 MiB for a model
		// that settles at 34 GB), and one that has released a transient buffer —
		// an mmproj loaded and freed, a large batch retired — reads below the
		// peak it will reach again. Admission has to reserve the high-water
		// mark, so that is what a probe reports.
		//
		// sampleVRAMPeak feeds this on a ticker for the life of the process;
		// the live reading is the fallback for a model too new to have been
		// sampled yet.
		peak := m.TuneProfilePeak(name)
		live, liveErr := h.MemoryMiB()
		switch {
		case peak > 0 && peak >= live:
			res.MemoryMiB = peak
			emit(TrialEvent{Stage: TrialMeasure, OK: true,
				Msg: fmt.Sprintf("%d MiB (peak observed; %d MiB right now)", peak, live),
				Data: map[string]any{"memoryMiB": peak, "liveMiB": live,
					"ramUsage": fmt.Sprintf("%dMiB", peak)}})
		case liveErr != nil:
			emit(TrialEvent{Stage: TrialMeasure, Msg: liveErr.Error()})
		default:
			res.MemoryMiB = live
			emit(TrialEvent{Stage: TrialMeasure, OK: true,
				Msg:  fmt.Sprintf("%d MiB (live; no peak recorded yet, so this is a floor)", live),
				Data: map[string]any{"memoryMiB": live, "ramUsage": fmt.Sprintf("%dMiB", live)}})
		}
	}

	// No teardown. The model was here before the question and stays after it.
	res.OK = true
	return res, nil
}

// StateOf reports the lifecycle state of one placement's process.
//
// Keyed by process key rather than by served name, because two placements of a
// model are two processes: reporting the model's state would make loading one
// look like loading both.
func (m *Manager) StateOf(key string) State {
	m.mu.Lock()
	p := m.procs[key]
	m.mu.Unlock()
	if p == nil {
		return StateAbsent
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.draining {
		return StateDraining
	}
	return p.state
}

// PlacementPeak is the largest footprint measured for a model ON one placement.
//
// Profiles are keyed by DEVICE, so this resolves the placement's server to its
// device first — the same model on two boxes has two peaks, and reporting one
// against the other is how a laptop's 34 GB ended up filed under an RTX 5090.
func (m *Manager) PlacementPeak(model string, pl config.Placement) int {
	if m.tuneCache == nil {
		return 0
	}
	dev := m.deviceNameFor(pl.Server, pl.RAMUsage)
	if dev == "" {
		return 0
	}
	if prof, ok := m.tuneCache.Get(dev, model); ok {
		return prof.PeakMiB
	}
	return 0
}
