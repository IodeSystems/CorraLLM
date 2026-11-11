package proxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/iodesystems/corrallm/internal/cacheattr"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

const llamaHelperEnv = "CORRALLM_PROXY_LLAMA_HELPER"

// fakeLlamaReply is what the stand-in llama-server sends for one turn:
// "prompt:cached:slot", slot empty for no id_slot.
func fakeLlamaReply(spec string) string {
	parts := strings.SplitN(spec, ":", 3)
	slot := ""
	if parts[2] != "" {
		slot = `,"id_slot":` + parts[2]
	}
	return fmt.Sprintf(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":%s,"completion_tokens":10},"timings":{"cache_n":%s,"prompt_per_second":1000}%s}`,
		parts[0], parts[1], slot)
}

// TestHelperLlama is the stand-in llama-server: the test binary re-executed
// by a spawn. Env: "port|turn,turn,...". It does nothing unless started so.
func TestHelperLlama(t *testing.T) {
	env := os.Getenv(llamaHelperEnv)
	if env == "" {
		t.Skip("stand-in llama-server for the cache attribution test")
	}
	port, specs, _ := strings.Cut(env, "|")
	turns := strings.Split(specs, ",")
	var mu sync.Mutex
	n := 0
	_ = http.ListenAndServe("127.0.0.1:"+port, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		mu.Lock()
		spec := turns[n%len(turns)]
		n++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fakeLlamaReply(spec)))
	}))
}

// Against a stand-in llama-server: a spawned backend (a cmd, so a process
// with an instance) whose replies carry timings.cache_n and, on some turns,
// id_slot. Each served chat turn gets an activity_cache row; the reply the
// caller sees is byte-for-byte what it gets with attribution off.
func TestCacheAttributionEndToEnd(t *testing.T) {
	turns := []string{
		"5000:0:",       // cold
		"9000:4990:0",   // hit
		"14000:100:1",   // slot-switch: the prefix matched, nothing cached, other slot
		"18000:13990:1", // hit
	}
	with, st := runFakeLlama(t, turns, true)
	without, _ := runFakeLlama(t, turns, false)
	for i := range turns {
		if with[i] != without[i] {
			t.Errorf("turn %d: with attribution the caller got %q, without %q", i, with[i], without[i])
		}
	}

	rows, err := st.CacheRows("mock", 0, time.Now().Add(time.Minute).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(turns) {
		t.Fatalf("rows = %d, want %d: %+v", len(rows), len(turns), rows)
	}
	wantClass := []string{cacheattr.ClassCold, cacheattr.ClassHit, cacheattr.ClassSlotSwitch, cacheattr.ClassHit}
	wantSlot := []int{-1, 0, 1, 1}
	for i, row := range rows {
		if row.Class != wantClass[i] || row.Slot != wantSlot[i] || row.Instance == "" {
			t.Errorf("row %d: class %s slot %d instance %q, want %s slot %d", i, row.Class, row.Slot, row.Instance, wantClass[i], wantSlot[i])
		}
		if i > 0 && row.PrevActivityID != rows[i-1].ActivityID {
			t.Errorf("row %d: prev activity %d, want %d", i, row.PrevActivityID, rows[i-1].ActivityID)
		}
		if i > 0 && row.MatchChars <= 0 {
			t.Errorf("row %d: no prefix match measured", i)
		}
	}
	if rows[2].LostTok < 8000 {
		t.Errorf("slot switch lost %d tokens, want ~8900", rows[2].LostTok)
	}
}

// runFakeLlama serves turns of one growing conversation through a proxy over
// a spawned stand-in, and returns the bodies the caller got.
func runFakeLlama(t *testing.T, turns []string, attribute bool) ([]string, *store.Store) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	cmd := fmt.Sprintf("%s='%d|%s' exec %s -test.run=^TestHelperLlama$ # --ctx-checkpoints 8",
		llamaHelperEnv, port, strings.Join(turns, ","), os.Args[0])
	cfg := mkConfig(t, "mock", fmt.Sprintf("http://127.0.0.1:%d", port))
	m := cfg.Models["mock"]
	m.Cmd = cmd
	cfg.Models["mock"] = m

	st, _ := store.Open(context.Background(), ":memory:")
	t.Cleanup(func() { _ = st.Close() })
	mgr := proc.NewManager(cfg)
	mgr.SetHealthTimeout(20 * time.Second)
	t.Cleanup(mgr.Shutdown)
	px := New(cfg, mgr, sched.New(), st)
	if attribute {
		px.SetCacheTracker(cacheattr.NewTracker(0, nil))
	}
	r := chi.NewRouter()
	px.Mount(r)

	long := strings.Repeat("system prompt ", 2000)
	msgs := `{"role":"system","content":"` + long + `"},{"role":"user","content":"task"}`
	var got []string
	for i := range turns {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"mock","messages":[`+msgs+`]}`)))
		if rec.Code != http.StatusOK {
			t.Fatalf("turn %d: status %d: %s", i, rec.Code, rec.Body.String())
		}
		got = append(got, rec.Body.String())
		msgs += fmt.Sprintf(`,{"role":"assistant","content":"ok"},{"role":"user","content":"%s"}`, strings.Repeat("more ", 4000))
	}
	return got, st
}

// id_slot is read from the top level of a reply, or from a streamed chunk;
// absent, it stays nil rather than reading as slot 0.
func TestExtractUsageIDSlot(t *testing.T) {
	if u := extractUsage([]byte(`{"usage":{"prompt_tokens":5},"id_slot":1}`), false); u.IDSlot == nil || *u.IDSlot != 1 {
		t.Errorf("non-streaming: %v", u.IDSlot)
	}
	if u := extractUsage([]byte(`{"usage":{"prompt_tokens":5}}`), false); u.IDSlot != nil {
		t.Errorf("absent: %v", *u.IDSlot)
	}
	stream := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"id_slot\":0}\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1}}\ndata: [DONE]\n")
	if u := extractUsage(stream, true); u.IDSlot == nil || *u.IDSlot != 0 {
		t.Errorf("streaming: %v", u.IDSlot)
	}
}
