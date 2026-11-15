// Package lease is the client of corrallm's pool-lease API (P78 §7,
// corrallm internal/proxy/leases.go): POST /api/v1/leases, GET ?wait=
// long-poll, DELETE. Hold wraps it for the runner: wait for a grant, watch
// for a yield request, re-register after a corrallm restart.
package lease

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Request is the POST body.
type Request struct {
	Extension string            `json:"extension,omitempty"`
	Job       string            `json:"job"`
	Stage     string            `json:"stage"`
	Owner     string            `json:"owner"`
	Usage     map[string]string `json:"usage"`
	Yieldable bool              `json:"yieldable"`
	Held      bool              `json:"held"`
}

// Grant is a 201.
type Grant struct {
	ID             string `json:"id"`
	GrantedAt      int64  `json:"granted_at"`
	ProtectedUntil int64  `json:"protected_until"`
}

// Status is a GET answer.
type Status struct {
	ID       string `json:"id"`
	State    string `json:"state"` // held | yield_requested
	WantedBy string `json:"wanted_by,omitempty"`
	Deadline int64  `json:"deadline,omitempty"`
}

// States.
const (
	StateHeld  = "held"
	StateYield = "yield_requested"
)

// BusyError is a 429 lease_busy.
type BusyError struct {
	RetryAfter time.Duration
	Blocking   []string
	Msg        string
}

// Error prints the Retry-After the client parsed. corrallm's message embeds
// its own unrounded value ("retry_after=0s" when no blocker has a known
// expiry) while the header says 1 s; the job log showed the 0 s.
func (e *BusyError) Error() string {
	ra := "retry_after=" + e.RetryAfter.String()
	if retryAfterText.MatchString(e.Msg) {
		return "lease busy: " + retryAfterText.ReplaceAllLiteralString(e.Msg, ra)
	}
	return "lease busy: " + e.Msg + " (" + ra + ")"
}

var retryAfterText = regexp.MustCompile(`retry_after=[^\s,)]+`)

// FatalError is an answer retrying cannot fix (400, 401, 403).
type FatalError struct {
	Status int
	Msg    string
}

func (e *FatalError) Error() string { return fmt.Sprintf("lease refused (%d): %s", e.Status, e.Msg) }

var (
	// ErrNeverFits is a 503 lease_never_fits.
	ErrNeverFits = errors.New("lease never fits")
	// ErrConflict is a 409: a held re-registration does not fit; yield now.
	ErrConflict = errors.New("lease conflict: yield now")
	// ErrNotFound is a 404: expired, released, or corrallm restarted.
	ErrNotFound = errors.New("lease not found")
)

// Leaser is the API (Client, or a fake in tests).
type Leaser interface {
	Acquire(ctx context.Context, r Request) (Grant, error)
	Status(ctx context.Context, id string, wait time.Duration) (Status, error)
	Release(ctx context.Context, id string) error
}

// Client talks to corrallm.
type Client struct {
	Base  string // http://127.0.0.1:8111
	Token string
	HTTP  *http.Client
}

func (c *Client) do(ctx context.Context, method, path string, body any) (*http.Response, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Base, "/")+path, rd)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp, b, err
}

type errBody struct {
	Error struct {
		Type       string   `json:"type"`
		Message    string   `json:"message"`
		Blocking   []string `json:"blocking"`
		RetryAfter int      `json:"retry_after"`
	} `json:"error"`
}

func classify(resp *http.Response, b []byte) error {
	var e errBody
	_ = json.Unmarshal(b, &e)
	msg := e.Error.Message
	if msg == "" {
		msg = strings.TrimSpace(string(b))
	}
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		ra := time.Duration(e.Error.RetryAfter) * time.Second
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			ra = time.Duration(s) * time.Second
		}
		if ra <= 0 {
			ra = 5 * time.Second
		}
		return &BusyError{RetryAfter: ra, Blocking: e.Error.Blocking, Msg: msg}
	case http.StatusConflict:
		return ErrConflict
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusServiceUnavailable:
		if e.Error.Type == "lease_never_fits" {
			return fmt.Errorf("%w: %s", ErrNeverFits, msg)
		}
		return fmt.Errorf("lease unavailable (503 %s): %s", e.Error.Type, msg)
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
		return &FatalError{Status: resp.StatusCode, Msg: msg}
	}
	return fmt.Errorf("lease API answered %d: %s", resp.StatusCode, msg)
}

// Acquire POSTs a lease.
func (c *Client) Acquire(ctx context.Context, r Request) (Grant, error) {
	resp, b, err := c.do(ctx, http.MethodPost, "/api/v1/leases", r)
	if err != nil {
		return Grant{}, err
	}
	if resp.StatusCode != http.StatusCreated {
		return Grant{}, classify(resp, b)
	}
	var g Grant
	if err := json.Unmarshal(b, &g); err != nil || g.ID == "" {
		return Grant{}, fmt.Errorf("lease grant: bad body %q", b)
	}
	return g, nil
}

// Status long-polls a lease.
func (c *Client) Status(ctx context.Context, id string, wait time.Duration) (Status, error) {
	p := "/api/v1/leases/" + url.PathEscape(id)
	if wait > 0 {
		p += "?wait=" + wait.String()
	}
	resp, b, err := c.do(ctx, http.MethodGet, p, nil)
	if err != nil {
		return Status{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return Status{}, classify(resp, b)
	}
	var s Status
	if err := json.Unmarshal(b, &s); err != nil {
		return Status{}, fmt.Errorf("lease status: bad body %q", b)
	}
	return s, nil
}

// Release DELETEs a lease; an unknown id is not an error.
func (c *Client) Release(ctx context.Context, id string) error {
	resp, b, err := c.do(ctx, http.MethodDelete, "/api/v1/leases/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return classify(resp, b)
}

// Always grants and never yields: standalone runs without corrallm.
type Always struct{}

func (Always) Acquire(context.Context, Request) (Grant, error) {
	return Grant{ID: "lease_standalone", GrantedAt: time.Now().Unix()}, nil
}
func (Always) Status(ctx context.Context, id string, wait time.Duration) (Status, error) {
	select {
	case <-ctx.Done():
		return Status{}, ctx.Err()
	case <-time.After(wait):
	}
	return Status{ID: id, State: StateHeld}, nil
}
func (Always) Release(context.Context, string) error { return nil }

// Yield is a yield request: stop the GPU child before Deadline.
type Yield struct {
	Deadline time.Time
	WantedBy string
}

// Held is a granted lease being watched.
type Held struct {
	l     Leaser
	req   Request
	mu    sync.Mutex
	id    string
	yield chan Yield
	stop  context.CancelFunc
	done  chan struct{}
}

// ID is the current lease id (it changes on re-registration).
func (h *Held) ID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.id
}

// Yield delivers at most one yield request.
func (h *Held) Yield() <-chan Yield { return h.yield }

// Release stops watching and DELETEs the lease.
func (h *Held) Release() {
	h.stop()
	<-h.done
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.l.Release(ctx, h.ID()); err != nil {
		slog.Warn("lease release failed", "id", h.ID(), "err", err)
	}
}

// Options tune Hold.
type Options struct {
	Poll     time.Duration // long-poll wait (default 30s)
	Backoff  time.Duration // retry delay after a network error (default 5s)
	MaxWait  time.Duration // give up waiting for a grant (default 24h)
	OnWait   func(waiting bool, why string)
	Clock    func() time.Time
	MaxRetry time.Duration // cap on a Retry-After sleep (default 60s)
}

// ErrGaveUp: no grant within MaxWait.
var ErrGaveUp = errors.New("no lease granted within the wait limit")

// Hold waits for a grant (sleeping Retry-After on 429) and starts watching it.
func Hold(ctx context.Context, l Leaser, req Request, o Options) (*Held, error) {
	if o.Poll <= 0 {
		o.Poll = 30 * time.Second
	}
	if o.Backoff <= 0 {
		o.Backoff = 5 * time.Second
	}
	if o.MaxWait <= 0 {
		o.MaxWait = 24 * time.Hour
	}
	if o.MaxRetry <= 0 {
		o.MaxRetry = 60 * time.Second
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	start := o.Clock()
	waiting := false
	setWait := func(v bool, why string) {
		if v != waiting || v {
			waiting = v
			if o.OnWait != nil {
				o.OnWait(v, why)
			}
		}
	}
	for {
		g, err := l.Acquire(ctx, req)
		if err == nil {
			setWait(false, "")
			wctx, cancel := context.WithCancel(context.Background())
			h := &Held{l: l, req: req, id: g.ID, yield: make(chan Yield, 1), stop: cancel, done: make(chan struct{})}
			go h.watch(wctx, o)
			return h, nil
		}
		var fe *FatalError
		if errors.As(err, &fe) || errors.Is(err, ErrNeverFits) {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		delay := o.Backoff
		var be *BusyError
		if errors.As(err, &be) {
			delay = min(be.RetryAfter, o.MaxRetry)
		}
		setWait(true, err.Error())
		if o.Clock().Sub(start) > o.MaxWait {
			return nil, fmt.Errorf("%w (%s): last answer: %v", ErrGaveUp, o.MaxWait, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
}

func (h *Held) watch(ctx context.Context, o Options) {
	defer close(h.done)
	send := func(y Yield) {
		select {
		case h.yield <- y:
		default:
		}
	}
	for ctx.Err() == nil {
		st, err := h.l.Status(ctx, h.ID(), o.Poll)
		switch {
		case ctx.Err() != nil:
			return
		case err == nil && st.State == StateYield:
			send(Yield{Deadline: time.Unix(st.Deadline, 0), WantedBy: st.WantedBy})
			return
		case err == nil:
			continue
		case errors.Is(err, ErrNotFound):
			// corrallm restarted (leases are memory-only) or the lease
			// expired: re-register what is already in use.
			req := h.req
			req.Held = true
			g, err := h.l.Acquire(ctx, req)
			switch {
			case err == nil:
				h.mu.Lock()
				old := h.id
				h.id = g.ID
				h.mu.Unlock()
				slog.Info("lease re-registered", "old", old, "id", g.ID, "job", h.req.Job, "stage", h.req.Stage)
				continue
			case errors.Is(err, ErrConflict):
				send(Yield{Deadline: o.Clock().Add(30 * time.Second), WantedBy: "re-registration conflict"})
				return
			}
			slog.Warn("lease re-registration failed", "job", h.req.Job, "err", err)
		default:
			slog.Warn("lease poll failed", "id", h.ID(), "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(o.Backoff):
		}
	}
}
