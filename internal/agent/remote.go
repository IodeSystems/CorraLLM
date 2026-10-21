package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/corrallm/internal/host"
)

// RemoteHost is a host.Host backed by a `corrallm agent` on another machine.
//
// It lives in this package rather than internal/host to keep the dependency
// one-way: the agent server already imports host to spawn things, so host
// importing the agent's wire types back would be a cycle. proc.Manager imports
// both, which is where they meet.
type RemoteHost struct {
	server    string
	endpoints []string
	token     string
	cli       *http.Client
	probeCli  *http.Client

	mu       sync.Mutex
	endpoint string // the one that last answered; empty until probed
	// onCapacity receives a fresh reading of the agent's machine taken after a
	// backend's group is gone (SetCapacitySink). Guarded by mu.
	onCapacity func(Capacity)
}

// SetCapacitySink makes every handle of this host, on seeing its backend's
// group gone, fetch a fresh capacity reading and pass it to f BEFORE the exit
// is published (Alive false, Done closed).
//
// The primary counts agent memory that no live backend of its own accounts
// for as foreign, from the agent's last reading. That reading is up to a
// heartbeat interval old, so right after a stop it still holds the exited
// backend: a reload admitted against it fails with "no capacity" although the
// card is empty (box1 2026-10-21 01:46:04, a build-benchmark trial). With the
// sink, "the stop returned" also means "the ledger has seen the memory go".
func (r *RemoteHost) SetCapacitySink(f func(Capacity)) {
	r.mu.Lock()
	r.onCapacity = f
	r.mu.Unlock()
}

// Capacity fetches the agent's measurement of its machine now.
func (r *RemoteHost) Capacity(ctx context.Context) (Capacity, error) {
	endpoint, err := r.pick(ctx)
	if err != nil {
		return Capacity{}, err
	}
	var c Capacity
	err = r.call(ctx, http.MethodGet, endpoint+"/agent/v1/capacity", nil, &c)
	return c, err
}

// refreshCapacity hands a post-exit reading to the sink, if there is one.
// Bounded and best-effort: an unreachable agent costs the freshness (the next
// heartbeat brings it), never the exit.
func (r *RemoteHost) refreshCapacity() {
	r.mu.Lock()
	sink := r.onCapacity
	r.mu.Unlock()
	if sink == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), capacityRefreshTimeout)
	defer cancel()
	c, err := r.Capacity(ctx)
	if err != nil {
		slog.Warn("agent capacity refresh after a backend exit failed; the next heartbeat corrects it",
			"server", r.server, "err", err)
		return
	}
	sink(c)
}

// capacityRefreshTimeout bounds the post-exit capacity fetch. A probe is one
// nvidia-smi run on the agent: seconds, not tens of them.
const capacityRefreshTimeout = 10 * time.Second

// NewRemoteHost binds a server name to an agent reachable at any of endpoints.
func NewRemoteHost(server string, endpoints []string, token string) *RemoteHost {
	return &RemoteHost{
		server:    server,
		endpoints: endpoints,
		token:     token,
		// Bounded so an agent that accepts a connection and then stops talking
		// cannot wedge a caller. Long enough for a spawn on a loaded box.
		cli: &http.Client{Timeout: 20 * time.Second},
		// Probing is a LIVENESS question and gets its own short budget. With
		// the operation timeout, a blackholed address — a LAN entry while this
		// daemon is on the VPN, which is the normal case for a laptop — costs
		// 20s per candidate before the reachable one is even tried, turning a
		// spawn into a minute of nothing. Endpoints are candidates; finding out
		// one is dead must be cheap.
		probeCli: &http.Client{Timeout: probeTimeout},
	}
}

func (r *RemoteHost) Name() string { return r.server }

// pick returns an endpoint that answers /hello, remembering it for next time.
//
// Endpoints are candidates, not alternatives to choose between once: an agent
// legitimately has a LAN address, a VPN address and an external one at the same
// time, and which of them works depends on where this daemon is sitting right
// now — that can change without the config changing. So a remembered endpoint
// is re-probed on failure rather than trusted forever.
func (r *RemoteHost) pick(ctx context.Context) (string, error) {
	r.mu.Lock()
	remembered := r.endpoint
	r.mu.Unlock()

	tryOrder := r.endpoints
	if remembered != "" {
		tryOrder = append([]string{remembered}, r.endpoints...)
	}

	var errs []string
	seen := map[string]bool{}
	for _, e := range tryOrder {
		e = strings.TrimRight(strings.TrimSpace(e), "/")
		if e == "" || seen[e] {
			continue
		}
		seen[e] = true
		if err := r.hello(ctx, e); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", e, err))
			continue
		}
		r.mu.Lock()
		r.endpoint = e
		r.mu.Unlock()
		return e, nil
	}
	return "", fmt.Errorf("no agent endpoint answered for server %q: %s", r.server, strings.Join(errs, "; "))
}

// probeTimeout bounds a single endpoint liveness check. Generous for a LAN or
// VPN round trip, short enough that walking several dead candidates is quick.
const probeTimeout = 2 * time.Second

func (r *RemoteHost) hello(ctx context.Context, endpoint string) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	var h Hello
	if err := r.callWith(ctx, r.probeCli, http.MethodGet, endpoint+"/agent/v1/hello", nil, &h); err != nil {
		return err
	}
	if h.Protocol != Protocol {
		return fmt.Errorf("agent speaks protocol %d, this daemon speaks %d", h.Protocol, Protocol)
	}
	return nil
}

func (r *RemoteHost) call(ctx context.Context, method, url string, body, out any) error {
	return r.callWith(ctx, r.cli, method, url, body, out)
}

func (r *RemoteHost) callWith(ctx context.Context, cli *http.Client, method, url string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return err
	}
	req.Header.Set(ProtocolHeader, strconv.Itoa(Protocol))
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("agent %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Start asks the agent to run the backend and returns a handle that mirrors it.
func (r *RemoteHost) Start(s host.Spec) (host.Handle, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	endpoint, err := r.pick(ctx)
	if err != nil {
		return nil, err
	}
	var b Backend
	if err := r.call(ctx, http.MethodPost, endpoint+"/agent/v1/backends",
		StartRequest{Key: specKey(s), Model: s.Name, Cmd: s.Cmd, ConfigCmd: s.ConfigCmd, Port: s.Port}, &b); err != nil {
		return nil, fmt.Errorf("agent start on %s: %w", endpoint, err)
	}
	return r.handleFor(endpoint, b.ID, s.Out), nil
}

// Attach returns a handle for a backend the agent is ALREADY running, by its
// agent-side id — one this primary did not start in its current life, found
// on a heartbeat after a restart and adopted (proc.ReconcileAgent).
//
// Its output is read from the agent's log ring from the start of what is
// retained, so the manager's banner parsing sees as much as still exists.
func (r *RemoteHost) Attach(id string, out io.Writer) (host.Handle, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	endpoint, err := r.pick(ctx)
	if err != nil {
		return nil, err
	}
	return r.handleFor(endpoint, id, out), nil
}

func (r *RemoteHost) handleFor(endpoint, id string, out io.Writer) *remoteHandle {
	h := &remoteHandle{
		host: r, endpoint: endpoint, id: id,
		done: make(chan struct{}), out: out,
		alive: true, memMiB: -1, stop: make(chan struct{}),
	}
	go h.watch()
	return h
}

// specKey is the identity reconciliation will match on, defaulting to the
// served name for an ordinary model.
func specKey(s host.Spec) string {
	if s.Key != "" {
		return s.Key
	}
	return s.Name
}

// remoteHandle mirrors one backend running on the agent.
//
// The agent is POLLED, not subscribed: there is no push channel to lose, and a
// missed poll costs latency rather than correctness because every read is
// absolute (status is current state, logs are requested by sequence).
type remoteHandle struct {
	host     *RemoteHost
	endpoint string
	id       string
	out      io.Writer
	done     chan struct{}
	stop     chan struct{} // closed by Detach
	stopOnce sync.Once

	mu       sync.Mutex
	alive    bool
	err      error
	memMiB   int
	memByDev map[string]int // nil: the agent cannot (or does not) split by card
	nextSeq  int64
	finished bool
}

// pollInterval is how often the handle re-reads state and drains new output.
// Fast enough that llama.cpp's startup banner reaches the primary's log parser
// well before the model finishes loading — that banner is where the tuning
// profile comes from.
const pollInterval = 500 * time.Millisecond

func (h *remoteHandle) watch() {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		h.drainLogs()
		st, err := h.status()
		switch {
		case err != nil:
			// A failed poll is "cannot tell", NOT "dead". Declaring it dead
			// would free the pool reservation while a live process still holds
			// the memory — the exact over-commitment that makes every later
			// spawn OOM. Keep the last known state and try again.
			slog.Debug("agent poll failed", "id", h.id, "err", err)
		case st.Exited && st.Alive:
			// The leader was reaped but something in its GROUP still runs —
			// the process that holds the memory, when the leader was a shell.
			// Done closes (it reports the leader, as a local handle's does);
			// Alive stays true and polling goes on until the group is gone.
			h.mu.Lock()
			if st.Err != "" {
				h.err = fmt.Errorf("%s", st.Err)
			}
			if !h.finished {
				h.finished = true
				close(h.done)
			}
			h.mu.Unlock()
		case st.Exited:
			// Gone. Refresh the capacity reading BEFORE publishing that, so a
			// caller woken by the exit admits against a reading that no longer
			// shows this backend (SetCapacitySink).
			h.host.refreshCapacity()
			h.mu.Lock()
			h.alive = false
			if st.Err != "" {
				h.err = fmt.Errorf("%s", st.Err)
			}
			if !h.finished {
				h.finished = true
				close(h.done)
			}
			h.mu.Unlock()
			h.drainLogs() // one last pass so the tail is not lost
			return
		default:
			// Not exited yet: alive. A group that is empty before its leader
			// is reaped is a moment's gap; the next poll reports Exited and
			// goes through the refresh above before anyone is told.
			h.mu.Lock()
			h.alive = true
			h.memMiB = st.MemoryMiB
			h.memByDev = st.MemoryByDevice
			h.mu.Unlock()
		}
		select {
		case <-t.C:
		case <-h.stop:
			return
		}
	}
}

// Detach stops mirroring the backend without touching it. For a handle that
// was attached and then not used (adoption lost a race); Done never closes
// afterwards, so nothing may be waiting on it.
func (h *remoteHandle) Detach() { h.stopOnce.Do(func() { close(h.stop) }) }

func (h *remoteHandle) status() (Backend, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var b Backend
	err := h.host.call(ctx, http.MethodGet, h.endpoint+"/agent/v1/backends/"+h.id, nil, &b)
	return b, err
}

// drainLogs pulls everything since the last sequence seen and writes it to the
// manager's sink, so the primary's existing banner parsing works unchanged.
func (h *remoteHandle) drainLogs() {
	if h.out == nil {
		return
	}
	h.mu.Lock()
	from := h.nextSeq
	h.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var page struct {
		Lines []LogLine `json:"lines"`
		Next  int64     `json:"next"`
	}
	url := fmt.Sprintf("%s/agent/v1/backends/%s/logs?from=%d", h.endpoint, h.id, from)
	if err := h.host.call(ctx, http.MethodGet, url, nil, &page); err != nil {
		return // next tick retries from the same sequence; nothing is lost
	}
	for _, l := range page.Lines {
		_, _ = io.WriteString(h.out, l.Line+"\n")
	}
	if page.Next > 0 {
		h.mu.Lock()
		h.nextSeq = page.Next
		h.mu.Unlock()
	}
}

func (h *remoteHandle) ID() string { return h.endpoint + "#" + h.id }

// AgentID is the backend's id on its agent, which is what a heartbeat reports
// it under. Reconciliation compares it to tell this backend from a duplicate
// carrying the same key.
func (h *remoteHandle) AgentID() string { return h.id }

func (h *remoteHandle) Done() <-chan struct{} { return h.done }

func (h *remoteHandle) Err() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

func (h *remoteHandle) Signal(s host.Sig) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sig := "term"
	if s == host.SigKill {
		sig = "kill"
	}
	return h.host.call(ctx, http.MethodPost,
		h.endpoint+"/agent/v1/backends/"+h.id+"/signal", SignalRequest{Sig: sig}, nil)
}

// Alive reports the last state the poller observed.
//
// Deliberately NOT a synchronous request: Alive is called in reaping loops, and
// a network round trip per iteration against an unreachable agent turns a 15
// second grace period into minutes. The poller is at most pollInterval stale,
// which is far finer than the grace it feeds.
func (h *remoteHandle) Alive() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.alive
}

// MemoryMiB returns the agent's last per-process reading, or an error when that
// machine cannot attribute memory per process at all (macOS). The error is the
// supported "measurement unavailable" path, not a failure.
func (h *remoteHandle) MemoryMiB() (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.memMiB < 0 {
		return 0, fmt.Errorf("agent cannot attribute memory per process")
	}
	return h.memMiB, nil
}

// MemoryOnMiB returns the agent's last per-card reading for the card named by
// uuid — 0 when the group holds nothing there, the same answer a local handle
// gives for an idle card.
//
// Errors when the agent sent no per-card split: an agent older than
// Backend.MemoryByDevice, a machine that cannot attribute per device (macOS),
// or no poll answered yet. Returning the aggregate instead would be the exact
// cross-card over-charge this method exists to prevent — the caller falls back
// to MemoryMiB and the single-device path.
func (h *remoteHandle) MemoryOnMiB(uuid string) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.memByDev == nil {
		return 0, fmt.Errorf("agent reports no per-device memory breakdown")
	}
	return h.memByDev[uuid], nil
}

// ListBackends asks the agent what it is running now — the same view its
// heartbeat carries. A primary that has just taken over from another uses it
// to adopt without waiting up to a heartbeat interval for the first beat.
func (r *RemoteHost) ListBackends(ctx context.Context) ([]Backend, error) {
	endpoint, err := r.pick(ctx)
	if err != nil {
		return nil, err
	}
	var out struct {
		Backends []Backend `json:"backends"`
	}
	if err := r.call(ctx, http.MethodGet, endpoint+"/agent/v1/backends", nil, &out); err != nil {
		return nil, err
	}
	return out.Backends, nil
}

// KillBackend stops a backend on the agent by its agent-side id, without a
// Handle. Used by reconciliation to reap an orphan the primary has no handle
// for — precisely the case where a Handle does not exist.
func (r *RemoteHost) KillBackend(ctx context.Context, id string) error {
	endpoint, err := r.pick(ctx)
	if err != nil {
		return err
	}
	return r.call(ctx, http.MethodPost,
		endpoint+"/agent/v1/backends/"+id+"/signal", SignalRequest{Sig: "term"}, nil)
}
