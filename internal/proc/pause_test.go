package proc

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/iodesystems/corrallm/internal/config"
)

// memPauseStore is an in-memory PauseStore, standing in for the SQLite one.
type memPauseStore struct {
	mu    sync.Mutex
	rows  map[string]PersistedPause
	loads int
	fail  error
}

func newMemPauseStore(seed ...PersistedPause) *memPauseStore {
	s := &memPauseStore{rows: map[string]PersistedPause{}}
	for _, r := range seed {
		s.rows[r.Target] = r
	}
	return s
}

func (s *memPauseStore) LoadPauses() ([]PersistedPause, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	if s.fail != nil {
		return nil, s.fail
	}
	out := make([]PersistedPause, 0, len(s.rows))
	for _, r := range s.rows {
		out = append(out, r)
	}
	return out, nil
}

func (s *memPauseStore) SavePause(target, reason string, atMS, resumeAtMS int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[target] = PersistedPause{Target: target, Reason: reason, AtMS: atMS, ResumeAtMS: resumeAtMS}
	return nil
}

func (s *memPauseStore) DeletePause(target string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, target)
	return nil
}

func (s *memPauseStore) has(target string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.rows[target]
	return ok
}

// waitStopped blocks until no teardown is outstanding for key. A load issued
// during one is refused by design (see loadableLocked), so a test that unloads
// and then loads has to wait exactly as a caller would.
func waitStopped(t *testing.T, mgr *Manager, key string) {
	t.Helper()
	deadline := time.Now().Add(evictGrace + 5*time.Second)
	for time.Now().Before(deadline) {
		mgr.mu.Lock()
		_, stopping := mgr.stopping[key]
		mgr.mu.Unlock()
		if !stopping {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("teardown of %q never completed", key)
}

// TestPauseUnloadsAndRefusesLoad: pausing a resident model evicts it, and every
// later load — explicit or through EnsureReady — is refused until it resumes.
func TestPauseUnloadsAndRefusesLoad(t *testing.T) {
	cfg := resConfig(t, "10", "6", listenTCP(t), listenTCP(t))
	mgr := NewManager(cfg)
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()
	ctx := context.Background()

	if _, err := mgr.LoadModel(ctx, "A"); err != nil {
		t.Fatalf("LoadModel: %v", err)
	}

	res, err := mgr.PauseModel("A", "gpu needed elsewhere", time.Time{})
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if res.Evicted != 1 {
		t.Errorf("evicted %d, want 1 (%+v)", res.Evicted, res)
	}
	if got := mgr.Snapshot(); len(got.Models) != 0 {
		t.Errorf("after pause, still resident: %+v", got.Models)
	}

	// Explicit load is refused...
	if _, err := mgr.LoadModel(ctx, "A"); err == nil {
		t.Error("LoadModel on a paused model should fail")
	}
	// ...and so is the request path, permanently (so a lane 503s rather than
	// telling the caller to retry into an operator decision).
	_, _, _, err = mgr.EnsureReady(ctx, "A", cfg.Models["A"], nil)
	var ce *CapacityError
	if !errors.As(err, &ce) {
		t.Fatalf("EnsureReady err = %v, want *CapacityError", err)
	}
	if !ce.Permanent {
		t.Error("pause refusal must be permanent, not backpressure")
	}
	if !errors.Is(err, ErrNoCapacity) {
		t.Error("pause refusal should unwrap to ErrNoCapacity")
	}

	// Resuming puts it back in service.
	was, err := mgr.UnpauseModel(ctx, "A")
	if err != nil || !was {
		t.Fatalf("Unpause = (%v, %v), want (true, nil)", was, err)
	}
	waitStopped(t, mgr, "A") // the pause's eviction must finish first
	if _, err := mgr.LoadModel(ctx, "A"); err != nil {
		t.Fatalf("LoadModel after resume: %v", err)
	}
}

// TestPauseIgnoresPin: a pinned model is unloadable by pause even though an
// ordinary unload refuses it — otherwise pausing a pinned model is a no-op.
func TestPauseIgnoresPin(t *testing.T) {
	cfg := resConfig(t, "10", "6", listenTCP(t), listenTCP(t))
	m := cfg.Models["A"]
	m.Persistent = true
	cfg.Models["A"] = m
	mgr := NewManager(cfg)
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()

	if _, err := mgr.LoadModel(context.Background(), "A"); err != nil {
		t.Fatalf("LoadModel: %v", err)
	}
	if _, err := mgr.UnloadModel("A"); err == nil {
		t.Fatal("precondition: an ordinary unload of a pinned model must refuse")
	}
	res, err := mgr.PauseModel("A", "", time.Time{})
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if res.Evicted != 1 {
		t.Errorf("evicted %d, want 1 (%+v)", res.Evicted, res)
	}
}

// TestPauseSkipsPreload: a paused pinned model is not warmed at boot. Without
// this, a restart silently reloads the model the operator took out of service.
func TestPauseSkipsPreload(t *testing.T) {
	cfg := resConfig(t, "10", "6", listenTCP(t), listenTCP(t))
	for _, n := range []string{"A", "B"} {
		m := cfg.Models[n]
		m.Persistent = true
		cfg.Models[n] = m
	}
	mgr := NewManager(cfg)
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()

	if _, err := mgr.PauseModel("A", "", time.Time{}); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	mgr.Preload(context.Background())

	snap := mgr.Snapshot()
	if len(snap.Models) != 1 || snap.Models[0].Name != "B" {
		t.Errorf("preloaded %+v, want only B", snap.Models)
	}
}

// TestPauseExpiresOnResumeTime: a timed pause lifts by itself once its resume
// time passes, with no sweeper involved — every read expires it lazily.
func TestPauseExpiresOnResumeTime(t *testing.T) {
	cfg := resConfig(t, "10", "6", listenTCP(t), listenTCP(t))
	mgr := NewManager(cfg)
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()

	if _, err := mgr.PauseModel("A", "brief", time.Now().Add(80*time.Millisecond)); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if !mgr.IsPaused("A") {
		t.Fatal("A should be paused immediately after Pause")
	}
	time.Sleep(150 * time.Millisecond)
	if mgr.IsPaused("A") {
		t.Error("A should have resumed once its resume time passed")
	}
	if len(mgr.Pauses()) != 0 {
		t.Errorf("expired pause still listed: %+v", mgr.Pauses())
	}
}

// TestPauseRejectsPastResumeTime: a resume time that has already gone by is a
// mistake (a mis-set clock, a stale form), not an instantly-expiring pause.
func TestPauseRejectsPastResumeTime(t *testing.T) {
	cfg := resConfig(t, "10", "6", listenTCP(t), listenTCP(t))
	mgr := NewManager(cfg)
	defer mgr.Shutdown()

	if _, err := mgr.PauseModel("A", "", time.Now().Add(-time.Minute)); err == nil {
		t.Error("expected an error for a resume time in the past")
	}
	if mgr.IsPaused("A") {
		t.Error("a rejected pause must not take effect")
	}
}

// TestPauseUnknownModel refuses a name that is not in the config, rather than
// recording a pause nothing will ever consult.
func TestPauseUnknownModel(t *testing.T) {
	mgr := NewManager(resConfig(t, "10", "6", listenTCP(t), listenTCP(t)))
	defer mgr.Shutdown()
	if _, err := mgr.PauseModel("nope", "", time.Time{}); err == nil {
		t.Error("expected an error pausing an unknown model")
	}
}

// TestUnpauseOfUnpausedModel reports "was not paused" instead of erroring — the
// operator got the state they asked for either way.
func TestUnpauseOfUnpausedModel(t *testing.T) {
	mgr := NewManager(resConfig(t, "10", "6", listenTCP(t), listenTCP(t)))
	defer mgr.Shutdown()
	was, err := mgr.UnpauseModel(context.Background(), "A")
	if err != nil {
		t.Fatalf("Unpause: %v", err)
	}
	if was {
		t.Error("was = true for a model that was never paused")
	}
}

// TestPausePersists: a pause is written through to the store, dropped on
// resume, and restored on the next boot.
func TestPausePersists(t *testing.T) {
	cfg := resConfig(t, "10", "6", listenTCP(t), listenTCP(t))
	st := newMemPauseStore()

	mgr := NewManager(cfg)
	mgr.UsePauseStore(st)
	defer mgr.Shutdown()

	if _, err := mgr.PauseModel("A", "maintenance", time.Time{}); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if !st.has("A") {
		t.Fatal("pause was not persisted")
	}

	// A fresh manager over the same store comes up with A still paused.
	restored := NewManager(cfg)
	restored.UsePauseStore(st)
	defer restored.Shutdown()
	p, ok := restored.PauseOf("A")
	if !ok {
		t.Fatal("pause did not survive the restart")
	}
	if p.Reason != "maintenance" {
		t.Errorf("reason = %q, want maintenance", p.Reason)
	}

	if _, err := restored.UnpauseModel(context.Background(), "A"); err != nil {
		t.Fatalf("Unpause: %v", err)
	}
	if st.has("A") {
		t.Error("resume did not delete the persisted pause")
	}
}

// TestRestoreDropsExpiredPause: a pause whose resume time passed while corrallm
// was down is retired at boot, not resurrected.
func TestRestoreDropsExpiredPause(t *testing.T) {
	cfg := resConfig(t, "10", "6", listenTCP(t), listenTCP(t))
	past := time.Now().Add(-time.Hour).UnixMilli()
	st := newMemPauseStore(PersistedPause{Target: "A", AtMS: past, ResumeAtMS: past + 1})

	mgr := NewManager(cfg)
	mgr.UsePauseStore(st)
	defer mgr.Shutdown()

	if mgr.IsPaused("A") {
		t.Error("an expired pause should not be restored")
	}
	if st.has("A") {
		t.Error("the expired row should have been deleted")
	}
}

// TestRestoreSurvivesStoreError: an unreadable pause table starts the manager
// unpaused rather than failing boot.
func TestRestoreSurvivesStoreError(t *testing.T) {
	cfg := resConfig(t, "10", "6", listenTCP(t), listenTCP(t))
	st := newMemPauseStore()
	st.fail = errors.New("disk on fire")

	mgr := NewManager(cfg)
	mgr.UsePauseStore(st)
	defer mgr.Shutdown()

	if mgr.IsPaused("A") {
		t.Error("no pause should be in effect after a failed restore")
	}
	// The store is still attached, so a later pause persists normally.
	st.fail = nil
	if _, err := mgr.PauseModel("A", "", time.Time{}); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if !st.has("A") {
		t.Error("store was detached by the failed load")
	}
}

// TestPauseHostedModelPausesExtension: an extension's models are ONE process,
// so pausing any of them pauses the extension and every sibling — the same
// blast radius an unload of that model already had. Pausing three of four and
// leaving the process up for the fourth would be a pause that frees nothing.
func TestPauseHostedModelPausesExtension(t *testing.T) {
	cfg, mgr := extFixture(t)
	defer mgr.Shutdown()
	ctx := context.Background()

	if _, err := mgr.LoadModel(ctx, "x"); err != nil {
		t.Fatalf("LoadModel x: %v", err)
	}

	res, err := mgr.PauseModel("x", "gpu needed", time.Time{})
	if err != nil {
		t.Fatalf("PauseModel x: %v", err)
	}
	if res.Target != "extension:ext" {
		t.Errorf("target = %q, want extension:ext", res.Target)
	}
	if len(res.Affected) != 2 {
		t.Errorf("affected = %v, want both hosted models", res.Affected)
	}
	if res.Evicted != 1 {
		t.Errorf("evicted = %d, want 1 (the shared process)", res.Evicted)
	}
	if got := mgr.Snapshot(); len(got.Models) != 0 {
		t.Errorf("shared process still up: %+v", got.Models)
	}

	// BOTH models are refused, not just the one named.
	for _, name := range []string{"x", "y"} {
		if !mgr.IsPaused(name) {
			t.Errorf("%s should be paused via its extension", name)
		}
		if _, _, _, err := mgr.EnsureReady(ctx, name, cfg.Models[name], nil); err == nil {
			t.Errorf("%s should be refused", name)
		}
	}

	// And resuming through either name brings the whole extension back.
	if was, err := mgr.UnpauseModel(ctx, "y"); err != nil || !was {
		t.Fatalf("UnpauseModel y = (%v, %v)", was, err)
	}
	if mgr.IsPaused("x") {
		t.Error("x should have resumed with its extension")
	}
}

// TestPauseExtensionDirect: an extension can be paused by its own name, which is
// how the dashboard's extension panel drives it.
func TestPauseExtensionDirect(t *testing.T) {
	cfg, mgr := extFixture(t)
	defer mgr.Shutdown()
	ctx := context.Background()

	if _, err := mgr.LoadExtension(ctx, "ext"); err != nil {
		t.Fatalf("LoadExtension: %v", err)
	}

	res, err := mgr.PauseExtension("ext", "maintenance", time.Time{})
	if err != nil {
		t.Fatalf("PauseExtension: %v", err)
	}
	if res.Evicted != 1 || res.Target != "extension:ext" {
		t.Errorf("result = %+v", res)
	}
	if _, err := mgr.LoadExtension(ctx, "ext"); err == nil {
		t.Error("loading a paused extension should fail")
	}

	// The state the panel reads reports it.
	var seen bool
	for _, st := range mgr.ExtensionStates() {
		if st.Name != "ext" {
			continue
		}
		seen = true
		if !st.Paused || st.PauseReason != "maintenance" {
			t.Errorf("ExtensionState = %+v", st)
		}
	}
	if !seen {
		t.Fatal("ext missing from ExtensionStates")
	}

	if was, err := mgr.UnpauseExtension(ctx, "ext"); err != nil || !was {
		t.Fatalf("UnpauseExtension = (%v, %v)", was, err)
	}
	waitStopped(t, mgr, "extension:ext")
	if _, err := mgr.LoadExtension(ctx, "ext"); err != nil {
		t.Errorf("LoadExtension after resume: %v", err)
	}
	_ = cfg
}

// TestPauseExtensionUnknown refuses a name that is not an extension.
func TestPauseExtensionUnknown(t *testing.T) {
	_, mgr := extFixture(t)
	defer mgr.Shutdown()
	if _, err := mgr.PauseExtension("nope", "", time.Time{}); err == nil {
		t.Error("expected an error pausing an unknown extension")
	}
	if _, err := mgr.UnpauseExtension(context.Background(), "nope"); err == nil {
		t.Error("expected an error resuming an unknown extension")
	}
}

// TestPauseExtensionSkipsPreload: a paused extension is not warmed at boot even
// though it is pinned — which is the case that matters, since oidio is pinned
// and would otherwise reload itself on every restart.
func TestPauseExtensionSkipsPreload(t *testing.T) {
	cfg, mgr := extFixture(t)
	defer mgr.Shutdown()
	ext := cfg.Extensions["ext"]
	ext.Persistent = true
	cfg.Extensions["ext"] = ext
	mgr.SetConfig(cfg)

	if _, err := mgr.PauseExtension("ext", "", time.Time{}); err != nil {
		t.Fatalf("PauseExtension: %v", err)
	}
	mgr.Preload(context.Background())
	if got := mgr.Snapshot(); len(got.Models) != 0 {
		t.Errorf("paused extension was preloaded: %+v", got.Models)
	}
}

// extFixture builds a server with one extension "ext" hosting models x and y in
// a single process.
func extFixture(t *testing.T) (*config.Config, *Manager) {
	t.Helper()
	port := listenTCP(t)
	base := resModel(t, "box", map[string]string{"gpu": "4"}, port)
	cfg := &config.Config{
		Servers:    map[string]config.Server{"box": {Pools: map[string]string{"gpu": "20"}}},
		Extensions: map[string]config.Extension{"ext": {Cmd: "exec sleep 30", Server: "box", RAMUsage: map[string]string{"gpu": "4"}}},
		Models: map[string]config.Model{
			"x": {Extension: "ext", ExtensionHosted: true, Proxy: base.Proxy, Type: "local"},
			"y": {Extension: "ext", ExtensionHosted: true, Proxy: base.Proxy, Type: "local"},
		},
	}
	mgr := NewManager(cfg)
	mgr.healthTimeout = 5 * time.Second
	return cfg, mgr
}

// TestUnpauseRewarmsPinnedModel: resuming a pinned model brings it back without
// anyone asking for it.
//
// This is the whole reason repin exists. Nothing requests a pinned model by name
// — Preload is the only thing that loads one, and it already ran at boot — so
// without this a "pause until 09:00" on a pinned embedder would lift on paper
// and leave the model down until the next restart.
//
// The caller's context is canceled immediately to match how the API op behaves
// (an HTTP request context dies when the response is written). Note this does
// NOT discriminate the WithoutCancel fix in repin: EnsureReady starts its load
// goroutine before it waits, so the spawn completes either way and the fix only
// removes a bogus "context canceled" warning. The assertion here is the
// re-warm itself.
func TestUnpauseRewarmsPinnedModel(t *testing.T) {
	cfg := resConfig(t, "10", "6", listenTCP(t), listenTCP(t))
	m := cfg.Models["A"]
	m.Persistent = true
	cfg.Models["A"] = m
	mgr := NewManager(cfg)
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()

	if _, err := mgr.PauseModel("A", "", time.Time{}); err != nil {
		t.Fatalf("PauseModel: %v", err)
	}
	if got := mgr.Snapshot(); len(got.Models) != 0 {
		t.Fatalf("pause left it resident: %+v", got.Models)
	}

	ctx, cancel := context.WithCancel(context.Background())
	if _, err := mgr.UnpauseModel(ctx, "A"); err != nil {
		t.Fatalf("UnpauseModel: %v", err)
	}
	cancel()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snap := mgr.Snapshot()
		if len(snap.Models) == 1 && snap.Models[0].State == string(StateReady) {
			if snap.Models[0].Refs != 0 {
				t.Errorf("re-warm leaked a residency ref: %+v", snap.Models[0])
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("pinned model was not re-warmed after resume: %+v", mgr.Snapshot().Models)
}

// TestRepinRetriesPastTeardown: a resume issued while the paused process is
// still tearing down must still bring a pinned model back.
//
// Real failure this reproduces: pause force-evicts oidio (pinned), the operator
// resumes a second later, and the respawn collides with the dying process over
// a fixed systemd unit name — "Unit oidio.scope was already loaded" — exits 1,
// so the backend never answers /health and the extension stays down forever,
// because nothing ever requests a pinned process by name. Eviction is
// asynchronous, so this window is reachable whenever a resume follows a pause
// quickly. Note pause is the ONLY way to reach it for a pinned process:
// UnloadModel refuses a pin, and pause deliberately overrides it.
//
// Here the backend's port stays dead for the first attempts and starts
// answering partway through the backoff, standing in for the resource the dying
// process was still holding.
func TestRepinRetriesPastTeardown(t *testing.T) {
	// A port nothing is listening on yet.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	var pn yaml.Node
	if err := pn.Encode(port); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Servers: map[string]config.Server{"box": {Pools: map[string]string{"gpu": "10"}}},
		Models: map[string]config.Model{
			"A": {
				Cmd: "exec sleep 30", Server: "box",
				RAMUsage: map[string]string{"gpu": "6"},
				Proxy:    pn, Type: "local", Persistent: true,
			},
		},
	}
	mgr := NewManager(cfg)
	mgr.healthTimeout = 400 * time.Millisecond // each attempt gives up quickly
	defer mgr.Shutdown()

	if _, err := mgr.PauseModel("A", "", time.Time{}); err != nil {
		t.Fatalf("PauseModel: %v", err)
	}
	if _, err := mgr.UnpauseModel(context.Background(), "A"); err != nil {
		t.Fatalf("UnpauseModel: %v", err)
	}

	// The "old process" lets go partway through the backoff.
	time.Sleep(1500 * time.Millisecond)
	srv := &http.Server{
		Addr:              "127.0.0.1:" + strconv.Itoa(port),
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
		ReadHeaderTimeout: time.Second,
	}
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Close() })

	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		snap := mgr.Snapshot()
		if len(snap.Models) == 1 && snap.Models[0].State == string(StateReady) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("pinned model never recovered after the teardown race: %+v", mgr.Snapshot().Models)
}
