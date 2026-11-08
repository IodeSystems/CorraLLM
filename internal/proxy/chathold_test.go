package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/corrallm/internal/config"
)

// chatChunk is one data line of a chat stream with delta on choice 0.
func chatChunk(delta string) string {
	return "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"fw\",\"choices\":[{\"index\":0,\"delta\":" + delta + "}]}\n\n"
}

const (
	chatToolStart = `{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"write","arguments":"{\"text\":\""}}]}`
	chatFinish    = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"fw\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n"
)

// chatArgsLoop is a chat reply that writes text, starts a tool call and then
// repeats one fragment of its arguments, max times or until it is hung up on.
func chatArgsLoop(max int, sent *atomic.Int64, hungUp *atomic.Bool) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, chatChunk(`{"content":"Let me look."}`)+chatChunk(chatToolStart))
		fl.Flush()
		for i := range max {
			if r != nil && r.Context().Err() != nil {
				hungUp.Store(true)
				return
			}
			fmt.Fprint(w, chatChunk(`{"tool_calls":[{"index":0,"function":{"arguments":`+inputFragment(loopSentence(0))+`}}]}`))
			fl.Flush()
			sent.Store(int64(i + 1))
			time.Sleep(time.Millisecond)
		}
		fmt.Fprint(w, chatFinish+"data: [DONE]\n\n")
	}
}

// A cut inside a tool call's arguments drops the held call: the caller gets
// the text before it, then the content_filter chunk with the loop object, and
// no tool_calls at all.
func TestAChatCutInsideToolArgumentsSendsNoToolCalls(t *testing.T) {
	treatAsRemote(t)
	sent, hungUp := new(atomic.Int64), new(atomic.Bool)
	up := httptest.NewServer(http.HandlerFunc(chatArgsLoop(2000, sent, hungUp)))
	t.Cleanup(up.Close)
	out := streamChat(t, mountLoopProxy(t, up.URL, &config.LoopDetection{}, nil))
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
	if strings.Contains(out, "tool_calls") {
		t.Errorf("a cut stream carried a tool call: %s", out)
	}
	want := chatChunk(`{"content":"Let me look."}`)
	if !strings.HasPrefix(out, want) || !strings.Contains(out, `"finish_reason":"content_filter"`) ||
		!strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("not the text, then the cut: %s", out)
	}
	var last struct {
		Loop *llm.RepetitionInfo `json:"loop"`
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "content_filter") {
			_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &last)
		}
	}
	if last.Loop == nil || last.Loop.Kind != "exact" {
		t.Errorf("loop = %+v", last.Loop)
	}
}

// Under the tolerance the held lines go out exactly as the backend sent them,
// content after the first tool-call delta included, in order, and SSE
// comments pass while the call is held.
func TestAChatToolCallUnderTheToleranceIsReleasedByteForByte(t *testing.T) {
	treatAsRemote(t)
	body := chatChunk(`{"content":"Let me look."}`) +
		chatChunk(chatToolStart) +
		chatChunk(`{"content":"and some text in between"}`) +
		chatChunk(`{"tool_calls":[{"index":0,"function":{"arguments":"hi\"}"}}]}`) +
		chatFinish + "data: [DONE]\n\n"
	if out := streamChat(t, fixedBackend(t, "text/event-stream", body)); out != body {
		t.Fatalf("release differs from the backend's stream:\ngot  %q\nwant %q", out, body)
	}
	withComment := chatChunk(`{"content":"Let me look."}`) + chatChunk(chatToolStart) + ": keep-alive\n\n" +
		chatChunk(`{"tool_calls":[{"index":0,"function":{"arguments":"hi\"}"}}]}`) + chatFinish + "data: [DONE]\n\n"
	out := streamChat(t, fixedBackend(t, "text/event-stream", withComment))
	if i, j := strings.Index(out, ": keep-alive"), strings.Index(out, "tool_calls"); i < 0 || j < 0 || i > j {
		t.Errorf("the comment did not pass ahead of the held call: %q", out)
	}
}

// A stream that ends without a finish_reason, with or without [DONE], drops
// the held lines: no tool call with its arguments cut short.
func TestAChatStreamThatEndsUnfinishedSendsNoToolCalls(t *testing.T) {
	treatAsRemote(t)
	head := chatChunk(`{"content":"Let me look."}`) + chatChunk(chatToolStart) +
		chatChunk(`{"content":"held text"}`)
	for name, tail := range map[string]string{"[DONE]": "data: [DONE]\n\n", "drop": "", "drop mid-line": "data: {\"choi"} {
		out := streamChat(t, fixedBackend(t, "text/event-stream", head+tail))
		if strings.Contains(out, "tool_calls") || strings.Contains(out, "held text") {
			t.Errorf("%s: held lines reached the caller: %q", name, out)
		}
		if want := chatChunk(`{"content":"Let me look."}`) + tail; name != "drop mid-line" && out != want {
			t.Errorf("%s: out %q, want %q", name, out, want)
		}
	}
}

// guardOutWriter sits outside loopWriter on the chat path: a released call
// reaches it as a normal stream and is checked; a cut reaches it with no
// tool call, so it has nothing to check.
func TestAChatToolCallHoldWithGuardOut(t *testing.T) {
	g := newGuardRigFor(t, map[string]config.GuardMode{"violence": config.GuardHard},
		config.Guardrail{Model: "guard", Policy: "Does this give instructions for violence?"},
		func(url string) config.Model {
			m := modelTo(t, url, "local")
			m.LoopDetection = &config.LoopDetection{}
			return m
		})

	g.reply.Store(toolStream(`{"cmd":`, `"ls"}`))
	before := g.guardCalls.Load()
	rec := g.askStream(t, "list files")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `\"ls\"`) || !strings.Contains(body, `"finish_reason":"tool_calls"`) ||
		!strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Fatalf("released call: status %d, body %s", rec.Code, body)
	}
	if n := g.guardCalls.Load() - before; n != 2 {
		t.Errorf("guard calls = %d, want 2 (input, tool calls)", n)
	}

	sent := new(atomic.Int64)
	loop := chatArgsLoop(300, sent, nil)
	g.reply.Store(func(w http.ResponseWriter) { loop(w, nil) })
	before = g.guardCalls.Load()
	rec = g.askStream(t, "list files")
	body = rec.Body.String()
	if strings.Contains(body, "tool_calls") || !strings.Contains(body, "Let me look.") ||
		!strings.Contains(body, `"finish_reason":"content_filter"`) || !strings.Contains(body, `"loop":`) ||
		!strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Fatalf("cut: status %d, body %s", rec.Code, body)
	}
	if n := g.guardCalls.Load() - before; n != 1 {
		t.Errorf("guard calls = %d, want 1 (the input only: the cut left no tool call)", n)
	}
}

// On converted /v1/messages both holds run, loopWriter's inside
// messagesWriter's: a released call reaches the caller once, in order.
func TestAConvertedToolCallPassesBothHoldsOnce(t *testing.T) {
	treatAsRemote(t)
	body := chatChunk(`{"content":"Let me look."}`) + chatChunk(chatToolStart) +
		chatChunk(`{"tool_calls":[{"index":0,"function":{"arguments":"hi\"}"}}]}`) + chatFinish + "data: [DONE]\n\n"
	url := fixedBackend(t, "text/event-stream", body)
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
	if strings.Count(out, `"type":"tool_use"`) != 1 || strings.Count(out, "input_json_delta") != 2 {
		t.Fatalf("tool_use released %d times: %s", strings.Count(out, `"type":"tool_use"`), out)
	}
	text, start := strings.Index(out, "Let me look."), strings.Index(out, `"type":"tool_use"`)
	args, delta := strings.Index(out, `"partial_json":"hi\"}"`), strings.Index(out, "event: message_delta")
	if !(text < start && start < args && args < delta) || !strings.Contains(out, `"stop_reason":"tool_use"`) ||
		!strings.HasSuffix(strings.TrimSpace(out), messageStopEnd) {
		t.Errorf("out of order or not closed: %s", out)
	}
}
