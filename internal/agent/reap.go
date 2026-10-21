package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/corrallm/internal/host"
)

// A backend outlives the agent that started it — on purpose, now.
//
// Loading a model costs minutes and tens of GB; restarting the agent (a crash,
// a manual stop, start.sh's loop, a self-update) used to kill every model on
// the machine, so a code deploy cost every warm model a cold load. Backends are
// therefore started detached (their own session, output to a file rather than
// a pipe into the agent) and the agent leaves them running when it exits. The
// next agent ADOPTS them: same id, same key, so the primary sees nothing change.
//
// That makes this file the most safety-critical part of the agent. An
// unaccounted-for process holding tens of GB is the worst state in the system
// (observed: llama-server pid 39015 with ppid 1, holding 33.5 GB, answering on
// its port while the agent reported nothing alive). So a record is adopted only
// when EVERYTHING matches — the process instance (start time + argv, which
// defeats pid reuse) and the port it should answer on — and anything that only
// partly matches is killed, exactly as every leftover was before adoption
// existed. A record that cannot be verified at all is left alone: leaking one
// backend is recoverable, killing a stranger whose pid we once held is not.
//
// The note has to survive the agent, so it lives on disk beside the binary.

// supervisedFile is where the note is kept, beside the agent's own binary.
const supervisedFile = "supervised.json"

// supervisedRec records one process group well enough to find it again.
//
// PGID and Cmd are the original fields; a record carrying only those (written
// by an agent older than adoption) can never be adopted and is reaped the old
// way. Everything after them is what adoption needs to rebuild the backend as
// the primary knew it.
type supervisedRec struct {
	PGID int    `json:"pgid"`
	Cmd  string `json:"cmd"`

	ID        string `json:"id,omitempty"`
	Key       string `json:"key,omitempty"`
	Model     string `json:"model,omitempty"`
	ConfigCmd string `json:"configCmd,omitempty"`
	Port      int    `json:"port,omitempty"`
	Started   int64  `json:"started,omitempty"` // unix millis
	Log       string `json:"log,omitempty"`
	// Ident is the leader's identity, observed AFTER the spawn — see
	// observeIdentity for why it cannot be known at spawn time.
	Ident host.Identity `json:"ident,omitzero"`
}

var supervisedMu sync.Mutex

// recordSupervised adds or replaces the record for rec.PGID. Best-effort: an
// unwritable state directory must not stop a backend starting.
func recordSupervised(dir string, rec supervisedRec) {
	if dir == "" || rec.PGID <= 0 {
		return
	}
	supervisedMu.Lock()
	defer supervisedMu.Unlock()
	recs := readSupervised(dir)
	for i, r := range recs {
		if r.PGID == rec.PGID {
			recs[i] = rec
			writeSupervised(dir, recs)
			return
		}
	}
	writeSupervised(dir, append(recs, rec))
}

// RecordSupervised notes that pgid is ours, so a future agent can find it.
func RecordSupervised(dir string, pgid int, cmd string) {
	recordSupervised(dir, supervisedRec{PGID: pgid, Cmd: cmd})
}

// updateIdentity stores an observed identity on an existing record. A record
// that is gone (the backend exited meanwhile) is not resurrected.
func updateIdentity(dir string, pgid int, id host.Identity) {
	if dir == "" || pgid <= 0 {
		return
	}
	supervisedMu.Lock()
	defer supervisedMu.Unlock()
	recs := readSupervised(dir)
	for i, r := range recs {
		if r.PGID == pgid {
			if r.Ident == id {
				return
			}
			recs[i].Ident = id
			writeSupervised(dir, recs)
			return
		}
	}
}

// ForgetSupervised drops a group that has exited.
func ForgetSupervised(dir string, pgid int) {
	if dir == "" || pgid <= 0 {
		return
	}
	supervisedMu.Lock()
	defer supervisedMu.Unlock()
	recs := readSupervised(dir)
	out := recs[:0]
	for _, r := range recs {
		if r.PGID != pgid {
			out = append(out, r)
		}
	}
	writeSupervised(dir, out)
}

// verdict is what startup decides about one record.
type verdict int

const (
	verdictGone  verdict = iota // not running; drop the note
	verdictAdopt                // ours, intact, answering — keep it
	verdictKill                 // ours or partly ours, not adoptable — stop it
	verdictLeave                // cannot tell whose it is — do not touch
)

// portAnswers is swappable so tests do not need a listener per case.
var portAnswers = func(port int) bool {
	// Retried for a while: an agent restarting right after a spawn can meet a
	// backend that has not bound yet. llama-server binds before it loads, so
	// in the normal case the first dial already succeeds.
	deadline := time.Now().Add(adoptPortWait)
	for {
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
		if err == nil {
			_ = c.Close()
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// adoptPortWait bounds how long startup waits for a recorded backend's port.
const adoptPortWait = 10 * time.Second

// procIdentity is swappable for tests; see host.ProcIdentity.
var procIdentity = host.ProcIdentity

// judge decides one record. Pure apart from the probes, which tests replace.
func judge(r supervisedRec) verdict {
	if !pidAlive(r.PGID) {
		return verdictGone
	}
	live, err := procIdentity(r.PGID)
	if r.Ident.Start != "" && err == nil {
		switch {
		case live.Start != r.Ident.Start:
			// A different process instance under a reused pid. Not ours —
			// and never killed, whatever its command line says.
			return verdictLeave
		case live.Cmdline != r.Ident.Cmdline:
			// Same instance, different argv: it exec'd into something else
			// after we looked. Ours, and not what was recorded.
			return verdictKill
		}
		port := r.Port
		if port == 0 {
			port = portFromCmd(r.Cmd)
		}
		if port > 0 && !portAnswers(port) {
			return verdictKill // alive and ours, but not serving: a wedged backend
		}
		if r.ID == "" || r.Key == "" {
			return verdictKill // nothing to re-register it under
		}
		return verdictAdopt
	}
	// No usable identity: an older agent's note, a backend that died before
	// its identity was observed, or a platform without one. Fall back to the
	// pre-adoption rule — kill it if the command still looks like ours.
	if stillOurs(r) {
		return verdictKill
	}
	return verdictLeave
}

// Recover settles every record a previous agent left: adoptable ones are
// returned (and stay in the note), the rest are killed or forgotten.
//
// Called once at startup, BEFORE anything is spawned and before the first
// heartbeat, so the primary's first view of this machine already includes the
// adopted backends — otherwise it would see them vanish and come back.
func Recover(dir string) (adopt []supervisedRec, killed int) {
	if dir == "" {
		return nil, 0
	}
	supervisedMu.Lock()
	recs := readSupervised(dir)
	supervisedMu.Unlock()
	var keep []supervisedRec
	for _, r := range recs {
		switch judge(r) {
		case verdictAdopt:
			adopt = append(adopt, r)
			keep = append(keep, r)
		case verdictKill:
			slog.Warn("agent: killing a backend left by a previous agent (not adoptable)",
				"pgid", r.PGID, "id", r.ID, "key", r.Key, "cmd", firstToken(r.Cmd))
			// The group, not the leader: the shell may already be gone while
			// the process actually holding the memory is its child.
			_ = signalStaleGroup(r.PGID)
			killed++
		case verdictLeave:
			slog.Warn("agent: a recorded backend pid is alive but cannot be confirmed as ours; leaving it",
				"pgid", r.PGID, "cmd", firstToken(r.Cmd))
		}
	}
	supervisedMu.Lock()
	writeSupervised(dir, keep)
	supervisedMu.Unlock()
	return adopt, killed
}

// ReapStale kills everything a previous agent left, adopting nothing: the
// full-stop counterpart of Recover, for a caller that wants a clean machine.
func ReapStale(dir string) int {
	adopt, killed := Recover(dir)
	for _, r := range adopt {
		_ = signalStaleGroup(r.PGID)
		killed++
	}
	if len(adopt) > 0 {
		supervisedMu.Lock()
		writeSupervised(dir, nil)
		supervisedMu.Unlock()
	}
	return killed
}

// stillOurs reports whether pgid is alive AND still running what we recorded.
//
// The command check is the safety catch on pid reuse for records that carry no
// identity. Without it, a stale entry from days ago could name a pid the OS has
// since handed to something the operator cares about.
func stillOurs(r supervisedRec) bool {
	if !pidAlive(r.PGID) {
		return false // no such process
	}
	out, err := exec.Command(psCommand(r.PGID)[0], psCommand(r.PGID)[1:]...).Output()
	if err != nil {
		return false // cannot confirm; leaking is better than killing a stranger
	}
	live := strings.TrimSpace(string(out))
	want := firstToken(r.Cmd)
	return want != "" && strings.Contains(live, want)
}

// firstToken is the executable path from a shell command, which is the most
// identifying part that survives into ps output.
func firstToken(cmd string) string {
	f := strings.Fields(strings.TrimSpace(cmd))
	if len(f) == 0 {
		return ""
	}
	if f[0] == "exec" && len(f) > 1 {
		return f[1] // `exec llama-server …` — the shell is replaced by the real one
	}
	return f[0]
}

var rePort = regexp.MustCompile(`(?:^|\s)--port[=\s]+(\d+)`)

// portFromCmd reads `--port N` out of a command, for records from a primary
// that did not send the port. 0 when there is none.
func portFromCmd(cmd string) int {
	m := rePort.FindStringSubmatch(cmd)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func readSupervised(dir string) []supervisedRec {
	f, err := os.Open(filepath.Join(dir, supervisedFile))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []supervisedRec
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20) // an argv can be long
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r supervisedRec
		if json.Unmarshal([]byte(line), &r) == nil && r.PGID > 0 {
			out = append(out, r)
		}
	}
	return out
}

// writeSupervised replaces the file atomically. A torn note is worse than none:
// it could name a pgid without its command and disable the reuse check.
func writeSupervised(dir string, recs []supervisedRec) {
	path := filepath.Join(dir, supervisedFile)
	if len(recs) == 0 {
		_ = os.Remove(path)
		return
	}
	var b strings.Builder
	for _, r := range recs {
		j, err := json.Marshal(r)
		if err != nil {
			continue
		}
		b.Write(j)
		b.WriteByte('\n')
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		slog.Debug("agent: could not record supervised backends", "err", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		slog.Debug("agent: could not record supervised backends", "err", err)
	}
}

// defaultIdentityDelays is when, after a spawn, the leader's identity is
// (re)read. Per Server (Server.identityDelays) so tests can shorten it.
//
// It cannot be read at spawn time: the leader starts as `sh -c "<cmd>"` and,
// for the usual `exec llama-server …`, replaces itself a few milliseconds
// later. The first look is after that has almost always happened; the later
// ones catch a command that sleeps or does work before it execs, and Shutdown
// takes a last look (refreshIdentities). A backend that changes its argv
// after the last look is killed on adoption rather than adopted — the safe
// direction.
var defaultIdentityDelays = []time.Duration{100 * time.Millisecond, time.Second, 5 * time.Second, 30 * time.Second}

// observeIdentity records the leader's identity on the schedule above, until
// the backend exits.
func observeIdentity(dir string, pgid int, done <-chan struct{}, delays []time.Duration) {
	if dir == "" || pgid <= 0 {
		return
	}
	var waited time.Duration
	for _, d := range delays {
		select {
		case <-done:
			return
		case <-time.After(d - waited):
		}
		waited = d
		id, err := procIdentity(pgid)
		if err != nil {
			continue
		}
		updateIdentity(dir, pgid, id)
	}
}

// backendSeq parses the counter out of an id from newBackendID, so a restarted
// agent keeps counting past what it adopted. 0 when it is not one of ours.
func backendSeq(id string) int {
	rest, ok := strings.CutPrefix(id, "b")
	if !ok {
		return 0
	}
	num, _, _ := strings.Cut(rest, "-")
	n, _ := strconv.Atoi(num)
	return n
}

// newBackendID names a backend uniquely across agent lifetimes. The boot stamp
// is what makes that true: a restarted agent that adopted nothing starts its
// counter again, and a bare "b1" would then name a new process to a primary
// still holding a handle for the old b1 — it would poll, signal and read logs
// of the wrong backend.
func newBackendID(seq int, booted time.Time) string {
	return fmt.Sprintf("b%d-%s", seq, strconv.FormatInt(booted.UnixMilli(), 36))
}
