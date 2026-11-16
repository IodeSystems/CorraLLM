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
	// Newer llama.cpp (seen on build abeada33, 2026-10-07) no longer prints
	// "n_ctx = N" at its default verbosity, only
	//   load_model: initializing, n_slots = 1, n_ctx_slot = 188160, kv_unified = 'false'
	// so the total is n_ctx_slot × n_slots.
	reNCtxSlot = regexp.MustCompile(`n_ctx_slot\s*=\s*(\d+)`)
	reKVMiB    = regexp.MustCompile(`KV\s+\w+\s+size\s*=\s*([\d.]+)\s*MiB`)

	// Memory geometry, for the size of one host prompt-cache entry (see
	// MemGeometry). One line per context; the draft (MTP) context prints its
	// own, and both are saved with each entry:
	//   llama_kv_cache: size = 5984.00 MiB (180224 cells,  16 layers,  3/1 seqs), ...
	//   llama_memory_recurrent: size = 2244.38 MiB (     3 cells,  64 layers,  3 seqs  4 rs_seq), ...
	//   srv    load_model: prompt cache is enabled, size limit: 27648 MiB
	//   slot create_check: id  0 | task 0 | created context checkpoint 1 of 8 (..., size = 149.646 MiB)
	reKVCells   = regexp.MustCompile(`llama_kv_cache: size =\s*([\d.]+) MiB \(\s*(\d+) cells`)
	reRecurrent = regexp.MustCompile(`llama_memory_recurrent: size =\s*([\d.]+) MiB \(\s*(\d+) cells,\s*\d+ layers,\s*\d+ seqs\s+(\d+) rs_seq`)
	reCacheMiB  = regexp.MustCompile(`prompt cache is enabled, size limit: (\d+) MiB`)
	reCkptMiB   = regexp.MustCompile(`context checkpoint .*size = ([\d.]+) MiB`)
)

// MemGeometry is what one llama-server context costs per token and per
// sequence, from its startup log. Each field is 0 when its line was not seen.
//
// A host prompt-cache entry (server prompt_save) is llama_state_seq of the
// target context plus the draft context, plus the slot's context checkpoints:
// tokens × KVCellBytes + RSSeqBytes + checkpoints × CheckpointBytes. On Swift
// that predicts a 28674-token save within 1.3 MiB of the logged 1215 MiB.
type MemGeometry struct {
	NCtxSlot        int     // per-slot context cap (n_ctx_slot)
	KVCellBytes     float64 // KV bytes per cell, summed over the target and draft contexts
	RSSeqBytes      float64 // recurrent state of one sequence (hybrid models); 0 otherwise
	CheckpointBytes float64 // one context checkpoint, first seen in the log
	CacheRAMBytes   int64   // the prompt-cache size limit the server logged
}

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
	geo     MemGeometry
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
	b.parseGeometry(line)
	if b.nSlots == 0 {
		if m := reNSlots.FindStringSubmatch(line); m != nil {
			if v, err := strconv.Atoi(m[1]); err == nil {
				b.nSlots = v
			}
		}
	}
	if b.nCtx == 0 && b.nSlots > 0 {
		if m := reNCtxSlot.FindStringSubmatch(line); m != nil {
			if v, err := strconv.Atoi(m[1]); err == nil {
				b.nCtx = v * b.nSlots
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

// parseGeometry reads MemGeometry's lines (caller holds b.mu). Context lines
// add up only before the n_slots banner: every context the server keeps is
// built by then.
func (b *logBuffer) parseGeometry(line string) {
	g := &b.geo
	if b.nSlots == 0 {
		if m := reKVCells.FindStringSubmatch(line); m != nil {
			mib, _ := strconv.ParseFloat(m[1], 64)
			if cells, _ := strconv.Atoi(m[2]); cells > 0 {
				g.KVCellBytes += mib * (1 << 20) / float64(cells)
			}
			return
		}
		// Rows are cells × (1 + rs_seq): each sequence keeps rs_seq rollback
		// snapshots beside its live state, and only the live state is saved.
		if m := reRecurrent.FindStringSubmatch(line); m != nil {
			mib, _ := strconv.ParseFloat(m[1], 64)
			cells, _ := strconv.Atoi(m[2])
			rs, _ := strconv.Atoi(m[3])
			if cells > 0 {
				g.RSSeqBytes += mib * (1 << 20) / float64(cells*(1+rs))
			}
			return
		}
	}
	if g.NCtxSlot == 0 {
		if m := reNCtxSlot.FindStringSubmatch(line); m != nil {
			g.NCtxSlot, _ = strconv.Atoi(m[1])
		}
	}
	if g.CacheRAMBytes == 0 {
		if m := reCacheMiB.FindStringSubmatch(line); m != nil {
			mib, _ := strconv.ParseInt(m[1], 10, 64)
			g.CacheRAMBytes = mib << 20
		}
	}
	if g.CheckpointBytes == 0 {
		if m := reCkptMiB.FindStringSubmatch(line); m != nil {
			mib, _ := strconv.ParseFloat(m[1], 64)
			g.CheckpointBytes = mib * (1 << 20)
		}
	}
}

// Geometry returns the memory geometry parsed so far.
func (b *logBuffer) Geometry() MemGeometry {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.geo
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
