package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// The trigger is the PROTOCOL, not the build (owner decision 10-20). A primary
// rebuilt several times a day used to restart every agent several times a day;
// different bytes speaking the same protocol are no reason to.
func TestSelfUpdateOnlyWhenTheProtocolDiffers(t *testing.T) {
	cases := []struct {
		name string
		ack  HeartbeatAck
		want bool
	}{
		{"different build, same protocol", HeartbeatAck{Protocol: Protocol, BuildID: "bbbbbbbbbbbb", Version: "v2"}, false},
		{"different protocol", HeartbeatAck{Protocol: Protocol + 1}, true},
		{"primary older than the field", HeartbeatAck{BuildID: "bbbbbbbbbbbb", Version: "v2"}, false},
	}
	for _, c := range cases {
		if got := protocolDiffers(c.ack); got != c.want {
			t.Errorf("%s: protocolDiffers = %v, want %v", c.name, got, c.want)
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
