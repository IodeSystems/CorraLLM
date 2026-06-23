package proc

import (
	"fmt"
	"testing"
)

// TestLogBufferParsesBanner: n_ctx / n_slots are extracted from llama-server's
// startup lines, even split across writes.
func TestLogBufferParsesBanner(t *testing.T) {
	b := newLogBuffer(100)
	_, _ = b.Write([]byte("srv  load_model: initializing slots, n_slots = 1\n"))
	// A line delivered in two writes (no trailing newline on the first).
	_, _ = b.Write([]byte("slot load_model: id 0 | new slot, n_ctx = 22"))
	_, _ = b.Write([]byte("0160\n"))

	if nCtx, nSlots := b.Stats(); nCtx != 220160 || nSlots != 1 {
		t.Fatalf("parsed n_ctx=%d n_slots=%d, want 220160 / 1", nCtx, nSlots)
	}
	if lines := b.Lines(); len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}
}

// TestLogBufferRingTrim: only the last `max` lines are retained.
func TestLogBufferRingTrim(t *testing.T) {
	b := newLogBuffer(3)
	for i := 0; i < 10; i++ {
		_, _ = b.Write([]byte(fmt.Sprintf("line %d\n", i)))
	}
	lines := b.Lines()
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d", len(lines))
	}
	if lines[0] != "line 7" || lines[2] != "line 9" {
		t.Errorf("ring kept wrong tail: %v", lines)
	}
}
