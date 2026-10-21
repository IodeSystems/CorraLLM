package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"time"

	"github.com/iodesystems/corrallm/internal/agent"
	"github.com/iodesystems/corrallm/internal/api"
	"github.com/iodesystems/corrallm/internal/events"
	"github.com/iodesystems/corrallm/internal/handoff"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/proxy"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
	"github.com/iodesystems/corrallm/internal/toolchain"
)

// Blue-green restarts of the primary. See internal/handoff for the mechanism;
// this file is the daemon's side of it:
//
//	old: SIGUSR2 → start the new binary with the listener → wait for ready
//	new: inherit listener → MAINPID=self to systemd → serve → "ready"
//	old: drain — forward unadmitted work, finish admitted work, exit
//	new: count the old one's slots and local backends until it is gone
//
// A handoff that fails at any step before "ready" leaves the old process
// serving exactly as before, and says why in its handoff status file.

// handoffStatusPaths are where a daemon records its last handoff attempt for
// `corrallm service upgrade` to report: named by the daemon's PID, so the
// command needs nothing but the pid it signalled (not the daemon's --home or
// --db). The daemon writes the first; the command reads each, since a shell
// may lack the XDG_RUNTIME_DIR a user unit has.
func handoffStatusPaths(pid int) []string {
	name := fmt.Sprintf("corrallm-handoff-%d.json", pid)
	var out []string
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		out = append(out, filepath.Join(d, name))
	}
	return append(out, filepath.Join(os.TempDir(), name))
}

// defaultHandoffDrain caps how long a predecessor finishes its requests. Long
// enough for a long generation; a request still running past it is cut and
// recorded as such by the successor's sweep.
const defaultHandoffDrain = 10 * time.Minute

// handoffStatus is one handoff attempt, as written to handoffStatusPaths.
type handoffStatus struct {
	From  int    `json:"from"`
	To    int    `json:"to,omitempty"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	AtMS  int64  `json:"at"`
}

func writeHandoffStatus(path string, s handoffStatus) {
	s.AtMS = time.Now().UnixMilli()
	b, _ := json.Marshal(s)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err == nil {
		_ = os.Rename(tmp, path)
	}
}

// selfExe is the binary to start as a successor: the path this process was
// started by, NOT /proc/self/exe — a deploy replaces the file at that path,
// and the new file is the point.
func selfExe() string {
	exe := os.Args[0]
	if filepath.Base(exe) == exe {
		if p, err := exec.LookPath(exe); err == nil {
			exe = p
		}
	}
	if abs, err := filepath.Abs(exe); err == nil {
		exe = abs
	}
	return exe
}

// watchUpgrade starts a successor on each upgrade signal until one succeeds,
// then hands it to serve's select. A failed attempt is logged and recorded;
// this process keeps serving and a later signal may try again.
func watchUpgrade(ctx context.Context, ln net.Listener, out chan<- *handoff.Predecessor) {
	statusPath := handoffStatusPaths(os.Getpid())[0]
	sigs := upgradeSignals()
	if len(sigs) == 0 {
		return
	}
	// Resolved now, while the file at that path is still the one we run:
	// the comparison in the log is what tells an operator what was swapped.
	exe := selfExe()
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, sigs...)
	defer signal.Stop(ch)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
		}
		fail := func(err error) {
			slog.Error("handoff failed; this process keeps serving", "err", err)
			writeHandoffStatus(statusPath, handoffStatus{From: os.Getpid(), Error: err.Error()})
		}
		if err := handoff.CheckSupervisor(); err != nil {
			fail(err)
			continue
		}
		slog.Info("handoff: starting successor", "exe", exe)
		pred, err := handoff.Start(exe, os.Args[1:], ln, handoff.DefaultReadyTimeout)
		if err != nil {
			fail(err)
			continue
		}
		slog.Info("handoff: successor is ready; draining", "successor", pred.PID())
		writeHandoffStatus(statusPath, handoffStatus{From: os.Getpid(), To: pred.PID(), OK: true})
		out <- pred
		return
	}
}

// successorBase is where a draining process forwards requests: the listening
// socket it handed over, dialled on loopback when it was bound to every
// address.
func successorBase(ln net.Listener) string {
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return "http://" + ln.Addr().String()
	}
	ip := addr.IP
	if ip == nil || ip.IsUnspecified() {
		ip = net.IPv4(127, 0, 0, 1)
	}
	return "http://" + net.JoinHostPort(ip.String(), strconv.Itoa(addr.Port))
}

// drainForSuccessor is the predecessor's last act: admit nothing more, hand
// what is queued to the successor, finish what is admitted (up to drainCap),
// and report its occupancy all the while. Returns when it is time to exit;
// serve's defers then stop the local backends (agent ones stay).
func drainForSuccessor(pred *handoff.Predecessor, srv *http.Server, ln net.Listener, conns *handoff.Conns,
	broker *events.Broker, sc *sched.Scheduler, mgr *proc.Manager, px *proxy.Proxy, h *api.Handlers,
	builds *toolchain.Builder, stopJobs context.CancelFunc, drainCap time.Duration) {
	// Order matters. The successor is armed before the scheduler drains, so a
	// request woken out of the queue by the drain has somewhere to go; the
	// first report goes out after the drain, so it is final or shrinking.
	if err := px.SetSuccessor(successorBase(ln)); err != nil {
		slog.Error("handoff: successor address", "err", err)
	}
	sc.Drain()
	mgr.Drain()
	// Jobs die with the process that started them (owner decision): the
	// scheduled ones stop, and a running bench or tool build is killed now
	// rather than orphaned.
	stopJobs()
	h.Bench.Cancel()
	builds.Stop()
	report := func() {
		if err := pred.Send(handoff.Status{Draining: true, Active: sc.OwnActive(), Local: mgr.LocalBackends()}); err != nil {
			slog.Warn("handoff: could not report to the successor", "err", err)
		}
	}
	report()

	handoff.StopAccepting(srv, ln)
	// Event streams never end on their own. Closed now, a browser reconnects —
	// to the successor — instead of holding this drain open.
	broker.Close()

	start := time.Now()
	deadline := start.Add(drainCap)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	succDone := pred.Done()
	for {
		report()
		busy, live := conns.Busy(), px.LiveRequests()
		if busy == 0 && live == 0 {
			break
		}
		if time.Now().After(deadline) {
			slog.Warn("handoff: drain cap reached with requests still running; they are cut",
				"cap", drainCap, "connections", busy, "requests", live)
			break
		}
		select {
		case <-tick.C:
		case err := <-succDone:
			// Nothing to hand back to: this process stopped accepting. systemd
			// restarts the unit when its main pid (the successor) dies.
			slog.Error("handoff: the successor exited while this process was draining", "err", err)
			succDone = nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	slog.Info("handoff: drained; exiting", "took", time.Since(start).Round(time.Millisecond))
}

// watchPredecessor applies the predecessor's reports to this process and, when
// it is gone, lifts every hold and settles what it left.
func watchPredecessor(succ *handoff.Successor, sc *sched.Scheduler, mgr *proc.Manager, px *proxy.Proxy, st *store.Store) {
	// Spend the predecessor records while draining reaches this process's
	// ledger on its own charges (deltas, quota.mergeLocked) — and here, for
	// windows this process is not charging.
	stopResync := make(chan struct{})
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stopResync:
				return
			case <-t.C:
				px.ResyncQuota()
			}
		}
	}()
	succ.Watch(func(s handoff.Status) {
		if !s.Draining {
			return
		}
		sc.SetForeign(s.Active)
		sc.ReleaseAdmission()
		if s.Local == 0 {
			mgr.ReleaseLocalSpawns()
		}
	}, func() {
		close(stopResync)
		sc.SetForeign(nil)
		sc.ReleaseAdmission()
		mgr.ReleaseLocalSpawns()
		px.ResyncQuota()
		// Whatever the predecessor left in flight was cut by its exit.
		if n, err := st.SweepInflight(processAlive); err != nil {
			slog.Warn("sweep the predecessor's in-flight requests", "err", err)
		} else if n > 0 {
			slog.Warn("requests the predecessor did not finish, recorded as cut", "count", n)
		}
		slog.Info("handoff complete: the predecessor has exited", "predecessor", succ.PredecessorPID)
	})
}

// primeAgents adopts what each agent is running now, instead of after its
// next heartbeat: a successor starts while its predecessor is still serving
// from those very backends, so there is no reason to hold their models for up
// to a heartbeat interval.
func primeAgents(ctx context.Context, mgr *proc.Manager) {
	cfg := mgr.Config()
	for name, srv := range cfg.Servers {
		if srv.Agent == nil {
			continue
		}
		go func() {
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			rh := agent.NewRemoteHost(name, srv.Agent.Endpoints, srv.Agent.ExpandedToken())
			backends, err := rh.ListBackends(cctx)
			if err != nil {
				slog.Info("handoff: agent not asked directly; its heartbeat will report", "server", name, "err", err)
				return
			}
			a, r, v := mgr.ReconcileAgent(cctx, name, backends)
			slog.Info("handoff: adopted from the agent's report", "server", name, "adopted", a, "reaped", r, "vanished", v)
		}()
	}
}
