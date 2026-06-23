// Package events is corrallm's minimal pub/sub for live UI updates. The proxy
// publishes lightweight events (a new activity record, a "state changed" ping)
// as requests flow; an SSE endpoint fans them out to subscribed browsers so the
// observability views update on push instead of polling (P8-beyond).
//
// Server-Sent Events (not WebSocket) carry the stream: the traffic is purely
// server→client, SSE needs no extra dependency, and the browser's EventSource
// auto-reconnects. This is a deviation from the plan's "ws (subBroker-style)" —
// the subBroker fan-out pattern is preserved; only the wire protocol differs.
//
// Delivery is best-effort: a subscriber whose buffer is full drops the event
// rather than blocking the request path. Events are advisory — the UI keeps a
// slow fallback poll — so a dropped "changed" ping only delays a refresh.
package events

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Event is one broadcast message. Type is "activity" (Data is the new record)
// or "changed" (Data nil — a hint to refetch live views).
type Event struct {
	Type string `json:"type"`
	Data any    `json:"data,omitempty"`
}

// Broker fans events out to current subscribers.
type Broker struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

// NewBroker constructs an empty Broker.
func NewBroker() *Broker {
	return &Broker{subs: map[chan Event]struct{}{}}
}

// Subscribe registers a buffered channel and returns it with an unsubscribe
// func that closes and removes it. Always call unsubscribe (defer it).
func (b *Broker) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 32)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, ch)
			b.mu.Unlock()
			close(ch)
		})
	}
}

// Publish delivers e to every subscriber, skipping any whose buffer is full
// (best-effort, never blocks the caller).
func (b *Broker) Publish(e Event) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- e:
		default: // slow subscriber: drop rather than block the request path
		}
	}
}

// ServeSSE streams events to the client as Server-Sent Events until the request
// context goes away. A periodic comment keeps intermediaries from idling out.
func (b *Broker) ServeSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // disable proxy buffering

	ch, unsub := b.Subscribe()
	defer unsub()

	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	ctx := r.Context()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case ev := <-ch:
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
			flusher.Flush()
		}
	}
}
