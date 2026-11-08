package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// nativeLoopRig serves an Anthropic backend that thinks in a cycle until it
// is hung up on, or for max deltas, then finishes. hungUp says the backend
// saw its request cancelled.
func nativeLoopRig(t *testing.T, cfg *config.LoopDetection, max int) (string, *atomic.Int64, *atomic.Bool) {
	t.Helper()
	return nativeLoopRigIn(t, cfg, max, false)
}

// nativeLoopRigIn is nativeLoopRig; with tool the loop is one fragment said
// again and again in a tool_use input (the exact detector is the one that
// reads tool arguments), after a text block, with a ping every 20 deltas.
func nativeLoopRigIn(t *testing.T, cfg *config.LoopDetection, max int, tool bool) (string, *atomic.Int64, *atomic.Bool) {
	t.Helper()
	treatAsRemote(t)
	sent, hungUp := new(atomic.Int64), new(atomic.Bool)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		ev := func(name, data string) {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
			fl.Flush()
		}
		ev("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[],"usage":{"input_tokens":10,"output_tokens":1}}}`)
		if tool {
			ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
			ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Let me look."}}`)
			ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
			ev("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"write","input":{}}}`)
			ev("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"text\":\""}}`)
		} else {
			ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`)
		}
		for i := range max {
			if r.Context().Err() != nil {
				hungUp.Store(true)
				return
			}
			if tool {
				ev("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":`+inputFragment(loopSentence(0))+`}}`)
				if i%20 == 0 {
					ev("ping", `{"type":"ping"}`)
				}
			} else {
				b, _ := json.Marshal(loopSentence(i))
				ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":`+string(b)+`}}`)
			}
			sent.Store(int64(i + 1))
			time.Sleep(time.Millisecond)
		}
		if tool {
			ev("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"}"}}`)
			ev("content_block_stop", `{"type":"content_block_stop","index":1}`)
		} else {
			ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`)
			ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
		}
		ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":500}}`)
		ev("message_stop", `{"type":"message_stop"}`)
	}))
	t.Cleanup(up.Close)

	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr := proc.NewManager(&config.Config{})
	t.Cleanup(mgr.Shutdown)
	r := chi.NewRouter()
	New(&config.Config{
		Models:         map[string]config.Model{"claude": claudeModel(t, up.URL, "")},
		PriorityGroups: map[string]config.PriorityGroup{"default": {Weight: 1}},
		Keys:           map[string]config.KeyPolicy{"k": {Group: "default"}},
		LoopDetection:  cfg,
	}, mgr, sched.New(), st).Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv.URL, sent, hungUp
}

func streamNativeMessages(t *testing.T, url string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/messages",
		strings.NewReader(`{"model":"claude","max_tokens":1000,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", "k")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	return string(b)
}

// messageDeltas are the message_delta events of a Messages stream.
func messageDeltas(t *testing.T, out string) []map[string]json.RawMessage {
	t.Helper()
	var ds []map[string]json.RawMessage
	for _, ev := range strings.Split(out, "\n\n") {
		if data, ok := strings.CutPrefix(ev, "event: message_delta\ndata: "); ok {
			var d map[string]json.RawMessage
			if err := json.Unmarshal([]byte(data), &d); err != nil {
				t.Fatalf("message_delta: %v: %s", err, data)
			}
			ds = append(ds, d)
		}
	}
	return ds
}

func stopReason(t *testing.T, d map[string]json.RawMessage) string {
	t.Helper()
	var delta struct {
		StopReason string `json:"stop_reason"`
	}
	_ = json.Unmarshal(d["delta"], &delta)
	return delta.StopReason
}

const messageStopEnd = "event: message_stop\ndata: {\"type\":\"message_stop\"}"

// A native /v1/messages stream is watched through the tee: at the cut the
// caller gets a Messages ending corrallm wrote, and the backend is hung up on.
func TestALoopingNativeMessagesStreamIsCut(t *testing.T) {
	url, sent, hungUp := nativeLoopRig(t, &config.LoopDetection{}, 2000)
	out := streamNativeMessages(t, url)
	tail := out[max(0, len(out)-600):]
	if sent.Load() > 500 {
		t.Errorf("backend sent %d deltas before it stopped", sent.Load())
	}
	deadline := time.Now().Add(2 * time.Second)
	for !hungUp.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !hungUp.Load() {
		t.Error("the backend's request was not cancelled")
	}
	if strings.Contains(out, "[DONE]") || strings.Contains(out, "chat.completion.chunk") || strings.Contains(out, "content_filter") {
		t.Errorf("chat events reached a Messages caller: ...%s", tail)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), messageStopEnd) || strings.Count(out, "event: message_stop") != 1 {
		t.Fatalf("stream does not end with one message_stop: ...%s", tail)
	}
	// The open thinking block is closed, signed as corrallm's.
	if !strings.Contains(out, `"signature":"corrallm"`) || strings.Count(out, "event: content_block_stop") != 1 {
		t.Errorf("thinking block not closed: ...%s", tail)
	}
	ds := messageDeltas(t, out)
	if len(ds) != 1 {
		t.Fatalf("message_delta events = %d, want 1: ...%s", len(ds), tail)
	}
	if got := stopReason(t, ds[0]); got != "refusal" {
		t.Errorf("stop_reason = %q, want refusal", got)
	}
	var loop *llm.RepetitionInfo
	if err := json.Unmarshal(ds[0]["loop"], &loop); err != nil || loop == nil || loop.Kind != "cycle" || loop.Where != llm.ChannelReasoning {
		t.Errorf("loop on message_delta = %+v (%v)", loop, err)
	}
}

// Under a negative tolerance the native stream runs to its end, and the
// finding goes onto its message_delta with the backend's stop_reason.
func TestNegativeToleranceOnlyCountsOnNativeMessages(t *testing.T) {
	url, sent, _ := nativeLoopRig(t, &config.LoopDetection{Tolerance: -1}, 300)
	out := streamNativeMessages(t, url)
	if sent.Load() != 300 || strings.Contains(out, "refusal") {
		t.Fatalf("a count-only detector changed the stream: sent %d", sent.Load())
	}
	if !strings.HasSuffix(strings.TrimSpace(out), messageStopEnd) || !strings.Contains(out, `"signature":"sig"`) {
		t.Errorf("the backend's own ending was lost: ...%s", out[max(0, len(out)-400):])
	}
	ds := messageDeltas(t, out)
	if len(ds) != 1 || stopReason(t, ds[0]) != "end_turn" {
		t.Fatalf("message_delta = %v", ds)
	}
	var loop *llm.RepetitionInfo
	if err := json.Unmarshal(ds[0]["loop"], &loop); err != nil || loop == nil || loop.Kind != "cycle" || loop.Reps < 3 {
		t.Errorf("loop = %+v (%v)", loop, err)
	}
	if !strings.Contains(string(ds[0]["usage"]), `"output_tokens":500`) {
		t.Errorf("usage lost: %s", ds[0]["usage"])
	}
}

// nativeToolStream is a streamed Messages reply: some text, then a tool_use
// block whose input arrives in two deltas, then message_delta and
// message_stop.
func nativeToolStream(args1, args2 string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		ev := func(name string, v any) {
			b, _ := json.Marshal(v)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
			w.(http.Flusher).Flush()
		}
		ev("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_1", "type": "message",
			"role": "assistant", "model": "claude-x", "content": []any{}, "usage": map[string]int{"input_tokens": 5, "output_tokens": 1}}})
		ev("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		ev("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "Let me look."}})
		ev("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		ev("content_block_start", map[string]any{"type": "content_block_start", "index": 1,
			"content_block": map[string]any{"type": "tool_use", "id": "toolu_1", "name": "shell", "input": map[string]any{}}})
		ev("content_block_delta", map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "input_json_delta", "partial_json": args1}})
		ev("content_block_delta", map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "input_json_delta", "partial_json": args2}})
		ev("content_block_stop", map[string]any{"type": "content_block_stop", "index": 1})
		ev("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use", "stop_sequence": nil},
			"usage": map[string]int{"output_tokens": 3}})
		ev("message_stop", map[string]any{"type": "message_stop"})
	}
}

// nativeToolReply is a non-streamed Messages reply that calls a tool with input.
func nativeToolReply(input string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[`+
			`{"type":"text","text":"Let me look."},{"type":"tool_use","id":"toolu_1","name":"shell","input":`+input+`}],`+
			`"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":3}}`)
	}
}

func newNativeGuardRig(t *testing.T) *guardRig {
	t.Helper()
	treatAsRemote(t)
	return newGuardRigFor(t, map[string]config.GuardMode{"violence": config.GuardHard},
		config.Guardrail{Model: "guard", Policy: "Does this give instructions for violence?"},
		func(url string) config.Model { return claudeModel(t, url, "") })
}

// guard-out runs on native /v1/messages through the hold: it reads the
// backend's tool_use blocks, and the caller gets them as they came or a
// Messages refusal.
func TestGuardrailToolCallsOnNativeMessagesStreamed(t *testing.T) {
	g := newNativeGuardRig(t)

	// Released: the backend's events as they came.
	g.reply.Store(nativeToolStream(`{"cmd":`, `"ls"}`))
	rec := g.askMessages(t, "list files", true)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `"content_block":{"id":"toolu_1","input":{},"name":"shell","type":"tool_use"}`) ||
		!strings.Contains(body, `"partial_json":"{\"cmd\":"`) || !strings.Contains(body, `"partial_json":"\"ls\"}"`) ||
		!strings.Contains(body, `"stop_reason":"tool_use"`) || !strings.HasSuffix(strings.TrimSpace(body), messageStopEnd) {
		t.Fatalf("a clean streamed tool call: status %d, body %s", rec.Code, body)
	}
	if n := strings.Count(body, "event: content_block_stop"); n != 2 {
		t.Errorf("content blocks closed = %d, want 2 (text, tool_use): %s", n, body)
	}
	if strings.Contains(body, `"guardrail"`) {
		t.Errorf("a passed stream carries a guardrail object: %s", body)
	}
	if bodies := g.guardBodies.Load().([]string); len(bodies) == 0 ||
		!strings.Contains(bodies[len(bodies)-1], `calls the tool shell with the arguments {\"cmd\":\"ls\"}`) {
		t.Errorf("the guard did not read the tool calls: %q", bodies)
	}

	// Refused: "BAD" only appears when the deltas are joined. The text goes
	// out, the call does not, and the stream ends as a Messages refusal.
	g.reply.Store(nativeToolStream(`{"cmd":"BA`, `D"}`))
	rec = g.askMessages(t, "list files", true)
	body = rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "Let me look.") {
		t.Fatalf("refused stream: status %d, body %s", rec.Code, body)
	}
	if strings.Contains(body, "tool_use") || strings.Contains(body, "cmd") {
		t.Errorf("the refused tool call reached the caller: %s", body)
	}
	if strings.Contains(body, "[DONE]") || strings.Contains(body, "content_filter") || !strings.HasSuffix(strings.TrimSpace(body), messageStopEnd) {
		t.Errorf("refused stream did not end as a Messages stream: %s", body)
	}
	ds := messageDeltas(t, body)
	if len(ds) != 1 || stopReason(t, ds[0]) != "refusal" {
		t.Fatalf("message_delta = %v: %s", ds, body)
	}
	var gr struct{ Name, Surface, Mode string }
	if err := json.Unmarshal(ds[0]["guardrail"], &gr); err != nil || gr.Name != "violence" || gr.Surface != "tool_calls" || gr.Mode != "hard" {
		t.Errorf("guardrail on message_delta = %s", ds[0]["guardrail"])
	}
	if !strings.Contains(string(ds[0]["usage"]), `"output_tokens":3`) {
		t.Errorf("the backend's usage was lost: %s", ds[0]["usage"])
	}
	acts, err := g.st.RecentActivity(store.Window{}, 10, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range acts {
		if strings.Contains(a.Error, "on the tool_calls") && strings.Contains(a.Error, "stop_reason refusal") {
			found = true
		}
	}
	if !found {
		t.Errorf("no activity row for the streamed refusal: %+v", acts)
	}
}

func TestGuardrailToolCallsOnNativeMessagesNonStreamed(t *testing.T) {
	g := newNativeGuardRig(t)

	// Refused: the guardrail's status in Anthropic's error shape, with the
	// guardrail object top level.
	g.reply.Store(nativeToolReply(`{"cmd":"BAD"}`))
	rec := g.askMessages(t, "list files", false)
	var e struct {
		Type      string
		Error     struct{ Type, Message string }
		Guardrail struct{ Name, Surface string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || rec.Code != http.StatusUnprocessableEntity ||
		e.Type != "error" || e.Error.Type != "invalid_request_error" || !strings.Contains(e.Error.Message, "tool_calls") ||
		e.Guardrail.Name != "violence" || e.Guardrail.Surface != "tool_calls" {
		t.Fatalf("refused reply: status %d, body %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "BAD") {
		t.Errorf("the refused tool call reached the caller: %s", rec.Body)
	}

	// Released: the backend's reply as it came.
	g.reply.Store(nativeToolReply(`{"cmd":"ls"}`))
	rec = g.askMessages(t, "list files", false)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `{"type":"tool_use","id":"toolu_1","name":"shell","input":{"cmd":"ls"}}`) {
		t.Fatalf("clean reply: status %d, body %s", rec.Code, rec.Body)
	}
	if h := strings.Join(rec.Header().Values(HeaderGuardrail), " | "); !strings.Contains(h, "violence=pass") {
		t.Errorf("guardrail headers = %q", h)
	}
}

// inputFragment is s as a piece of a JSON string inside a tool's input,
// itself a JSON string: escaped twice.
func inputFragment(s string) string {
	b, _ := json.Marshal(s)
	b, _ = json.Marshal(string(b[1 : len(b)-1]))
	return string(b)
}

// A loop inside a tool_use block's input is cut, and the caller gets no tool
// call at all: the text before it, closed, then the refusal.
func TestALoopInANativeToolInputIsCutWithNoToolCall(t *testing.T) {
	url, sent, hungUp := nativeLoopRigIn(t, &config.LoopDetection{}, 2000, true)
	out := streamNativeMessages(t, url)
	tail := out[max(0, len(out)-600):]
	if sent.Load() > 500 {
		t.Errorf("backend sent %d deltas before it stopped", sent.Load())
	}
	deadline := time.Now().Add(2 * time.Second)
	for !hungUp.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !hungUp.Load() {
		t.Error("the backend's request was not cancelled")
	}
	if strings.Contains(out, "tool_use") || strings.Contains(out, "input_json_delta") || strings.Contains(out, "toolu_1") {
		t.Errorf("a cut reply carried a tool call: ...%s", tail)
	}
	if !strings.Contains(out, "Let me look.") || strings.Count(out, "event: content_block_stop") != 1 {
		t.Errorf("the text block before the call was lost or left open: %s", tail)
	}
	if !strings.Contains(out, "event: ping") {
		t.Error("pings were held with the tool_use block")
	}
	if !strings.HasSuffix(strings.TrimSpace(out), messageStopEnd) || strings.Count(out, "event: message_stop") != 1 {
		t.Fatalf("stream does not end with one message_stop: ...%s", tail)
	}
	ds := messageDeltas(t, out)
	if len(ds) != 1 || stopReason(t, ds[0]) != "refusal" {
		t.Fatalf("message_delta = %v", ds)
	}
	var loop *llm.RepetitionInfo
	if err := json.Unmarshal(ds[0]["loop"], &loop); err != nil || loop == nil {
		t.Errorf("loop on message_delta = %s (%v)", ds[0]["loop"], err)
	}
}

// Under the tolerance the held tool_use block is released as it came.
func TestANativeToolCallUnderTheToleranceIsReleased(t *testing.T) {
	url, sent, _ := nativeLoopRigIn(t, &config.LoopDetection{Tolerance: -1}, 50, true)
	out := streamNativeMessages(t, url)
	if sent.Load() != 50 || !strings.Contains(out, `"id":"toolu_1"`) || strings.Count(out, "input_json_delta") != 52 ||
		strings.Count(out, "event: content_block_stop") != 2 || !strings.HasSuffix(strings.TrimSpace(out), messageStopEnd) {
		t.Fatalf("sent %d, out ...%s", sent.Load(), out[max(0, len(out)-400):])
	}
	if ds := messageDeltas(t, out); len(ds) != 1 || stopReason(t, ds[0]) != "end_turn" {
		t.Errorf("message_delta = %v", ds)
	}
}

// The same on converted /v1/messages: a chat stream cut inside a tool call's
// arguments reaches the Messages caller with no tool_use block.
func TestALoopInAConvertedToolCallIsCutWithNoToolCall(t *testing.T) {
	treatAsRemote(t)
	sent := new(atomic.Int64)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		chunk := func(delta string) {
			fmt.Fprintf(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"fw\",\"choices\":[{\"index\":0,\"delta\":%s}]}\n\n", delta)
			fl.Flush()
		}
		chunk(`{"content":"Let me look."}`)
		chunk(`{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"write","arguments":"{\"text\":\""}}]}`)
		for i := range 2000 {
			if r.Context().Err() != nil {
				return
			}
			chunk(`{"tool_calls":[{"index":0,"function":{"arguments":` + inputFragment(loopSentence(0)) + `}}]}`)
			sent.Store(int64(i + 1))
			time.Sleep(time.Millisecond)
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(up.Close)
	url := mountLoopProxy(t, up.URL, &config.LoopDetection{}, nil)
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/messages",
		strings.NewReader(`{"model":"fw","max_tokens":1000,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", "k")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	out := string(b)
	tail := out[max(0, len(out)-600):]
	if sent.Load() > 500 {
		t.Errorf("backend sent %d deltas before it stopped", sent.Load())
	}
	if strings.Contains(out, "tool_use") || strings.Contains(out, "input_json_delta") {
		t.Errorf("a cut reply carried a tool call: ...%s", tail)
	}
	if !strings.Contains(out, "Let me look.") || strings.Count(out, "event: content_block_stop") != 1 ||
		!strings.HasSuffix(strings.TrimSpace(out), messageStopEnd) {
		t.Errorf("not a well-formed Messages ending: ...%s", tail)
	}
	ds := messageDeltas(t, out)
	if len(ds) != 1 || stopReason(t, ds[0]) != "refusal" || ds[0]["loop"] == nil {
		t.Fatalf("message_delta = %v", ds)
	}
}

// The hold lets a ping through, and a refused stream that ended without
// message_stop still ends with one.
func TestMessagesGuardHoldPingAndEndWithoutStop(t *testing.T) {
	rec := httptest.NewRecorder()
	g := &messagesGuardWriter{ResponseWriter: rec, check: func(string) []guardVerdict {
		return []guardVerdict{{Name: "g", Hard: true, Failed: true, FailStatus: 422}}
	}}
	g.Header().Set("Content-Type", "text/event-stream")
	g.WriteHeader(http.StatusOK)
	for _, ev := range []string{
		`{"type":"message_start","message":{"usage":{"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"shell","input":{}}}`,
		`{"type":"ping"}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
	} {
		var e struct{ Type string }
		_ = json.Unmarshal([]byte(ev), &e)
		_, _ = fmt.Fprintf(g, "event: %s\ndata: %s\n\n", e.Type, ev)
	}
	if !strings.Contains(rec.Body.String(), "event: ping") {
		t.Errorf("the ping was held: %s", rec.Body)
	}
	g.finish()
	out := rec.Body.String()
	if strings.Contains(out, "tool_use") || !strings.HasSuffix(strings.TrimSpace(out), messageStopEnd) {
		t.Fatalf("out = %s", out)
	}
	if ds := messageDeltas(t, out); len(ds) != 1 || stopReason(t, ds[0]) != "refusal" || ds[0]["guardrail"] == nil {
		t.Errorf("message_delta = %v", ds)
	}
}

// A converted stream with loop detection on whose backend drops inside a
// tool call (no finish_reason, no [DONE]): messagesWriter's finish ends the
// shim's stream, and the held tool_use block goes out before message_delta
// and message_stop. Nothing held is lost.
func TestAConvertedStreamThatDropsReleasesTheHeldToolCall(t *testing.T) {
	treatAsRemote(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, d := range []string{
			`{"content":"Let me look."}`,
			`{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"shell","arguments":"{\"cmd\":"}}]}`,
			`{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}`,
		} {
			fmt.Fprintf(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"fw\",\"choices\":[{\"index\":0,\"delta\":%s}]}\n\n", d)
		}
		w.(http.Flusher).Flush() // and drop: no finish_reason, no [DONE]
	}))
	t.Cleanup(up.Close)
	url := mountLoopProxy(t, up.URL, &config.LoopDetection{}, nil)
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/messages",
		strings.NewReader(`{"model":"fw","max_tokens":1000,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", "k")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	out := string(b)
	start := strings.Index(out, `"type":"tool_use"`)
	args := strings.Index(out, `"partial_json":"\"ls\"}"`)
	delta := strings.Index(out, "event: message_delta")
	if start < 0 || args < 0 || delta < 0 || !(start < args && args < delta) {
		t.Fatalf("the held tool_use block was lost or out of order: %s", out)
	}
	if strings.Count(out, "event: content_block_stop") != 2 || !strings.HasSuffix(strings.TrimSpace(out), messageStopEnd) {
		t.Errorf("not a closed Messages stream: %s", out)
	}
}
