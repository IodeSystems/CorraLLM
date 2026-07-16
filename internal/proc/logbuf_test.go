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

	if nCtx, nSlots, _ := b.Stats(); nCtx != 220160 || nSlots != 1 {
		t.Fatalf("parsed n_ctx=%d n_slots=%d, want 220160 / 1", nCtx, nSlots)
	}
	if lines := b.Lines(); len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}
}

// TestLogBufferParsesKVSize: the KV-cache size is extracted from all three
// real-world llama.cpp log line shapes ("KV self size", "KV buffer size", and
// the same pattern under a different context — i.e. the regex is generic on
// the middle word, not hardcoded to one variant).
func TestLogBufferParsesKVSize(t *testing.T) {
	cases := []struct {
		name string
		line string
		want int
	}{
		{"kv_cache_init self", "llama_kv_cache_init:      CUDA0 KV self size = 1234.00 MiB", 1234},
		{"new_context self", "llama_new_context_with_model: KV self size = 1234.00 MiB", 1234},
		{"buffer size", "KV buffer size = 1234.00 MiB", 1234},
		{"cache size", "llama_new_context_with_model: KV cache size = 512.50 MiB", 512},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newLogBuffer(10)
			_, _ = b.Write([]byte(c.line + "\n"))
			if _, _, kv := b.Stats(); kv != c.want {
				t.Errorf("kvMiB = %d, want %d (line %q)", kv, c.want, c.line)
			}
		})
	}
}

// TestLogBufferKVSizeFirstOccurrenceWins: like n_ctx/n_slots, once kvMiB is
// parsed a later line doesn't overwrite it.
func TestLogBufferKVSizeFirstOccurrenceWins(t *testing.T) {
	b := newLogBuffer(10)
	_, _ = b.Write([]byte("KV buffer size = 1000.00 MiB\n"))
	_, _ = b.Write([]byte("KV buffer size = 9999.00 MiB\n"))
	if _, _, kv := b.Stats(); kv != 1000 {
		t.Errorf("kvMiB = %d, want 1000 (first occurrence)", kv)
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
