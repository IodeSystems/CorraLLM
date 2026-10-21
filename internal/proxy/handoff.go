package proxy

import (
	"bytes"
	"crypto/subtle"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/iodesystems/corrallm/internal/store"
)

// Blue-green handoff, the request side.
//
// Once this process has handed over, it admits nothing (sched.ErrDraining)
// and spawns nothing (proc.ErrDraining). A request that reaches it anyway —
// one already read off a connection when the listener closed, or one still
// queued for a slot — is not refused: it is FORWARDED to the successor, which
// admits it against the slots it knows both processes hold, serves it and
// writes its one activity row. Nothing has been written to the client or the
// log here when a request is handed over, so the caller sees one ordinary
// response.

// HandoffHeader marks a request forwarded by a draining predecessor (value:
// its pid). A process that is itself draining does not forward such a
// request again.
const HandoffHeader = "X-Corrallm-Handoff"

// HandoffWaitedHeader is how long, in milliseconds, a forwarded request had
// already waited in the predecessor before it was handed over, and
// HandoffTokenHeader the handoff's secret that makes the successor believe it
// (handoff.Predecessor.Token). Without them the successor's row counted only
// its own share: a request that queued ~27 s in the old process logged a few
// hundred ms of dwell.
const (
	HandoffWaitedHeader = "X-Corrallm-Handoff-Waited-Ms"
	HandoffTokenHeader  = "X-Corrallm-Handoff-Token"
)

// maxHandedWait caps a believed predecessor wait: well past any real drain,
// so only a corrupt value is cut.
const maxHandedWait = 24 * time.Hour

// SetSuccessor arms forwarding to the process that took over the listener,
// at base (http://host:port). token is the handoff's secret, stamped on every
// forward so the successor can trust what the forward reports. Called once,
// when this process starts draining.
func (p *Proxy) SetSuccessor(base, token string) error {
	u, err := url.Parse(base)
	if err != nil {
		return err
	}
	p.successorToken.Store(&token)
	p.successor.Store(u)
	return nil
}

// SetPredecessorToken is the successor's side of SetSuccessor: requests that
// carry token in HandoffTokenHeader were forwarded by the predecessor, and
// their HandoffWaitedHeader is counted. Empty trusts nothing.
func (p *Proxy) SetPredecessorToken(token string) {
	p.predecessorToken.Store(&token)
}

// handedWait is how long r already waited in the predecessor, when it was
// forwarded by it; 0 otherwise. The handoff headers are removed either way:
// they are this hop's, and a backend (or a paid API) has no business seeing
// the token.
func (p *Proxy) handedWait(r *http.Request) time.Duration {
	got, waited := r.Header.Get(HandoffTokenHeader), r.Header.Get(HandoffWaitedHeader)
	r.Header.Del(HandoffTokenHeader)
	r.Header.Del(HandoffWaitedHeader)
	want := p.predecessorToken.Load()
	if want == nil || *want == "" || got == "" ||
		subtle.ConstantTimeCompare([]byte(got), []byte(*want)) != 1 {
		return 0
	}
	ms, err := strconv.ParseInt(waited, 10, 64)
	if err != nil || ms <= 0 {
		return 0
	}
	return min(time.Duration(ms)*time.Millisecond, maxHandedWait)
}

// draining reports whether requests should be handed to a successor.
func (p *Proxy) draining() bool { return p.successor.Load() != nil }

// handOver forwards r to the successor and relays its response. body is the
// request body as it ARRIVED (before any rewrite here, so the successor
// applies its own exactly once) and contentType its Content-Type as it
// arrived (a transcode here rewrites it); a nil body streams r.Body through
// untouched. Reports false when there is no successor or the request is
// already a forwarded one — the caller then carries on.
//
// waited is how long r has been in this process; the successor adds it to the
// row's dwell and queued time.
func (p *Proxy) handOver(w http.ResponseWriter, r *http.Request, contentType string, body []byte, waited time.Duration) bool {
	succ := p.successor.Load()
	if succ == nil || r.Header.Get(HandoffHeader) != "" {
		return false
	}
	out := r.Clone(r.Context())
	if body != nil {
		if contentType != "" {
			out.Header.Set("Content-Type", contentType)
		}
		out.Body = io.NopCloser(bytes.NewReader(body))
		out.ContentLength = int64(len(body))
		out.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}
	out.Header.Set(HandoffHeader, strconv.Itoa(pid))
	out.Header.Del(HandoffTokenHeader)
	out.Header.Del(HandoffWaitedHeader)
	if tok := p.successorToken.Load(); tok != nil && *tok != "" {
		out.Header.Set(HandoffTokenHeader, *tok)
		if ms := waited.Milliseconds(); ms > 0 {
			out.Header.Set(HandoffWaitedHeader, strconv.FormatInt(ms, 10))
		}
	}
	// The successor stamps its own pid; ours would make the answer look as
	// if this process served it.
	w.Header().Del(PidHeader)
	// Counted as live: an upgraded (websocket) forward is hijacked, and
	// http.Server.Shutdown would not wait for it.
	p.handing.Add(1)
	defer p.handing.Add(-1)
	slog.Info("handoff: forwarding request to the successor", "path", r.URL.Path, "to", succ.Host)
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(succ)
			pr.Out.Host = pr.In.Host
			// The caller's address, for the successor's activity row: the
			// connection it sees is ours, from loopback. RealIP reads this.
			pr.Out.Header.Set("X-Real-IP", clientIP(pr.In))
		},
		// Stream as it arrives: a forwarded SSE response must not be buffered.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			slog.Error("handoff: successor unreachable for a forwarded request", "err", err)
			writeJSONError(w, http.StatusServiceUnavailable, "server_error",
				"corrallm is restarting and its successor did not answer; retry")
		},
	}
	rp.ServeHTTP(w, out)
	return true
}

// refuseUnhanded answers a request the drained scheduler refused and that
// could not be handed over (no successor, or it was forwarded here already):
// a 503 the caller can retry, and the one row saying so. It used to fall to
// the "client canceled" 499 path, which writes nothing — the caller got an
// empty 200 and the row blamed the caller.
func (p *Proxy) refuseUnhanded(w http.ResponseWriter, r *http.Request, a store.Activity) {
	slog.Warn("handoff: a refused request could not be handed over", "path", r.URL.Path,
		"forwardedBy", r.Header.Get(HandoffHeader))
	w.Header().Set("Retry-After", "1")
	writeJSONError(w, http.StatusServiceUnavailable, "server_error", "corrallm is restarting; retry")
	a.Status = http.StatusServiceUnavailable
	a.Error = "restarting: not handed over"
	p.logReq(r, a)
}

// pid is this process's, stamped on forwarded requests and on responses.
var pid = os.Getpid()

// PidHeader names the corrallm process that answered a request, so a client
// (or a test across a handoff) can tell the old process from the new one.
const PidHeader = "X-Corrallm-Pid"

// LiveRequests counts requests this process is serving, upgraded sessions
// included — which http.Server.Shutdown does not wait for, so a drain does.
func (p *Proxy) LiveRequests() int {
	p.inflightMu.Lock()
	defer p.inflightMu.Unlock()
	return len(p.inflight) + int(p.handing.Load())
}

// ResyncQuota folds spend another process has persisted into this one's
// ledger (quota.Ledger.Resync) — the successor's view of what its draining
// predecessor charges.
func (p *Proxy) ResyncQuota() { p.quota.Resync() }
