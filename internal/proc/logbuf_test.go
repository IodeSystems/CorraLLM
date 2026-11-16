package proc

import (
	"fmt"
	"math"
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

// Newer llama.cpp prints only n_ctx_slot; the context is per slot times slots.
func TestLogBufferNCtxFromSlotLine(t *testing.T) {
	b := newLogBuffer(100)
	_, _ = b.Write([]byte("0.07.272.575 I srv    load_model: initializing, n_slots = 2, n_ctx_slot = 94080, kv_unified = 'false'\n"))
	nCtx, nSlots, _ := b.Stats()
	if nCtx != 188160 || nSlots != 2 {
		t.Errorf("nCtx %d nSlots %d, want 188160 and 2", nCtx, nSlots)
	}
}

// Swift's startup lines (build 0b1bad14f + patches): target and draft KV add
// up, recurrent rows divide by cells × (1 + rs_seq), and context lines after
// the n_slots banner do not count.
func TestLogBufferParsesGeometry(t *testing.T) {
	b := newLogBuffer(100)
	for _, l := range []string{
		"0.28.602.688 I llama_kv_cache: size = 5984.00 MiB (180224 cells,  16 layers,  3/1 seqs), K (q8_0): 2992.00 MiB, V (q8_0): 2992.00 MiB",
		"0.28.605.617 I llama_memory_recurrent: size = 2244.38 MiB (     3 cells,  64 layers,  3 seqs  4 rs_seq), R (f32):   84.38 MiB, S (f32): 2160.00 MiB, P (f32):    0.00 MiB",
		"0.28.969.287 I llama_kv_cache: size =  704.00 MiB (180224 cells,   1 layers,  3/1 seqs), K (f16):  352.00 MiB, V (f16):  352.00 MiB",
		"0.30.679.390 I srv    load_model: initializing, n_slots = 3, n_ctx_slot = 86016, kv_unified = 'true'",
		"0.30.999.427 I srv    load_model: prompt cache is enabled, size limit: 27648 MiB",
		"9.99.999.999 I llama_kv_cache: size = 9999.00 MiB (1 cells,  1 layers,  1/1 seqs)",
		"1.08.663.762 I slot create_check: id  0 | task 0 | created context checkpoint 1 of 8 (pos_min = 15129, pos_max = 15129, n_tokens = 15130, size = 149.646 MiB)",
		"1.08.663.762 I slot create_check: id  0 | task 0 | created context checkpoint 2 of 8 (pos_min = 15129, pos_max = 15129, n_tokens = 15130, size = 999.000 MiB)",
	} {
		_, _ = b.Write([]byte(l + "\n"))
	}
	g := b.Geometry()
	const mib = 1 << 20
	if g.NCtxSlot != 86016 || g.CacheRAMBytes != 27648*mib {
		t.Errorf("n_ctx_slot %d, cache %d", g.NCtxSlot, g.CacheRAMBytes)
	}
	if want := 6688.0 * mib / 180224; math.Abs(g.KVCellBytes-want) > 0.01 {
		t.Errorf("kv/cell = %.2f, want %.2f (38 KiB)", g.KVCellBytes, want)
	}
	if want := 2244.38 * mib / 15; math.Abs(g.RSSeqBytes-want) > 1 {
		t.Errorf("rs/seq = %.0f, want %.0f (149.6 MiB)", g.RSSeqBytes, want)
	}
	if g.CheckpointBytes != 149.646*mib {
		t.Errorf("checkpoint = %.0f, want first seen", g.CheckpointBytes)
	}
}
