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

// Off: the backend gets the client's bytes. On (--parallel 2): the same bytes
// with "id_slot":N spliced first, and activity_cache says what was done.
func TestSlotAffinityEndToEnd(t *testing.T) {
	bodies := func(on bool) ([]string, []store.CacheRow, []string) {
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
		m.SlotAffinity = on
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
		msgs := `{"role":"system","content":"` + strings.Repeat("sys ", 300) + `"},{"role":"user","content":"task"}`
		var sent []string
		for i := range 3 {
			body := `{"model":"mock","messages":[` + msgs + `]}`
			sent = append(sent, body)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("turn %d: %d %s", i, rec.Code, rec.Body.String())
			}
			msgs += `,{"role":"assistant","content":"ok"},{"role":"user","content":"more"}`
		}
		var got []string
		for i := range 3 {
			b, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("%03d.json", i)))
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, string(b))
		}
		rows, err := st.CacheRows("mock", 0, time.Now().Add(time.Minute).UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
		return got, rows, sent
	}

	got, rows, sent := bodies(false)
	for i := range got {
		if got[i] != sent[i] {
			t.Errorf("off, turn %d: the backend got a different body:\n got %.120s\nsent %.120s", i, got[i], sent[i])
		}
	}
	for _, row := range rows {
		if row.Affinity != "" {
			t.Errorf("off: affinity %q recorded", row.Affinity)
		}
	}

	got, rows, sent = bodies(true)
	wantOut := []string{affAssigned, affPinned, affPinned}
	for i := range got {
		want := `{"id_slot":0,` + sent[i][1:]
		if got[i] != want {
			t.Errorf("on, turn %d:\n got %.120s\nwant %.120s", i, got[i], want)
		}
	}
	if len(rows) != 3 {
		t.Fatalf("rows %d", len(rows))
	}
	for i, row := range rows {
		if row.Affinity != wantOut[i] || row.Slot != 0 {
			t.Errorf("on, row %d: affinity %q slot %d, want %q 0", i, row.Affinity, row.Slot, wantOut[i])
		}
	}
}
