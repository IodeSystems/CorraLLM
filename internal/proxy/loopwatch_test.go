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

func loopSentence(i int) string {
	return fmt.Sprintf("Looking again at subject %d of the backlog, it blocks nothing urgent so it stays where it is for now. ", i%30)
}

// loopRig serves a backend that thinks in a cycle until it is hung up on, or for
// max events, then finishes.
func loopRig(t *testing.T, cfg *config.LoopDetection, max int) (string, *atomic.Int64) {
	return loopRigFor(t, cfg, nil, max)
}

func loopRigFor(t *testing.T, global, perModel *config.LoopDetection, max int) (string, *atomic.Int64) {
	t.Helper()
	treatAsRemote(t)
	sent := new(atomic.Int64)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := range max {
			if r.Context().Err() != nil {
				return
			}
			fmt.Fprintf(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"fw\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":%q}}]}\n\n", loopSentence(i))
			fl.Flush()
			sent.Store(int64(i + 1))
			time.Sleep(time.Millisecond)
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		fl.Flush()
	}))
	t.Cleanup(up.Close)
	return mountLoopProxy(t, up.URL, global, perModel), sent
}

func mountLoopProxy(t *testing.T, target string, global, perModel *config.LoopDetection) string {
	t.Helper()
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr := proc.NewManager(&config.Config{})
	t.Cleanup(mgr.Shutdown)
	r := chi.NewRouter()
	model := modelTo(t, target, "chat")
	model.LoopDetection = perModel
	New(&config.Config{
		Models:         map[string]config.Model{"fw": model},
		PriorityGroups: map[string]config.PriorityGroup{"default": {Weight: 1}},
		Keys:           map[string]config.KeyPolicy{"k": {Group: "default"}},
		LoopDetection:  global,
	}, mgr, sched.New(), st).Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv.URL
}

// fixedBackend answers every request with body, through a proxy watching streams.
func fixedBackend(t *testing.T, contentType, body string) string {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(up.Close)
	return mountLoopProxy(t, up.URL, &config.LoopDetection{}, nil)
}

func streamChat(t *testing.T, url string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/chat/completions",
		strings.NewReader(`{"model":"fw","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`))
	req.Header.Set("X-Corrallm-Key", "k")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestALoopingStreamIsEndedAndTheBackendHungUpOn(t *testing.T) {
	url, sent := loopRig(t, &config.LoopDetection{}, 2000)
	out := streamChat(t, url)
	if !strings.Contains(out, `"finish_reason":"content_filter"`) || !strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
		t.Fatalf("stream did not end with the cut: ...%s", out[max(0, len(out)-300):])
	}
	if !strings.Contains(out, `"kind":"cycle"`) {
		t.Errorf("no loop object: ...%s", out[max(0, len(out)-300):])
	}
	for _, field := range []string{`"first":"Looking again`, `"last":"Looking again`, `"trailing":`} {
		if !strings.Contains(out, field) {
			t.Errorf("loop object lacks %s: ...%s", field, out[max(0, len(out)-500):])
		}
	}
	if sent.Load() > 500 {
		t.Errorf("backend sent %d events before it stopped", sent.Load())
	}
}

// The whole chain: what corrallm sends at a cut is what an agentkit client needs
// to treat it as a cut.
func TestAnAgentkitClientReadsACorrallmCut(t *testing.T) {
	url, _ := loopRig(t, &config.LoopDetection{}, 2000)
	c := llm.NewClient(url+"/v1", "k", "fw")
	ch, err := c.ChatStream(context.Background(), []llm.Message{{Role: "user", Content: "go"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var last llm.StreamChunk
	for chunk := range ch {
		if chunk.Done {
			last = chunk
		}
	}
	if last.StopReason != llm.StopReasonRepetition || last.Repetition == nil || last.Repetition.Where != llm.ChannelReasoning {
		t.Fatalf("final chunk = %+v", last)
	}
}

func TestNegativeToleranceOnlyCounts(t *testing.T) {
	url, sent := loopRig(t, &config.LoopDetection{Tolerance: -1}, 300)
	out := streamChat(t, url)
	if strings.Contains(out, "content_filter") || sent.Load() != 300 {
		t.Fatalf("a count-only detector changed the stream: sent %d", sent.Load())
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Error("the backend's own ending was lost")
	}
	// The finding goes to the caller even though nothing was cut, on the event
	// that ended the reply, in JSON that still parses.
	var last string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, `"finish_reason"`) {
			last = strings.TrimPrefix(line, "data: ")
		}
	}
	var ev struct {
		Loop *llm.RepetitionInfo `json:"loop"`
	}
	if err := json.Unmarshal([]byte(last), &ev); err != nil {
		t.Fatalf("final event: %v: %s", err, last)
	}
	if ev.Loop == nil || ev.Loop.Kind != "cycle" || ev.Loop.Reps < 3 {
		t.Errorf("loop = %+v", ev.Loop)
	}
}

func TestAQuietStreamCarriesNoLoopObject(t *testing.T) {
	treatAsRemote(t)
	url := fixedBackend(t, "text/event-stream",
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n"+
			"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]")
	out := streamChat(t, url)
	if strings.Contains(out, `"loop"`) || !strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
		t.Fatalf("out = %s", out)
	}
}

func TestAnErrorBodyPassesThrough(t *testing.T) {
	treatAsRemote(t)
	url := fixedBackend(t, "application/json", `{"error":{"message":"no newline here"}}`)
	if out := streamChat(t, url); !strings.Contains(out, "no newline here") {
		t.Fatalf("out = %q", out)
	}
}

func TestWithoutConfigNothingIsWatched(t *testing.T) {
	url, sent := loopRig(t, nil, 300)
	if out := streamChat(t, url); strings.Contains(out, "content_filter") || sent.Load() != 300 {
		t.Fatal("the stream was changed with no loopDetection set")
	}
}

func TestAModelCanTuneOrLeaveTheTopLevelSetting(t *testing.T) {
	url, sent := loopRigFor(t, &config.LoopDetection{}, &config.LoopDetection{Off: true}, 300)
	if out := streamChat(t, url); strings.Contains(out, "content_filter") || sent.Load() != 300 {
		t.Fatal("a model with off: true was cut")
	}
	url, sent = loopRigFor(t, &config.LoopDetection{Tolerance: -1}, &config.LoopDetection{}, 300)
	if out := streamChat(t, url); !strings.Contains(out, "content_filter") {
		t.Fatalf("a model's own setting did not replace the top level: sent %d", sent.Load())
	}
	url, sent = loopRigFor(t, nil, &config.LoopDetection{}, 300)
	if out := streamChat(t, url); !strings.Contains(out, "content_filter") {
		t.Fatalf("a model's own setting did not apply alone: sent %d", sent.Load())
	}
}

func TestRequestHistoryReadsBothContentShapes(t *testing.T) {
	tc := llm.ToolCall{ID: "a"}
	tc.Function.Name = "read"
	body, _ := json.Marshal(map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []map[string]string{{"type": "text", "text": "one"}, {"type": "text", "text": "two"}}},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []llm.ToolCall{tc}},
		map[string]any{"role": "tool", "content": "done", "tool_call_id": "a"},
	}})
	h := requestHistory(body)
	if len(h) != 3 || h[0].Content != "onetwo" || h[1].ToolCalls[0].Function.Name != "read" || h[2].ToolCallID != "a" {
		t.Fatalf("history = %+v", h)
	}
	if requestHistory([]byte("not json")) != nil {
		t.Error("a body that is not JSON must give no history")
	}
}
