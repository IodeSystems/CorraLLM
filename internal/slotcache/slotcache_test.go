package slotcache

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func chat(msgs ...[2]string) []byte {
	type m struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	var out struct {
		Messages []m `json:"messages"`
	}
	for _, p := range msgs {
		out.Messages = append(out.Messages, m{Role: p[0], Content: p[1]})
	}
	b, _ := json.Marshal(out)
	return b
}

// The key has to survive a conversation growing — that is the entire point. Turn
// five must find what turn four left.
func TestKeyIsStableAsTheConversationGrows(t *testing.T) {
	sys := [2]string{"system", "You are a coding agent with tools."}
	first := [2]string{"user", "Refactor the parser in internal/config."}

	turn1, ok := Key("sk-aw4", chat(sys, first))
	if !ok {
		t.Fatal("no key for a normal chat request")
	}
	turn4, ok := Key("sk-aw4", chat(sys, first,
		[2]string{"assistant", "Looking at it."},
		[2]string{"user", "Now do the same for the loader."},
		[2]string{"assistant", "Done."},
		[2]string{"user", "And the tests?"}))
	if !ok {
		t.Fatal("no key for a later turn")
	}
	if turn1 != turn4 {
		t.Errorf("key changed as the conversation grew:\n  turn 1 %s\n  turn 4 %s", turn1, turn4)
	}
}

// Two tasks under one agent share a system prompt and are NOT the same
// conversation. Keying on the system block alone would hand one task's context
// to the other.
func TestKeySeparatesTasksThatShareASystemPrompt(t *testing.T) {
	sys := [2]string{"system", "You are a coding agent with tools."}
	a, _ := Key("sk-aw4", chat(sys, [2]string{"user", "Refactor the parser."}))
	b, _ := Key("sk-aw4", chat(sys, [2]string{"user", "Write release notes."}))
	if a == b {
		t.Error("two different first turns produced one key — one task would restore the other's cache")
	}
}

// THE LEAK CASE, and the reason the caller is in the key at all. A KV cache is
// the conversation's content in another form; restoring one caller's state for
// another hands over what they said.
func TestKeyNeverCrossesCallers(t *testing.T) {
	msgs := chat([2]string{"system", "shared"}, [2]string{"user", "identical opening"})
	a, _ := Key("sk-aw4", msgs)
	b, _ := Key("dun", msgs)
	if a == b {
		t.Error("two callers with identical prompts share a key — that is a content leak, not a cache hit")
	}
}

// Requests this cannot key are refused rather than guessed at: no messages, or a
// system block with nothing after it.
func TestKeyRefusesWhatItCannotIdentify(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"not json", []byte("{oh no")},
		{"no messages", []byte(`{"model":"x"}`)},
		{"system only", chat([2]string{"system", "just a preamble"})},
	} {
		if _, ok := Key("sk-aw4", tc.body); ok {
			t.Errorf("%s: expected no key", tc.name)
		}
	}
}

// Save and restore speak llama.cpp's own shape, and a failure says what the
// backend said — "slot is busy" and "file not found" need different answers.
func TestClientSaveRestoreAndErrors(t *testing.T) {
	var gotPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path+"?"+r.URL.RawQuery)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Query().Get("action") {
		case "save":
			_, _ = fmt.Fprintf(w, `{"id_slot":0,"filename":%q,"n_saved":19639,"n_written":740644420,"timings":{"save_ms":643.2}}`, body["filename"])
		case "restore":
			if body["filename"] == "missing.bin" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"file not found"}}`))
				return
			}
			_, _ = fmt.Fprintf(w, `{"id_slot":0,"n_restored":19639,"n_read":740644420,"timings":{"restore_ms":331.2}}`)
		}
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	tok, written, err := c.Save(context.Background(), 0, "abc")
	if err != nil || tok != 19639 || written != 740644420 {
		t.Fatalf("save = %d tokens, %d bytes, err %v", tok, written, err)
	}
	if tok, read, err := c.Restore(context.Background(), 0, "abc"); err != nil || tok != 19639 || read != 740644420 {
		t.Fatalf("restore = %d tokens, %d bytes, err %v", tok, read, err)
	}
	if _, _, err := c.Restore(context.Background(), 0, "missing"); err == nil {
		t.Error("a missing state must be an error the caller can see")
	} else if want := "file not found"; !contains(err.Error(), want) {
		t.Errorf("the backend's reason was lost: %v", err)
	}
	if len(gotPaths) != 3 || gotPaths[0] != "/slots/0?action=save" {
		t.Errorf("unexpected calls: %v", gotPaths)
	}
}

// A state belongs to one backend. A re-quantised model or an edited cmd must
// find NO files rather than someone else's.
func TestNamespaceChangesWithTheBackend(t *testing.T) {
	base := Namespace("local-Qwen3.8-27B", "llama-server -c 188000", 188000)
	if base == Namespace("local-Qwen3.8-27B", "llama-server -c 188000 --cache-reuse 256", 188000) {
		t.Error("a changed cmd kept the same namespace")
	}
	if base == Namespace("local-Qwen3.8-27B", "llama-server -c 188000", 65536) {
		t.Error("a changed context size kept the same namespace")
	}
	if base != Namespace("local-Qwen3.8-27B", "llama-server -c 188000", 188000) {
		t.Error("the same backend produced two namespaces")
	}
}

// Eviction is least-recently-used and stops at the cap, because at 36.8 KB per
// token an unbounded store is a full disk within a day.
func TestEvictDropsTheOldestUntilUnderCap(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Dir: dir, MaxBytes: 300}
	ns := "ns"
	if err := s.Prepare(ns); err != nil {
		t.Fatal(err)
	}
	// Three 200-byte states, written oldest first.
	for i, name := range []string{"old", "middle", "new"} {
		p := s.Path(ns, name)
		if err := os.WriteFile(p, make([]byte, 200), 0o600); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(time.Duration(i-3) * time.Hour)
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
	}
	removed, freed, err := s.Evict()
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 || freed != 400 {
		t.Fatalf("evicted %d files freeing %d bytes; want 2 and 400", removed, freed)
	}
	if _, err := os.Stat(s.Path(ns, "new")); err != nil {
		t.Error("the newest state was evicted")
	}
	if _, err := os.Stat(s.Path(ns, "old")); !os.IsNotExist(err) {
		t.Error("the oldest state survived")
	}
}

// Has() touches the file, so asking for a state protects it from the next
// eviction. Without that, the store evicts exactly what is about to be used.
func TestHasMarksAStateRecentlyUsed(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Dir: dir}
	ns := "ns"
	_ = s.Prepare(ns)
	p := s.Path(ns, "k")
	_ = os.WriteFile(p, []byte("x"), 0o600)
	old := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(p, old, old)

	if !s.Has(ns, "k") {
		t.Fatal("Has said no to a file that exists")
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.ModTime().Before(time.Now().Add(-time.Minute)) {
		t.Error("Has did not refresh the file's recency")
	}
	if s.Has(ns, "nope") {
		t.Error("Has said yes to a file that does not exist")
	}
}

func TestDropNamespaceRemovesEverythingForOneBackend(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Dir: dir}
	_ = s.Prepare("gone")
	_ = s.Prepare("stays")
	_ = os.WriteFile(s.Path("gone", "a"), []byte("x"), 0o600)
	_ = os.WriteFile(s.Path("stays", "b"), []byte("x"), 0o600)

	if err := s.DropNamespace("gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "gone")); !os.IsNotExist(err) {
		t.Error("the namespace survived")
	}
	if _, err := os.Stat(s.Path("stays", "b")); err != nil {
		t.Error("dropping one backend removed another's states")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
