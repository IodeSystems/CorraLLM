package proxy

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestCalibration_BlocksOthersServesHolder(t *testing.T) {
	s := NewCalibrationState()
	if blocked, _ := s.Blocks("anyone"); blocked {
		t.Fatal("an inactive lease must block nobody")
	}
	if _, ok := s.Begin("bench-key", "measuring", time.Minute); !ok {
		t.Fatal("Begin should succeed on a free lease")
	}
	if blocked, _ := s.Blocks("bench-key"); blocked {
		t.Error("the lease holder must be served")
	}
	blocked, remaining := s.Blocks("someone-else")
	if !blocked {
		t.Error("other callers must be turned away")
	}
	if remaining <= 0 || remaining > time.Minute {
		t.Errorf("remaining = %s, want (0, 1m]", remaining)
	}
	// An unkeyed caller is NOT the holder and must be blocked — otherwise the
	// easiest way to bypass exclusive mode is to send no key at all.
	if blocked, _ := s.Blocks(""); !blocked {
		t.Error("unkeyed callers must be blocked, else the mode is trivially bypassed")
	}
}

// THE critical property. A bench that crashes, is killed, or loses the network
// must not leave the box refusing every caller forever. Expiry is evaluated on
// read, so no cleanup goroutine has to survive for the box to heal.
func TestCalibration_LeaseSelfExpires(t *testing.T) {
	s := NewCalibrationState()
	if _, ok := s.Begin("bench", "", 20*time.Millisecond); !ok {
		t.Fatal("Begin failed")
	}
	if blocked, _ := s.Blocks("other"); !blocked {
		t.Fatal("should block while held")
	}
	time.Sleep(40 * time.Millisecond)
	if blocked, _ := s.Blocks("other"); blocked {
		t.Error("an expired lease must stop blocking WITHOUT anyone calling End")
	}
	if active, _, _ := s.Status(); active {
		t.Error("Status must report an expired lease as inactive")
	}
}

// Two concurrent calibration runs would evict each other's models and produce
// garbage from both.
func TestCalibration_RejectsSecondHolder(t *testing.T) {
	s := NewCalibrationState()
	s.Begin("first", "", time.Minute)
	if _, ok := s.Begin("second", "", time.Minute); ok {
		t.Error("a second key must not be able to steal a held lease")
	}
	// The same key re-entering is fine — it extends its own lease (a long run
	// renewing) rather than deadlocking against itself.
	if _, ok := s.Begin("first", "", time.Minute); !ok {
		t.Error("the holder must be able to extend its own lease")
	}
}

// End is idempotent so a bench can always defer it.
func TestCalibration_EndIdempotent(t *testing.T) {
	s := NewCalibrationState()
	s.End("never-held") // must not panic
	s.Begin("k", "", time.Minute)
	s.End("k")
	s.End("k")
	if blocked, _ := s.Blocks("other"); blocked {
		t.Error("released lease should block nobody")
	}
	// A different key must not be able to release someone else's lease.
	s.Begin("owner", "", time.Minute)
	s.End("intruder")
	if blocked, _ := s.Blocks("other"); !blocked {
		t.Error("a non-holder must not be able to release the lease")
	}
}

// The turn-away must be 429 with a Retry-After the client can shape against —
// NOT a 5xx. Clients cap 5xx retries hard, so a fault code would fail in-flight
// agents that a backpressure code merely pauses.
func TestCalibration_WriteBackpressureShape(t *testing.T) {
	w := httptest.NewRecorder()
	writeCalibrationBackpressure(w, 42*time.Second, "nightly")
	if w.Code != 429 {
		t.Errorf("status = %d, want 429 (backpressure, not fault)", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "42" {
		t.Errorf("Retry-After = %q, want the lease's remaining time", got)
	}
	if w.Header().Get("X-Corrallm-Calibrating") != "1" {
		t.Error("a caller needs a way to tell calibration from ordinary load")
	}
	if body := w.Body.String(); !contains(body, "nightly") || !contains(body, "calibrat") {
		t.Errorf("body should explain why: %s", body)
	}
}

// A zero/short remaining must never emit Retry-After: 0, which invites a
// hot-loop against a still-closed door.
func TestCalibration_RetryAfterFloor(t *testing.T) {
	w := httptest.NewRecorder()
	writeCalibrationBackpressure(w, 0, "")
	if got := w.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want a floor of 1s", got)
	}
}

// A nil state (calibration unavailable) must be inert, not a panic.
func TestCalibration_NilSafe(t *testing.T) {
	var s *CalibrationState
	if blocked, _ := s.Blocks("k"); blocked {
		t.Error("nil state must block nobody")
	}
	if active, _, _ := s.Status(); active {
		t.Error("nil state must be inactive")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
