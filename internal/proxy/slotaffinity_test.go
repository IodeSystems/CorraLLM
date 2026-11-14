package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestSlotAssigner(t *testing.T) {
	t.Run("previous slot free: pinned", func(t *testing.T) {
		var a slotAssigner
		slot, out, rel := a.acquire("m", "i1", 2, 1, -1, false, true)
		if slot != 1 || out != affPinned {
			t.Fatalf("got %d %s", slot, out)
		}
		rel()
	})
	t.Run("previous slot busy: the other one", func(t *testing.T) {
		var a slotAssigner
		_, _, rel1 := a.acquire("m", "i1", 2, 1, -1, false, true)
		slot, out, rel2 := a.acquire("m", "i1", 2, 1, -1, false, true)
		if slot != 0 || out != affMoved {
			t.Fatalf("got %d %s", slot, out)
		}
		rel1()
		rel2()
	})
	t.Run("both busy: unpinned, and nothing pinned while it runs", func(t *testing.T) {
		var a slotAssigner
		_, _, rel0 := a.acquire("m", "i1", 2, 0, -1, false, true)
		_, _, rel1 := a.acquire("m", "i1", 2, 1, -1, false, true)
		slot, out, rel2 := a.acquire("m", "i1", 2, 0, -1, false, true)
		if slot != -1 || out != affUnpinned {
			t.Fatalf("got %d %s", slot, out)
		}
		rel0() // slot 0 frees; the unpinned request may be on it now
		if slot, out, rel := a.acquire("m", "i1", 2, 0, -1, false, true); slot != -1 || out != affUnpinned {
			t.Fatalf("with an untracked request in flight: got %d %s", slot, out)
		} else {
			rel()
		}
		rel2()
		rel1()
		if slot, out, rel := a.acquire("m", "i1", 2, 0, -1, false, true); slot != 0 || out != affPinned {
			t.Fatalf("after all released: got %d %s", slot, out)
		} else {
			rel()
		}
	})
	t.Run("no previous slot: least recently assigned free one", func(t *testing.T) {
		var a slotAssigner
		_, _, rel := a.acquire("m", "i1", 2, 0, -1, false, true)
		rel()
		slot, out, rel := a.acquire("m", "i1", 2, -1, -1, false, true)
		if slot != 1 || out != affAssigned {
			t.Fatalf("got %d %s", slot, out)
		}
		rel()
	})
	t.Run("unknown parallel: nothing", func(t *testing.T) {
		var a slotAssigner
		slot, out, rel := a.acquire("m", "i1", -1, 1, -1, false, true)
		if slot != -1 || out != affUnknown {
			t.Fatalf("got %d %s", slot, out)
		}
		rel()
		if slot, out, _ := a.acquire("m", "", 2, 1, -1, false, true); slot != -1 || out != affUnknown {
			t.Fatalf("no instance: got %d %s", slot, out)
		}
	})
	t.Run("client-sent id_slot: not replaced, but counted", func(t *testing.T) {
		var a slotAssigner
		slot, out, rel := a.acquire("m", "i1", 2, 1, 1, true, true)
		if slot != -1 || out != affClient {
			t.Fatalf("got %d %s", slot, out)
		}
		if slot, out, rel2 := a.acquire("m", "i1", 2, 1, -1, false, true); slot != 0 || out != affMoved {
			t.Fatalf("slot 1 held by the client's request: got %d %s", slot, out)
		} else {
			rel2()
		}
		rel()
	})
	t.Run("release is idempotent (cancel path plus defer)", func(t *testing.T) {
		var a slotAssigner
		_, _, rel := a.acquire("m", "i1", 2, 1, -1, false, true)
		rel()
		rel()
		_, _, held := a.acquire("m", "i1", 2, 1, -1, false, true) // slot 1
		if slot, out, rel := a.acquire("m", "i1", 2, 0, -1, false, true); slot != 0 || out != affPinned {
			t.Fatalf("a double release freed someone else's slot: got %d %s", slot, out)
		} else {
			rel()
		}
		held()
	})
	t.Run("instance change resets", func(t *testing.T) {
		var a slotAssigner
		_, _, old := a.acquire("m", "i1", 2, 0, -1, false, true)
		_, _, old2 := a.acquire("m", "i1", 2, 1, -1, false, true)
		slot, out, rel := a.acquire("m", "i2", 2, 0, -1, false, true)
		if slot != 0 || out != affPinned {
			t.Fatalf("new instance: got %d %s", slot, out)
		}
		old() // the old instance's leases must not free the new one's slots
		old2()
		if slot, out, rel2 := a.acquire("m", "i2", 2, 0, -1, false, true); slot != 1 || out != affMoved {
			t.Fatalf("got %d %s", slot, out)
		} else {
			rel2()
		}
		rel()
	})
	t.Run("not a chat body: counted, never pinned", func(t *testing.T) {
		var a slotAssigner
		_, out, rel := a.acquire("m", "i1", 2, -1, -1, false, false)
		if out != affUnpinned {
			t.Fatalf("got %s", out)
		}
		if slot, _, rel2 := a.acquire("m", "i1", 2, 0, -1, false, true); slot != -1 {
			t.Fatalf("pinned with an untracked request in flight: %d", slot)
		} else {
			rel2()
		}
		rel()
	})
	t.Run("concurrent acquire/release balances", func(t *testing.T) {
		var a slotAssigner
		var wg sync.WaitGroup
		for i := range 64 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _, rel := a.acquire("m", "i1", 2, i%3-1, -1, false, true)
				rel()
			}()
		}
		wg.Wait()
		st := a.by["m"]
		if st.busy[0] != 0 || st.busy[1] != 0 || st.untracked != 0 {
			t.Fatalf("leaked: %+v", st)
		}
	})
}

func TestIDSlotBody(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`{"model":"m"}`, `{"id_slot":1,"model":"m"}`},
		{` { "model":"m"}`, ` {"id_slot":1, "model":"m"}`},
		{`{}`, `{"id_slot":1}`},
		{`{ }`, `{"id_slot":1 }`},
	} {
		got, ok := withIDSlot([]byte(c.in), 1)
		if !ok || string(got) != c.want {
			t.Errorf("%q → %q, want %q", c.in, got, c.want)
		}
	}
	if _, ok := withIDSlot([]byte(`[1]`), 1); ok {
		t.Error("spliced into an array")
	}
	for _, c := range []struct {
		in   string
		slot int
		sent bool
	}{
		{`{"model":"m"}`, -1, false},
		{`{"id_slot":1,"model":"m"}`, 1, true},
		{`{"id_slot":-1}`, -1, true},
		{`{"id_slot":null}`, -1, true},
		{`{"messages":[{"content":"\"id_slot\": 3"}]}`, -1, false}, // in content, not top level
	} {
		if slot, sent := clientIDSlot([]byte(c.in)); slot != c.slot || sent != c.sent {
			t.Errorf("%s: %d %v, want %d %v", c.in, slot, sent, c.slot, c.sent)
		}
	}
}

const slotHelperEnv = "CORRALLM_PROXY_SLOT_HELPER"

// TestHelperSlotLlama is a stand-in llama-server that writes every chat body
// it receives to a file and answers with id_slot = the body's id_slot (0 when
// absent). Env: "port|dir".
func TestHelperSlotLlama(t *testing.T) {
	env := os.Getenv(slotHelperEnv)
	if env == "" {
		t.Skip("stand-in llama-server for the slot affinity test")
	}
	port, dir, _ := strings.Cut(env, "|")
	var mu sync.Mutex
	n := 0
	_ = http.ListenAndServe("127.0.0.1:"+port, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		body := make([]byte, r.ContentLength)
		_, _ = io.ReadFull(r.Body, body)
		mu.Lock()
		_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("%03d.json", n)), body, 0o600)
		n++
		mu.Unlock()
		slot, _ := clientIDSlot(body)
		if slot < 0 {
			slot = 0
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":100,"completion_tokens":10},"timings":{"cache_n":90,"prompt_per_second":1000},"id_slot":%d}`, slot)
	}))
}

// slotRig is a proxy in front of the stand-in llama-server (--parallel 2,
// maxConcurrent 2) with the given flags. send posts one chat body as caller
// key; bodies returns what the backend got, in order; rows the cache rows.
type slotRig struct {
	t    *testing.T
	dir  string
	h    http.Handler
	st   *store.Store
	sent int
}

func newSlotRig(t *testing.T, affinity, dequeue bool) *slotRig {
	t.Helper()
	dir := t.TempDir()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	cfg := mkConfig(t, "mock", fmt.Sprintf("http://127.0.0.1:%d", port))
	m := cfg.Models["mock"]
	m.Cmd = fmt.Sprintf("%s='%d|%s' exec %s -test.run=^TestHelperSlotLlama$ # --parallel 2", slotHelperEnv, port, dir, os.Args[0])
	m.MaxConcurrent = 2
	m.SlotAffinity = affinity
	m.SlotAwareDequeue = dequeue
	cfg.Models["mock"] = m
	st, _ := store.Open(context.Background(), ":memory:")
	t.Cleanup(func() { _ = st.Close() })
	mgr := proc.NewManager(cfg)
	mgr.SetHealthTimeout(20 * time.Second)
	t.Cleanup(mgr.Shutdown)
	px := New(cfg, mgr, sched.New(), st)
	px.SetCacheTracker(cacheattr.NewTracker(0, nil))
	r := chi.NewRouter()
	px.Mount(r)
	return &slotRig{t: t, dir: dir, h: r, st: st}
}

func (g *slotRig) send(body, key string) *httptest.ResponseRecorder {
	g.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	g.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		g.t.Fatalf("turn %d: %d %s", g.sent, rec.Code, rec.Body.String())
	}
	g.sent++
	return rec
}

func (g *slotRig) bodies() []string {
	g.t.Helper()
	var got []string
	for i := range g.sent {
		b, err := os.ReadFile(filepath.Join(g.dir, fmt.Sprintf("%03d.json", i)))
		if err != nil {
			g.t.Fatal(err)
		}
		got = append(got, string(b))
	}
	return got
}

func (g *slotRig) rows() []store.CacheRow {
	g.t.Helper()
	rows, err := g.st.CacheRows("mock", 0, time.Now().Add(time.Minute).UnixMilli())
	if err != nil {
		g.t.Fatal(err)
	}
	return rows
}

// Off: the backend gets the client's bytes and no X-Corrallm-Slot. On
// (slotAffinity, or slotAwareDequeue alone): the same bytes with "id_slot":N
// spliced first, X-Corrallm-Slot names it, and activity_cache says what was
// done.
func TestSlotAffinityEndToEnd(t *testing.T) {
	run := func(affinity, dequeue bool) ([]string, []store.CacheRow, []string, []string) {
		g := newSlotRig(t, affinity, dequeue)
		msgs := `{"role":"system","content":"` + strings.Repeat("sys ", 300) + `"},{"role":"user","content":"task"}`
		var sent, hdrs []string
		for range 3 {
			body := `{"model":"mock","messages":[` + msgs + `]}`
			sent = append(sent, body)
			hdrs = append(hdrs, g.send(body, "").Header().Get(HeaderSlot))
			msgs += `,{"role":"assistant","content":"ok"},{"role":"user","content":"more"}`
		}
		return g.bodies(), g.rows(), sent, hdrs
	}

	got, rows, sent, hdrs := run(false, false)
	for i := range got {
		if got[i] != sent[i] {
			t.Errorf("off, turn %d: the backend got a different body:\n got %.120s\nsent %.120s", i, got[i], sent[i])
		}
		if hdrs[i] != "" {
			t.Errorf("off, turn %d: %s %q set", i, HeaderSlot, hdrs[i])
		}
	}
	for _, row := range rows {
		if row.Affinity != "" {
			t.Errorf("off: affinity %q recorded", row.Affinity)
		}
	}

	for _, mode := range []struct {
		name              string
		affinity, dequeue bool
	}{{"slotAffinity", true, false}, {"slotAwareDequeue", false, true}} {
		got, rows, sent, hdrs = run(mode.affinity, mode.dequeue)
		wantOut := []string{affAssigned, affPinned, affPinned}
		for i := range got {
			want := `{"id_slot":0,` + sent[i][1:]
			if got[i] != want {
				t.Errorf("%s, turn %d:\n got %.120s\nwant %.120s", mode.name, i, got[i], want)
			}
			if !strings.HasPrefix(hdrs[i], "pgid:") || !strings.HasSuffix(hdrs[i], "/0") {
				t.Errorf("%s, turn %d: %s = %q, want pgid:<n>/0", mode.name, i, HeaderSlot, hdrs[i])
			}
		}
		if len(rows) != 3 {
			t.Fatalf("%s: rows %d", mode.name, len(rows))
		}
		for i, row := range rows {
			if row.Affinity != wantOut[i] || row.Slot != 0 {
				t.Errorf("%s, row %d: affinity %q slot %d, want %q 0", mode.name, i, row.Affinity, row.Slot, wantOut[i])
			}
			if want := row.Instance + "/0"; hdrs[i] != want {
				t.Errorf("%s, row %d: header %q, want %q (the row's instance)", mode.name, i, hdrs[i], want)
			}
		}
	}
}

// Unsettled turns (no slot home) are spread over the free slots, least
// recently assigned first, rather than all landing on slot 0.
func TestSlotAwareDequeueSpreadsUnsettled(t *testing.T) {
	g := newSlotRig(t, false, true)
	var hdrs []string
	for i := range 4 {
		body := fmt.Sprintf(`{"model":"mock","messages":[{"role":"user","content":"new conversation %d"}]}`, i)
		hdrs = append(hdrs, g.send(body, "").Header().Get(HeaderSlot))
	}
	for i, want := range []string{"/0", "/1", "/0", "/1"} {
		if !strings.HasSuffix(hdrs[i], want) {
			t.Errorf("unsettled turn %d: %s = %q, want slot %s", i, HeaderSlot, hdrs[i], want)
		}
	}
}

// prompt_cache_key names the conversation for slot assignment: a turn whose
// head changed (pruned) still goes home; the same string from another caller
// key is another conversation; the field reaches the backend untouched.
func TestPromptCacheKeySlotHome(t *testing.T) {
	g := newSlotRig(t, false, true)
	turn := func(key, head string) string {
		return `{"model":"mock","prompt_cache_key":"` + key + `","messages":[{"role":"system","content":"` + head + `"},{"role":"user","content":"go"}]}`
	}
	g.send(turn("sess", "head one"), "sk-a")             // assigned slot 0
	g.send(turn("other", "x"), "sk-a")                   // assigned slot 1
	h := g.send(turn("sess", "head two pruned"), "sk-a") // home: slot 0
	if !strings.HasSuffix(h.Header().Get(HeaderSlot), "/0") {
		t.Errorf("same prompt_cache_key: %s = %q, want its home slot 0", HeaderSlot, h.Header().Get(HeaderSlot))
	}
	g.send(turn("sess", "head one"), "sk-b") // another caller: unsettled
	rows := g.rows()
	want := []string{affAssigned, affAssigned, affPinned, affAssigned}
	if len(rows) != len(want) {
		t.Fatalf("rows %d, want %d", len(rows), len(want))
	}
	for i, row := range rows {
		if row.Affinity != want[i] {
			t.Errorf("row %d: affinity %q, want %q", i, row.Affinity, want[i])
		}
	}
	if rows[0].Conversation != rows[2].Conversation || rows[0].Conversation == rows[3].Conversation {
		t.Errorf("conversations %q %q %q: want rows 0 and 2 equal, 3 different", rows[0].Conversation, rows[2].Conversation, rows[3].Conversation)
	}
	for i, b := range g.bodies() {
		if !strings.Contains(b, `"prompt_cache_key":"`) {
			t.Errorf("turn %d: prompt_cache_key did not reach the backend: %.120s", i, b)
		}
	}
}
