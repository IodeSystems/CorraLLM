package agent

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// backendOn starts a stand-in for llama-server on loopback and returns its port.
func backendOn(t *testing.T, h http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// The backend must see the path the CLIENT sent. If the agent's own prefix
// leaks through, every request 404s at a backend that has never heard of
// /agent/v1/proxy/....
func TestProxyStripsItsOwnPrefix(t *testing.T) {
	var gotPath, gotAuth string
	port := backendOn(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))

	a := New("test", "secret")
	srv := httptest.NewServer(a.Routes())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost,
		srv.URL+proxyPrefix+port+"/v1/chat/completions", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("backend saw %q, want /v1/chat/completions — the agent's prefix leaked", gotPath)
	}
	// Forwarding our own credential would write it into the backend's logs for
	// no benefit; a local llama-server has no use for it.
	if gotAuth != "" {
		t.Errorf("agent token was forwarded to the backend: %q", gotAuth)
	}
}

// The data plane is gated by the same token as the control plane. Without this,
// moving traffic onto the agent's port would hand anyone who can reach it an
// unauthenticated model — the exact hole this change exists to close.
func TestProxyRequiresTheAgentToken(t *testing.T) {
	port := backendOn(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "reached the backend")
	}))

	a := New("test", "secret")
	srv := httptest.NewServer(a.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + proxyPrefix + port + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 — the data plane is unauthenticated", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "reached the backend") {
		t.Error("an unauthenticated request reached the backend")
	}
}

// SSE has to arrive as it is produced. Buffering shows up directly as latency a
// user feels — tokens in clumps rather than as generated — which is why the
// proxy flushes every write instead of on a timer.
func TestProxyStreamsWithoutBuffering(t *testing.T) {
	release := make(chan struct{})
	port := backendOn(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("test backend cannot flush")
			return
		}
		_, _ = io.WriteString(w, "data: first\n\n")
		fl.Flush()
		<-release // hold the response open: a buffering proxy sends nothing yet
		_, _ = io.WriteString(w, "data: second\n\n")
		fl.Flush()
	}))

	a := New("test", "")
	srv := httptest.NewServer(a.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + proxyPrefix + port + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	// The first chunk must be readable while the backend still holds the
	// response open. With buffering this read blocks until release.
	type read struct {
		line string
		err  error
	}
	ch := make(chan read, 1)
	go func() {
		l, err := bufio.NewReader(resp.Body).ReadString('\n')
		ch <- read{l, err}
	}()

	select {
	case got := <-ch:
		if got.err != nil {
			t.Fatalf("reading the first chunk: %v", got.err)
		}
		if !strings.Contains(got.line, "first") {
			t.Errorf("first chunk = %q, want the backend's first event", got.line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first SSE chunk never arrived — the proxy is buffering the stream")
	}
	close(release)
}

// A port that is not a port must be refused before anything is dialled.
func TestProxyRejectsABadPort(t *testing.T) {
	a := New("test", "")
	srv := httptest.NewServer(a.Routes())
	defer srv.Close()

	for _, bad := range []string{"0", "70000", "notaport"} {
		resp, err := http.Get(fmt.Sprintf("%s%s%s/v1/models", srv.URL, proxyPrefix, bad))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("port %q: status = %d, want 400", bad, resp.StatusCode)
		}
	}
}
