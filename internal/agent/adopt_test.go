//go:build unix

package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/host"
)

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func startOn(t *testing.T, srv *httptest.Server, req StartRequest) Backend {
	t.Helper()
	body, _ := json.Marshal(req)
	resp, out := do(t, srv, http.MethodPost, "/agent/v1/backends", "", string(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start: %d %s", resp.StatusCode, out)
	}
	var b Backend
	if err := json.Unmarshal([]byte(out), &b); err != nil {
		t.Fatal(err)
	}
	return b
}

// The behaviour the whole slice exists for: an agent restart costs a running
// backend NOTHING. The next agent finds it, checks it is the same process
// instance running the same argv, and re-registers it under the same id and
// key — so a primary polling that id sees no change at all.
func TestAgentRestart_AdoptsTheRunningBackend(t *testing.T) {
	dir := t.TempDir()

	a1 := New("test", "")
	a1.identityDelays = []time.Duration{20 * time.Millisecond, 100 * time.Millisecond}
	a1.SetStateDir(dir)
	srv1 := httptest.NewServer(a1.Routes())
	b := startOn(t, srv1, StartRequest{Key: "m", Model: "m", ConfigCmd: "cfg-cmd",
		Cmd: "while :; do echo tick; sleep 0.1; done"})
	a1.mu.Lock()
	pgid := pgidOf(a1.backends[b.ID].handle.ID())
	a1.mu.Unlock()
	waitFor(t, "identity recorded", 3*time.Second, func() bool {
		recs := readSupervised(dir)
		return len(recs) == 1 && recs[0].Ident.Start != ""
	})

	// Output goes to a FILE: a pipe into the agent would kill the backend on
	// its first write after the agent is gone.
	logPath := filepath.Join(dir, "logs", b.ID+".log")
	waitFor(t, "log file written", 3*time.Second, func() bool {
		st, err := os.Stat(logPath)
		return err == nil && st.Size() > 0
	})

	a1.Shutdown()
	srv1.Close()
	if !groupIsAlive(pgid) {
		t.Fatal("agent shutdown killed the backend")
	}

	a2 := New("test", "")
	a2.SetStateDir(dir)
	t.Cleanup(a2.StopBackends)
	a2.mu.Lock()
	got := a2.backends[b.ID]
	a2.mu.Unlock()
	if got == nil {
		t.Fatalf("restarted agent did not adopt %s; has %v", b.ID, a2.snapshot())
	}
	if got.key != "m" || got.configCmd != "cfg-cmd" {
		t.Errorf("adopted as key=%q configCmd=%q; the primary matches on both", got.key, got.configCmd)
	}
	if pgidOf(got.handle.ID()) != pgid {
		t.Errorf("adopted pgid %s, want %d (the same process, not a respawn)", got.handle.ID(), pgid)
	}
	// Its output keeps arriving through the new agent.
	waitFor(t, "adopted backend's output in the new agent's ring", 3*time.Second, func() bool {
		lines, _ := got.logs.Since(0)
		return len(lines) > 0 && strings.Contains(lines[len(lines)-1].Line, "tick")
	})
	// And new ids continue past the adopted one rather than reusing it.
	srv2 := httptest.NewServer(a2.Routes())
	defer srv2.Close()
	nb := startOn(t, srv2, StartRequest{Key: "n", Cmd: "exec sleep 30"})
	if nb.ID == b.ID {
		t.Errorf("new backend reused the adopted id %s", b.ID)
	}

	// The explicit full stop still stops an adopted backend.
	a2.StopBackends()
	waitFor(t, "adopted backend stopped", 5*time.Second, func() bool { return !groupIsAlive(pgid) })
}

// An agent stopped before its first scheduled identity look must still leave
// an adoptable record: Shutdown takes that look itself. Found in the sandbox,
// where a stop one second after a spawn got the backend killed on restart.
func TestAgentRestart_ShutdownRecordsIdentityForAFreshBackend(t *testing.T) {
	dir := t.TempDir()

	a1 := New("test", "")
	a1.identityDelays = []time.Duration{time.Hour}
	a1.SetStateDir(dir)
	srv1 := httptest.NewServer(a1.Routes())
	b := startOn(t, srv1, StartRequest{Key: "m", ConfigCmd: "c", Cmd: "exec sleep 300"})
	a1.mu.Lock()
	pgid := pgidOf(a1.backends[b.ID].handle.ID())
	a1.mu.Unlock()
	waitFor(t, "sleep exec", 3*time.Second, func() bool {
		id, err := host.ProcIdentity(pgid)
		return err == nil && strings.Contains(id.Cmdline, "sleep")
	})
	a1.Shutdown()
	srv1.Close()

	a2 := New("test", "")
	a2.SetStateDir(dir)
	t.Cleanup(a2.StopBackends)
	a2.mu.Lock()
	got := a2.backends[b.ID]
	a2.mu.Unlock()
	if got == nil || !groupIsAlive(pgid) {
		t.Fatal("a backend stopped before its first identity look was not adopted")
	}
}

func groupIsAlive(pgid int) bool { return syscall.Kill(-pgid, 0) == nil }

// startSleeper runs a process in its own group that nothing in the agent
// started, the way a previous agent's backend looks to a new one.
func startSleeper(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	go func() { _ = cmd.Wait() }()
	pid := cmd.Process.Pid
	// Right after fork the child has not exec'd yet and has no argv to read.
	waitFor(t, "sleeper exec", 3*time.Second, func() bool {
		id, err := host.ProcIdentity(pid)
		return err == nil && strings.Contains(id.Cmdline, "sleep")
	})
	return pid
}

// Each way a record can partly match, and the one answer each must get.
// Adopting a wrong process serves traffic from something nobody configured;
// killing a stranger under a reused pid is worse. Only a full match adopts.
func TestRecover_AdoptsOnlyAFullMatch(t *testing.T) {
	answers := true
	oldPort := portAnswers
	portAnswers = func(int) bool { return answers }
	t.Cleanup(func() { portAnswers = oldPort })

	cases := []struct {
		name    string
		mutate  func(*supervisedRec)
		answers bool
		want    verdict
	}{
		{"full match", func(*supervisedRec) {}, true, verdictAdopt},
		{"argv changed since recorded", func(r *supervisedRec) { r.Ident.Cmdline = "something else" }, true, verdictKill},
		{"pid reused by another instance", func(r *supervisedRec) { r.Ident.Start = "other-boot:1" }, true, verdictLeave},
		{"not answering on its port", func(*supervisedRec) {}, false, verdictKill},
		{"no id to re-register under", func(r *supervisedRec) { r.ID = "" }, true, verdictKill},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pid := startSleeper(t)
			id, err := host.ProcIdentity(pid)
			if err != nil {
				t.Fatal(err)
			}
			r := supervisedRec{PGID: pid, Cmd: "exec sleep 300", ID: "b1-x", Key: "m", Port: 5999, Ident: id}
			c.mutate(&r)
			answers = c.answers
			if got := judge(r); got != c.want {
				t.Errorf("judge = %v, want %v", got, c.want)
			}
		})
	}
}

// Recover acts on the verdicts: a partial match is killed, a full match stays
// recorded and is returned, a dead one is forgotten.
func TestRecover_KillsPartialMatchesAndKeepsAdoptable(t *testing.T) {
	oldPort := portAnswers
	portAnswers = func(int) bool { return true }
	t.Cleanup(func() { portAnswers = oldPort })
	dir := t.TempDir()

	keep := startSleeper(t)
	kill := startSleeper(t)
	idKeep, _ := host.ProcIdentity(keep)
	idKill, _ := host.ProcIdentity(kill)
	idKill.Cmdline = "not what it runs"
	recordSupervised(dir, supervisedRec{PGID: keep, Cmd: "exec sleep 300", ID: "b1-k", Key: "a", Ident: idKeep})
	recordSupervised(dir, supervisedRec{PGID: kill, Cmd: "exec sleep 300", ID: "b2-k", Key: "b", Ident: idKill})
	recordSupervised(dir, supervisedRec{PGID: 0x7FFFFFF0, Cmd: "exec sleep 300", ID: "b3-k", Key: "c"})

	adopt, killed := Recover(dir)
	if len(adopt) != 1 || adopt[0].PGID != keep || killed != 1 {
		t.Fatalf("adopt=%+v killed=%d; want only %d adopted and one killed", adopt, killed, keep)
	}
	waitFor(t, "partial match killed", 3*time.Second, func() bool { return !groupIsAlive(kill) })
	if !groupIsAlive(keep) {
		t.Error("the adoptable backend was killed")
	}
	if recs := readSupervised(dir); len(recs) != 1 || recs[0].PGID != keep {
		t.Errorf("note after recovery = %+v; want only the adopted backend", recs)
	}
}

// An agent's log file must not grow without bound, and rotating it must not
// stop the tail: the child keeps appending to the same file.
func TestTailLog_CapsAndKeepsTailing(t *testing.T) {
	const capBytes = 4 << 10
	path := filepath.Join(t.TempDir(), "b.log")
	f, err := openBackendLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ring := newLogRing(1000)
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() { tailLog(path, ring, false, done, capBytes); close(finished) }()

	line := strings.Repeat("x", 99) + "\n"
	for i := 0; i < 100; i++ { // 10 KB: past the 4 KB cap twice over
		_, _ = f.WriteString(line)
	}
	waitFor(t, "rotation", 3*time.Second, func() bool {
		st, err := os.Stat(path)
		return err == nil && st.Size() < capBytes
	})
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("no rotated copy kept: %v", err)
	}
	_, _ = f.WriteString("after-rotation\n")
	waitFor(t, "a line written after rotation", 3*time.Second, func() bool {
		lines, _ := ring.Since(0)
		return len(lines) > 0 && lines[len(lines)-1].Line == "after-rotation"
	})
	close(done)
	<-finished
}

// Ids must not repeat across agent lifetimes: a primary still holding a
// handle for a previous agent's "b1" would otherwise poll, signal and read the
// logs of an unrelated new backend.
func TestBackendIDsAreUniqueAcrossBoots(t *testing.T) {
	a := newBackendID(1, time.UnixMilli(1000))
	b := newBackendID(1, time.UnixMilli(2000))
	if a == b {
		t.Errorf("same id %q for two boots", a)
	}
	if backendSeq(a) != 1 || backendSeq("b12-zz") != 12 || backendSeq("x") != 0 {
		t.Error("backendSeq does not round-trip")
	}
}
