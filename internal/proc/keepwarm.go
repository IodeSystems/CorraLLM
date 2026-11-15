package proc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
)

// sticky.keepWarm: resident but kickable (config.Sticky.KeepWarm).
//
// A keepWarm model is an ordinary eviction victim, and an operator can unload
// or restart it. What differs is the way back: when it is down, the sweeper
// starts it again as soon as it fits in the FREE capacity of its server. It
// never evicts for that — a respawn that displaced something would only trade
// one cold load for another, and two keepWarm models sharing a card would
// evict each other forever.
//
// Four things hold a respawn back:
//
//   - an explicit unload (suppressed, until a request loads it or the config
//     reloads): the operator said "down", and coming straight back would make
//     the order a no-op;
//   - a P80 restart or a teardown in progress: the restart loads it itself;
//   - crash backoff: a model that fails to start or dies is retried at
//     keepWarmBackoffMin, doubling to keepWarmBackoffMax, and the count resets
//     once it has stayed up keepWarmHealthy;
//   - keepWarmAfterEvict after a capacity eviction: whatever evicted it is
//     usually still loading or serving, and a respawn into the gap that closes
//     a moment later is evicted again on the next request.

const (
	keepWarmBackoffMin = 10 * time.Second
	keepWarmBackoffMax = 10 * time.Minute
	keepWarmHealthy    = 10 * time.Minute
	keepWarmAfterEvict = 30 * time.Second
)

// warmState is the respawn bookkeeping of one keepWarm process key. Guarded
// by Manager.mu.
type warmState struct {
	suppressed bool      // explicit unload: no respawn until a request load or a reload
	inflight   bool      // a respawn's EnsureReady is running
	failures   int       // consecutive failed starts and crashes
	nextTry    time.Time // backoff: no respawn before this
	evictedAt  time.Time // last capacity eviction
	why        string    // why it is down, for the log line
}

type keepWarmKey struct{}

// withKeepWarm marks a load as a keepWarm respawn: EnsureReady then refuses
// instead of evicting when it does not fit. reason is logged.
func withKeepWarm(ctx context.Context, reason string) context.Context {
	return context.WithValue(ctx, keepWarmKey{}, reason)
}

// keepWarmFrom is the respawn reason, or "" for every other load.
func keepWarmFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	s, _ := ctx.Value(keepWarmKey{}).(string)
	return s
}

// keepWarmBackoff is the wait after the n-th consecutive failure (n ≥ 1).
func keepWarmBackoff(n int) time.Duration {
	d := keepWarmBackoffMin
	for i := 1; i < n && d < keepWarmBackoffMax; i++ {
		d *= 2
	}
	return min(d, keepWarmBackoffMax)
}

// warmLocked returns key's state, creating it. Caller holds m.mu.
func (m *Manager) warmLocked(key string) *warmState {
	if m.warm == nil {
		m.warm = map[string]*warmState{}
	}
	ws := m.warm[key]
	if ws == nil {
		ws = &warmState{}
		m.warm[key] = ws
	}
	return ws
}

// nudgeKeepWarm asks the sweeper for a respawn pass now: something exited or
// was evicted, so free capacity may have changed. Never blocks.
func (m *Manager) nudgeKeepWarm() {
	select {
	case m.warmNudge <- struct{}{}:
	default:
	}
}

// keepWarmFailed records a failed start or a crash and sets the backoff.
func (m *Manager) keepWarmFailed(key, why string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keepWarmFailedLocked(key, why, time.Now())
}

func (m *Manager) keepWarmFailedLocked(key, why string, now time.Time) {
	ws := m.warmLocked(key)
	ws.failures++
	ws.why = why
	wait := keepWarmBackoff(ws.failures)
	ws.nextTry = now.Add(wait)
	slog.Warn("keepWarm backoff", "key", key, "reason", why, "failures", ws.failures, "retryIn", wait)
}

// keepWarmPass starts every keepWarm model that is down and fits without
// eviction. Each start runs in its own goroutine (a cold load takes minutes);
// the returned channel closes when all the starts this pass launched have
// resolved, which is what the tests wait on.
func (m *Manager) keepWarmPass(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	cfg := m.config()
	if m.draining.Load() || cfg == nil {
		close(done)
		return done
	}
	names := make([]string, 0, len(cfg.Models))
	for name := range cfg.Models {
		names = append(names, name)
	}
	sort.Strings(names) // an extension's models share a key: the first name starts it

	type start struct {
		name, key, why string
	}
	var starts []start
	seen := map[string]bool{}
	now := time.Now()
	for _, name := range names {
		mdl, ok := cfg.Effective(name)
		if !ok || mdl.Disabled || mdl.Cmd == "" || mdl.Sticky == nil || !mdl.Sticky.KeepWarm {
			continue
		}
		key := mdl.ProcKey(name)
		if seen[key] {
			continue
		}
		seen[key] = true
		if m.IsPaused(name) {
			continue
		}
		m.mu.Lock()
		ws := m.warmLocked(key)
		if p := m.residentLocked(name, key); p != nil {
			p.mu.Lock()
			healthy := p.state == StateReady && now.Sub(p.readyAt) >= keepWarmHealthy
			p.mu.Unlock()
			if healthy && ws.failures > 0 {
				ws.failures, ws.nextTry = 0, time.Time{}
			}
			m.mu.Unlock()
			continue
		}
		_, tearingDown := m.stopping[key]
		hold := tearingDown || m.restarting[key] || ws.suppressed || ws.inflight ||
			now.Before(ws.nextTry) || now.Sub(ws.evictedAt) < keepWarmAfterEvict
		if !hold {
			ws.inflight = true
			why := ws.why
			if why == "" {
				why = "down"
			}
			starts = append(starts, start{name, key, why})
		}
		m.mu.Unlock()
	}
	if len(starts) == 0 {
		close(done)
		return done
	}
	pending := make(chan struct{}, len(starts))
	for _, s := range starts {
		go func() {
			defer func() { pending <- struct{}{} }()
			mdl, _ := cfg.Effective(s.name)
			_, release, _, err := m.EnsureReady(withKeepWarm(ctx, s.why), s.name, mdl, nil, nil)
			m.mu.Lock()
			defer m.mu.Unlock()
			ws := m.warmLocked(s.key)
			ws.inflight = false
			var ce *CapacityError
			switch {
			case err == nil:
				release()
				ws.why = ""
			case errors.As(err, &ce), errors.Is(err, context.Canceled), errors.Is(err, ErrDraining):
				// Not a failure of the model: no room, paused, or shutting
				// down. The next pass looks again.
				slog.Debug("keepWarm respawn deferred", "model", s.name, "err", err)
			default:
				m.keepWarmFailedLocked(s.key, "start-failed", time.Now())
			}
		}()
	}
	go func() {
		for range starts {
			<-pending
		}
		close(done)
	}()
	return done
}

// residentLocked is the process serving name, under its plain key or any of
// its placement keys ("<name>@<placement>"). Caller holds m.mu.
func (m *Manager) residentLocked(name, key string) *Process {
	if p := m.procs[key]; p != nil {
		return p
	}
	for k, p := range m.procs {
		if p.ModelName == name || strings.HasPrefix(k, name+"@") {
			return p
		}
	}
	return nil
}

// keepWarmRefuseLocked is the no-eviction rule for a respawn: nil when usage
// fits in what is free now. Caller holds m.mu.
func (m *Manager) keepWarmRefuseLocked(name, server string, usage map[string]int64) error {
	m.expireLeasesLocked(time.Now())
	if m.fitsLocked(server, usage, nil) {
		return nil
	}
	return &CapacityError{Reason: fmt.Sprintf("keepWarm: %s does not fit in free capacity on %s; a respawn never evicts", name, server)}
}
