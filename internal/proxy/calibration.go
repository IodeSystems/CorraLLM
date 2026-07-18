package proxy

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Calibration mode — exclusive access for a measurement run.
//
// llm-bench measures VRAM footprint and verifies capabilities by loading and
// unloading models deliberately. Both are worthless under contention: a
// footprint read while another model is mid-load is simply wrong, and an
// eviction taken to force a cold probe fights live traffic trying to keep the
// same model warm (observed: 107 no-capacity spills when a bench run and the
// chat lane evicted each other between turns).
//
// So a calibration run takes the box: for its duration, only the calibration
// key is served and everyone else gets 429 + Retry-After.
//
// 429 rather than 503 deliberately. This is backpressure — "wait your turn" —
// not a fault, and clients treat the two oppositely: an agentkit-style client
// retries 429 against its whole budget but caps 5xx at a handful of attempts.
// A calibration pass therefore makes in-flight agents PAUSE rather than FAIL,
// which is the same distinction the capacity fix drew earlier.

// CalibrationState holds the active calibration lease, if any.
//
// A lease ALWAYS carries a deadline. A bench process that crashes, is killed, or
// loses its network mid-run must not leave the box refusing every caller
// forever; the lease simply expires. This is the single most important property
// here — an exclusive mode without a self-healing timeout turns one crashed
// benchmark into a total outage.
type CalibrationState struct {
	mu       sync.RWMutex
	active   bool
	key      string
	reason   string
	deadline time.Time
}

// NewCalibrationState returns an inactive state.
func NewCalibrationState() *CalibrationState { return &CalibrationState{} }

// Begin claims the box for key until deadline. Returns false if a lease is
// already held by a DIFFERENT key — two concurrent calibration runs would evict
// each other's models and produce garbage from both.
func (s *CalibrationState) Begin(key, reason string, ttl time.Duration) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.active && now.Before(s.deadline) && s.key != key {
		return s.deadline, false
	}
	s.active = true
	s.key = key
	s.reason = reason
	s.deadline = now.Add(ttl)
	return s.deadline, true
}

// End releases the lease. Idempotent: releasing an already-expired or
// never-held lease is not an error, so a bench can always call it in a defer.
func (s *CalibrationState) End(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.key == key || key == "" {
		s.active = false
		s.key = ""
		s.reason = ""
		s.deadline = time.Time{}
	}
}

// Status reports the lease. active is false once the deadline passes, without
// needing anyone to call End — expiry is checked on read so a dead bench heals
// the box on its own.
func (s *CalibrationState) Status() (active bool, reason string, remaining time.Duration) {
	if s == nil {
		return false, "", 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.active || time.Now().After(s.deadline) {
		return false, "", 0
	}
	return true, s.reason, time.Until(s.deadline)
}

// Blocks reports whether a caller presenting callerKey must be turned away, and
// for how long to suggest they wait.
func (s *CalibrationState) Blocks(callerKey string) (bool, time.Duration) {
	if s == nil {
		return false, 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.active || time.Now().After(s.deadline) {
		return false, 0
	}
	if callerKey != "" && callerKey == s.key {
		return false, 0 // the calibration run itself
	}
	return true, time.Until(s.deadline)
}

// writeCalibrationBackpressure turns a caller away for the duration of a
// calibration lease.
//
// Retry-After is the lease's REMAINING time, not a fixed guess, so a client that
// honors it wakes up when the box is actually free instead of hammering a
// closed door. Shape mirrors writeBackpressure (same headers, same JSON
// envelope) so an existing client needs no new handling — a calibration pause
// is indistinguishable from any other backpressure except for its reason.
func writeCalibrationBackpressure(w http.ResponseWriter, remaining time.Duration, reason string) {
	secs := int(remaining.Round(time.Second) / time.Second)
	if secs < 1 {
		secs = 1
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Retry-After", strconv.Itoa(secs))
	h.Set("X-Corrallm-Calibrating", "1")
	w.WriteHeader(http.StatusTooManyRequests)
	msg := "calibration in progress; retry after backoff"
	if reason != "" {
		msg = "calibration in progress (" + reason + "); retry after backoff"
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message":     msg,
			"type":        "backpressure",
			"reason":      "calibrating",
			"retry_after": secs,
		},
	})
}
