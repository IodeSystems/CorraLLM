package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/store"
)

// toolReply is a non-streamed chat reply that calls a tool with args.
func toolReply(args string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		b, _ := json.Marshal(map[string]any{"id": "c1", "choices": []any{map[string]any{
			"index": 0, "finish_reason": "tool_calls",
			"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
				"id": "t1", "type": "function",
				"function": map[string]any{"name": "shell", "arguments": args},
			}}},
		}}, "usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 3}})
		// The backend's length; a replaced reply must not carry it.
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		_, _ = w.Write(b)
	}
}

// toolStream is a streamed chat reply: some text, then a tool call whose
// arguments arrive in two deltas, then finish, usage and [DONE].
func toolStream(args1, args2 string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		ev := func(v any) {
			b, _ := json.Marshal(v)
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
			w.(http.Flusher).Flush()
		}
		chunk := func(delta any, finish any) map[string]any {
			return map[string]any{"id": "s1", "object": "chat.completion.chunk", "model": "chat",
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		}
		ev(chunk(map[string]any{"content": "Let me look."}, nil))
		ev(chunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "t1", "type": "function",
			"function": map[string]any{"name": "shell", "arguments": args1}}}}, nil))
		ev(chunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0,
			"function": map[string]any{"arguments": args2}}}}, nil))
		ev(chunk(map[string]any{}, "tool_calls"))
		ev(map[string]any{"id": "s1", "object": "chat.completion.chunk", "choices": []any{},
			"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 3}})
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}
}

func (g *guardRig) askStream(t *testing.T, text string) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"model": "chat", "stream": true,
		"messages": []any{map[string]string{"role": "user", "content": text}}})
	return g.do(t, b, "")
}

func TestGuardrailToolCallsNonStreamed(t *testing.T) {
	g := newGuardRig(t, map[string]config.GuardMode{"violence": config.GuardHard})

	g.reply.Store(toolReply(`{"cmd":"ls"}`))
	rec := g.ask(t, "list files", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"tool_calls"`) {
		t.Fatalf("a clean tool call: status %d, body %s", rec.Code, rec.Body)
	}
	if n := g.guardCalls.Load(); n != 2 {
		t.Errorf("guard calls = %d, want 2 (input, tool calls)", n)
	}
	if h := rec.Header().Values(HeaderGuardrail); len(h) != 2 {
		t.Errorf("guardrail headers = %q, want the input's and the tool calls'", h)
	}

	g.reply.Store(toolReply(`{"cmd":"BAD"}`))
	rec = g.ask(t, "list files", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a failing tool call: status %d, want 422; body %s", rec.Code, rec.Body)
	}
	var out struct {
		Error struct {
			Code      string
			Guardrail struct{ Name, Surface string }
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body %q: %v", rec.Body, err)
	}
	if out.Error.Code != "content_filter" || out.Error.Guardrail.Surface != "tool_calls" {
		t.Errorf("error body = %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), "BAD") {
		t.Errorf("the refused tool call reached the caller: %s", rec.Body)
	}
	acts, err := g.st.RecentActivity(store.Window{}, 10, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range acts {
		if a.Status == http.StatusUnprocessableEntity && strings.Contains(a.Error, "on the tool_calls") {
			found = true
		}
	}
	if !found {
		t.Errorf("no activity row for the tool-call refusal: %+v", acts)
	}
}

func TestGuardrailToolCallsSoftPassesWithFlag(t *testing.T) {
	g := newGuardRig(t, map[string]config.GuardMode{"violence": config.GuardSoft})
	g.reply.Store(toolReply(`{"cmd":"BAD"}`))
	rec := g.ask(t, "list files", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "BAD") {
		t.Fatalf("soft: status %d, body %s", rec.Code, rec.Body)
	}
	if h := strings.Join(rec.Header().Values(HeaderGuardrail), " | "); !strings.Contains(h, "violence=flag") {
		t.Errorf("guardrail headers = %q", h)
	}
}

func TestGuardrailToolCallsStreamed(t *testing.T) {
	g := newGuardRig(t, map[string]config.GuardMode{"violence": config.GuardHard})

	g.reply.Store(toolStream(`{"cmd":`, `"ls"}`))
	rec := g.askStream(t, "list files")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `\"ls\"`) || !strings.Contains(body, "[DONE]") {
		t.Fatalf("a clean streamed tool call: status %d, body %s", rec.Code, body)
	}

	// "BAD" only appears when the deltas are joined: the check reads the call whole.
	g.reply.Store(toolStream(`{"cmd":"BA`, `D"}`))
	rec = g.askStream(t, "list files")
	body = rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: a stream's status went out before the tool call", rec.Code)
	}
	if !strings.Contains(body, "Let me look.") {
		t.Errorf("the text before the tool call was dropped: %s", body)
	}
	if strings.Contains(body, `\"cmd\"`) || strings.Contains(body, `"tool_calls":[`) {
		t.Errorf("the refused tool call reached the caller: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"content_filter"`) || !strings.Contains(body, `"surface":"tool_calls"`) {
		t.Errorf("no content_filter chunk: %s", body)
	}
	if !strings.Contains(body, `"usage"`) || !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Errorf("usage or [DONE] lost: %s", body)
	}
}

func TestGuardrailToolCallsUnavailableFailsClosed(t *testing.T) {
	g := newGuardRig(t, map[string]config.GuardMode{"violence": config.GuardHard})
	// Pass the input check, then take the guard down for the reply's check.
	g.reply.Store(func(w http.ResponseWriter) {
		g.guardDown.Store(true)
		toolReply(`{"cmd":"ls"}`)(w)
	})
	rec := g.ask(t, "list files", "")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "guardrail_unavailable") {
		t.Errorf("status %d, body %s", rec.Code, rec.Body)
	}
}

func TestGuardrailNoToolCallsPassesUntouched(t *testing.T) {
	g := newGuardRig(t, map[string]config.GuardMode{"violence": config.GuardHard})
	rec := g.ask(t, "hello", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	if n := g.guardCalls.Load(); n != 1 {
		t.Errorf("guard calls = %d, want 1 (the input only)", n)
	}
}

func (g *guardRig) askMessages(t *testing.T, text string, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"model": "chat", "max_tokens": 100, "stream": stream,
		"messages": []any{map[string]string{"role": "user", "content": text}}})
	return postMessages(g.router, string(b), map[string]string{"x-api-key": "sk-t"})
}

// guard-out runs on converted /v1/messages, inside messagesWriter: it reads
// the backend's chat reply, and the caller gets Messages.
func TestGuardrailToolCallsOnConvertedMessages(t *testing.T) {
	g := newGuardRig(t, map[string]config.GuardMode{"violence": config.GuardHard})

	// Streamed, released: one well-formed tool_use block.
	g.reply.Store(toolStream(`{"cmd":`, `"ls"}`))
	rec := g.askMessages(t, "list files", true)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `"content_block":{"id":"t1","input":{},"name":"shell","type":"tool_use"}`) ||
		!strings.Contains(body, `"partial_json":"{\"cmd\":"`) || !strings.Contains(body, `"partial_json":"\"ls\"}"`) ||
		!strings.Contains(body, `"stop_reason":"tool_use"`) || !strings.HasSuffix(strings.TrimSpace(body), `{"type":"message_stop"}`) {
		t.Fatalf("a clean streamed tool call: status %d, body %s", rec.Code, body)
	}
	if n := strings.Count(body, "event: content_block_stop"); n != 2 {
		t.Errorf("content blocks closed = %d, want 2 (text, tool_use): %s", n, body)
	}
	if bodies := g.guardBodies.Load().([]string); len(bodies) == 0 || !strings.Contains(bodies[len(bodies)-1], "calls the tool shell") {
		t.Errorf("the guard did not read the tool calls: %q", bodies)
	}

	// Streamed, refused: the text goes out, the call does not, and the stream
	// ends with stop_reason refusal and the guardrail object.
	g.reply.Store(toolStream(`{"cmd":"BA`, `D"}`))
	rec = g.askMessages(t, "list files", true)
	body = rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "Let me look.") {
		t.Fatalf("refused stream: status %d, body %s", rec.Code, body)
	}
	if strings.Contains(body, "tool_use") || strings.Contains(body, "cmd") {
		t.Errorf("the refused tool call reached the caller: %s", body)
	}
	if !strings.Contains(body, `"stop_reason":"refusal"`) || !strings.Contains(body, `"surface":"tool_calls"`) ||
		!strings.HasSuffix(strings.TrimSpace(body), `{"type":"message_stop"}`) || strings.Contains(body, "[DONE]") {
		t.Errorf("refused stream did not end as a Messages refusal: %s", body)
	}

	// Not streamed, refused: an Anthropic error with the guardrail's status.
	g.reply.Store(toolReply(`{"cmd":"BAD"}`))
	rec = g.askMessages(t, "list files", false)
	var e struct {
		Type  string
		Error struct{ Type, Message string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || rec.Code != http.StatusUnprocessableEntity ||
		e.Type != "error" || e.Error.Type != "invalid_request_error" || !strings.Contains(e.Error.Message, "tool_calls") {
		t.Fatalf("refused reply: status %d, body %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "BAD") {
		t.Errorf("the refused tool call reached the caller: %s", rec.Body)
	}

	// Not streamed, released: a Messages reply with the tool_use block.
	g.reply.Store(toolReply(`{"cmd":"ls"}`))
	rec = g.askMessages(t, "list files", false)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `{"id":"t1","input":{"cmd":"ls"},"name":"shell","type":"tool_use"}`) {
		t.Fatalf("clean reply: status %d, body %s", rec.Code, rec.Body)
	}
}
