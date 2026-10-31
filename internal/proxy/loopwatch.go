package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/agentkit/loopdetector"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/metrics"
)

// Loop detection: agentkit's loopdetector, installed on streamed chat replies.
// The proxy has what it needs, the request's messages and the reply going by,
// thinking included, so a client without agentkit is covered too and the count
// comes from before any client has cut or trimmed the reply.
//
// The bytes pass through unchanged, except that the reply's last event carries a
// `loop` object when a loop was found. The proxy cannot know a client's semantics,
// so it does not judge: that the stream passed says only that the count stayed
// under the tolerance. At the tolerance the stream ends with
// finish_reason "content_filter" and a loop object, the way a guardrail ends
// one, and the request is cancelled so the backend stops generating. agentkit
// clients read the loop object as a cut of their own (llm.RepetitionInfo).
//
// IT SITS BEFORE THE GUARDRAILS, inside guardOutWriter: they see the same events
// the caller would, the closing ones included.

// errLoopCut is the cancellation cause of a stream cut for looping.
var errLoopCut = errors.New("loop detected")

type loopWriter struct {
	http.ResponseWriter
	session *loopdetector.Session
	cancel  func()
	line    []byte // the partial line
	calls   map[int]*toolCall
	order   []int
	first   []byte // the first event, for its id and model

	// cut is the finding that ended the stream.
	cut *llm.RepetitionInfo
}

func newLoopWriter(w http.ResponseWriter, cfg *config.LoopDetection, body []byte, cancel func()) *loopWriter {
	d := loopdetector.New(loopdetector.Config{
		Tolerance: cfg.Tolerance, Window: cfg.Window, MinShare: cfg.MinShare,
		Similarity: cfg.Similarity, ToolWindow: cfg.ToolWindow,
		ToolTolerance: cfg.ToolTolerance, SampleBytes: cfg.SampleBytes,
	})
	return &loopWriter{ResponseWriter: w, session: d.Begin(requestHistory(body)), cancel: cancel, calls: map[int]*toolCall{}}
}

// requestHistory reads the messages of a chat request. Content may be a string
// or an array of parts, which llm.Message does not decode.
func requestHistory(body []byte) []llm.Message {
	var req struct {
		Messages []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCalls  []llm.ToolCall  `json:"tool_calls"`
			ToolCallID string          `json:"tool_call_id"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return nil
	}
	out := make([]llm.Message, len(req.Messages))
	for i, m := range req.Messages {
		out[i] = llm.Message{Role: m.Role, ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID}
		var parts []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(m.Content, &out[i].Content) != nil && json.Unmarshal(m.Content, &parts) == nil {
			var b strings.Builder
			for _, p := range parts {
				b.WriteString(p.Text)
			}
			out[i].Content = b.String()
		}
	}
	return out
}

func (l *loopWriter) Write(b []byte) (int, error) {
	if l.cut != nil {
		return len(b), nil
	}
	if !strings.HasPrefix(l.Header().Get("Content-Type"), "text/event-stream") {
		return l.ResponseWriter.Write(b)
	}
	l.line = append(l.line, b...)
	rest := l.line
	defer func() { l.line = l.line[:copy(l.line, rest)] }()
	for {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			return len(b), nil
		}
		line := rest[:i+1]
		rest = rest[i+1:]
		out, info := l.event(line)
		if _, err := l.ResponseWriter.Write(out); err != nil {
			return 0, err
		}
		if info != nil {
			l.interrupt(info)
			return len(b), nil
		}
	}
}

// release sends a last line that had no newline. Runs before the guardrails
// finish, which is the end of the stream as far as the caller can tell.
func (l *loopWriter) release() {
	if len(l.line) > 0 && l.cut == nil {
		_, _ = l.ResponseWriter.Write(l.line)
	}
	l.line = nil
}

func (l *loopWriter) Flush() {
	if f, ok := l.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// event feeds one line of the stream to the detector and returns the line to
// send: the same, unless it ends the reply and a loop was found. Then it carries
// the finding, cut or not, for the caller to weigh.
func (l *loopWriter) event(line []byte) ([]byte, *llm.RepetitionInfo) {
	info, finished := l.watch(line)
	f := l.session.Finding()
	if info != nil || !finished || f == nil {
		return line, info
	}
	found, err := json.Marshal(f)
	end := bytes.LastIndexByte(line, '}')
	if err != nil || end < 0 {
		return line, nil
	}
	out := append([]byte(nil), line[:end]...)
	out = append(append(out, `,"loop":`...), found...)
	return append(out, line[end:]...), nil
}

// watch feeds one line to the detector. finished says it ended the reply.
func (l *loopWriter) watch(line []byte) (cut *llm.RepetitionInfo, finished bool) {
	data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
	if !ok {
		return nil, false
	}
	data = bytes.TrimSpace(data)
	var ev struct {
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
				ToolCalls        []struct {
					Index    int `json:"index"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return nil, false
	}
	if l.first == nil {
		l.first = append([]byte(nil), data...)
	}
	for _, c := range ev.Choices {
		if c.Index != 0 {
			continue
		}
		if r := c.Delta.ReasoningContent + c.Delta.Reasoning; r != "" {
			if info := l.session.Watch(llm.ChannelReasoning, r); info != nil {
				return info, false
			}
		}
		if info := l.session.Watch(llm.ChannelContent, c.Delta.Content); info != nil {
			return info, false
		}
		for _, td := range c.Delta.ToolCalls {
			call := l.calls[td.Index]
			if call == nil {
				call = &toolCall{}
				l.calls[td.Index] = call
				l.order = append(l.order, td.Index)
			}
			if td.Function.Name != "" {
				call.name = td.Function.Name
			}
			call.args.WriteString(td.Function.Arguments)
			if info := l.session.Watch(llm.ToolChannel(td.Index), td.Function.Arguments); info != nil {
				return info, false
			}
		}
		if c.FinishReason != "" {
			finished = true
			for _, i := range l.order {
				if info := l.session.WatchToolCall(l.calls[i].name, l.calls[i].args.String()); info != nil {
					return info, false
				}
			}
		}
	}
	return nil, finished
}

// interrupt ends the stream for the caller, then stops the backend.
func (l *loopWriter) interrupt(info *llm.RepetitionInfo) {
	l.cut = info
	var head struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
	}
	_ = json.Unmarshal(l.first, &head)
	end, _ := json.Marshal(map[string]any{
		"id": head.ID, "object": head.Object, "created": head.Created, "model": head.Model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "content_filter"}},
		"loop":    info,
	})
	_, _ = fmt.Fprintf(l.ResponseWriter, "data: %s\n\ndata: [DONE]\n\n", end)
	l.Flush()
	l.cancel()
}

// done reports the reply to the metrics.
func (l *loopWriter) done(model string) {
	loops, kind := l.session.Loops()
	metrics.Loop(model, kind, loops, l.cut != nil)
	if kind != "" {
		slog.Warn("loop detected", "model", model, "kind", kind, "loops", loops, "cut", l.cut != nil)
	}
}
