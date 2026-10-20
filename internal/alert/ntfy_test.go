package alert

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSendThrottlesPerKeyAndForgetResets(t *testing.T) {
	got := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- r.Header.Get("Title") + "|" + r.Header.Get("Markdown") + "|" + string(b)
	}))
	t.Cleanup(srv.Close)
	n := New(srv.URL)
	n.Send("k", "T", "body", "warning")
	n.Send("k", "T", "again") // throttled
	select {
	case s := <-got:
		if s != "T|yes|body" {
			t.Errorf("push = %q", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no push")
	}
	n.Forget("k")
	n.Send("k", "T", "after forget")
	select {
	case s := <-got:
		if s != "T|yes|after forget" {
			t.Errorf("push = %q", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no push after Forget")
	}
	select {
	case s := <-got:
		t.Errorf("throttled push went out: %q", s)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestNilAndEmptyNotifierDropEverything(t *testing.T) {
	var n *Notifier
	n.Send("k", "t", "b")
	n.Forget("k")
	New("").Send("k", "t", "b")
}
