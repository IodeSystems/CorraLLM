package cacheattr

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Target is one running backend the poller reads.
type Target struct {
	Served   string
	Instance string
	BaseURL  string
	// Cmd is the command the instance was spawned with. Read only for the
	// cache budget and --ctx-checkpoints.
	Cmd string
	// Geometry is the backend's memory geometry from its startup log.
	Geometry Geometry
}

// sample is one /metrics read. -1 = the counter is not exposed.
type sample struct {
	ts        int64
	entries   int64
	bytes     int64
	evictions int64
	// arenaFree is host_pool_arena_free_bytes{arena="cache"} (patch p3/p4):
	// bytes + arenaFree is the cache budget the backend actually has.
	arenaFree int64
	// youngEvicted and evictedCount are the prompt_cache_evicted_age_seconds
	// histogram's le="300" bucket and _count: evictions of entries under
	// 5 min old, and all evictions.
	youngEvicted int64
	evictedCount int64
}

// pollRing is an hour of samples at the default interval.
const pollRing = 240

// Poller reads each instance's prompt-cache counters from /metrics every
// interval (never per request) and keeps an hour of them.
//
// The counters come from patch 0005 (P75 N3): llama-server's /metrics carries
// no prompt-cache figures without it, and a backend started without
// --metrics answers 404/501. Either way the instance reads as "not exposed"
// (-1) and the classifier falls back to the other-traffic proxy. Names are
// matched by suffix (prompt_cache_entries, _bytes, _evictions[_total]) so the
// `llamacpp:` prefix can change with D2 without touching this.
type Poller struct {
	targets func() []Target
	every   time.Duration
	client  *http.Client

	mu      sync.Mutex
	samples map[string][]sample // instance → oldest first
	served  map[string]string   // instance → served
	absent  map[string]bool     // instance → /metrics has no prompt-cache counters
	slots   map[string]int      // instance → /props total_slots; -1 = /props missing
	cmds    map[string]string   // instance → spawn cmd
	geos    map[string]Geometry // instance → memory geometry
	poke    chan struct{}
}

// NewPoller makes a poller over the targets the function returns. every 0 =
// 15 s.
func NewPoller(targets func() []Target, every time.Duration) *Poller {
	if every <= 0 {
		every = 15 * time.Second
	}
	return &Poller{targets: targets, every: every, client: &http.Client{Timeout: 5 * time.Second},
		samples: map[string][]sample{}, served: map[string]string{}, absent: map[string]bool{},
		slots: map[string]int{}, cmds: map[string]string{}, geos: map[string]Geometry{}, poke: make(chan struct{}, 1)}
}

// Poke asks Run for a poll now instead of at the next tick: a backend that
// just became ready reports its slots without waiting up to one interval.
func (p *Poller) Poke() {
	if p == nil {
		return
	}
	select {
	case p.poke <- struct{}{}:
	default:
	}
}

// Run polls until ctx ends.
func (p *Poller) Run(ctx context.Context) {
	t := time.NewTicker(p.every)
	defer t.Stop()
	for {
		p.PollOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-p.poke:
		}
	}
}

// PollOnce reads every target once, and forgets instances no longer running.
func (p *Poller) PollOnce(ctx context.Context) {
	if p == nil || p.targets == nil {
		return
	}
	live := map[string]bool{}
	for _, tg := range p.targets() {
		live[tg.Instance] = true
		p.mu.Lock()
		absent := p.absent[tg.Instance]
		p.served[tg.Instance] = tg.Served
		p.cmds[tg.Instance] = tg.Cmd
		p.geos[tg.Instance] = tg.Geometry
		_, slotsKnown := p.slots[tg.Instance]
		p.mu.Unlock()
		// total_slots is fixed for a process's life: read once. A backend with
		// no /props (not llama-server) reads -1 and is not asked again.
		if !slotsKnown {
			if n, err := p.readSlots(ctx, tg.BaseURL); err != nil {
				slog.Debug("backend /props", "served", tg.Served, "err", err)
			} else {
				p.mu.Lock()
				p.slots[tg.Instance] = n
				p.mu.Unlock()
			}
		}
		if absent {
			continue
		}
		s, ok, err := p.read(ctx, tg.BaseURL)
		if err != nil {
			slog.Debug("prompt-cache metrics", "served", tg.Served, "err", err)
			continue // transient: try again next tick
		}
		p.mu.Lock()
		if !ok {
			p.absent[tg.Instance] = true
		} else {
			ring := append(p.samples[tg.Instance], s)
			if len(ring) > pollRing {
				ring = ring[len(ring)-pollRing:]
			}
			p.samples[tg.Instance] = ring
		}
		p.mu.Unlock()
	}
	p.mu.Lock()
	for inst := range p.served {
		if !live[inst] {
			delete(p.served, inst)
			delete(p.samples, inst)
			delete(p.absent, inst)
			delete(p.slots, inst)
			delete(p.cmds, inst)
			delete(p.geos, inst)
		}
	}
	p.mu.Unlock()
}

// read fetches one /metrics. ok false: the endpoint is missing or carries
// no prompt-cache counter.
func (p *Poller) read(ctx context.Context, base string) (sample, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/metrics", nil)
	if err != nil {
		return sample{}, false, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return sample{}, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNotImplemented {
		return sample{}, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return sample{}, false, errStatus(resp.StatusCode)
	}
	s, ok := parseMetrics(io.LimitReader(resp.Body, 1<<20))
	s.ts = time.Now().UnixMilli()
	return s, ok, nil
}

// readSlots fetches /props total_slots. -1 (no error): the endpoint is
// missing or does not say.
func (p *Poller) readSlots(ctx context.Context, base string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/props", nil)
	if err != nil {
		return 0, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNotImplemented {
		return -1, nil
	}
	if resp.StatusCode != http.StatusOK {
		return 0, errStatus(resp.StatusCode)
	}
	return parseProps(io.LimitReader(resp.Body, 4<<20))
}

// parseProps reads total_slots from a llama-server /props body; -1 when the
// body has none.
func parseProps(r io.Reader) (int, error) {
	var pr struct {
		TotalSlots int `json:"total_slots"`
	}
	if err := json.NewDecoder(r).Decode(&pr); err != nil {
		return 0, err
	}
	if pr.TotalSlots <= 0 {
		return -1, nil
	}
	return pr.TotalSlots, nil
}

type errStatus int

func (e errStatus) Error() string { return "metrics returned " + strconv.Itoa(int(e)) }

// parseMetrics reads the prompt-cache counters from Prometheus text.
func parseMetrics(r io.Reader) (sample, bool) {
	s := sample{entries: -1, bytes: -1, evictions: -1, arenaFree: -1, youngEvicted: -1, evictedCount: -1}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		name, rest, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		labels := ""
		if i := strings.IndexByte(line, '{'); i >= 0 && i < len(name) {
			if j := strings.LastIndexByte(line, '}'); j > i {
				name, labels, rest = line[:i], line[i+1:j], line[j+1:]
			}
		}
		f, err := strconv.ParseFloat(strings.Fields(rest + " x")[0], 64)
		if err != nil {
			continue
		}
		switch {
		case strings.HasSuffix(name, "prompt_cache_entries"):
			s.entries = int64(f)
		case strings.HasSuffix(name, "prompt_cache_bytes"):
			s.bytes = int64(f)
		case strings.HasSuffix(name, "prompt_cache_evictions"), strings.HasSuffix(name, "prompt_cache_evictions_total"):
			// 0005 counts by reason. "superseded" is an entry that was a
			// prefix of a newer one: its tokens are still cached, so it is
			// not a loss. The rest (budget, fragmentation, tokens) are.
			if strings.Contains(labels, `reason="superseded"`) {
				continue
			}
			s.evictions = max(s.evictions, 0) + int64(f)
		case strings.HasSuffix(name, "host_pool_arena_free_bytes"):
			if strings.Contains(labels, `arena="cache"`) {
				s.arenaFree = int64(f)
			}
		case strings.HasSuffix(name, "prompt_cache_evicted_age_seconds_bucket"):
			if strings.Contains(labels, `le="300"`) {
				s.youngEvicted = int64(f)
			}
		case strings.HasSuffix(name, "prompt_cache_evicted_age_seconds_count"):
			s.evictedCount = int64(f)
		}
	}
	return s, s.entries >= 0 || s.bytes >= 0 || s.evictions >= 0
}

// EvictionsBetween implements Evictions: the counter's rise from the last
// sample at or before fromMS to the first at or after toMS (else the newest).
// -1 when the instance exposes no counter, when no sample precedes fromMS, or
// when the counter went backwards (the process restarted under one id).
func (p *Poller) EvictionsBetween(instance string, fromMS, toMS int64) int {
	if p == nil {
		return -1
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	ring := p.samples[instance]
	var a, b *sample
	for i := range ring {
		s := &ring[i]
		if s.evictions < 0 {
			continue
		}
		if s.ts <= fromMS {
			a = s
		}
		if b == nil && s.ts >= toMS {
			b = s
		}
	}
	if b == nil && len(ring) > 0 {
		b = &ring[len(ring)-1]
	}
	if a == nil || b == nil || b.evictions < a.evictions || b.evictions < 0 {
		return -1
	}
	return int(b.evictions - a.evictions)
}

// Latest returns the newest counters summed over a served model's instances,
// and evictions per hour over the ring; ok false when none has any.
func (p *Poller) Latest(served string) (entries, bytes int64, evictionsPerHour float64, ok bool) {
	if p == nil {
		return -1, -1, -1, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	entries, bytes, evictionsPerHour = -1, -1, -1
	for inst, sv := range p.served {
		ring := p.samples[inst]
		if sv != served || len(ring) == 0 {
			continue
		}
		ok = true
		last := ring[len(ring)-1]
		if last.entries >= 0 {
			entries = max(entries, 0) + last.entries
		}
		if last.bytes >= 0 {
			bytes = max(bytes, 0) + last.bytes
		}
		first := ring[0]
		if span := last.ts - first.ts; span > 0 && last.evictions >= first.evictions && first.evictions >= 0 {
			evictionsPerHour = max(evictionsPerHour, 0) + float64(last.evictions-first.evictions)*3.6e6/float64(span)
		}
	}
	return entries, bytes, evictionsPerHour, ok
}
