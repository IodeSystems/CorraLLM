//go:build unix

package handoff

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestHelperSuccessor is the successor half: the test binary re-executed by
// Start. It serves its pid on the inherited listener, says ready, echoes each
// status to stdout and exits when its predecessor is gone.
func TestHelperSuccessor(t *testing.T) {
	if os.Getenv(envFDs) == "" {
		t.Skip("successor half of the handoff tests")
	}
	s, err := Inherit()
	if err != nil || s == nil {
		fmt.Println("inherit failed:", err)
		os.Exit(3)
	}
	go func() {
		_ = http.Serve(s.Listener, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, os.Getpid())
		}))
	}()
	if err := s.Ready(); err != nil {
		os.Exit(4)
	}
	s.Watch(func(st Status) {
		fmt.Printf("status draining=%v active=%v local=%d\n", st.Draining, st.Active, st.Local)
	},
		func() { os.Exit(0) })
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// The whole exchange: a client hammering the port across the handover never
// sees a refused connection, new connections reach the successor once the
// predecessor stops accepting, the status arrives, and the successor learns
// when the predecessor is gone.
func TestHandoff_ListenerMovesWithoutRefusals(t *testing.T) {
	ln := listen(t)
	addr := ln.Addr().String()
	oldSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, os.Getpid())
	})}
	go func() { _ = oldSrv.Serve(ln) }()

	var refused, served atomic.Int64
	pids := sync.Map{}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cli := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 5 * time.Second}
			for {
				select {
				case <-stop:
					return
				default:
				}
				resp, err := cli.Get("http://" + addr + "/")
				if err != nil {
					refused.Add(1)
					t.Log(err)
					continue
				}
				b, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				pids.Store(string(b), true)
				served.Add(1)
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)

	// Start's child writes to our stdout; capture it to read the statuses.
	r, w, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = w
	p, err := Start(os.Args[0], []string{"-test.run=^TestHelperSuccessor$"}, ln, 20*time.Second)
	os.Stdout = stdout
	_ = w.Close()
	if err != nil {
		t.Fatal(err)
	}
	// The predecessor stops accepting: from here only the successor answers.
	// NOT Shutdown, which drops a request read after it begins (net/http
	// returns without answering); see StopAccepting.
	StopAccepting(oldSrv, ln)
	defer func() { _ = oldSrv.Shutdown(context.Background()) }()
	time.Sleep(200 * time.Millisecond)
	if err := p.Send(Status{Draining: true, Active: map[string]int{"m": 1}, Local: 1}); err != nil {
		t.Fatal(err)
	}
	if err := p.Send(Status{Draining: true, Local: 0}); err != nil {
		t.Fatal(err)
	}
	close(stop)
	wg.Wait()

	if refused.Load() != 0 {
		t.Errorf("%d requests failed across the handover (of %d)", refused.Load(), served.Load()+refused.Load())
	}
	if _, ok := pids.Load(strconv.Itoa(p.PID())); !ok {
		t.Error("no request reached the successor")
	}
	// The predecessor exits (its end of the control socket closes).
	_ = p.conn.Close()
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("successor did not notice its predecessor was gone")
	}
	sc := bufio.NewScanner(r)
	var lines []string
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "status") {
			lines = append(lines, sc.Text())
		}
	}
	want := []string{"status draining=true active=map[m:1] local=1", "status draining=true active=map[] local=0"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Errorf("successor saw %q, want %q", lines, want)
	}
}

// A successor that cannot start leaves the predecessor exactly as it was:
// error returned (with what the successor said), listener still accepting.
func TestHandoff_FailedStartKeepsServing(t *testing.T) {
	ln := listen(t)
	defer func() { _ = ln.Close() }()
	_, err := Start("/bin/sh", []string{"-c", "echo 'unknown flag: --bogus' >&2; exit 1"}, ln, 10*time.Second)
	if err == nil || !strings.Contains(err.Error(), "exited before it was ready") || !strings.Contains(err.Error(), "unknown flag: --bogus") {
		t.Fatalf("err = %v", err)
	}
	go func() {
		c, err := ln.Accept()
		if err == nil {
			_ = c.Close()
		}
	}()
	c, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("listener no longer accepting after a failed handoff: %v", err)
	}
	_ = c.Close()
}

// A successor that never says ready is killed at the timeout.
func TestHandoff_ReadyTimeout(t *testing.T) {
	ln := listen(t)
	defer func() { _ = ln.Close() }()
	start := time.Now()
	_, err := Start("/bin/sleep", []string{"30"}, ln, 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "not ready within") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("timeout not honoured")
	}
}

// Under a systemd unit with no notify socket a handoff is refused up front.
func TestCheckSupervisor(t *testing.T) {
	t.Setenv("INVOCATION_ID", "x")
	t.Setenv("NOTIFY_SOCKET", "")
	if err := CheckSupervisor(); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Type=simple unit: %v", err)
	}
	t.Setenv("NOTIFY_SOCKET", "/run/user/1/systemd/notify")
	if err := CheckSupervisor(); err != nil {
		t.Errorf("notify unit: %v", err)
	}
	t.Setenv("INVOCATION_ID", "")
	t.Setenv("NOTIFY_SOCKET", "")
	if err := CheckSupervisor(); err != nil {
		t.Errorf("not under systemd: %v", err)
	}
}

// Notify writes the datagram systemd reads.
func TestNotify(t *testing.T) {
	path := t.TempDir() + "/notify"
	c, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	t.Setenv("NOTIFY_SOCKET", path)
	if ok, err := Notify("MAINPID=42\nREADY=1"); !ok || err != nil {
		t.Fatalf("notify = %v, %v", ok, err)
	}
	buf := make([]byte, 128)
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	n, _, err := c.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "MAINPID=42\nREADY=1" {
		t.Errorf("got %q, %v", buf[:n], err)
	}
}
