package proc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"
)

// Pool leases (P78 §7).
//
// A lease is a residency entry with usage and no process: an extension's own
// process (the colmap runner) borrows pool memory for work it runs outside
// any request — a COLMAP GPU stage that lasts hours — and promises to give it
// back when asked. Without it that memory is either invisible to the ledger
// (gpu1 over-committed) or held as an in-flight request (never a victim, so
// the guard 429s for hours).
//
// Lifecycle:
//
//	grant    AcquireLease: same placement as a cold load — idle residents may
//	         be evicted for it — but never at another lease's expense.
//	yield    makeRoomLocked, after idle victims are not enough, frees a
//	         yieldable lease past leaseProtect: state yield_requested, usage
//	         back in the ledger now, the incoming load waits on lease.gone.
//	release  ReleaseLease (the holder's DELETE) ends it and closes gone.
//	ignored  yieldGrace after a yield request with no release, corrallm stops
//	         the extension's process group (D7); gone closes once it is gone.
//	expired  no LeaseStatus poll for leaseTTL: released with a WARN.
//
// Leases are memory-only. After a corrallm restart the holder's poll gets
// ErrLeaseNotFound and it re-registers with Held set.

// defaultLeaseTTL is how long a lease lives without a status poll.
const defaultLeaseTTL = 90 * time.Second

// maxLeaseWait caps a status long-poll.
const maxLeaseWait = 60 * time.Second

// Lease states on the wire.
const (
	LeaseHeld           = "held"
	LeaseYieldRequested = "yield_requested"
)

var (
	// ErrLeaseNotFound: no such lease (expired, released, or corrallm restarted).
	ErrLeaseNotFound = errors.New("lease not found")
	// ErrLeaseConflict: a Held re-registration does not fit; the holder must
	// yield at once.
	ErrLeaseConflict = errors.New("lease does not fit: yield now")
)

// LeaseRequestError is a malformed lease request (400).
type LeaseRequestError struct{ Msg string }

func (e *LeaseRequestError) Error() string { return e.Msg }

// LeaseRequest asks for a lease. Usage is in bytes per pool of the
// extension's server.
type LeaseRequest struct {
	Extension string
	Job       string
	Stage     string
	Owner     string
	Usage     map[string]int64
	Yieldable bool
	// Held re-registers a lease whose memory is already in use (after a
	// corrallm restart): granted only if it fits counting that memory once.
	Held bool
}

// LeaseGrant is a granted lease.
type LeaseGrant struct {
	ID             string
	GrantedAt      time.Time
	ProtectedUntil time.Time
}

// LeaseStatus is a lease's state for its holder.
type LeaseStatus struct {
	ID       string
	State    string
	WantedBy string    // set with yield_requested: who needs the memory
	Deadline time.Time // set with yield_requested: release before this
}

// LeaseView is a lease for read surfaces (Snapshot).
type LeaseView struct {
	ID               string
	Name             string // lease:<extension>/<job>/<stage>
	Extension        string
	Job              string
	Stage            string
	Owner            string
	Server           string
	State            string
	Yieldable        bool
	Usage            []PoolUsage
	GrantedAtMS      int64
	ProtectedUntilMS int64
	LastSeenMS       int64
	DeadlineMS       int64 // 0 unless yield_requested
	WantedBy         string
}

// Lease is one held lease. Every field is guarded by Manager.mu.
type Lease struct {
	ID, Extension, Job, Stage, Owner string

	server    string
	usage     map[string]int64
	yieldable bool
	grantedAt time.Time
	lastSeen  time.Time

	yieldAt  time.Time // zero while held
	wantedBy string
	deadline time.Time

	// freed: usage is back in the ledger (a yield request, or the end).
	freed bool
	ended bool
	// changed is closed and replaced on every state change, waking polls.
	changed chan struct{}
	// gone closes when the lease is over AND its memory is free: on release,
	// or once the stopped extension's group has exited.
	gone chan struct{}
}

func (l *Lease) name() string {
	return "lease:" + l.Extension + "/" + l.Job + "/" + l.Stage
}

func (l *Lease) state() string {
	if l.yieldAt.IsZero() {
		return LeaseHeld
	}
	return LeaseYieldRequested
}

func newLeaseID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "lease_" + hex.EncodeToString(b[:])
}

// AcquireLease grants a lease, evicting idle residents if needed, and returns
// once their memory is free. A *CapacityError means it does not fit now
// (transient → 429) or ever (Permanent → 503).
func (m *Manager) AcquireLease(ctx context.Context, req LeaseRequest) (LeaseGrant, error) {
	cfg := m.config()
	ext, ok := cfg.Extensions[req.Extension]
	if !ok || ext.Lease == nil {
		return LeaseGrant{}, &LeaseRequestError{fmt.Sprintf("extension %q takes no leases", req.Extension)}
	}
	if len(req.Usage) == 0 {
		return LeaseGrant{}, &LeaseRequestError{"usage names no pool"}
	}
	caps := ext.Lease.MaxBytes()
	for pool, b := range req.Usage {
		lim, ok := caps[pool]
		switch {
		case !ok:
			return LeaseGrant{}, &LeaseRequestError{fmt.Sprintf("pool %q cannot be leased (lease.max names %v)", pool, sortedKeys(caps))}
		case b <= 0:
			return LeaseGrant{}, &LeaseRequestError{fmt.Sprintf("usage.%s must be positive", pool)}
		case b > lim:
			return LeaseGrant{}, &LeaseRequestError{fmt.Sprintf("usage.%s %d bytes is over lease.max %d", pool, b, lim)}
		}
	}
	usage := make(map[string]int64, len(req.Usage))
	for k, v := range req.Usage {
		usage[k] = v
	}
	l := &Lease{
		ID: newLeaseID(), Extension: req.Extension, Job: req.Job, Stage: req.Stage, Owner: req.Owner,
		server: ext.Server, usage: usage, yieldable: req.Yieldable,
		changed: make(chan struct{}), gone: make(chan struct{}),
	}

	m.mu.Lock()
	if m.draining.Load() {
		m.mu.Unlock()
		return LeaseGrant{}, ErrDraining
	}
	now := time.Now()
	m.expireLeasesLocked(now)
	var gone []chan struct{}
	var evicted []string
	if req.Held {
		if !m.fitsHeldLocked(l.server, usage) {
			m.mu.Unlock()
			return LeaseGrant{}, ErrLeaseConflict
		}
	} else {
		ev, g, err := m.makeRoomLocked(l.server, usage, l.name(), false)
		if err != nil {
			m.mu.Unlock()
			return LeaseGrant{}, err
		}
		evicted, gone = ev, g
	}
	m.reserveLocked(l.server, usage)
	l.grantedAt, l.lastSeen = now, now
	m.leases[l.ID] = l
	m.recordEvent(BackendEvent{Model: l.name(), Server: l.server, Instance: l.ID, Kind: EventStart, Reason: "lease-grant", OK: true})
	m.mu.Unlock()
	slog.Info("lease granted", "id", l.ID, "lease", l.name(), "owner", l.Owner, "usage", usage,
		"yieldable", l.yieldable, "held", req.Held, "evicted", evicted)

	// Granted only once what was evicted for it is gone: the holder starts a
	// GPU child the moment this returns.
	for _, ch := range gone {
		select {
		case <-ch:
		case <-ctx.Done():
			_ = m.ReleaseLease(l.ID)
			return LeaseGrant{}, ctx.Err()
		}
	}
	return LeaseGrant{ID: l.ID, GrantedAt: now, ProtectedUntil: now.Add(m.leaseProtect)}, nil
}

// fitsHeldLocked is fitsLocked for memory that is already in use: the
// agent's reading counts it as foreign, so it is not charged a second time.
func (m *Manager) fitsHeldLocked(server string, usage map[string]int64) bool {
	for pool, want := range usage {
		foreign := m.foreignUsedLocked(server, pool) - want
		if foreign < 0 {
			foreign = 0
		}
		if want > m.budget[server][pool]-m.used[server][pool]-foreign {
			return false
		}
	}
	return true
}

// ReleaseLease ends a lease (the holder's DELETE).
func (m *Manager) ReleaseLease(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.leases[id]
	if !ok {
		return ErrLeaseNotFound
	}
	m.endLeaseLocked(l, "released", nil)
	return nil
}

// LeaseStatus long-polls a lease: it returns at once when a yield is
// requested, else after wait (capped at maxLeaseWait) with state held. Each
// call counts as the holder being alive.
func (m *Manager) LeaseStatus(ctx context.Context, id string, wait time.Duration) (LeaseStatus, error) {
	if wait > maxLeaseWait {
		wait = maxLeaseWait
	}
	m.mu.Lock()
	m.expireLeasesLocked(time.Now())
	l, ok := m.leases[id]
	if !ok {
		m.mu.Unlock()
		return LeaseStatus{}, ErrLeaseNotFound
	}
	l.lastSeen = time.Now()
	st, changed := l.statusLocked(), l.changed
	m.mu.Unlock()
	if st.State != LeaseHeld || wait <= 0 {
		return st, nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-changed:
	case <-t.C:
	case <-ctx.Done():
		return LeaseStatus{}, ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.leases[id] != l {
		return LeaseStatus{}, ErrLeaseNotFound
	}
	l.lastSeen = time.Now()
	return l.statusLocked(), nil
}

func (l *Lease) statusLocked() LeaseStatus {
	st := LeaseStatus{ID: l.ID, State: l.state()}
	if !l.yieldAt.IsZero() {
		st.WantedBy, st.Deadline = l.wantedBy, l.deadline
	}
	return st
}

// LeaseExtension names the extension a lease belongs to, so the handler can
// refuse one extension's token acting on another's lease.
func (m *Manager) LeaseExtension(id string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.leases[id]
	if !ok {
		return "", false
	}
	return l.Extension, true
}

// leasesTouchingLocked lists the unfreed leases on server that hold any pool
// usage needs. Caller holds m.mu.
func (m *Manager) leasesTouchingLocked(server string, usage map[string]int64) []*Lease {
	var out []*Lease
	for _, l := range m.leases {
		if l.server == server && !l.freed && touchesAny(l.usage, usage) {
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].grantedAt.Before(out[j].grantedAt) })
	return out
}

// yieldForLocked is makeRoomLocked's last pass: idle victims alone did not
// free enough, so it asks yieldable leases (past leaseProtect) to yield,
// oldest first, adding idle victims on top only if the leases alone are not
// enough. All-or-nothing like the eviction pass: nothing is asked or evicted
// unless the incoming then fits. Caller holds m.mu.
//
// Leases go first because a yield is cheap — the holder resumes from disk —
// while an evicted model costs a cold load the next time it is wanted.
func (m *Manager) yieldForLocked(server string, usage map[string]int64, incoming string, idle []*Process, now time.Time) (names []string, gone []chan struct{}, ok bool) {
	var cands []*Lease
	for _, l := range m.leasesTouchingLocked(server, usage) {
		if l.yieldable && now.Sub(l.grantedAt) >= m.leaseProtect {
			cands = append(cands, l)
		}
	}
	if len(cands) == 0 {
		return nil, nil, false
	}
	var chosen []*Lease
	victims := map[string]*Process{}
	fits := false
	for _, l := range cands {
		chosen = append(chosen, l)
		if m.fitsExceptLocked(server, usage, victims, chosen) {
			fits = true
			break
		}
	}
	if !fits {
		for i, v := range idle {
			victims[v.Name] = v
			if m.fitsExceptLocked(server, usage, victims, chosen) {
				m.pruneVictimsLocked(server, usage, idle[:i+1], victims, chosen)
				fits = true
				break
			}
		}
	}
	if !fits {
		return nil, nil, false
	}
	for _, v := range victims {
		v.setStopReason("evicted-for:" + incoming)
		m.evictLocked(v)
		names = append(names, v.Name)
		if ch, ok := m.stopping[v.key]; ok {
			gone = append(gone, ch)
		}
	}
	for _, l := range chosen {
		m.requestYieldLocked(l, incoming, now)
		names = append(names, l.name())
		gone = append(gone, l.gone)
	}
	sort.Strings(names)
	slog.Info("lease yield requested for capacity", "server", server, "for", incoming, "displaced", names)
	return names, gone, true
}

// requestYieldLocked marks l yield_requested, returns its memory to the
// ledger for the incoming load, and arms the D7 timer. Caller holds m.mu.
func (m *Manager) requestYieldLocked(l *Lease, wantedBy string, now time.Time) {
	grace := m.config().Extensions[l.Extension].Lease.YieldGraceDuration()
	l.yieldAt, l.wantedBy, l.deadline = now, wantedBy, now.Add(grace)
	m.freeLocked(l.server, l.usage)
	l.freed = true
	l.notifyLocked()
	time.AfterFunc(grace, func() { m.yieldIgnored(l) })
}

// yieldIgnored is D7: the holder did not release within yieldGrace, so the
// extension's process group — which holds the GPU child — is stopped. Jobs
// resume from disk when it restarts. Every lease of that extension ends with
// it; their gone channels close once the group has exited.
func (m *Manager) yieldIgnored(l *Lease) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.leases[l.ID] != l {
		return // released in time
	}
	key := "extension:" + l.Extension
	var after chan struct{}
	if p := m.procs[key]; p != nil {
		slog.Warn("lease yield ignored past yieldGrace: stopping the extension", "lease", l.name(), "id", l.ID,
			"extension", l.Extension, "wantedBy", l.wantedBy)
		p.setStopReason("lease-yield-ignored:" + l.ID)
		m.evictLocked(p)
		after = m.stopping[key]
	} else {
		slog.Warn("lease yield ignored past yieldGrace, and the extension has no process here to stop",
			"lease", l.name(), "id", l.ID, "extension", l.Extension)
	}
	for _, o := range m.leases {
		if o.Extension == l.Extension {
			m.endLeaseLocked(o, "yield-ignored", after)
		}
	}
}

// expireLeasesLocked releases leases nobody has polled for leaseTTL. Caller
// holds m.mu.
func (m *Manager) expireLeasesLocked(now time.Time) {
	for _, l := range m.leases {
		if now.Sub(l.lastSeen) > m.leaseTTL {
			slog.Warn("lease expired: no status poll", "lease", l.name(), "id", l.ID,
				"idle", now.Sub(l.lastSeen).Round(time.Second), "ttl", m.leaseTTL)
			m.endLeaseLocked(l, "expired", nil)
		}
	}
}

// endLeaseLocked removes l, frees what it still holds, wakes its pollers and
// closes gone — at once, or once after closes. Caller holds m.mu.
func (m *Manager) endLeaseLocked(l *Lease, reason string, after chan struct{}) {
	if l.ended {
		return
	}
	delete(m.leases, l.ID)
	if !l.freed {
		m.freeLocked(l.server, l.usage)
		l.freed = true
	}
	l.ended = true
	l.notifyLocked()
	held := time.Since(l.grantedAt).Round(time.Second)
	m.recordEvent(BackendEvent{Model: l.name(), Server: l.server, Instance: l.ID, Kind: EventStop,
		Reason: fmt.Sprintf("lease-%s after %s", reason, held), OK: true})
	slog.Info("lease ended", "id", l.ID, "lease", l.name(), "owner", l.Owner, "reason", reason, "held", held)
	if after == nil {
		close(l.gone)
		return
	}
	go func() {
		<-after
		close(l.gone)
	}()
}

func (l *Lease) notifyLocked() {
	close(l.changed)
	l.changed = make(chan struct{})
}

// leaseViewsLocked lists leases for Snapshot. Caller holds m.mu.
func (m *Manager) leaseViewsLocked() []LeaseView {
	out := make([]LeaseView, 0, len(m.leases))
	for _, l := range m.leases {
		v := LeaseView{
			ID: l.ID, Name: l.name(), Extension: l.Extension, Job: l.Job, Stage: l.Stage, Owner: l.Owner,
			Server: l.server, State: l.state(), Yieldable: l.yieldable, WantedBy: l.wantedBy,
			GrantedAtMS: l.grantedAt.UnixMilli(), ProtectedUntilMS: l.grantedAt.Add(m.leaseProtect).UnixMilli(),
			LastSeenMS: l.lastSeen.UnixMilli(),
		}
		if !l.deadline.IsZero() {
			v.DeadlineMS = l.deadline.UnixMilli()
		}
		for pool, b := range l.usage {
			v.Usage = append(v.Usage, PoolUsage{Pool: pool, Bytes: b})
		}
		sort.Slice(v.Usage, func(i, j int) bool { return v.Usage[i].Pool < v.Usage[j].Pool })
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func sortedKeys(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
