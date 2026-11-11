package slotcache

import (
	"encoding/json"
	"strings"
	"testing"
)

func chatBody(t testing.TB, tools bool, msgs ...map[string]any) []byte {
	t.Helper()
	req := map[string]any{"model": "swift", "messages": msgs}
	if tools {
		req["tools"] = []map[string]any{{"type": "function", "function": map[string]any{"name": "read", "parameters": map[string]any{}}}}
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func msg(role, content string) map[string]any {
	return map[string]any{"role": role, "content": content}
}

// Inspect is Key and Messages in one decode: it must agree with both, or the
// activity row's conversation and the slot cache's would part ways.
func TestInspectAgreesWithKeyAndMessages(t *testing.T) {
	bodies := [][]byte{
		chatBody(t, false, msg("system", "s"), msg("user", "u1")),
		chatBody(t, true, msg("system", "s"), msg("system", "s2"), msg("user", "u1"), msg("assistant", "a1"), msg("user", "u2")),
		chatBody(t, false, msg("user", "only")),
		chatBody(t, false, msg("system", "only system")),
		[]byte(`{"messages":[]}`),
		[]byte(`not json`),
		[]byte(`{"messages":[{"role":"tool","content":"x","tool_call_id":7}]}`),
	}
	for i, b := range bodies {
		for _, declared := range []string{"", "conv-1"} {
			wantKey, wantOK := Key("caller", declared, b)
			got, ok := Inspect("caller", declared, b)
			if ok != wantOK || got.Key != wantKey {
				t.Errorf("body %d declared %q: Inspect = %q,%v; Key = %q,%v", i, declared, got.Key, ok, wantKey, wantOK)
			}
			if ok && got.Messages != Messages(b) {
				t.Errorf("body %d: messages %d, want %d", i, got.Messages, Messages(b))
			}
		}
	}
}

// A conversation that only appends matches the whole previous turn; one that
// edits a 200 KB system message in the middle matches up to the edit, which a
// per-message hash would have reported as a divergence at character 0.
func TestMatchCharsInsideLongSystemMessage(t *testing.T) {
	sys := strings.Repeat("a", 200_000)
	prev, _ := Inspect("c", "", chatBody(t, true, msg("system", sys), msg("user", "u1")))
	next, _ := Inspect("c", "", chatBody(t, true, msg("system", sys), msg("user", "u1"), msg("assistant", "a1"), msg("user", "u2")))
	// To block resolution: the previous turn's last, partial block is not a
	// block of this one.
	chars, _ := MatchChars(prev.Blocks, prev.Chars, next)
	if prev.Chars-chars >= BlockSize || chars > prev.Chars {
		t.Errorf("append-only: matched %d of %d chars", chars, prev.Chars)
	}

	edited := sys[:100_000] + "B" + sys[100_001:]
	ed, _ := Inspect("c", "", chatBody(t, true, msg("system", edited), msg("user", "u1"), msg("user", "u2")))
	chars, seg := MatchChars(prev.Blocks, prev.Chars, ed)
	if chars < 100_000-2*BlockSize || chars > 100_000+BlockSize {
		t.Errorf("edit at ~100k: matched %d chars", chars)
	}
	if ed.Roles[seg] != "system" {
		t.Errorf("edit in the system message: segment %d is %s", seg, ed.Roles[seg])
	}
	if !ed.Tools || ed.Roles[0] != "tools" {
		t.Errorf("tools segment missing: %+v", ed.Roles)
	}
	if ed.LastUserStart <= ed.Ends[1] {
		t.Errorf("last user start %d, want after the system message ending %d", ed.LastUserStart, ed.Ends[1])
	}
}

// An image part counts as a digest, not as its base64: a 1 MB picture is not a
// million characters of prompt.
func TestInspectMediaPartsAreDigests(t *testing.T) {
	img := "data:image/png;base64," + strings.Repeat("A", 1<<20)
	b := chatBody(t, false, map[string]any{"role": "user", "content": []map[string]any{
		{"type": "text", "text": "what is this"},
		{"type": "image_url", "image_url": map[string]any{"url": img}},
	}})
	turn, ok := Inspect("c", "", b)
	if !ok || turn.Chars > 1000 {
		t.Fatalf("chars = %d, ok = %v", turn.Chars, ok)
	}
	other := chatBody(t, false, map[string]any{"role": "user", "content": []map[string]any{
		{"type": "text", "text": "what is this"},
		{"type": "image_url", "image_url": map[string]any{"url": img + "B"}},
	}})
	t2, _ := Inspect("c", "", other)
	if t2.Blocks[len(t2.Blocks)-1] == turn.Blocks[len(turn.Blocks)-1] {
		t.Error("a different image hashed the same")
	}
}

// bench600KB is an agent turn the size the P75 retro saw at the tail: a 30 KB
// system prompt, tools, and ~140 turns of history.
func bench600KB(b *testing.B) []byte {
	msgs := []map[string]any{msg("system", strings.Repeat("You are a coding agent. ", 1300))}
	for i := 0; len(msgs) < 400; i++ {
		msgs = append(msgs, msg("user", strings.Repeat("read file and report ", 60)))
		msgs = append(msgs, map[string]any{"role": "assistant", "content": "",
			"tool_calls": []map[string]any{{"id": "c", "type": "function", "function": map[string]any{"name": "read", "arguments": `{"path":"x.go"}`}}}})
		msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": "c", "content": strings.Repeat("func x() {}\n", 300)})
	}
	body := chatBody(b, true, msgs...)
	for len(body) < 600_000 {
		msgs = append(msgs, msg("user", strings.Repeat("z", 4000)))
		body = chatBody(b, true, msgs...)
	}
	return body
}

// The gate (P75 N4): one Inspect must cost no more than the decodes it
// replaced. The old request path ran Key+Messages twice (activity row, then
// session affinity).
func BenchmarkInspect600KB(b *testing.B) {
	body := bench600KB(b)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := Inspect("caller", "", body); !ok {
			b.Fatal("no conversation")
		}
	}
}

func BenchmarkConversationOfOld600KB(b *testing.B) {
	body := bench600KB(b)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		for range 2 {
			if _, ok := Key("caller", "", body); !ok {
				b.Fatal("no conversation")
			}
			_ = Messages(body)
		}
	}
}
