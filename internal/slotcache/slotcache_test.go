package slotcache

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

	turn1, ok := Key("sk-aw4", "", chat(sys, first))
	if !ok {
		t.Fatal("no key for a normal chat request")
	}
	turn4, ok := Key("sk-aw4", "", chat(sys, first,
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
	a, _ := Key("sk-aw4", "", chat(sys, [2]string{"user", "Refactor the parser."}))
	b, _ := Key("sk-aw4", "", chat(sys, [2]string{"user", "Write release notes."}))
	if a == b {
		t.Error("two different first turns produced one key — one task would restore the other's cache")
	}
}

// THE LEAK CASE, and the reason the caller is in the key at all. A KV cache is
// the conversation's content in another form; restoring one caller's state for
// another hands over what they said.
func TestKeyNeverCrossesCallers(t *testing.T) {
	msgs := chat([2]string{"system", "shared"}, [2]string{"user", "identical opening"})
	a, _ := Key("sk-aw4", "", msgs)
	b, _ := Key("dun", "", msgs)
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
		if _, ok := Key("sk-aw4", "", tc.body); ok {
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
		p := s.Path("m", ns, name)
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
	if _, err := os.Stat(s.Path("m", ns, "new")); err != nil {
		t.Error("the newest state was evicted")
	}
	if _, err := os.Stat(s.Path("m", ns, "old")); !os.IsNotExist(err) {
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
	p := s.Path("m", ns, "k")
	_ = os.WriteFile(p, []byte("x"), 0o600)
	old := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(p, old, old)

	if !s.Has("m", ns, "k") {
		t.Fatal("Has said no to a file that exists")
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.ModTime().Before(time.Now().Add(-time.Minute)) {
		t.Error("Has did not refresh the file's recency")
	}
	if s.Has("m", ns, "nope") {
		t.Error("Has said yes to a file that does not exist")
	}
}

func TestDropNamespaceRemovesEverythingForOneBackend(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Dir: dir}
	_ = s.Prepare("gone")
	_ = s.Prepare("stays")
	_ = os.WriteFile(s.Path("m", "gone", "a"), []byte("x"), 0o600)
	_ = os.WriteFile(s.Path("m", "stays", "b"), []byte("x"), 0o600)

	if err := s.DropNamespace("gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "gone")); !os.IsNotExist(err) {
		t.Error("the namespace survived")
	}
	if _, err := os.Stat(s.Path("m", "stays", "b")); err != nil {
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

// fakeBackend records the save/restore calls a Manager makes, in order, so the
// ORDER can be asserted — saving after restoring would write the state just
// loaded under the previous conversation's name, which corrupts rather than
// merely slows.
type fakeBackend struct {
	calls  []string
	status int
	body   string
}

func (f *fakeBackend) server(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]string
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.calls = append(f.calls, r.URL.Query().Get("action")+":"+b["filename"])
		if f.status != 0 {
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(f.body))
			return
		}
		_, _ = w.Write([]byte(`{"n_saved":10,"n_restored":10,"n_written":100,"n_read":100}`))
	}))
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL}
}

func TestSwapSavesTheOutgoingBeforeRestoringTheIncoming(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{Store: &Store{Dir: dir}}
	f := &fakeBackend{}
	c := f.server(t)
	ns := "nsA"

	// First conversation: nothing resident, nothing saved for it.
	if what, restored := m.Swap(context.Background(), c, "b1", ns, "conv-a"); restored {
		t.Errorf("nothing was saved yet, so nothing can be restored (%q)", what)
	}
	if len(f.calls) != 0 {
		t.Fatalf("a first conversation should touch the backend not at all, got %v", f.calls)
	}

	// Pretend conv-b has a state on disk from earlier.
	_ = m.Store.Prepare(ns)
	_ = os.WriteFile(m.Store.Path("b1", ns, "conv-b"), []byte("kv"), 0o600)

	if what, restored := m.Swap(context.Background(), c, "b1", ns, "conv-b"); !restored {
		t.Fatalf("conv-b had a state and was not restored: %q", what)
	}
	if len(f.calls) != 2 {
		t.Fatalf("want a save then a restore, got %v", f.calls)
	}
	if f.calls[0] != "save:"+filePrefix("b1")+ns+"-conv-a.bin" {
		t.Errorf("the outgoing conversation was not saved first: %v", f.calls)
	}
	if f.calls[1] != "restore:"+filePrefix("b1")+ns+"-conv-b.bin" {
		t.Errorf("the incoming conversation was not restored second: %v", f.calls)
	}
}

func TestSwapDoesNothingWhenTheConversationIsAlreadyResident(t *testing.T) {
	m := &Manager{Store: &Store{Dir: t.TempDir()}}
	f := &fakeBackend{}
	c := f.server(t)
	_, _ = m.Swap(context.Background(), c, "b1", "ns", "same")
	before := len(f.calls)
	if what, _ := m.Swap(context.Background(), c, "b1", "ns", "same"); what != "" {
		t.Errorf("a repeat of the resident conversation did something: %q", what)
	}
	if len(f.calls) != before {
		t.Errorf("a repeat touched the backend: %v", f.calls)
	}
}

// A backend started without --slot-save-path answers 501. Asking it again on
// every request would put a failure in the log for something that can never work.
func TestSwapStopsAskingABackendThatCannot(t *testing.T) {
	m := &Manager{Store: &Store{Dir: t.TempDir()}}
	f := &fakeBackend{status: 501, body: `{"error":{"message":"slot save is not supported"}}`}
	c := f.server(t)

	_, _ = m.Swap(context.Background(), c, "b1", "ns", "one") // becomes resident, no call
	_, _ = m.Swap(context.Background(), c, "b1", "ns", "two") // tries to save -> 501
	calls := len(f.calls)
	_, _ = m.Swap(context.Background(), c, "b1", "ns", "three") // must not ask again
	if len(f.calls) != calls {
		t.Errorf("kept asking a backend that said no: %v", f.calls)
	}
}

// A failed save must not stop the request, and must not leave the manager
// believing the old conversation is still resident — it is not, the incoming one
// is about to overwrite it.
func TestSwapSurvivesAFailedSave(t *testing.T) {
	m := &Manager{Store: &Store{Dir: t.TempDir()}}
	f := &fakeBackend{status: 500, body: `{"error":{"message":"slot is busy"}}`}
	c := f.server(t)

	_, _ = m.Swap(context.Background(), c, "b1", "ns", "first")
	what, restored := m.Swap(context.Background(), c, "b1", "ns", "second")
	if restored {
		t.Error("nothing was on disk to restore")
	}
	if !contains(what, "save failed") {
		t.Errorf("the failure was not reported: %q", what)
	}
	// Third swap must save "second" — not "first" — or the state would be filed
	// under a conversation that has not been in the slot for two requests.
	f.status = 0
	_, _ = m.Swap(context.Background(), c, "b1", "ns", "third")
	last := f.calls[len(f.calls)-1]
	if !contains(last, "second") {
		t.Errorf("saved the wrong conversation after a failure: %v", f.calls)
	}
}

// THE CASE THE GUESS CANNOT HANDLE, and the reason a caller should be able to
// say what conversation this is. A client that prunes the head of a long
// conversation — the very behaviour that produced 2026-09-05's four-hour
// slowdown — rewrites the messages the inferred key is built from. The state it
// saved one turn ago becomes unreachable exactly when it is worth the most.
func TestDeclaredIdSurvivesTheHeadBeingPruned(t *testing.T) {
	sys := [2]string{"system", "You are a coding agent with tools."}
	first := [2]string{"user", "Refactor the parser."}
	full := chat(sys, first, [2]string{"assistant", "Done."}, [2]string{"user", "Now the loader."})
	// The client drops the opening turns to stay inside the window.
	pruned := chat([2]string{"system", "You are a coding agent with tools."},
		[2]string{"user", "Now the loader."})

	guessBefore, _ := Key("sk-aw4", "", full)
	guessAfter, _ := Key("sk-aw4", "", pruned)
	if guessBefore == guessAfter {
		t.Fatal("this test is pointless if pruning does not change the guess")
	}

	saidBefore, ok1 := Key("sk-aw4", "task-4711", full)
	saidAfter, ok2 := Key("sk-aw4", "task-4711", pruned)
	if !ok1 || !ok2 {
		t.Fatal("a declared conversation must always produce a key")
	}
	if saidBefore != saidAfter {
		t.Error("a declared id did not survive the head being pruned")
	}
}

// A declared id is still the caller's own namespace: naming somebody else's
// conversation gets you yours, not theirs.
func TestDeclaredIdIsStillPerCaller(t *testing.T) {
	a, _ := Key("sk-aw4", "task-4711", nil)
	b, _ := Key("dun", "task-4711", nil)
	if a == b {
		t.Error("two callers naming the same conversation shared a key")
	}
}

// A declared id needs no body at all — an embeddings or completions request can
// say what it belongs to just as well as a chat one.
func TestDeclaredIdNeedsNoParseableBody(t *testing.T) {
	if _, ok := Key("sk-aw4", "task-4711", []byte("{not json")); !ok {
		t.Error("a declared id should not depend on the body parsing")
	}
}

// THE BUG THIS PACKAGE SHIPPED WITH FOR ONE COMMIT. llama.cpp validates the
// filename it is given and rejects anything with a path separator in it:
//
//	{"error":{"code":400,"message":"Invalid filename"}}
//
// A directory per backend therefore failed EVERY save and restore, and because
// this package is built never to fail a request, it would have failed silently —
// a feature that logs "save failed" forever and helps nobody. The namespace is a
// filename prefix, and nothing may put a separator back.
func TestNothingHandsASeparatorToTheBackend(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{Store: &Store{Dir: dir}}
	f := &fakeBackend{}
	c := f.server(t)
	ns := Namespace("local-Qwen3.8-27B", "llama-server -c 188000", 188000)

	_ = m.Store.Prepare(ns)
	_ = os.WriteFile(m.Store.Path("b1", ns, "conv-b"), []byte("kv"), 0o600)
	_, _ = m.Swap(context.Background(), c, "b1", ns, "conv-a")
	_, _ = m.Swap(context.Background(), c, "b1", ns, "conv-b")

	if len(f.calls) == 0 {
		t.Fatal("expected the backend to be asked something")
	}
	for _, call := range f.calls {
		filename := call[strings.Index(call, ":")+1:]
		if strings.ContainsAny(filename, "/\\") {
			t.Errorf("handed the backend a filename it will reject: %q", filename)
		}
	}
	// And the file the store writes has to be the one the backend was told about,
	// or a save lands somewhere Has() never looks.
	if got := filepath.Base(m.Store.Path("b1", ns, "conv-b")); got != filePrefix("b1")+ns+"-conv-b.bin" {
		t.Errorf("store path and backend filename disagree: %q", got)
	}
}

// An edited cmd changes the namespace, so every state already on disk becomes
// unreachable. Removing one flag from box1's model orphaned 16 files and 10 GB,
// which then waited for eviction to happen upon them. The first swap after such
// a change clears them on purpose.
func TestSweepDropsStatesFromAnOlderShapeOfTheBackend(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Dir: dir}
	_ = s.Prepare("")
	old, live := "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb"
	for _, f := range []string{s.Path("m1", old, "c1"), s.Path("m1", old, "c2"), s.Path("m1", live, "c3")} {
		if err := os.WriteFile(f, make([]byte, 1<<20), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Another model's states must survive: this sweeps one backend, not the store.
	other := s.Path("m2", old, "c4")
	_ = os.WriteFile(other, make([]byte, 1<<20), 0o600)

	removed, freed := s.SweepOthers("m1", live)
	if removed != 2 || freed != 2<<20 {
		t.Fatalf("removed %d freeing %d; want 2 and %d", removed, freed, 2<<20)
	}
	if _, err := os.Stat(s.Path("m1", live, "c3")); err != nil {
		t.Error("the live namespace was swept")
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("another model's states were swept")
	}
}
