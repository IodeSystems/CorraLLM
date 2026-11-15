package sched

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// kvBox is backend "b" with capacity 3 and a KV pool of 100 tokens, on a
// clock the test moves.
type kvBox struct {
	t   *testing.T
	s   *Scheduler
	off atomic.Int64 // clock offset, ns
}

const kvTestPool = 100

func newKVBox(t *testing.T) *kvBox {
	b := &kvBox{t: t, s: New()}
	base := time.Now()
	b.s.now = func() time.Time { return base.Add(time.Duration(b.off.Load())) }
	return b
}

func (b *kvBox) advance(d time.Duration) { b.off.Add(int64(d)) }

type kvReq struct {
	release func(...Done)
	ask     *KVAsk
	done    chan error
}

// admit starts an admission of tokens on "b" and returns at once.
func (b *kvBox) admit(ctx context.Context, tokens int) *kvReq {
	r := &kvReq{ask: &KVAsk{Tokens: tokens, Pool: kvTestPool, Bypass: 30 * time.Second}, done: make(chan error, 1)}
	go func() {
		rel, _, err := b.s.Admit(WithKVAsk(ctx, r.ask), "b", "local", 3, "g", 1, false, queueStage)
		r.release = rel
		r.done <- err
	}()
	return r
}

// granted waits for r's admission and fails on an error.
func (b *kvBox) granted(r *kvReq, what string) {
	b.t.Helper()
	select {
	case err := <-r.done:
		if err != nil {
			b.t.Fatalf("%s: %v", what, err)
		}
	case <-time.After(2 * time.Second):
		b.t.Fatalf("%s: not granted", what)
	}
}

// queued checks r is still waiting, after its waiter joined the queue.
func (b *kvBox) queued(r *kvReq, what string) {
	b.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		b.s.mu.Lock()
		in := false
		if bs := b.s.backends["b"]; bs != nil {
			for _, w := range bs.waiters {
				if w.slot.kv == r.ask {
					in = true
				}
			}
		}
		b.s.mu.Unlock()
		if in {
			break
		}
		select {
		case err := <-r.done:
			b.t.Fatalf("%s: admitted (err %v), want queued", what, err)
		default:
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("%s: never queued", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func (b *kvBox) load() BackendLoad {
	for _, l := range b.s.Snapshot().Backends {
		if l.Backend == "b" {
			return l
		}
	}
	return BackendLoad{}
}

func TestKVPoolHoldsUntilItFits(t *testing.T) {
	b := newKVBox(t)
	ctx := context.Background()
	a := b.admit(ctx, 60)
	b.granted(a, "a")
	c := b.admit(ctx, 50) // 60 + 50 > 100 with two slots free
	b.queued(c, "c")
	if !c.ask.Held.Load() {
		t.Error("c not marked held")
	}
	if l := b.load(); l.KVInService != 60 || l.KVHeld != 1 || l.KVHolds != 1 || l.KVPool != kvTestPool {
		t.Errorf("snapshot %+v, want in service 60, held 1, holds 1, pool 100", l)
	}
	d := b.admit(ctx, 40) // 60 + 40 = 100 fits: goes ahead inside the bypass window
	b.granted(d, "d")
	d.release()
	a.release() // 0 in service: c fits
	b.granted(c, "c after release")
	if l := b.load(); l.KVInService != 50 || l.KVPeak != 100 || l.KVHeld != 0 {
		t.Errorf("snapshot %+v, want in service 50, peak 100, held 0", l)
	}
}

func TestKVPoolLoneOversizedAdmitted(t *testing.T) {
	b := newKVBox(t)
	big := b.admit(context.Background(), 500)
	b.granted(big, "lone 500 > pool 100")
	if big.ask.Held.Load() {
		t.Error("lone request marked held")
	}
	// Queued behind something, then alone: admitted on the release.
	big.release()
	a := b.admit(context.Background(), 10)
	b.granted(a, "a")
	big2 := b.admit(context.Background(), 500)
	b.queued(big2, "big2")
	a.release()
	b.granted(big2, "big2 alone")
}

func TestKVPoolIdleSlotsNotCounted(t *testing.T) {
	b := newKVBox(t)
	ctx := context.Background()
	a := b.admit(ctx, 90)
	b.granted(a, "a")
	a.release() // a's llama.cpp slot is idle now: llama-server clears it
	c := b.admit(ctx, 90)
	b.granted(c, "c")
	d := b.admit(ctx, 10)
	b.granted(d, "d: 90 + 10 = 100 fits")
	if l := b.load(); l.KVInService != 100 {
		t.Errorf("in service %d, want 100", l.KVInService)
	}
}

func TestKVPoolBypassWindow(t *testing.T) {
	b := newKVBox(t)
	ctx := context.Background()
	a := b.admit(ctx, 60)
	b.granted(a, "a")
	h := b.admit(ctx, 50)
	b.queued(h, "h")
	b.advance(time.Second)
	s1 := b.admit(ctx, 30) // fits, inside the window: goes ahead
	b.granted(s1, "s1")
	b.advance(30 * time.Second) // h's window ran out
	s2 := b.admit(ctx, 5)       // would fit (95), but h is starved
	b.queued(s2, "s2")
	s1.release() // 60 in service: h (50) still does not fit, s2 must not pass it
	time.Sleep(20 * time.Millisecond)
	select {
	case <-s2.done:
		t.Fatal("s2 admitted past a starved waiter")
	default:
	}
	a.release() // 0 in service: h, then s2 (50 + 5)
	b.granted(h, "h")
	b.granted(s2, "s2 after h")
}

func TestKVPoolHeldWaiterLeavingPromotesOthers(t *testing.T) {
	b := newKVBox(t)
	a := b.admit(context.Background(), 60)
	b.granted(a, "a")
	hctx, cancel := context.WithCancel(context.Background())
	h := b.admit(hctx, 50)
	b.queued(h, "h")
	b.advance(31 * time.Second)
	s := b.admit(context.Background(), 5)
	b.queued(s, "s (h starved)")
	cancel()
	if err := <-h.done; err == nil {
		t.Fatal("h: admitted, want the cancel")
	}
	b.granted(s, "s after h left")
}

func TestKVPoolOffWithoutAsk(t *testing.T) {
	s := New()
	var rels []func(...Done)
	for i := range 3 {
		r, _, err := s.Admit(context.Background(), "b", "local", 3, "g", 1, false, queueStage)
		if err != nil {
			t.Fatalf("%d: %v", i, err)
		}
		rels = append(rels, r)
	}
	if l := s.Snapshot().Backends[0]; l.KVPool != 0 || l.KVInService != 0 {
		t.Errorf("snapshot %+v, want no KV fields", l)
	}
	for _, r := range rels {
		r()
	}
}
