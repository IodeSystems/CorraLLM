package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

// SSE KEEP-ALIVE FOR A QUEUED STREAM (owner design, 2026-11-20).
//
// A stream:true request that is still waiting for a backend after
// `scheduler.streamCommitAfter` (default 30 s) commits to 200 with SSE headers
// and from then on gets one SSE comment every `streamKeepAliveEvery` until the
// upstream's reply starts. Before the commit nothing changes: every refusal
// keeps its status code and Retry-After. After it the status line is gone, so
// a failure that would have been a 4xx/5xx is sent as ONE event in the OpenAI
// error shape, then `data: [DONE]` (as a loop cut ends a stream):
//
//	data: {"error":{"message":…,"type":…,"code":…,"retry_after":N,"status":429,…}}
//
// The error object is the body the status-code path would have sent, so its
// type (and reason, ticket, …) are the ones a client already maps; `code`
// falls back to the HTTP status and `retry_after` to the Retry-After header.
//
// It is the OUTERMOST writer: everything the handler writes passes through it,
// so a refusal, an upstream error and a normal reply all take the same path,
// whichever code wrote them. The request stays the same queued entry — this
// writes to the client while admission blocks; it never re-queues.

// keepAlivePhase is what the request is waiting on, said in the comment.
type keepAlivePhase int

const (
	phaseQueued   keepAlivePhase = iota // in admission control
	phaseLoading                        // admitted; waiting for the backend to be resident
	phaseUpstream                       // sent upstream; no new commit from here
)

func (ph keepAlivePhase) comment() string {
	switch ph {
	case phaseLoading:
		return ": loading\n\n"
	case phaseUpstream:
		return ": processing\n\n"
	}
	return ": queued\n\n"
}

// keepAliveErrCap bounds the error body held for the SSE error event.
const keepAliveErrCap = 64 << 10

type sseKeepAlive struct {
	w         http.ResponseWriter
	threshold time.Duration
	interval  time.Duration
	served    string // set at commit when the request can only be served by one model

	mu        sync.Mutex
	phase     keepAlivePhase
	committed bool          // 200 + SSE headers sent by us
	started   bool          // the handler wrote a status or a byte
	finished  bool          // the handler returned; nothing more may be written
	status    int           // the handler's status after the commit
	errMode   bool          // that status was an error: its body becomes the event
	errBuf    bytes.Buffer  // the error body, bounded
	sentHdr   http.Header   // headers as committed
	comments  int           // keep-alive comments sent
	waited    time.Duration // time to the commit
	begin     time.Time
	stop      chan struct{}
}

type keepAliveCtxKey struct{}

// keepAliveFrom is the request's keep-alive writer, nil when it has none.
func keepAliveFrom(r *http.Request) *sseKeepAlive {
	k, _ := r.Context().Value(keepAliveCtxKey{}).(*sseKeepAlive)
	return k
}

// keepAliveRoutes are the OpenAI SSE routes the keep-alive covers. Not
// /v1/messages: an Anthropic client expects its own error event shape.
var keepAliveRoutes = map[string]bool{"/v1/chat/completions": true, "/v1/completions": true}

// newSSEKeepAlive wraps w when the request qualifies, and returns w unchanged
// (and nil) when it does not. The caller must call finish before returning.
func newSSEKeepAlive(w http.ResponseWriter, r *http.Request, streaming bool, after, every time.Duration, served string) (*sseKeepAlive, *http.Request) {
	if !streaming || after <= 0 || !keepAliveRoutes[r.URL.Path] {
		return nil, r
	}
	k := &sseKeepAlive{w: w, threshold: after, interval: every, served: served,
		begin: time.Now(), stop: make(chan struct{})}
	go k.run()
	return k, r.WithContext(context.WithValue(r.Context(), keepAliveCtxKey{}, k))
}

func (k *sseKeepAlive) run() {
	t := time.NewTimer(k.threshold)
	defer t.Stop()
	for {
		select {
		case <-k.stop:
			return
		case <-t.C:
		}
		k.mu.Lock()
		if k.started || k.finished {
			k.mu.Unlock()
			return
		}
		next := k.interval
		switch {
		case k.committed:
			k.commentLocked()
		case k.phase == phaseUpstream:
			// Past the queue before the threshold: no commit, so an upstream
			// error keeps its status. Polled in case a KV-full retry or a
			// spill puts the request back in a queue.
			next = time.Second
		default:
			k.commitLocked()
		}
		k.mu.Unlock()
		t.Reset(next)
	}
}

// setPhase records what the request now waits on.
func (k *sseKeepAlive) setPhase(ph keepAlivePhase) {
	if k == nil {
		return
	}
	k.mu.Lock()
	k.phase = ph
	k.mu.Unlock()
}

func (k *sseKeepAlive) commitLocked() {
	h := k.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	h.Del("Content-Length")
	if k.served != "" {
		h.Set(HeaderServed, k.served)
	}
	k.sentHdr = h.Clone()
	k.committed = true
	k.waited = time.Since(k.begin)
	k.w.WriteHeader(http.StatusOK)
	k.commentLocked()
}

func (k *sseKeepAlive) commentLocked() {
	_, _ = k.w.Write([]byte(k.phase.comment()))
	k.comments++
	k.flushLocked()
}

func (k *sseKeepAlive) flushLocked() {
	if f, ok := k.w.(http.Flusher); ok {
		f.Flush()
	}
}

// Header is the real header map. After the commit net/http ignores changes to
// it; finish still reads Retry-After from it.
func (k *sseKeepAlive) Header() http.Header { return k.w.Header() }

func (k *sseKeepAlive) WriteHeader(code int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.finished {
		return
	}
	if !k.committed {
		if code >= 200 {
			k.started = true
		}
		k.w.WriteHeader(code)
		return
	}
	if code < 200 || k.started {
		return // interim, or a second status: the 200 is already on the wire
	}
	k.started = true
	k.status = code
	if code >= 300 {
		k.errMode = true
		return
	}
	k.headerCommentLocked()
}

// headerCommentLocked sends, as one SSE comment block, the corrallm headers
// the reply would have carried and the commit could not (served, timing,
// slot): known only once the upstream answered.
func (k *sseKeepAlive) headerCommentLocked() {
	h := k.w.Header()
	var names []string
	for name := range h {
		if !strings.HasPrefix(name, "X-Corrallm-") {
			continue
		}
		if v := h.Get(name); v != "" && v != k.sentHdr.Get(name) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, ": %s: %s\n", strings.ToLower(name), strings.ReplaceAll(h.Get(name), "\n", " "))
	}
	b.WriteString("\n")
	_, _ = k.w.Write([]byte(b.String()))
}

func (k *sseKeepAlive) Write(b []byte) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.finished {
		return 0, http.ErrHandlerTimeout
	}
	if !k.committed {
		k.started = true
		return k.w.Write(b)
	}
	if !k.started {
		k.started, k.status = true, http.StatusOK
		k.headerCommentLocked()
	}
	if k.errMode {
		if room := keepAliveErrCap - k.errBuf.Len(); room > 0 {
			k.errBuf.Write(b[:min(len(b), room)])
		}
		return len(b), nil
	}
	return k.w.Write(b)
}

func (k *sseKeepAlive) Flush() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.finished || k.errMode {
		return
	}
	k.flushLocked()
}

func (k *sseKeepAlive) Unwrap() http.ResponseWriter { return k.w }

// finish stops the keep-alive and, when the reply after the commit was an
// error (or nothing at all), sends it as the SSE error event. After it returns
// nothing more is written.
func (k *sseKeepAlive) finish() {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.finished {
		return
	}
	k.finished = true
	close(k.stop)
	if !k.committed {
		return
	}
	switch {
	case k.errMode:
	case !k.started:
		// The handler ended with nothing written (the caller left, or a path
		// that writes no body).
		k.status = http.StatusInternalServerError
	default:
		return
	}
	_, _ = k.w.Write(keepAliveErrorEvent(k.status, k.errBuf.Bytes(), k.w.Header().Get("Retry-After")))
	k.flushLocked()
}

// noteFor is the activity row's account of the keep-alive, "" when it did not
// commit. The row's status stays the real outcome (status); this says what
// the wire saw instead.
func (k *sseKeepAlive) noteFor(status int) string {
	if k == nil {
		return ""
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.committed {
		return ""
	}
	s := fmt.Sprintf("sse-keepalive: 200 committed after %.1fs waiting, %d comments", k.waited.Seconds(), k.comments)
	if status >= 300 {
		s += fmt.Sprintf("; status %d sent as an SSE error event", status)
	}
	return s
}

// keepAliveErrorEvent renders the error the status-code path would have sent
// as one SSE event and the stream's end.
func keepAliveErrorEvent(status int, body []byte, retryAfter string) []byte {
	e := map[string]any{}
	var env struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil && len(env.Error) > 0 {
		if json.Unmarshal(env.Error, &e) != nil {
			var s string
			if json.Unmarshal(env.Error, &s) == nil {
				e = map[string]any{"message": s}
			}
		}
	} else if json.Unmarshal(body, &e) != nil || e == nil {
		e = map[string]any{}
	}
	if m, _ := e["message"].(string); m == "" {
		msg := strings.TrimSpace(string(body))
		if msg == "" || strings.HasPrefix(msg, "{") {
			msg = http.StatusText(status)
		}
		e["message"] = msg
	}
	if t, _ := e["type"].(string); t == "" {
		if status >= 500 {
			e["type"] = "server_error"
		} else {
			e["type"] = "invalid_request_error"
		}
	}
	if e["code"] == nil {
		e["code"] = status
	}
	if e["retry_after"] == nil {
		if secs, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil {
			e["retry_after"] = secs
		}
	}
	e["status"] = status
	b, _ := json.Marshal(map[string]any{"error": e})
	return []byte("data: " + string(b) + "\n\ndata: [DONE]\n\n")
}

// streamKeepAliveContract is the /v1/capabilities entry for the long-wait
// contract, with this box's threshold and interval.
func streamKeepAliveContract(sc config.SchedulerConfig) map[string]any {
	after, every := sc.StreamKeepAlive()
	return map[string]any{
		"enabled":         after > 0,
		"routes":          []string{"/v1/chat/completions", "/v1/completions"},
		"commit_after_s":  after.Seconds(),
		"keepalive_every_s": every.Seconds(),
		"description": "A stream:true request still waiting for a backend (queue or cold load) after commit_after_s gets HTTP 200 with text/event-stream headers at once, then an SSE comment line (`: queued`, `: loading` or `: processing`) every keepalive_every_s seconds until the model's stream starts; then the stream is proxied as usual. " +
			"Refusals before that point (auth, permission, budget, validation, a queue refusal or timeout inside the window) keep their normal status codes and Retry-After. " +
			"After the commit the status line is spent: a failure that would have been a 4xx/5xx (queue timeout, backend restarting, upstream error) arrives as ONE event `data: {\"error\":{...}}` followed by `data: [DONE]`, and the stream closes. " +
			"The error object is the JSON the status-code path would have sent (same `type`, `reason`, `ticket`, ...), plus `status` (the HTTP status it replaces), `code` (the body's own code, else that status) and `retry_after` (seconds, when the status-code path would have sent Retry-After). Treat it as that status: retry after retry_after on 429/503. " +
			"Headers the reply would have carried that are only known once the model answers (X-Corrallm-Served for a lane, the timing headers, X-Corrallm-Slot) arrive as SSE comment lines `: x-corrallm-<name>: <value>` before the first data event; X-Corrallm-Served is a real header when only one model can serve the request. " +
			"Non-streaming requests are unchanged. Configured by scheduler.streamCommitAfter (\"off\" disables) and scheduler.streamKeepAliveEvery.",
		"error_event": `data: {"error":{"message":"backend at capacity; retry after backoff","type":"backpressure","reason":"queue-timeout","code":429,"status":429,"retry_after":12,"ticket":"..."}}` + "\n\ndata: [DONE]\n\n",
	}
}
