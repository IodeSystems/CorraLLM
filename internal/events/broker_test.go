package events

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPublishSubscribe(t *testing.T) {
	b := NewBroker()
	ch, unsub := b.Subscribe()
	defer unsub()

	b.Publish(Event{Type: "changed"})
	select {
	case ev := <-ch:
		if ev.Type != "changed" {
			t.Fatalf("got %q", ev.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("no event delivered")
	}
}

// TestPublishNeverBlocks: a subscriber that never drains must not block the
// publisher (events past the buffer are dropped).
func TestPublishNeverBlocks(t *testing.T) {
	b := NewBroker()
	_, unsub := b.Subscribe() // never drained
	defer unsub()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			b.Publish(Event{Type: "changed"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a slow subscriber")
	}
}

// TestUnsubscribe: after unsub the channel is closed and further publishes are
// safe (the subscriber is gone).
func TestUnsubscribe(t *testing.T) {
	b := NewBroker()
	ch, unsub := b.Subscribe()
	unsub()
	if _, open := <-ch; open {
		t.Fatal("channel not closed after unsubscribe")
	}
	b.Publish(Event{Type: "changed"}) // must not panic or block
	unsub()                           // idempotent
}

func TestNilBrokerPublish(t *testing.T) {
	var b *Broker
	b.Publish(Event{Type: "changed"}) // nil-safe no-op
}

// TestServeSSE streams an event to a connected client over a real HTTP server.
func TestServeSSE(t *testing.T) {
	b := NewBroker()
	srv := httptest.NewServer(http.HandlerFunc(b.ServeSSE))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}

	// Give the handler a moment to subscribe, then publish.
	time.Sleep(100 * time.Millisecond)
	b.Publish(Event{Type: "activity", Data: map[string]any{"served": "m"}})

	sc := bufio.NewScanner(resp.Body)
	var sawEvent, sawData bool
	deadline := time.Now().Add(2 * time.Second)
	for sc.Scan() && time.Now().Before(deadline) {
		line := sc.Text()
		if line == "event: activity" {
			sawEvent = true
		}
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"served":"m"`) {
			sawData = true
		}
		if sawEvent && sawData {
			break
		}
	}
	if !sawEvent || !sawData {
		t.Fatalf("did not observe streamed event (event=%v data=%v)", sawEvent, sawData)
	}
}
