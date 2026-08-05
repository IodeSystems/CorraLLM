package agent

import (
	"sync"
	"time"
)

// Reachability is what the primary believes about an agent-backed server.
type Reachability string

const (
	// Unknown: no heartbeat has ever arrived. The agent may not be running, or
	// may not be configured to point at this primary.
	Unknown Reachability = "unknown"
	// Up: a heartbeat arrived within the miss window.
	Up Reachability = "up"
	// Down: heartbeats stopped. Says nothing about whether the MACHINE is
	// alive — only that this primary cannot hear it.
	Down Reachability = "down"
)

// HeartbeatInterval is how often an agent reports in.
const HeartbeatInterval = 10 * time.Second

// MissWindow is how long silence must last before a server is marked down.
//
// Three intervals rather than one: a single dropped beat is a packet loss, not
// an outage, and marking a host down on it would flap a server that is serving
// perfectly well. Three is long enough to ride out a hiccup and short enough
// that a real outage is visible before anyone files a ticket.
const MissWindow = 3 * HeartbeatInterval

// Liveness tracks the last heartbeat from each agent-backed server.
//
// The agent PINGS the primary rather than the primary polling the agent. The
// agent is the side that knows it is alive, it is the side that may sit behind
// NAT or move between networks, and it needs no inbound reachability from the
// primary to report in — which is exactly the topology a laptop has.
//
// A server going down does NOT remove its models or its configuration. The
// declaration is the operator's statement of intent and outlives any outage;
// what changes is only that corrallm will not try to spawn there. Membership
// ends when the agent's TOKEN is revoked, not when the network hiccups.
type Liveness struct {
	mu   sync.Mutex
	seen map[string]time.Time
	cap  map[string]Capacity
}

// NewLiveness returns an empty tracker.
func NewLiveness() *Liveness {
	return &Liveness{seen: map[string]time.Time{}, cap: map[string]Capacity{}}
}

// RecordCapacity stores what an agent last measured about its machine.
//
// Kept beside liveness because it has the same lifetime and the same source: it
// arrives on the heartbeat, it is only meaningful while the agent is up, and it
// is lost on restart — which is correct, since a stale capacity reading is
// worse than none. It is deliberately NOT persisted to config: this is an
// observation, not a declaration.
func (l *Liveness) RecordCapacity(server string, c Capacity) {
	if l == nil || server == "" {
		return
	}
	l.mu.Lock()
	if l.cap == nil {
		l.cap = map[string]Capacity{}
	}
	l.cap[server] = c
	l.mu.Unlock()
}

// Capacity returns the last measurement from server's agent.
func (l *Liveness) Capacity(server string) (Capacity, bool) {
	if l == nil {
		return Capacity{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.cap[server]
	return c, ok
}

// Beat records that server's agent just reported in.
func (l *Liveness) Beat(server string, at time.Time) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil {
		l.seen = map[string]time.Time{}
	}
	l.seen[server] = at
}

// LastSeen returns the newest heartbeat time for server, if any.
func (l *Liveness) LastSeen(server string) (time.Time, bool) {
	if l == nil {
		return time.Time{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.seen[server]
	return t, ok
}

// Status classifies a server as of now.
func (l *Liveness) Status(server string, now time.Time) Reachability {
	t, ok := l.LastSeen(server)
	if !ok {
		return Unknown
	}
	if now.Sub(t) > MissWindow {
		return Down
	}
	return Up
}

// Reachable reports whether it is worth attempting a spawn on server.
//
// Unknown counts as reachable on purpose. An agent that has never reported in
// may simply predate this primary's start, and refusing on that basis would
// make a restart of the PRIMARY look like an outage of the AGENT. Let the spawn
// attempt find out — it fails in seconds with a clear error, which is a better
// answer than a guess.
func (l *Liveness) Reachable(server string, now time.Time) bool {
	return l.Status(server, now) != Down
}
