package proc

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/host"
)

// lateHandle ignores SIGTERM, reads alive until goneAt (an agent reports the
// exit a poll and a capacity read late), and answers SIGKILL "no such
// process": the process exited between the grace and the kill.
type lateHandle struct{ goneAt time.Time }

func (h *lateHandle) ID() string                      { return "late" }
func (h *lateHandle) Done() <-chan struct{}           { return nil }
func (h *lateHandle) Err() error                      { return nil }
func (h *lateHandle) MemoryMiB() (int, error)         { return 0, fmt.Errorf("none") }
func (h *lateHandle) MemoryOnMiB(string) (int, error) { return 0, fmt.Errorf("none") }
func (h *lateHandle) Alive() bool                     { return time.Now().Before(h.goneAt) }
func (h *lateHandle) Signal(s host.Sig) error {
	if s == host.SigKill {
		return errors.New(`agent 500 Internal Server Error: {"error":{"message":"no such process"}}`)
	}
	return nil
}

// A SIGKILL refused with "no such process" still waits for the exit to be
// seen: the 2026-11-10 tool benchmark's second trial was refused for capacity
// because the stop returned while the reading still held the first trial.
func TestReapWaitsForTheExitAfterNoSuchProcess(t *testing.T) {
	m := &Manager{}
	h := &lateHandle{goneAt: time.Now().Add(400 * time.Millisecond)}
	m.reapGroupWithin("bench-swift", h, 50*time.Millisecond)
	if h.Alive() {
		t.Fatal("the stop returned while the group still read alive")
	}
}

// A trial refused for room is the typed refusal, so a caller can retry it.
func TestProbeNoRoomIsTyped(t *testing.T) {
	err := fmt.Errorf("%w on %q for gpu0: 31000 MiB", errProbeNoRoom, "box1")
	if !errors.Is(err, errProbeNoRoom) || err.Error() != `not enough free capacity on "box1" for gpu0: 31000 MiB` {
		t.Errorf("err = %v", err)
	}
}
