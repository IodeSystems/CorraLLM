// Package handoff replaces a running corrallm with a new binary without
// closing the listening socket: blue-green for a single-unit daemon.
//
// The running process (the predecessor) starts the new binary with two
// inherited descriptors — the listening socket and one end of a control
// socket — and keeps serving. The new process (the successor) starts up, tells
// systemd it is the service's main process now, starts accepting on the
// inherited socket and says "ready" over the control socket. Only then does
// the predecessor stop accepting and drain: it finishes what it admitted and
// exits. Every connection attempt meanwhile lands in the one kernel accept
// queue the two share, so no client ever sees a refused connection.
//
// For the overlap the control socket carries the predecessor's occupancy —
// slots held per backend, local backends still alive — so the successor can
// count them (see sched.SetForeign). It closes when the predecessor exits,
// which is how the successor learns the drain is over, crash or not.
//
// This is the tableflip pattern, sized to one daemon: no library, and no
// SIGHUP (that is config reload).
package handoff

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

// Environment the predecessor sets for its successor: which descriptors were
// inherited (listener, control) and who started it.
const (
	envFDs    = "CORRALLM_HANDOFF_FDS"
	envParent = "CORRALLM_HANDOFF_PARENT"
)

// Status is the predecessor's occupancy, sent whenever it changes.
type Status struct {
	// Draining is true from the moment the predecessor admits nothing more;
	// until a Status with it arrives, the successor must not admit.
	Draining bool `json:"draining"`
	// Active is admission slots held per backend (scheduler name).
	Active map[string]int `json:"active,omitempty"`
	// Local counts the predecessor's local backends that may still hold
	// memory. The successor spawns no local backend until it is zero.
	Local int `json:"local"`
	// LocalServers names the servers those local backends run on. The
	// successor holds an AGENT spawn onto one of them too: a server that moved
	// under an agent beside this primary spawns onto the GPU these children
	// still occupy. Always sent, empty when there are none — no omitempty, so
	// a successor can tell "none" from a predecessor older than this field
	// (absent → nil), for which it keeps every agent spawn held.
	LocalServers []string `json:"localServers"`
}

// message is one line on the control socket.
type message struct {
	Ready  bool    `json:"ready,omitempty"`
	Error  string  `json:"error,omitempty"` // successor failed to start
	PID    int     `json:"pid,omitempty"`
	Status *Status `json:"status,omitempty"`
}

func encode(m message) []byte {
	b, _ := json.Marshal(m)
	return append(b, '\n')
}

// ErrUnsupported is returned where a handoff cannot work: no descriptor
// passing (Windows), or a systemd unit that cannot move its main pid.
var ErrUnsupported = errors.New("handoff: not supported here")

// DefaultReadyTimeout bounds how long a predecessor waits for its successor's
// "ready". Startup opens the store, applies migrations and loads the config —
// seconds; model preloads run in the background and do not count.
const DefaultReadyTimeout = 2 * time.Minute

// StopAccepting is how a predecessor stops taking connections: keep-alives
// off (each open connection closes after its current response; idle ones
// close now), then the listener closed. The successor holds the same socket,
// so new connections queue for it instead of being refused.
//
// NOT srv.Shutdown. Once Shutdown begins, net/http drops any request it
// finishes reading afterwards without an answer, so a connection accepted a
// moment before the listener closed would see EOF — measured: 1–2 of ~3,000
// requests per handover in this package's test. Call Shutdown only once
// Conns.Busy is zero.
func StopAccepting(srv *http.Server, ln net.Listener) {
	srv.SetKeepAlivesEnabled(false)
	_ = ln.Close()
}

// Conns counts a server's connections that have a request to finish. Install
// Hook as http.Server.ConnState.
type Conns struct {
	mu   sync.Mutex
	busy map[net.Conn]connState
}

type connState struct {
	st    http.ConnState
	since time.Time
}

// Hook tracks one connection's state change.
func (c *Conns) Hook(conn net.Conn, st http.ConnState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.busy == nil {
		c.busy = map[net.Conn]connState{}
	}
	switch st {
	case http.StateNew, http.StateActive:
		c.busy[conn] = connState{st, time.Now()}
	default: // idle, hijacked (counted by whoever hijacked it), closed
		delete(c.busy, conn)
	}
}

// newConnGrace is how long a connection that has sent nothing yet counts as
// busy — net/http's own rule for Shutdown (issue 22682): past it, a silent
// connection is idle and must not hold a drain open.
const newConnGrace = 5 * time.Second

// Busy is how many connections are mid-request, or new within newConnGrace.
func (c *Conns) Busy() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, s := range c.busy {
		if s.st == http.StateActive || time.Since(s.since) < newConnGrace {
			n++
		}
	}
	return n
}
