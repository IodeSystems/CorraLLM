package proc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

// leaseConfig: gpu1 holds 10 bytes; the guard needs 6, a lease up to 8, so a
// 6-byte lease and the guard never co-fit. colmap is a hosted extension whose
// process is `sleep` (the group D7 stops).
func leaseConfig(t *testing.T, grace string) *config.Config {
	t.Helper()
	body := fmt.Sprintf(`
servers:
  box:
    pools: { gpu1: "10", system: "100" }
models:
  guard: { cmd: "exec sleep 30", server: box, proxy: %d, ramUsage: { gpu1: "6" }, type: chat }
  other: { cmd: "exec sleep 30", server: box, proxy: %d, ramUsage: { gpu1: "3" }, type: chat }
extensions:
  colmap:
    cmd: "exec sleep 30"
    server: box
    proxy: %d
    ramUsage: { system: "1" }
    lease: { token: t, max: { gpu1: "8", system: "50" }, yieldGrace: %s }
    provides:
      sfm: { type: reconstruction }
`, listenTCP(t), listenTCP(t), listenTCP(t), grace)
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func leaseMgr(t *testing.T, grace string) (*Manager, *config.Config) {
	t.Helper()
	cfg := leaseConfig(t, grace)
	m := NewManager(cfg)
	m.healthTimeout = 5 * time.Second
	m.activeUse = 0
	m.leaseProtect = 0
	t.Cleanup(m.Shutdown)
	return m, cfg
}

func gpuLease(n int64, yieldable bool) LeaseRequest {
	return LeaseRequest{Extension: "colmap", Job: "recon_1", Stage: "match", Owner: "own",
		Usage: map[string]int64{"gpu1": n}, Yieldable: yieldable}
}

func used(m *Manager, pool string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.used["box"][pool]
}

func TestLeaseGrantFitsAndShowsInSnapshot(t *testing.T) {
	m, _ := leaseMgr(t, "60s")
	g, err := m.AcquireLease(context.Background(), gpuLease(6, true))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(g.ID, "lease_") {
		t.Errorf("id = %q", g.ID)
	}
	if u := used(m, "gpu1"); u != 6 {
		t.Errorf("gpu1 used = %d, want 6", u)
	}
	snap := m.Snapshot()
	if len(snap.Leases) != 1 || snap.Leases[0].Name != "lease:colmap/recon_1/match" || snap.Leases[0].State != LeaseHeld {
		t.Fatalf("snapshot leases = %+v", snap.Leases)
	}
	if err := m.ReleaseLease(g.ID); err != nil {
		t.Fatal(err)
	}
	if u := used(m, "gpu1"); u != 0 {
		t.Errorf("gpu1 used after release = %d", u)
	}
	if err := m.ReleaseLease(g.ID); !errors.Is(err, ErrLeaseNotFound) {
		t.Errorf("second release = %v, want not found", err)
	}
}

func TestLeaseGrantEvictsIdleModel(t *testing.T) {
	m, cfg := leaseMgr(t, "60s")
	ctx := context.Background()
	pg, done, _, err := m.EnsureReady(ctx, "guard", cfg.Models["guard"], nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	done()
	h := pg.handleOf()
	// The holder starts its GPU child as soon as the grant returns, so the
	// grant must wait for the evicted group to be gone.
	if _, err := m.AcquireLease(ctx, gpuLease(6, true)); err != nil {
		t.Fatalf("lease should evict the idle guard: %v", err)
	}
	if h.Alive() {
		t.Error("lease granted while the evicted guard's group is still alive")
	}
	m.mu.Lock()
	_, resident := m.procs["guard"]
	m.mu.Unlock()
	if resident {
		t.Error("guard still resident")
	}
}

// A model inside activeUse is not a victim: the grant is a transient miss
// with a Retry-After, which the handler turns into 429.
func TestLeaseRefusedWhileModelActive(t *testing.T) {
	m, cfg := leaseMgr(t, "60s")
	m.activeUse = 30 * time.Second
	ctx := context.Background()
	_, done, _, err := m.EnsureReady(ctx, "guard", cfg.Models["guard"], nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	done()
	_, err = m.AcquireLease(ctx, gpuLease(6, true))
	var ce *CapacityError
	if !errors.As(err, &ce) || ce.Permanent || ce.RetryAfter <= 0 || ce.RetryAfter > 30*time.Second {
		t.Fatalf("err = %v, want transient CapacityError with RetryAfter (0,30s]", err)
	}
}

func TestLeaseRequestValidation(t *testing.T) {
	m, _ := leaseMgr(t, "60s")
	ctx := context.Background()
	for _, req := range []LeaseRequest{
		gpuLease(9, true), // over lease.max 8
		{Extension: "colmap", Usage: map[string]int64{"gpu7": 1}},
		{Extension: "colmap", Usage: map[string]int64{"gpu1": 0}},
		{Extension: "colmap"},
		{Extension: "nope", Usage: map[string]int64{"gpu1": 1}},
	} {
		var le *LeaseRequestError
		if _, err := m.AcquireLease(ctx, req); !errors.As(err, &le) {
			t.Errorf("%+v: err = %v, want LeaseRequestError", req, err)
		}
	}
	if _, err := m.AcquireLease(ctx, LeaseRequest{Extension: "colmap", Usage: map[string]int64{"gpu1": 11}}); err == nil {
		t.Error("11 > max accepted")
	}
}

// The central case (P78 §7 sequence): a guard load blocked only by a lease
// asks it to yield, the holder sees it at once, and the guard spawns after
// the release — no 429.
func TestLeaseYieldLetsGuardLoad(t *testing.T) {
	m, cfg := leaseMgr(t, "60s")
	ctx := context.Background()
	g, err := m.AcquireLease(ctx, gpuLease(6, true))
	if err != nil {
		t.Fatal(err)
	}
	polled := make(chan LeaseStatus, 1)
	go func() {
		st, err := m.LeaseStatus(ctx, g.ID, 10*time.Second)
		if err != nil {
			t.Error(err)
		}
		polled <- st
	}()
	time.Sleep(50 * time.Millisecond) // the poll is parked

	loaded := make(chan error, 1)
	go func() {
		_, done, _, err := m.EnsureReady(ctx, "guard", cfg.Models["guard"], nil, nil)
		if err == nil {
			done()
		}
		loaded <- err
	}()
	start := time.Now()
	select {
	case st := <-polled:
		if st.State != LeaseYieldRequested || st.WantedBy != "guard" || st.Deadline.IsZero() {
			t.Fatalf("status = %+v", st)
		}
		if d := time.Since(start); d > 100*time.Millisecond {
			t.Errorf("yield seen after %s, want < 100ms", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("poll did not see the yield request")
	}
	// The guard waits for the release: its memory is not free yet.
	select {
	case err := <-loaded:
		t.Fatalf("guard finished before the lease was released: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	// The yielded lease's usage is already off the ledger; the guard's is on.
	if u := used(m, "gpu1"); u != 6 {
		t.Errorf("gpu1 used during yield = %d, want 6 (guard only)", u)
	}
	if err := m.ReleaseLease(g.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-loaded:
		if err != nil {
			t.Fatalf("guard after release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("guard did not load after the release")
	}
	// The holder comes back for gpu1 while the guard is resident and in
	// activeUse: a transient refusal, not a yield of the guard.
	m.activeUse = 30 * time.Second
	_, err = m.AcquireLease(ctx, gpuLease(6, true))
	var ce *CapacityError
	if !errors.As(err, &ce) || ce.Permanent {
		t.Fatalf("re-acquire = %v, want transient CapacityError", err)
	}
}

// A fresh lease is protected: the guard gets a transient miss whose
// Retry-After is the protection's remainder, and the lease is not asked.
func TestLeaseProtectedIsRetryAfter(t *testing.T) {
	m, cfg := leaseMgr(t, "60s")
	m.leaseProtect = 20 * time.Second
	ctx := context.Background()
	g, err := m.AcquireLease(ctx, gpuLease(6, true))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = m.EnsureReady(ctx, "guard", cfg.Models["guard"], nil, nil)
	var ce *CapacityError
	if !errors.As(err, &ce) || ce.Permanent {
		t.Fatalf("err = %v, want transient CapacityError", err)
	}
	if ce.RetryAfter <= 15*time.Second || ce.RetryAfter > 20*time.Second {
		t.Errorf("RetryAfter = %s, want the protection's remainder (~20s)", ce.RetryAfter)
	}
	if len(ce.Blocking) != 1 || ce.Blocking[0] != "lease:colmap/recon_1/match" {
		t.Errorf("Blocking = %v", ce.Blocking)
	}
	if st, _ := m.LeaseStatus(ctx, g.ID, 0); st.State != LeaseHeld {
		t.Errorf("protected lease state = %s", st.State)
	}
}

// A non-yieldable lease (a CPU stage's RAM) is never asked; the miss is
// transient with no predictable Retry-After.
func TestLeaseNonYieldableNeverYields(t *testing.T) {
	m, cfg := leaseMgr(t, "60s")
	ctx := context.Background()
	g, err := m.AcquireLease(ctx, gpuLease(6, false))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = m.EnsureReady(ctx, "guard", cfg.Models["guard"], nil, nil)
	var ce *CapacityError
	if !errors.As(err, &ce) || ce.Permanent || ce.RetryAfter != 0 {
		t.Fatalf("err = %v, want transient CapacityError with no RetryAfter", err)
	}
	if st, _ := m.LeaseStatus(ctx, g.ID, 0); st.State != LeaseHeld {
		t.Errorf("state = %s, want held", st.State)
	}
}

// All-or-nothing: when yielding the lease would still not fit because an
// ACTIVE model holds the rest, nothing is asked and the miss carries the
// active model's Retry-After.
func TestLeaseYieldAllOrNothing(t *testing.T) {
	m, cfg := leaseMgr(t, "60s")
	m.activeUse = 30 * time.Second
	ctx := context.Background()
	_, done, _, err := m.EnsureReady(ctx, "other", cfg.Models["other"], nil, nil) // 3, active
	if err != nil {
		t.Fatal(err)
	}
	done()
	g, err := m.AcquireLease(ctx, gpuLease(5, true)) // 3+5 = 8 ≤ 10
	if err != nil {
		t.Fatal(err)
	}
	// guard 6: yielding the lease leaves 3+6 = 9 ≤ 10 — fits. Make it not:
	// a second lease of 2 (non-yieldable) → 3+2+6 = 11.
	if _, err := m.AcquireLease(ctx, gpuLease(2, false)); err != nil {
		t.Fatal(err)
	}
	_, _, _, err = m.EnsureReady(ctx, "guard", cfg.Models["guard"], nil, nil)
	var ce *CapacityError
	if !errors.As(err, &ce) || ce.Permanent || ce.RetryAfter <= 0 {
		t.Fatalf("err = %v, want transient with the active model's RetryAfter", err)
	}
	if st, _ := m.LeaseStatus(ctx, g.ID, 0); st.State != LeaseHeld {
		t.Errorf("lease asked to yield although the guard could not fit: %s", st.State)
	}
}

// D7: a holder that ignores the yield past yieldGrace has its extension's
// process group stopped; the waiting load then proceeds (it is not refused).
func TestLeaseYieldIgnoredStopsExtension(t *testing.T) {
	m, cfg := leaseMgr(t, "300ms")
	ctx := context.Background()
	if _, err := m.LoadExtension(ctx, "colmap"); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	ext := m.procs["extension:colmap"]
	m.mu.Unlock()
	if ext == nil {
		t.Fatal("extension not resident")
	}
	h := ext.handleOf()
	if _, err := m.AcquireLease(ctx, gpuLease(6, true)); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, done, _, err := m.EnsureReady(ctx, "guard", cfg.Models["guard"], nil, nil)
	if err != nil {
		t.Fatalf("guard after an ignored yield: %v", err)
	}
	done()
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Errorf("guard loaded after %s, before yieldGrace", d)
	}
	if h.Alive() {
		t.Error("extension group still alive after the ignored yield")
	}
	snap := m.Snapshot()
	if len(snap.Leases) != 0 {
		t.Errorf("leases after D7 = %+v", snap.Leases)
	}
	for _, rm := range snap.Models {
		if rm.ProcKey == "extension:colmap" {
			t.Error("extension still in the ledger")
		}
	}
}

func TestLeaseExpiresWithoutPoll(t *testing.T) {
	m, _ := leaseMgr(t, "60s")
	m.leaseTTL = 50 * time.Millisecond
	ctx := context.Background()
	g, err := m.AcquireLease(ctx, gpuLease(6, true))
	if err != nil {
		t.Fatal(err)
	}
	// A poll keeps it alive past one TTL.
	time.Sleep(30 * time.Millisecond)
	if _, err := m.LeaseStatus(ctx, g.ID, 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if _, err := m.LeaseStatus(ctx, g.ID, 0); err != nil {
		t.Fatalf("polled lease expired: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	if len(m.Snapshot().Leases) != 0 {
		t.Error("unpolled lease still listed")
	}
	if _, err := m.LeaseStatus(ctx, g.ID, 0); !errors.Is(err, ErrLeaseNotFound) {
		t.Errorf("status = %v, want not found", err)
	}
	if u := used(m, "gpu1"); u != 0 {
		t.Errorf("gpu1 used after expiry = %d", u)
	}
}

// Re-registration after a corrallm restart: granted if it fits, 409 if not;
// it never evicts.
func TestLeaseHeldReRegistration(t *testing.T) {
	m, _ := leaseMgr(t, "60s")
	ctx := context.Background()
	req := gpuLease(6, true)
	req.Held = true
	if _, err := m.AcquireLease(ctx, req); err != nil {
		t.Fatalf("held fits: %v", err)
	}
	if _, err := m.AcquireLease(ctx, req); !errors.Is(err, ErrLeaseConflict) {
		t.Errorf("second held = %v, want conflict", err)
	}
}

// One lease never displaces another: the second grant is a transient miss
// and the first stays held.
func TestLeaseDoesNotYieldLease(t *testing.T) {
	m, _ := leaseMgr(t, "60s")
	ctx := context.Background()
	g, err := m.AcquireLease(ctx, gpuLease(6, true))
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.AcquireLease(ctx, gpuLease(6, true))
	var ce *CapacityError
	if !errors.As(err, &ce) || ce.Permanent {
		t.Fatalf("second lease = %v, want transient CapacityError", err)
	}
	if st, _ := m.LeaseStatus(ctx, g.ID, 0); st.State != LeaseHeld {
		t.Errorf("first lease state = %s, want held", st.State)
	}
}

// pruneConfig: gpu1 10 bytes; nomic 2, chandra 6 (8 used, 2 free).
func pruneMgr(t *testing.T) (*Manager, *config.Config) {
	t.Helper()
	body := fmt.Sprintf(`
servers:
  box:
    pools: { gpu1: "10", system: "100" }
models:
  nomic:   { cmd: "exec sleep 30", server: box, proxy: %d, ramUsage: { gpu1: "2" }, type: embedding }
  chandra: { cmd: "exec sleep 30", server: box, proxy: %d, ramUsage: { gpu1: "6" }, type: chat }
extensions:
  colmap:
    cmd: "exec sleep 30"
    server: box
    proxy: %d
    ramUsage: { system: "1" }
    lease: { token: t, max: { gpu1: "10", system: "50" }, yieldGrace: 60s }
    provides:
      sfm: { type: reconstruction }
`, listenTCP(t), listenTCP(t), listenTCP(t))
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(cfg)
	m.healthTimeout = 5 * time.Second
	m.activeUse = 0
	m.leaseProtect = 0
	t.Cleanup(m.Shutdown)
	ctx := context.Background()
	for _, n := range []string{"nomic", "chandra"} { // nomic first: the LRU victim
		_, done, _, err := m.EnsureReady(ctx, n, cfg.Models[n], nil, nil)
		if err != nil {
			t.Fatalf("load %s: %v", n, err)
		}
		done()
	}
	return m, cfg
}

// 11-14 T1b: a lease short on gpu1 evicted LRU nomic and then chandra, though
// chandra alone frees enough. The greedy pick is pruned: nomic stays.
func TestLeaseEvictsMinimalVictimSet(t *testing.T) {
	m, _ := pruneMgr(t)
	if _, err := m.AcquireLease(context.Background(), gpuLease(7, true)); err != nil {
		t.Fatalf("lease 7: %v", err)
	}
	if !isResident(m, "nomic") {
		t.Error("nomic evicted, but evicting chandra alone frees 8 ≥ 7")
	}
	if isResident(m, "chandra") {
		t.Error("chandra still resident")
	}
}

// When no single victim is enough, both go.
func TestLeaseEvictsTwoWhenBothNeeded(t *testing.T) {
	m, _ := pruneMgr(t)
	if _, err := m.AcquireLease(context.Background(), gpuLease(9, true)); err != nil {
		t.Fatalf("lease 9: %v", err)
	}
	if isResident(m, "nomic") || isResident(m, "chandra") {
		t.Errorf("nomic resident %v, chandra resident %v; 9 needs both gone", isResident(m, "nomic"), isResident(m, "chandra"))
	}
}

// When either victim alone is enough, the one sorted first (LRU) goes: the
// prune spares later victims first, so the eviction order still decides.
func TestLeaseMinimalSetKeepsEvictionOrder(t *testing.T) {
	m, _ := pruneMgr(t)
	if _, err := m.AcquireLease(context.Background(), gpuLease(2, true)); err != nil {
		t.Fatalf("lease 2: %v", err) // 2 free: fits without eviction
	}
	if !isResident(m, "nomic") || !isResident(m, "chandra") {
		t.Fatal("evicted although the lease fit")
	}
	// 2 more: nomic (2) or chandra (6) each frees enough; LRU nomic goes.
	if _, err := m.AcquireLease(context.Background(), gpuLease(2, true)); err != nil {
		t.Fatalf("second lease 2: %v", err)
	}
	if isResident(m, "nomic") {
		t.Error("nomic (LRU, enough alone) still resident")
	}
	if !isResident(m, "chandra") {
		t.Error("chandra evicted, but nomic alone was enough")
	}
}
