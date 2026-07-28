package agent

import (
	"testing"
	"time"
)

// A single dropped beat is packet loss, not an outage. Marking a host down on
// one would flap a server that is serving perfectly well, so the window is
// several intervals wide.
func TestLiveness_TolerantOfOneMissedBeat(t *testing.T) {
	l := NewLiveness()
	now := time.Now()
	l.Beat("mac1", now)

	if got := l.Status("mac1", now.Add(HeartbeatInterval+time.Second)); got != Up {
		t.Errorf("one missed beat = %s, want up", got)
	}
	if got := l.Status("mac1", now.Add(MissWindow+time.Second)); got != Down {
		t.Errorf("silence past the window = %s, want down", got)
	}
	// And it recovers on the next beat — down is not sticky.
	l.Beat("mac1", now.Add(MissWindow+2*time.Second))
	if got := l.Status("mac1", now.Add(MissWindow+3*time.Second)); got != Up {
		t.Errorf("after a fresh beat = %s, want up", got)
	}
}

// A server that has never reported must NOT be treated as down. A primary that
// just restarted has heard from nobody yet, and refusing on that basis would
// make a restart of the PRIMARY look like an outage of every AGENT.
func TestLiveness_UnknownIsStillWorthTrying(t *testing.T) {
	l := NewLiveness()
	now := time.Now()
	if got := l.Status("never-seen", now); got != Unknown {
		t.Errorf("status = %s, want unknown", got)
	}
	if !l.Reachable("never-seen", now) {
		t.Error("an unheard-of server must still be attempted; let the spawn find out")
	}
}

// Down blocks new spawns; that is the whole point of tracking it.
func TestLiveness_DownIsNotReachable(t *testing.T) {
	l := NewLiveness()
	now := time.Now()
	l.Beat("mac1", now)
	if !l.Reachable("mac1", now) {
		t.Error("a freshly-beating server must be reachable")
	}
	if l.Reachable("mac1", now.Add(MissWindow+time.Second)) {
		t.Error("a silent server must not be spawned onto")
	}
}

// A nil tracker is the single-host case: nothing heartbeats, and nothing may
// be refused because of it.
func TestLiveness_NilNeverBlocks(t *testing.T) {
	var l *Liveness
	if !l.Reachable("box1", time.Now()) {
		t.Error("a nil tracker must never mark a server down")
	}
}
