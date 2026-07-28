package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newTestAgent(t *testing.T, token string) (*Server, *httptest.Server) {
	t.Helper()
	a := New("test", token)
	srv := httptest.NewServer(a.Routes())
	t.Cleanup(func() {
		a.Shutdown()
		srv.Close()
	})
	return a, srv
}

func do(t *testing.T, srv *httptest.Server, method, path, token, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set(ProtocolHeader, strconv.Itoa(Protocol))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp, buf.String()
}

// This endpoint runs shell commands. An unauthenticated one is a remote shell,
// so every route must refuse without the token — not just the spawn route.
func TestAgent_RequiresToken(t *testing.T) {
	_, srv := newTestAgent(t, "s3cret")
	for _, p := range []string{"/agent/v1/hello", "/agent/v1/capacity", "/agent/v1/backends"} {
		if resp, _ := do(t, srv, http.MethodGet, p, "", ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without a token = %d, want 401", p, resp.StatusCode)
		}
		if resp, _ := do(t, srv, http.MethodGet, p, "wrong", ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s with a wrong token = %d, want 401", p, resp.StatusCode)
		}
	}
	if resp, _ := do(t, srv, http.MethodGet, "/agent/v1/hello", "s3cret", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("hello with the right token = %d, want 200", resp.StatusCode)
	}
}

// A primary speaking a version this agent does not understand could ask for a
// backend the agent cannot later describe or reap. Refuse before spawning.
func TestAgent_RejectsUnknownProtocol(t *testing.T) {
	_, srv := newTestAgent(t, "")
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/agent/v1/hello", nil)
	req.Header.Set(ProtocolHeader, "999")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an unknown protocol", resp.StatusCode)
	}
}

func TestAgent_HelloIdentifiesTheMachine(t *testing.T) {
	_, srv := newTestAgent(t, "")
	_, body := do(t, srv, http.MethodGet, "/agent/v1/hello", "", "")
	var h Hello
	if err := json.Unmarshal([]byte(body), &h); err != nil {
		t.Fatal(err)
	}
	if h.Protocol != Protocol || h.OS == "" || h.Arch == "" || h.Booted == 0 {
		t.Errorf("hello = %+v, want protocol/os/arch/booted populated", h)
	}
}

// The full lifecycle the primary depends on: spawn, see it listed and alive,
// read its output by sequence, signal it, watch it exit.
func TestAgent_SpawnListLogsSignal(t *testing.T) {
	_, srv := newTestAgent(t, "")

	_, body := do(t, srv, http.MethodPost, "/agent/v1/backends", "",
		`{"key":"extension:x","model":"m","cmd":"echo hello-from-backend; sleep 30"}`)
	var b Backend
	if err := json.Unmarshal([]byte(body), &b); err != nil {
		t.Fatalf("start: %v (%s)", err, body)
	}
	if b.ID == "" || b.Key != "extension:x" {
		t.Fatalf("start returned %+v", b)
	}

	// Listed, and carrying the key the primary gave it — that key is what lets
	// a reconnecting primary match what it thinks is running against reality.
	_, body = do(t, srv, http.MethodGet, "/agent/v1/backends", "", "")
	var listed struct {
		Backends []Backend `json:"backends"`
	}
	if err := json.Unmarshal([]byte(body), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Backends) != 1 || listed.Backends[0].Key != "extension:x" {
		t.Fatalf("list = %+v", listed.Backends)
	}

	// Output shows up with sequence numbers, so a primary can ask for exactly
	// what it missed rather than re-reading or losing the banner.
	var lines struct {
		Lines []LogLine `json:"lines"`
		Next  int64     `json:"next"`
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, body = do(t, srv, http.MethodGet, "/agent/v1/backends/"+b.ID+"/logs?from=0", "", "")
		if err := json.Unmarshal([]byte(body), &lines); err != nil {
			t.Fatal(err)
		}
		if len(lines.Lines) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(lines.Lines) == 0 || !strings.Contains(lines.Lines[0].Line, "hello-from-backend") {
		t.Fatalf("logs = %+v, want the backend's output", lines.Lines)
	}
	if lines.Lines[0].Seq != 1 {
		t.Errorf("first line seq = %d, want 1", lines.Lines[0].Seq)
	}
	// Asking from past the end yields nothing, not a replay.
	_, body = do(t, srv, http.MethodGet, "/agent/v1/backends/"+b.ID+"/logs?from="+strconv.FormatInt(lines.Next, 10), "", "")
	var after struct {
		Lines []LogLine `json:"lines"`
	}
	_ = json.Unmarshal([]byte(body), &after)
	if len(after.Lines) != 0 {
		t.Errorf("from=next returned %d lines, want 0", len(after.Lines))
	}

	// Signal, then it should stop being alive.
	if resp, _ := do(t, srv, http.MethodPost, "/agent/v1/backends/"+b.ID+"/signal", "", `{"sig":"term"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("signal status = %d", resp.StatusCode)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, body = do(t, srv, http.MethodGet, "/agent/v1/backends/"+b.ID, "", "")
		var st Backend
		_ = json.Unmarshal([]byte(body), &st)
		if !st.Alive {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("backend still alive after SIGTERM")
}

func TestAgent_StartRejectsEmptyCmd(t *testing.T) {
	_, srv := newTestAgent(t, "")
	if resp, _ := do(t, srv, http.MethodPost, "/agent/v1/backends", "", `{"key":"k"}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an empty cmd", resp.StatusCode)
	}
}

// An agent that exits leaving backends running strands them: the primary's
// handles are gone and nothing will ever reap them.
func TestAgent_ShutdownStopsBackends(t *testing.T) {
	a, srv := newTestAgent(t, "")
	_, body := do(t, srv, http.MethodPost, "/agent/v1/backends", "", `{"key":"k","cmd":"exec sleep 30"}`)
	var b Backend
	if err := json.Unmarshal([]byte(body), &b); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	sup := a.backends[b.ID]
	a.mu.Unlock()
	if sup == nil || !sup.handle.Alive() {
		t.Fatal("precondition: backend should be running")
	}
	a.Shutdown()
	if sup.handle.Alive() {
		t.Error("backend survived agent shutdown — it would be stranded")
	}
}
