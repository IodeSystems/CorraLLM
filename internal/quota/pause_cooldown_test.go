package quota

import (
	"net/http"
	"testing"
)

// TestPause503RetryAfterDoesNotCoolBackend guards the interaction between a
// PAUSE and upstream backend cooldown.
//
// corrallm answers a timed pause with 503 + Retry-After (see proxy.writePaused).
// In a peer deployment one corrallm proxies to another, so that response can
// arrive here as an upstream reply. coolUntil is gated on is429, and this locks
// that in: a 503 must never cool a backend, however long its Retry-After.
//
// The failure this prevents is severe and quiet — a peer pausing a model for an
// hour would otherwise take its whole backend out of rotation for that hour,
// including every OTHER model the peer serves, since cooldown is per backend.
func TestPause503RetryAfterDoesNotCoolBackend(t *testing.T) {
	l := New()
	l.ObserveResponse("peer", http.StatusServiceUnavailable, hdr("Retry-After", "3600"))
	if !l.Available("peer") {
		t.Error("a 503 + Retry-After cooled the backend; only a 429 may do that")
	}
}

// TestRateLimit429RetryAfterStillCools is the other half: the behavior above
// must not have been achieved by ignoring Retry-After everywhere.
func TestRateLimit429RetryAfterStillCools(t *testing.T) {
	l := New()
	l.ObserveResponse("provider", http.StatusTooManyRequests, hdr("Retry-After", "60"))
	if l.Available("provider") {
		t.Error("a 429 + Retry-After must cool the backend")
	}
}
