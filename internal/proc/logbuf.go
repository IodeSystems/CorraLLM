package proc

import (
	"bytes"
	"regexp"
	"strconv"
	"sync"
)

// llama-server prints these at startup, e.g.
//
//	slot load_model: id 0 | task -1 | new slot, n_ctx = 220160
//	srv  load_model: initializing slots, n_slots = 1
//	llama_kv_cache_init: CUDA0 KV self size = 1234.00 MiB
//	llama_new_context_with_model: KV self size = 1234.00 MiB
//	KV buffer size = 1234.00 MiB
//
// The KV line's middle word varies (self/buffer/cache/…) but the shape is
// constant, so one regex covers all of llama.cpp's variants — it's the
// total KV allocation across every slot, not a per-slot figure (the tuner
// divides by n_slots itself).
var (
	reNCtx   = regexp.MustCompile(`n_ctx\s*=\s*(\d+)`)
	reNSlots = regexp.MustCompile(`n_slots\s*=\s*(\d+)`)
	reKVMiB  = regexp.MustCompile(`KV\s+\w+\s+size\s*=\s*([\d.]+)\s*MiB`)
)

// logBuffer is a backend's bounded, line-oriented capture of its stdout/stderr.
// It is an io.Writer (wired into the spawned cmd) that keeps the last `max`
// lines and opportunistically parses the context length and slot count from the
// llama-server banner. Safe for concurrent Write/read.
type logBuffer struct {
	mu      sync.Mutex
	max     int
	lines   []string
	partial []byte
	nCtx    int
	nSlots  int
	kvMiB   int
}

func newLogBuffer(max int) *logBuffer { return &logBuffer{max: max} }

// Write splits incoming output on newlines into the ring, trimmed to max lines.
func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.partial = append(b.partial, p...)
	for {
		i := bytes.IndexByte(b.partial, '\n')
		if i < 0 {
			break
		}
		b.appendLine(string(b.partial[:i]))
		b.partial = b.partial[i+1:]
	}
	return len(p), nil
}

// appendLine records a line (caller holds b.mu), trims to max, and parses the
// llama-server n_ctx / n_slots banner once (first occurrence wins).
func (b *logBuffer) appendLine(line string) {
	b.lines = append(b.lines, line)
	if len(b.lines) > b.max {
		b.lines = append([]string(nil), b.lines[len(b.lines)-b.max:]...)
	}
	if b.nCtx == 0 {
		if m := reNCtx.FindStringSubmatch(line); m != nil {
			if v, err := strconv.Atoi(m[1]); err == nil {
				b.nCtx = v
			}
		}
	}
	if b.nSlots == 0 {
		if m := reNSlots.FindStringSubmatch(line); m != nil {
			if v, err := strconv.Atoi(m[1]); err == nil {
				b.nSlots = v
			}
		}
	}
	if b.kvMiB == 0 {
		if m := reKVMiB.FindStringSubmatch(line); m != nil {
			if v, err := strconv.ParseFloat(m[1], 64); err == nil {
				b.kvMiB = int(v)
			}
		}
	}
}

// Lines returns a copy of the buffered lines, oldest first.
func (b *logBuffer) Lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.lines...)
}

// Stats returns the parsed context length, slot count, and total KV-cache
// allocation in MiB (each 0 if not yet seen).
func (b *logBuffer) Stats() (nCtx, nSlots, kvMiB int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.nCtx, b.nSlots, b.kvMiB
}
