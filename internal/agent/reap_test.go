package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// The note has to survive the agent, because the whole point is cleaning up
// after a process that is gone.
func TestSupervisedRecordSurvivesAndForgets(t *testing.T) {
	dir := t.TempDir()

	RecordSupervised(dir, 4242, "/usr/bin/llama-server --port 5800")
	RecordSupervised(dir, 4243, "exec /usr/bin/other")
	if got := readSupervised(dir); len(got) != 2 {
		t.Fatalf("recorded %d groups, want 2", len(got))
	}

	// Recording the same group twice must not duplicate it: a spawn that is
	// retried should not leave two notes, or forgetting once leaves a ghost.
	RecordSupervised(dir, 4242, "/usr/bin/llama-server --port 5800")
	if got := readSupervised(dir); len(got) != 2 {
		t.Errorf("duplicate record created %d entries, want 2", len(got))
	}

	ForgetSupervised(dir, 4242)
	got := readSupervised(dir)
	if len(got) != 1 || got[0].PGID != 4243 {
		t.Errorf("after forgetting 4242, got %+v", got)
	}

	ForgetSupervised(dir, 4243)
	if _, err := os.Stat(filepath.Join(dir, supervisedFile)); !os.IsNotExist(err) {
		t.Error("the note should be removed once nothing is supervised")
	}
}

// PIDs are reused. A stale note naming a number the OS has since handed to
// something else must never get that process killed — leaking one backend is
// recoverable, killing a stranger is not.
func TestStaleEntryIsNotKilledWhenTheCommandDiffers(t *testing.T) {
	// This process is certainly alive, and is certainly not a llama-server.
	rec := supervisedRec{PGID: os.Getpid(), Cmd: "/definitely/not/running/llama-server"}
	if stillOurs(rec) {
		t.Error("a live pid running a DIFFERENT command was claimed as ours — " +
			"reaping it would kill an unrelated process")
	}
}

// A dead group is simply gone; nothing to signal.
func TestDeadGroupIsNotOurs(t *testing.T) {
	// A pid that cannot exist: Linux and darwin both cap well below this.
	if stillOurs(supervisedRec{PGID: 0x7FFFFFF0, Cmd: "/usr/bin/llama-server"}) {
		t.Error("a nonexistent pid was treated as live")
	}
}

// ReapStale must clear the note even when it signalled nothing, or a permanent
// stale entry gets re-examined on every start forever.
func TestReapStaleClearsTheNote(t *testing.T) {
	dir := t.TempDir()
	RecordSupervised(dir, 0x7FFFFFF0, "/usr/bin/llama-server")
	ReapStale(dir)
	if got := readSupervised(dir); len(got) != 0 {
		t.Errorf("note still holds %+v after a reap", got)
	}
}

// firstToken picks the identifying executable, including through `exec`, which
// is how every corrallm backend is spawned.
func TestFirstTokenFindsTheExecutable(t *testing.T) {
	cases := map[string]string{
		"exec /opt/llama-server --port 5800": "/opt/llama-server",
		"/opt/llama-server --port 5800":      "/opt/llama-server",
		"  ":                                 "",
	}
	for in, want := range cases {
		if got := firstToken(in); got != want {
			t.Errorf("firstToken(%q) = %q, want %q", in, got, want)
		}
	}
}
