package proxy

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
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

// SetSuccessor arms forwarding to the process that took over the listener,
// at base (http://host:port). Called once, when this process starts draining.
func (p *Proxy) SetSuccessor(base string) error {
	u, err := url.Parse(base)
	if err != nil {
		return err
	}
	p.successor.Store(u)
	return nil
}

// draining reports whether requests should be handed to a successor.
func (p *Proxy) draining() bool { return p.successor.Load() != nil }

// handOver forwards r to the successor and relays its response. body is the
// request body as it ARRIVED (before any rewrite here, so the successor
// applies its own exactly once) and contentType its Content-Type as it
// arrived (a transcode here rewrites it); a nil body streams r.Body through
// untouched. Reports false when there is no successor or the request is
// already a forwarded one — the caller then carries on.
func (p *Proxy) handOver(w http.ResponseWriter, r *http.Request, contentType string, body []byte) bool {
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
