//go:build unix

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/toolchain"
)

// hangingRemote accepts connections and never answers, standing in for a git
// remote that stalls. It reports each accepted connection and, later, when
// that connection was closed by the far side — i.e. when the git process
// holding it died.
func hangingRemote(t *testing.T) (url string, accepted <-chan net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	ch := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			ch <- c
		}
	}()
	return "http://" + ln.Addr().String() + "/x.git", ch
}

// A tool run is a job: when the primary's connection for it drops, the run
// and EVERYTHING it started must die. CommandContext alone killed only the
// recipe's bash; its git/cmake children kept running (and kept cmd.Run from
// returning) until they finished on their own.
func TestToolRun_CancelledWhenThePrimaryHangsUp(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	_, srv := newTestAgent(t, "")
	remote, accepted := hangingRemote(t)

	spec := toolchain.Spec{Name: "llama.cpp", Recipe: "llama.cpp", URL: remote, Ref: "master",
		Bin: "llama-server", InstalledAt: t.TempDir()}
	body, _ := json.Marshal(ToolRunRequest{Spec: spec, Verb: string(toolchain.VerbUpstream)})

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/agent/v1/tools/run", bytes.NewReader(body))
	req.Header.Set(ProtocolHeader, strconv.Itoa(Protocol))
	errc := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		errc <- err
	}()

	var conn net.Conn
	select {
	case conn = <-accepted:
	case <-time.After(15 * time.Second):
		t.Fatal("the recipe never reached the remote")
	}
	defer conn.Close()

	cancel() // the primary goes away
	<-errc

	// git holds the connection; it closing is git dying.
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Read(make([]byte, 1<<16)); err == nil {
		// A request line arrived; keep reading until EOF or the deadline.
		for err == nil {
			_, err = conn.Read(make([]byte, 1<<16))
		}
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Fatal("git was still connected 10s after the primary hung up — the tool run was not cancelled")
		}
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("git was still connected 10s after the primary hung up — the tool run was not cancelled")
	}
}

// The same for the agent shutting down: jobs die with whoever started them.
func TestToolRun_CancelledWhenTheAgentShutsDown(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	a, srv := newTestAgent(t, "")
	remote, accepted := hangingRemote(t)
	spec := toolchain.Spec{Name: "llama.cpp", Recipe: "llama.cpp", URL: remote, Ref: "master",
		Bin: "llama-server", InstalledAt: t.TempDir()}
	body, _ := json.Marshal(ToolRunRequest{Spec: spec, Verb: string(toolchain.VerbUpstream)})
	go func() { _, _ = http.Post(srv.URL+"/agent/v1/tools/run", "application/json", bytes.NewReader(body)) }()

	var conn net.Conn
	select {
	case conn = <-accepted:
	case <-time.After(15 * time.Second):
		t.Fatal("the recipe never reached the remote")
	}
	defer conn.Close()

	start := time.Now()
	a.Shutdown()
	if time.Since(start) > 9*time.Second {
		t.Errorf("Shutdown waited %s for a job that should have been cancelled", time.Since(start))
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var err error
	for err == nil {
		_, err = conn.Read(make([]byte, 1<<16))
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("git still connected after agent shutdown — the job outlived the agent")
	}
}
