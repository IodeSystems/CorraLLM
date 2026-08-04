package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// The case that motivated build ids: two untagged builds, a week apart, both
// stamped "dev". Version strings say "current"; the bytes say otherwise.
func TestOutOfDateDistinguishesTwoDevBuilds(t *testing.T) {
	ack := HeartbeatAck{Version: "dev", BuildID: "bbbbbbbbbbbb"}
	if !outOfDate("aaaaaaaaaaaa", "dev", ack) {
		t.Error("two different dev builds compared equal — the agent would never update during development")
	}
	// And the same bytes must terminate it, or the agent re-execs on every beat.
	if outOfDate("bbbbbbbbbbbb", "dev", ack) {
		t.Error("identical build ids reported out of date — this is the update loop")
	}
}

// Build ids beat version strings in BOTH directions, including the awkward one:
// the primary claims a new version while serving the build already installed.
func TestOutOfDateTrustsBytesOverVersionStrings(t *testing.T) {
	same := HeartbeatAck{Version: "v9.9.9", BuildID: "aaaaaaaaaaaa"}
	if outOfDate("aaaaaaaaaaaa", "v1.0.0", same) {
		t.Error("a new version string over identical bytes should not trigger an update")
	}
	differ := HeartbeatAck{Version: "v1.0.0", BuildID: "cccccccccccc"}
	if !outOfDate("aaaaaaaaaaaa", "v1.0.0", differ) {
		t.Error("matching version strings over different bytes should still update")
	}
}

// Without ids on both sides there is still no way to tell two dev builds apart,
// so the old rule has to stand — updating on every beat would be worse.
func TestOutOfDateFallsBackToVersionsWhenAnIDIsMissing(t *testing.T) {
	cases := []struct {
		name          string
		ownID, ownVer string
		ack           HeartbeatAck
		want          bool
	}{
		{"no id either side, both dev", "", "dev", HeartbeatAck{Version: "dev"}, false},
		{"no id either side, versions differ", "", "v1", HeartbeatAck{Version: "v2"}, true},
		{"no id either side, versions match", "", "v1", HeartbeatAck{Version: "v1"}, false},
		{"primary has no binary for us", "aaaaaaaaaaaa", "v1", HeartbeatAck{Version: "v2"}, true},
		{"agent cannot hash itself", "", "v1", HeartbeatAck{Version: "v2", BuildID: "bbbbbbbbbbbb"}, true},
		{"primary says nothing at all", "aaaaaaaaaaaa", "v1", HeartbeatAck{}, false},
	}
	for _, c := range cases {
		if got := outOfDate(c.ownID, c.ownVer, c.ack); got != c.want {
			t.Errorf("%s: outOfDate = %v, want %v", c.name, got, c.want)
		}
	}
}

// Both sides must compute the same identity for the same bytes, or an agent
// updates forever. One function is how that is guaranteed; this pins it.
func TestHashFileIsStableAndContentAddressed(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	if err := os.WriteFile(a, []byte("agent binary v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("agent binary v1"), 0o755); err != nil {
		t.Fatal(err)
	}

	ha, err := HashFile(a)
	if err != nil {
		t.Fatal(err)
	}
	hb, err := HashFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb {
		t.Errorf("same bytes hashed differently (%s vs %s) — the agent would never settle", ha, hb)
	}
	if len(ha) != buildIDLen {
		t.Errorf("build id length = %d, want %d", len(ha), buildIDLen)
	}

	if err := os.WriteFile(b, []byte("agent binary v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	hb2, err := HashFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if ha == hb2 {
		t.Error("different bytes hashed the same — a real change would never ship")
	}
}

func TestHashFileReportsAMissingBinary(t *testing.T) {
	if _, err := HashFile(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("hashing a missing file should fail, not return an empty id that compares equal to another failure")
	}
}
