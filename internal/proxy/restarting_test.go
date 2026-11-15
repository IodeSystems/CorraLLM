package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/host"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

const chatReply = `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`

// closedPort is a loopback port nothing listens on.
func closedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return "http://" + addr
}

func assertRestarting(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503: %s", rec.Code, rec.Body.String())
	}
	if ra, _ := strconv.Atoi(rec.Header().Get("Retry-After")); ra <= 0 {
		t.Errorf("Retry-After %q, want a positive number of seconds", rec.Header().Get("Retry-After"))
	}
	var body struct {
		Error struct {
			Type       string `json:"type"`
			RetryAfter int    `json:"retry_after"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Type != typeBackendRestarting || body.Error.RetryAfter <= 0 {
		t.Errorf("body %q (err %v), want type %s with retry_after", rec.Body.String(), err, typeBackendRestarting)
	}
}

// A local backend whose port refuses the connection (killed, or not up yet)
// answers 503 + Retry-After: nothing reached it, and a retry is safe.
func TestLocalConnectionRefusedIs503RetryAfter(t *testing.T) {
	h, _ := refusalRig(t, map[string]config.Model{"swift": modelTo(t, closedPort(t), "chat")}, nil)
	assertRestarting(t, post(h, "swift"))
}

// A local backend that closes the connection before any reply (llama-server
// on SIGTERM) answers the same 503.
func TestLocalEOFBeforeReplyIs503RetryAfter(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		c, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = c.Close()
		}
	}))
	t.Cleanup(up.Close)
	h, _ := refusalRig(t, map[string]config.Model{"swift": modelTo(t, up.URL, "chat")}, nil)
	assertRestarting(t, post(h, "swift"))
}

// A backend that answers with something that is not HTTP is broken, not
// restarting: 502 stays.
func TestLocalMalformedReplyStays502(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		c, buf, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_, _ = buf.WriteString("SPDY/9 what\r\n\r\n")
			_ = buf.Flush()
			_ = c.Close()
		}
	}))
	t.Cleanup(up.Close)
	h, _ := refusalRig(t, map[string]config.Model{"swift": modelTo(t, up.URL, "chat")}, nil)
	if rec := post(h, "swift"); rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502: %s", rec.Code, rec.Body.String())
	}
}

// A remote upstream that refuses the connection is the provider's outage:
// 502 stays, as before.
func TestRemoteConnectionRefusedStays502(t *testing.T) {
	treatAsRemote(t)
	h, _ := refusalRig(t, map[string]config.Model{"fw": modelTo(t, closedPort(t), "chat")}, nil)
	if rec := post(h, "fw"); rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502: %s", rec.Code, rec.Body.String())
	}
}

// A stream that breaks after its first byte cannot change status: the caller
// has its 200 and the stream is cut; the row says so.
func TestMidStreamEOFIsACutStream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n")
		w.(http.Flusher).Flush()
		c, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = c.Close()
		}
	}))
	t.Cleanup(up.Close)
	h, st := refusalRig(t, map[string]config.Model{"swift": modelTo(t, up.URL, "chat")}, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"swift","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Corrallm-Key", "k")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want the 200 already sent", resp.StatusCode)
	}
	var rows []store.Activity
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if rows, _ = st.RecentActivity(store.Window{}, 10, "", "", ""); len(rows) > 0 {
			break
		}
	}
	if len(rows) != 1 || !strings.Contains(rows[0].Error, "stream cut") {
		t.Fatalf("rows %+v, want one row recording the cut", rows)
	}
}

// The drain-restart end to end: a request in flight when the restart starts
// completes, requests that arrive during the drain and the reload are held and
// served by the new process, and nobody gets a 502.
func TestDrainRestartServesEveryRequest(t *testing.T) {
	prev := host.CheckPortFree
	host.CheckPortFree = false // the fake backend holds the port the `sleep` "listens" on
	t.Cleanup(func() { host.CheckPortFree = prev })

	arrived := make(chan struct{}, 16)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" { // /health, the UI probe, ...
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		arrived <- struct{}{}
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatReply))
	}))
	t.Cleanup(up.Close)
	port := up.Listener.Addr().(*net.TCPAddr).Port
	var pn yaml.Node
	if err := pn.Encode(port); err != nil {
		t.Fatal(err)
	}
	model := config.Model{Cmd: "exec sleep 300", Server: "box", RAMUsage: map[string]string{"gpu": "6"},
		Proxy: pn, Type: "chat", MaxConcurrent: 8} // room for every request: a full queue would 429 (backpressure), not hold
	cfg := &config.Config{
		Servers:        map[string]config.Server{"box": {Pools: map[string]string{"gpu": "10"}}},
		Models:         map[string]config.Model{"swift": model},
		PriorityGroups: map[string]config.PriorityGroup{"default": {Weight: 1}},
		Keys:           map[string]config.KeyPolicy{"k": {Group: "default"}},
	}
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr := proc.NewManager(cfg)
	t.Cleanup(mgr.Shutdown)
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	send := func() int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"swift","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("X-Corrallm-Key", "k")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return -1
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if code := send(); code != http.StatusOK { // warm: spawns the backend
		t.Fatalf("warm-up %d", code)
	}
	<-arrived

	codes := make(chan int, 16)
	go func() { codes <- send() }() // in flight when the restart starts
	<-arrived
	restarted := make(chan error, 1)
	go func() {
		res, err := mgr.Restart(context.Background(), "swift", proc.RestartOptions{Reason: "test", DrainTimeout: 10 * time.Second})
		if err == nil && (res.InFlight != 1 || res.Cut != 0) {
			err = fmt.Errorf("result %+v, want 1 in flight, 0 cut", res)
		}
		restarted <- err
	}()
	time.Sleep(50 * time.Millisecond)
	const during = 5
	for i := 0; i < during; i++ {
		go func() { codes <- send() }()
		time.Sleep(40 * time.Millisecond)
	}
	if err := <-restarted; err != nil {
		t.Fatalf("restart: %v", err)
	}
	for i := 0; i < during+1; i++ {
		select {
		case c := <-codes:
			if c != http.StatusOK {
				t.Errorf("request %d got %d, want 200 (held, then served)", i, c)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("a request never finished")
		}
	}
}
