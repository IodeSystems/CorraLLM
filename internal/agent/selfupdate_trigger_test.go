package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/host"
)

// liveHandle is a backend that never exits, so busy() counts it.
type liveHandle struct{ done chan struct{} }

func (h liveHandle) ID() string                      { return "fake" }
func (h liveHandle) Done() <-chan struct{}           { return h.done }
func (h liveHandle) Err() error                      { return nil }
func (h liveHandle) Signal(host.Sig) error           { return nil }
func (h liveHandle) Alive() bool                     { return true }
func (h liveHandle) MemoryMiB() (int, error)         { return 0, nil }
func (h liveHandle) MemoryOnMiB(string) (int, error) { return 0, nil }

// The update decision, end to end up to the download: it fires on a PROTOCOL
// difference only, and running backends no longer hold it back when they can
// be adopted by the updated agent. The served "binary" is too small to
// install, so a triggered update stops safely at the size check; what is
// measured is whether it was attempted at all.
func TestSelfUpdate_TriggersOnProtocolOnly(t *testing.T) {
	var fetches atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		_, _ = w.Write([]byte("tiny"))
	}))
	defer primary.Close()

	cases := []struct {
		name     string
		protocol int
		stateDir bool
		busy     bool
		want     bool
	}{
		{"same protocol, different build", Protocol, true, false, false},
		{"different protocol, idle", Protocol + 1, false, false, true},
		{"different protocol, busy, adoptable", Protocol + 1, true, true, true},
		{"different protocol, busy, nowhere to record backends", Protocol + 1, false, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fetches.Store(0)
			srv := New("v1", "")
			if c.stateDir {
				srv.stateDir = t.TempDir()
			}
			if c.busy {
				srv.backends["b1"] = &supervised{id: "b1", handle: liveHandle{done: make(chan struct{})}, logs: newLogRing(1)}
			}
			b := &Beacon{Primary: primary.URL, SelfUpdate: true, Srv: srv}
			ack := HeartbeatAck{Protocol: c.protocol, UpdateURL: "/install/x", BuildID: "different", Version: "v2"}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			b.maybeSelfUpdate(ctx, ack)
			if got := fetches.Load() > 0; got != c.want {
				t.Errorf("update attempted = %v, want %v", got, c.want)
			}
		})
	}
}
