package proxy

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

// closingBackend is a loopback backend that answers a chat request with a
// keep-alive response and then, ONCE, closes that connection when the next
// chat request arrives on it, before any reply. That is what llama-server
// does after a streamed reply (it closes the socket and sends no
// `Connection: close`), seen from a client that reused the connection inside
// the window before the close reached it.
type closingBackend struct {
	addr     string
	accepted atomic.Int32 // connections dialled
	served   atomic.Int32 // chat requests answered
	dropped  atomic.Bool  // the one drop happened
}

func newClosingBackend(t *testing.T) *closingBackend {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	b := &closingBackend{addr: "http://" + l.Addr().String()}
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			b.accepted.Add(1)
			wg.Add(1)
			go func() { defer wg.Done(); b.serve(c) }()
		}
	}()
	return b
}

func (b *closingBackend) serve(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	answeredChat := false
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, req.Body)
		body := "{}"
		if req.URL.Path == "/v1/chat/completions" {
			if answeredChat && b.dropped.CompareAndSwap(false, true) {
				return // the request reached a socket the server already closed
			}
			answeredChat = true
			b.served.Add(1)
			body = chatReply
		}
		_, err = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: "+
			strconv.Itoa(len(body))+"\r\n\r\n"+body)
		if err != nil {
			return
		}
	}
}

// A request that lands on a reused keep-alive connection the local backend
// has just closed gets EOF before any byte of a reply. Nothing reached the
// model, so it is sent again once on a fresh connection and the caller gets
// the answer, not a 503 (11-15..11-17: 204 such 503s, each within ~5 ms of
// another reply on the same backend).
func TestLocalStaleKeepAliveIsRetriedOnAFreshConnection(t *testing.T) {
	b := newClosingBackend(t)
	h, _ := refusalRig(t, map[string]config.Model{"swift": modelTo(t, b.addr, "chat")}, nil)
	if rec := post(h, "swift"); rec.Code != http.StatusOK {
		t.Fatalf("first request: status %d: %s", rec.Code, rec.Body.String())
	}
	rec := post(h, "swift")
	if !b.dropped.Load() {
		t.Fatal("the second request did not reuse the first connection; the test proves nothing")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("second request on the stale connection: status %d, want 200 after one retry: %s", rec.Code, rec.Body.String())
	}
	if got := b.served.Load(); got != 2 {
		t.Errorf("backend answered %d chat requests, want 2", got)
	}
}

// The retry is for a REUSED connection only. A fresh connection that closes
// before any reply is a backend going down (llama-server on SIGTERM): still
// 503 + Retry-After, sent once — TestLocalEOFBeforeReplyIs503RetryAfter covers
// the status; this covers the count.
func TestLocalEOFOnFreshConnectionIsNotRetried(t *testing.T) {
	var hits atomic.Int32
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			req, err := http.ReadRequest(bufio.NewReader(c))
			if err == nil && req.URL.Path == "/v1/chat/completions" {
				hits.Add(1)
			}
			_ = c.Close()
		}
	}()
	h, _ := refusalRig(t, map[string]config.Model{"swift": modelTo(t, "http://"+l.Addr().String(), "chat")}, nil)
	assertRestarting(t, post(h, "swift"))
	if got := hits.Load(); got != 1 {
		t.Errorf("backend saw %d chat requests, want 1 (no retry on a fresh connection)", got)
	}
}
