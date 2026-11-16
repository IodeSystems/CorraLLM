package cacheattr

import "math"

// Geometry is one llama-server's memory geometry, from its startup log (see
// proc.MemGeometry, which this mirrors). Zero fields were not seen.
type Geometry struct {
	NCtxSlot        int     // per-slot context cap
	KVCellBytes     float64 // KV bytes per token, target + draft contexts
	RSSeqBytes      float64 // one sequence's recurrent state; 0 for non-hybrid models
	CheckpointBytes float64 // one context checkpoint
	CacheRAMBytes   int64   // the prompt-cache size limit the server logged
}

// defaultCtxCheckpoints is llama.cpp's n_ctx_checkpoints default
// (common/common.h), used when the cmd does not set --ctx-checkpoints.
const defaultCtxCheckpoints = 32

// Capacity is what a served model's backends report about how many
// conversations they hold: concurrently (Slots) and warm (CacheSlots).
//
// A host prompt-cache entry (llama-server prompt_save) is the slot's
// sequence state in the target and draft contexts plus its context
// checkpoints. Its largest possible size, a slot filled to its context cap,
// is
//
//	worst_entry = rs_seq + n_ctx_slot × kv_per_cell + n_ckpt × checkpoint
//
// and the guaranteed count is
//
//	cache_slots = total_slots + floor(budget / worst_entry)
//
// Every conversation up to cache_slots stays warm even if each grows to the
// per-slot cap. CacheSlotsTypical is the same count at the hour's mean entry
// size instead of the worst one.
//
// budget is the smallest known of: the cache size limit the server logged,
// the cmd's --cache-ram, and prompt_cache_bytes + host_pool_arena_free_bytes
// {arena="cache"} from /metrics (patches p3/p4/p5). Every input comes from the
// backend; none is configured.
type Capacity struct {
	Slots int // /props total_slots summed over instances; 0 = not reported
	// CacheSlots is the guaranteed count; 0 when an input is unknown.
	CacheSlots      int
	WorstEntryBytes int64
	// CheckpointsCounted is false when the checkpoint size is unknown (none
	// seen in the log, and no recurrent state to stand in for one): the
	// worst entry then leaves checkpoints out and CacheSlots may overstate.
	CheckpointsCounted bool
	// CacheSlotsTypical is the count at the hour's mean entry size; 0 until a
	// sample holds minCacheEntries entries, or without cache metrics.
	CacheSlotsTypical int
	MeanEntryBytes    int64
	BudgetBytes       int64 // 0 = unknown
	// Churn is the share (0–1) of the hour's prompt-cache evictions whose
	// entry was under 5 min old: high means conversations are evicted while
	// still in use (thrash). -1 when the backend exposes no age histogram;
	// 0 with ChurnEvictions 0 when nothing was evicted.
	Churn          float64
	ChurnEvictions int64
}

// minCacheEntries is the fewest cache entries a sample must hold to count
// toward the mean entry size.
const minCacheEntries = 3

// Capacity returns the detected capacity of a served model's instances; ok
// false when no instance reported total_slots. With several instances a
// count is the sum, and is omitted when any instance cannot give it.
func (p *Poller) Capacity(served string) (c Capacity, ok bool) {
	c.Churn = -1
	if p == nil {
		return c, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	guaranteedOK, typicalOK := true, true
	c.CheckpointsCounted = true
	var young, count int64
	churnSeen := false
	for inst, sv := range p.served {
		if sv != served {
			continue
		}
		n := p.slots[inst]
		if n <= 0 {
			continue
		}
		ok = true
		c.Slots += n
		ic := instanceCapacity(n, p.samples[inst], p.cmds[inst], p.geos[inst])
		if ic.WorstEntryBytes > 0 {
			c.CacheSlots += ic.CacheSlots
			c.WorstEntryBytes = max(c.WorstEntryBytes, ic.WorstEntryBytes)
			c.CheckpointsCounted = c.CheckpointsCounted && ic.CheckpointsCounted
		} else {
			guaranteedOK = false
		}
		if ic.MeanEntryBytes > 0 {
			c.CacheSlotsTypical += ic.CacheSlotsTypical
			c.MeanEntryBytes = max(c.MeanEntryBytes, ic.MeanEntryBytes)
		} else {
			typicalOK = false
		}
		c.BudgetBytes += max(ic.BudgetBytes, 0)
		if y, k, has := churnOf(p.samples[inst]); has {
			churnSeen = true
			young += y
			count += k
		}
	}
	if !ok || !guaranteedOK {
		c.CacheSlots, c.WorstEntryBytes, c.CheckpointsCounted = 0, 0, false
	}
	if !ok || !typicalOK {
		c.CacheSlotsTypical, c.MeanEntryBytes = 0, 0
	}
	if churnSeen {
		c.ChurnEvictions, c.Churn = count, 0
		if count > 0 {
			c.Churn = float64(young) / float64(count)
		}
	}
	return c, ok
}

// cacheBudget is the smallest known cache budget, in bytes; -1 when none is
// known. --cache-ram 0 (cache off) is a known 0; -1 (no limit) is not a bound.
func cacheBudget(last *sample, cmd string, g Geometry) int64 {
	b := int64(-1)
	tighter := func(v int64) {
		if v >= 0 && (b < 0 || v < b) {
			b = v
		}
	}
	if g.CacheRAMBytes > 0 {
		tighter(g.CacheRAMBytes)
	}
	if mib := flagInt(cmd, "--cache-ram", "-cram"); mib >= 0 {
		tighter(int64(mib) << 20)
	}
	if last != nil && last.bytes >= 0 && last.arenaFree >= 0 {
		tighter(last.bytes + last.arenaFree)
	}
	return b
}

// worstEntry is the largest host-cache entry one slot can save, and whether
// checkpoints are in it. 0 when the context cap or KV size is unknown.
func worstEntry(cmd string, g Geometry) (bytes int64, ckptCounted bool) {
	if g.NCtxSlot <= 0 || g.KVCellBytes <= 0 {
		return 0, false
	}
	w := g.RSSeqBytes + float64(g.NCtxSlot)*g.KVCellBytes
	nCkpt := flagInt(cmd, "--ctx-checkpoints", "-ctxcp", "--swa-checkpoints")
	if nCkpt < 0 {
		nCkpt = defaultCtxCheckpoints
	}
	// A hybrid model's checkpoint is its recurrent state (Swift: 149.646 MiB
	// both), so that stands in until a checkpoint line is logged.
	ckpt := g.CheckpointBytes
	if ckpt <= 0 {
		ckpt = g.RSSeqBytes
	}
	switch {
	case nCkpt == 0:
		ckptCounted = true
	case ckpt > 0:
		w += float64(nCkpt) * ckpt
		ckptCounted = true
	}
	return int64(math.Ceil(w)), ckptCounted
}

// instanceCapacity computes one instance's counts, slots included.
func instanceCapacity(slots int, ring []sample, cmd string, g Geometry) Capacity {
	var last *sample
	if len(ring) > 0 {
		last = &ring[len(ring)-1]
	}
	c := Capacity{Slots: slots, BudgetBytes: cacheBudget(last, cmd, g)}
	if c.BudgetBytes < 0 {
		return c
	}
	if w, counted := worstEntry(cmd, g); w > 0 {
		c.WorstEntryBytes, c.CheckpointsCounted = w, counted
		c.CacheSlots = slots + int(c.BudgetBytes/w)
	}
	var sum float64
	n := 0
	for _, s := range ring {
		if s.entries >= minCacheEntries && s.bytes > 0 {
			sum += float64(s.bytes) / float64(s.entries)
			n++
		}
	}
	if n > 0 {
		if mean := int64(sum / float64(n)); mean > 0 {
			c.MeanEntryBytes = mean
			c.CacheSlotsTypical = slots + int(c.BudgetBytes/mean)
		}
	}
	return c
}

// churnOf is the rise of the young-eviction bucket and the eviction count
// across the ring (an hour). has false when no sample carries both, or a
// counter went backwards (a restart under one instance id).
func churnOf(ring []sample) (young, count int64, has bool) {
	var a, b *sample
	for i := range ring {
		s := &ring[i]
		if s.youngEvicted < 0 || s.evictedCount < 0 {
			continue
		}
		if a == nil {
			a = s
		}
		b = s
	}
	if a == nil || b.evictedCount < a.evictedCount || b.youngEvicted < a.youngEvicted {
		return 0, 0, false
	}
	return b.youngEvicted - a.youngEvicted, b.evictedCount - a.evictedCount, true
}
