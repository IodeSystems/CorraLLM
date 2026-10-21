//go:build unix

package agent

import (
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/host"
)

// capSink records the post-exit capacity readings a RemoteHost hands over.
type capSink struct {
	mu   sync.Mutex
	got  []Capacity
	when []time.Time
}

func (s *capSink) record(c Capacity) {
	s.mu.Lock()
	s.got = append(s.got, c)
	s.when = append(s.when, time.Now())
	s.mu.Unlock()
}

func (s *capSink) first() (Capacity, time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.got) == 0 {
		return Capacity{}, time.Time{}, false
	}
	return s.got[0], s.when[0], true
}

// A backend that takes seconds to exit after SIGTERM: the handle reports it
// alive for all of that, and the exit is published only AFTER a fresh
// capacity reading, taken after the signal, has reached the sink. That order
// is what lets a reload right after a stop admit against memory that no
// longer shows the stopped backend (box1 2026-10-21 01:46:04).
func TestRemoteHandle_ExitPublishedAfterFreshCapacity(t *testing.T) {
	a := New("test", "tok")
	srv := httptest.NewServer(a.Routes())
	defer func() { a.Shutdown(); srv.Close() }()

	r := NewRemoteHost("box", []string{srv.URL}, "tok")
	sink := &capSink{}
	r.SetCapacitySink(sink.record)

	// Exits 2 s after SIGTERM, as a llama-server in CUDA teardown does.
	h, err := r.Start(host.Spec{Name: "slow", Cmd: `trap 'sleep 2; exit 0' TERM; while :; do sleep 0.1; done`})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "backend alive", 5*time.Second, h.Alive)
	time.Sleep(300 * time.Millisecond) // let the shell install its trap

	signalled := time.Now()
	if err := h.Signal(host.SigTerm); err != nil {
		t.Fatal(err)
	}
	for h.Alive() {
		if time.Since(signalled) > 10*time.Second {
			t.Fatal("never reported gone")
		}
		time.Sleep(20 * time.Millisecond)
	}
	gone := time.Now()
	if el := gone.Sub(signalled); el < 1500*time.Millisecond {
		t.Errorf("reported gone %s after SIGTERM; the backend takes 2 s to exit", el)
	}
	c, at, ok := sink.first()
	if !ok {
		t.Fatal("exit published without a capacity refresh")
	}
	if at.After(gone) {
		t.Error("capacity refreshed after the exit was published")
	}
	if c.MeasuredAt < signalled.UnixMilli() {
		t.Errorf("capacity measured at %d, before the signal at %d", c.MeasuredAt, signalled.UnixMilli())
	}
	select {
	case <-h.Done():
	default:
		t.Error("Done still open after the group was reported gone")
	}
}

// The leader exits on SIGTERM, a group member ignores it for 2 s. Done follows
// the leader, as a local handle's does; Alive follows the group, which is what
// holds the memory.
func TestRemoteHandle_AliveFollowsGroupNotLeader(t *testing.T) {
	a := New("test", "tok")
	srv := httptest.NewServer(a.Routes())
	defer func() { a.Shutdown(); srv.Close() }()

	r := NewRemoteHost("box", []string{srv.URL}, "tok")
	h, err := r.Start(host.Spec{Name: "split",
		Cmd: `(trap '' TERM; sleep 2) >/dev/null 2>&1 & trap 'exit 0' TERM; while :; do sleep 0.1; done`})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "backend alive", 5*time.Second, h.Alive)
	time.Sleep(300 * time.Millisecond)
	if err := h.Signal(host.SigTerm); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done never closed after the leader exited")
	}
	if !h.Alive() {
		t.Error("Alive false while a group member still runs")
	}
	waitFor(t, "group gone", 10*time.Second, func() bool { return !h.Alive() })
}

// An older reading never replaces a newer one: a heartbeat probed before an
// exit can land after the post-exit fetch.
func TestLiveness_OlderCapacityDoesNotReplaceNewer(t *testing.T) {
	l := NewLiveness()
	l.RecordCapacity("box", Capacity{MeasuredAt: 200, GPU: &DeviceMem{UsedBytes: 1}})
	l.RecordCapacity("box", Capacity{MeasuredAt: 100, GPU: &DeviceMem{UsedBytes: 29 << 30}})
	if c, _ := l.Capacity("box"); c.MeasuredAt != 200 || c.GPU.UsedBytes != 1 {
		t.Errorf("stale reading replaced a fresh one: %+v", c)
	}
	// An agent older than MeasuredAt is always taken, as before.
	l.RecordCapacity("box", Capacity{GPU: &DeviceMem{UsedBytes: 7}})
	if c, _ := l.Capacity("box"); c.GPU.UsedBytes != 7 {
		t.Errorf("reading without MeasuredAt ignored: %+v", c)
	}
}
