package agent

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// A backend can outlive the agent that started it, and until now nothing could
// ever clean that up.
//
// ReconcileAgent reaps backends the agent REPORTS and the primary does not
// claim. A process whose agent died is reported by nobody: the replacement
// agent has an empty table, so the primary sees no orphan to reap and the
// agent has no handle to kill. It just sits there holding tens of gigabytes.
// Its own comment calls that "the worst state in the system" — and it was
// reachable by restarting the agent, which `start.sh` does on every crash and
// every manual stop.
//
// Observed: llama-server pid 39015 with ppid 1, holding 33.5 GB, while the
// agent supervising that machine reported eight backends and all of them dead.
// The model still answered on its port, so the primary believed it was healthy
// while being unable to measure or stop it.
//
// The fix is a note-to-self on disk. Each spawn records its process group; each
// exit removes it; startup kills whatever is still alive from last time. It has
// to survive the agent, so it cannot live in memory.

// supervisedFile is where the note is kept, beside the agent's own binary.
const supervisedFile = "supervised.json"

// supervised records one process group well enough to identify it later.
//
// Cmd is stored because PIDS ARE REUSED. Killing a bare pgid from a previous
// boot could hit an unrelated process that happens to have inherited the
// number, so the command is compared before anything is signalled — a
// false negative leaks one backend, a false positive kills a stranger.
type supervisedRec struct {
	PGID int    `json:"pgid"`
	Cmd  string `json:"cmd"`
}

var supervisedMu sync.Mutex

// RecordSupervised notes that pgid is ours, so a future agent can clean it up.
// Best-effort: an unwritable state directory must not stop a backend starting.
func RecordSupervised(dir string, pgid int, cmd string) {
	if dir == "" || pgid <= 0 {
		return
	}
	supervisedMu.Lock()
	defer supervisedMu.Unlock()
	recs := readSupervised(dir)
	for _, r := range recs {
		if r.PGID == pgid {
			return
		}
	}
	writeSupervised(dir, append(recs, supervisedRec{PGID: pgid, Cmd: cmd}))
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

// ReapStale kills process groups left behind by a previous agent.
//
// Called once at startup, BEFORE anything is spawned, so a restarted agent
// starts from a clean machine rather than competing with its own ghosts for
// memory and ports. Returns how many it stopped, for the log.
func ReapStale(dir string) int {
	if dir == "" {
		return 0
	}
	supervisedMu.Lock()
	defer supervisedMu.Unlock()
	recs := readSupervised(dir)
	if len(recs) == 0 {
		return 0
	}
	killed := 0
	for _, r := range recs {
		if !stillOurs(r) {
			continue // exited already, or the pid was reused by something else
		}
		slog.Warn("agent: killing a backend left over from a previous agent",
			"pgid", r.PGID, "cmd", firstToken(r.Cmd))
		// The group, not the leader: the shell may already be gone while the
		// process actually holding the memory is its child.
		_ = signalStaleGroup(r.PGID)
		killed++
	}
	writeSupervised(dir, nil)
	return killed
}

// stillOurs reports whether pgid is alive AND still running what we recorded.
//
// The command check is the safety catch on pid reuse. Without it, a stale entry
// from days ago could name a pid the OS has since handed to something the
// operator cares about.
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

func readSupervised(dir string) []supervisedRec {
	f, err := os.Open(filepath.Join(dir, supervisedFile))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []supervisedRec
	sc := bufio.NewScanner(f)
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
