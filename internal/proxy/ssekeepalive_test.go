package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// keepAliveRig: one model with one slot, a queueing default group, and the
// keep-alive at test-sized thresholds. The FIRST upstream call holds the slot
// until hold is closed; later calls answer at once (SSE for a stream request,
// JSON otherwise).
func keepAliveRig(t *testing.T, sc config.SchedulerConfig) (srv *httptest.Server, st *store.Store, holding <-chan struct{}, hold chan struct{}) {
	return keepAliveRigStatus(t, sc, 0)
}

// keepAliveRigStatus: streamStatus != 0 makes the upstream answer a stream
// request with that status and a llama-server error body.
func keepAliveRigStatus(t *testing.T, sc config.SchedulerConfig, streamStatus int) (srv *httptest.Server, st *store.Store, holding <-chan struct{}, hold chan struct{}) {
	t.Helper()
	held := make(chan struct{})
	hold = make(chan struct{})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		body, _ := io.ReadAll(r.Body)
		if calls.Add(1) == 1 {
			close(held)
			<-hold
		}
		if streamStatus != 0 && strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(streamStatus)
			_, _ = io.WriteString(w, `{"error":{"code":500,"message":"boom","type":"server_error"}}`)
			return
		}
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"hi"}}]}`+"\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}]}`)
	}))
	t.Cleanup(upstream.Close)

	var err error
	st, err = store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr := proc.NewManager(&config.Config{})
	t.Cleanup(mgr.Shutdown)
	cfg := mkConfig(t, "mock", upstream.URL)
	cfg.PriorityGroups = map[string]config.PriorityGroup{"default": {Weight: 1,
		OnSaturated: map[string]config.Stage{"default": {Queue: true}}}}
	cfg.Scheduler = sc
	r := chi.NewRouter()
	New(cfg, mgr, sched.NewWithConfig(cfg), st).Mount(r)
	srv = httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, st, held, hold
}

func kaPost(t *testing.T, base, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(base+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// occupy sends the request that holds the only slot and waits until it does.
func occupy(t *testing.T, base string, holding <-chan struct{}) {
	t.Helper()
	go func() {
		resp, err := http.Post(base+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"mock"}`))
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-holding:
	case <-time.After(5 * time.Second):
		t.Fatal("holder never reached the upstream")
	}
}

func rowWithStatus(t *testing.T, st *store.Store, status int) store.Activity {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		acts, _ := st.RecentActivity(store.Window{}, 20, "", "", "")
		for _, a := range acts {
			if a.Status == status && strings.Contains(a.Note, "sse-keepalive") {
				return a
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no row with status %d and a keep-alive note", status)
	return store.Activity{}
}

// A stream queued past the threshold gets 200 + SSE headers and comments at
// once, then the upstream's content, in that order.
func TestKeepAliveQueuedStreamGetsCommentsThenContent(t *testing.T) {
	srv, st, holding, hold := keepAliveRig(t, config.SchedulerConfig{MaxWait: "5s",
		StreamCommitAfter: "100ms", StreamKeepAliveEvery: "50ms"})
	occupy(t, srv.URL, holding)

	go func() { time.Sleep(400 * time.Millisecond); close(hold) }()
	t0 := time.Now()
	resp := kaPost(t, srv.URL, `{"model":"mock","stream":true}`)
	defer func() { _ = resp.Body.Close() }()
	// The status line arrives at the commit, long before the slot frees.
	if d := time.Since(t0); d > 350*time.Millisecond {
		t.Errorf("headers after %v; want them at the ~100ms commit", d)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d, content-type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if got := resp.Header.Get(HeaderServed); got != "mock" {
		t.Errorf("served header at commit = %q, want mock (the only candidate)", got)
	}
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	if !strings.HasPrefix(body, ": queued\n\n: queued\n\n") {
		t.Errorf("want several keep-alive comments first, got %q", body)
	}
	ci, di := strings.Index(body, `"content":"hi"`), strings.Index(body, "data: [DONE]")
	if ci < 0 || di < ci || strings.LastIndex(body, ": queued") > ci {
		t.Errorf("content must follow the comments and end with [DONE]: %q", body)
	}
	if !strings.Contains(body, ": x-corrallm-queued-ms: ") {
		t.Errorf("the reply's timing headers must arrive as a comment: %q", body)
	}
	if strings.Contains(body, `"error"`) {
		t.Errorf("a served stream carries no error event: %q", body)
	}
	row := rowWithStatus(t, st, http.StatusOK)
	if strings.Contains(row.Note, "error event") {
		t.Errorf("a served stream's note claims an error: %q", row.Note)
	}
}

// A stream that commits and then times out in the queue gets ONE SSE error
// event with the backpressure body and retry_after, then [DONE].
func TestKeepAliveQueueTimeoutIsAnSSEErrorEvent(t *testing.T) {
	srv, st, holding, hold := keepAliveRig(t, config.SchedulerConfig{MaxWait: "350ms",
		StreamCommitAfter: "100ms", StreamKeepAliveEvery: "50ms"})
	defer close(hold)
	occupy(t, srv.URL, holding)

	resp := kaPost(t, srv.URL, `{"model":"mock","stream":true}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want the committed 200", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	if !strings.HasPrefix(body, ": queued\n\n") || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("want comments, one error event, [DONE]: %q", body)
	}
	if strings.Count(body, "data: ") != 2 {
		t.Errorf("want exactly one error event before [DONE]: %q", body)
	}
	line := body[strings.Index(body, "data: {")+len("data: "):]
	line = line[:strings.Index(line, "\n")]
	var ev struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		t.Fatalf("error event is not JSON: %q (%v)", line, err)
	}
	e := ev.Error
	if e["type"] != "backpressure" || e["reason"] != "queue-timeout" || e["message"] == "" {
		t.Errorf("error event lost the status-code path's type/reason: %v", e)
	}
	if ra, ok := e["retry_after"].(float64); !ok || ra < 1 {
		t.Errorf("retry_after = %v, want seconds >= 1", e["retry_after"])
	}
	if e["status"] != float64(429) || e["code"] != float64(429) {
		t.Errorf("status/code = %v/%v, want 429", e["status"], e["code"])
	}
	row := rowWithStatus(t, st, http.StatusTooManyRequests)
	if !strings.Contains(row.Note, "status 429 sent as an SSE error event") {
		t.Errorf("row note = %q", row.Note)
	}
}

// An upstream error after the commit is the same one event, carrying the
// upstream's own error object, and the row keeps the real status.
func TestKeepAliveUpstreamErrorAfterCommit(t *testing.T) {
	srv, st, holding, hold := keepAliveRigStatus(t, config.SchedulerConfig{MaxWait: "5s",
		StreamCommitAfter: "100ms", StreamKeepAliveEvery: "50ms"}, http.StatusInternalServerError)
	occupy(t, srv.URL, holding)
	go func() { time.Sleep(250 * time.Millisecond); close(hold) }()

	resp := kaPost(t, srv.URL, `{"model":"mock","stream":true}`)
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	want := `data: {"error":{"code":500,"message":"boom","status":500,"type":"server_error"}}` + "\n\ndata: [DONE]\n\n"
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(body, ": queued") || !strings.HasSuffix(body, want) {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
	row := rowWithStatus(t, st, http.StatusInternalServerError)
	if !strings.Contains(row.Note, "status 500 sent as an SSE error event") {
		t.Errorf("row note = %q", row.Note)
	}
}

// A refusal before the threshold keeps its 429 and Retry-After.
func TestKeepAliveRefusalBeforeThresholdStays429(t *testing.T) {
	srv, _, holding, hold := keepAliveRig(t, config.SchedulerConfig{MaxWait: "100ms",
		StreamCommitAfter: "2s", StreamKeepAliveEvery: "50ms"})
	defer close(hold)
	occupy(t, srv.URL, holding)

	resp := kaPost(t, srv.URL, `{"model":"mock","stream":true}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("status %d, Retry-After %q; want 429 with Retry-After", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"retry_after"`) || strings.Contains(string(b), ": queued") {
		t.Errorf("body = %q", b)
	}
}

// A non-stream request queued past the threshold is unaffected.
func TestKeepAliveNonStreamUnaffected(t *testing.T) {
	srv, _, holding, hold := keepAliveRig(t, config.SchedulerConfig{MaxWait: "5s",
		StreamCommitAfter: "100ms", StreamKeepAliveEvery: "50ms"})
	occupy(t, srv.URL, holding)

	go func() { time.Sleep(300 * time.Millisecond); close(hold) }()
	resp := kaPost(t, srv.URL, `{"model":"mock"}`)
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("status %d, content-type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if string(b) != `{"choices":[{"message":{"content":"hi"}}]}` {
		t.Errorf("body = %q", b)
	}
}

func TestKeepAliveErrorEventShape(t *testing.T) {
	cases := []struct {
		name, body, retry string
		want              map[string]any
	}{
		{"llama-server error", `{"error":{"code":503,"message":"Loading model","type":"unavailable_error"}}`, "",
			map[string]any{"code": float64(503), "message": "Loading model", "type": "unavailable_error", "status": float64(503)}},
		{"plain text, Retry-After header", "no backend available\n", "7",
			map[string]any{"code": float64(503), "message": "no backend available", "type": "server_error", "retry_after": float64(7)}},
		{"string code kept", `{"error":{"message":"x","type":"invalid_request_error","code":"invalid_api_key"}}`, "",
			map[string]any{"code": "invalid_api_key", "status": float64(503)}},
	}
	for _, c := range cases {
		ev := string(keepAliveErrorEvent(503, []byte(c.body), c.retry))
		if !strings.HasPrefix(ev, "data: {") || !strings.HasSuffix(ev, "\n\ndata: [DONE]\n\n") {
			t.Fatalf("%s: frame = %q", c.name, ev)
		}
		var got struct {
			Error map[string]any `json:"error"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(strings.SplitN(ev, "\n", 2)[0], "data: ")), &got); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		for k, v := range c.want {
			if got.Error[k] != v {
				t.Errorf("%s: %s = %v, want %v", c.name, k, got.Error[k], v)
			}
		}
	}
}
