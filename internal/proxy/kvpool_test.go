package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/store"
)

// kvFullBody is llama-server's reply to a batch it cannot fit in the KV
// cache (format_error_response, ERROR_TYPE_SERVER).
const kvFullBody = `{"error":{"code":500,"message":"Context size has been exceeded.","type":"server_error"}}`

func TestKVFullRetriedOnceBeforeAnyByte(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(kvFullBody))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatReply))
	}))
	t.Cleanup(up.Close)
	h, st := refusalRig(t, map[string]config.Model{"swift": modelTo(t, up.URL, "chat")}, nil)
	rec := post(h, "swift")
	if rec.Code != http.StatusOK || hits.Load() != 2 {
		t.Fatalf("status %d after %d upstream calls, want 200 after 2: %s", rec.Code, hits.Load(), rec.Body.String())
	}
	rows, _ := st.RecentActivity(store.Window{}, 10, "", "", "")
	if len(rows) != 1 || !strings.Contains(rows[0].Note, "kv-full-retried") {
		t.Fatalf("rows %+v, want one row noting kv-full-retried", rows)
	}
}

func TestKVFullSecondTimePassesThrough(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(kvFullBody))
	}))
	t.Cleanup(up.Close)
	h, _ := refusalRig(t, map[string]config.Model{"swift": modelTo(t, up.URL, "chat")}, nil)
	rec := post(h, "swift")
	if rec.Code != http.StatusInternalServerError || hits.Load() != 2 || !strings.Contains(rec.Body.String(), kvFullText) {
		t.Fatalf("status %d after %d calls, body %q: want llama-server's 500 after 2", rec.Code, hits.Load(), rec.Body.String())
	}
}

// Once a stream started the error event is the caller's: no retry.
func TestKVFullAfterStreamStartNotRetried(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"o\"}}]}\n\n"))
		_, _ = w.Write([]byte("error: " + kvFullBody + "\n\n"))
	}))
	t.Cleanup(up.Close)
	h, _ := refusalRig(t, map[string]config.Model{"swift": modelTo(t, up.URL, "chat")}, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"swift","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Corrallm-Key", "k")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if hits.Load() != 1 || rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), kvFullText) {
		t.Fatalf("status %d after %d calls, body %q: want the stream and its error passed through once", rec.Code, hits.Load(), rec.Body.String())
	}
}

// Another 500 is not the KV-full one: passed through, not retried.
func TestOther500NotRetried(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":500,"message":"Compute error.","type":"server_error"}}`))
	}))
	t.Cleanup(up.Close)
	h, _ := refusalRig(t, map[string]config.Model{"swift": modelTo(t, up.URL, "chat")}, nil)
	if rec := post(h, "swift"); rec.Code != http.StatusInternalServerError || hits.Load() != 1 {
		t.Fatalf("status %d after %d calls, want 500 after 1", rec.Code, hits.Load())
	}
}

// Two requests whose asks do not fit the pool together are served one at a
// time although the model has two slots; the second is noted as held.
func TestKVPoolSerializesOverPool(t *testing.T) {
	var cur, peak atomic.Int32
	gate := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-gate
		cur.Add(-1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatReply))
	}))
	t.Cleanup(up.Close)
	m := modelTo(t, up.URL, "chat")
	m.MaxConcurrent = 2
	m.KVPool = &config.KVPool{Tokens: 100, ReserveTokens: 50} // ~70 each: two do not fit
	h, st := refusalRig(t, map[string]config.Model{"swift": m}, nil)
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); codes[i] = post(h, "swift").Code }()
	}
	deadline := time.Now().Add(2 * time.Second)
	for cur.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // a second request, if admitted, would arrive now
	close(gate)
	wg.Wait()
	if codes[0] != 200 || codes[1] != 200 || peak.Load() != 1 {
		t.Fatalf("codes %v, peak concurrency %d: want 200s, one at a time", codes, peak.Load())
	}
	rows, _ := st.RecentActivity(store.Window{}, 10, "", "", "")
	held := 0
	for _, r := range rows {
		if strings.Contains(r.Note, "kv-pool-held") {
			held++
		}
	}
	if held != 1 {
		t.Errorf("%d rows note kv-pool-held, want 1: %+v", held, rows)
	}
}

func TestKVAskEstimate(t *testing.T) {
	p := &Proxy{}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	body := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("x", 4000) + `"}]}`)
	m := config.Model{KVPool: &config.KVPool{Tokens: 188416}}
	if a := p.kvAsk(r, config.Model{}, body); a != nil {
		t.Fatalf("no kvPool: ask %+v, want nil", a)
	}
	a := p.kvAsk(r, m, body)
	prompt := len(body) / bytesPerTokenEstimate
	if a.Tokens != prompt+config.DefaultKVReserveTokens || a.Pool != 188416 || a.Bypass != config.DefaultKVBypassFor {
		t.Errorf("ask %+v, want %d + default reserve, pool 188416, default bypass", a, prompt)
	}
	small := []byte(`{"max_tokens":100,"messages":[]}`)
	if a := p.kvAsk(r, m, small); a.Tokens != len(small)/bytesPerTokenEstimate+100 {
		t.Errorf("max_tokens 100: %d, want prompt + 100", a.Tokens)
	}
	m.ContextPerRequest = 500
	if a := p.kvAsk(r, m, body); a.Tokens != 500 {
		t.Errorf("contextPerRequest 500: %d, want the cap", a.Tokens)
	}
}
