package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/iodesystems/corrallm/internal/config"
)

// Guardrails (P67), tool-call surface: the tool calls in a chat reply are held
// until each guardrail has checked them. Text before them goes out as it
// arrives; from the first tool-call delta on, the stream is held to its end.
//
// A hard fail of a NON-STREAMED reply replaces it with the guardrail's error
// (422 by default), the same reply the input surface gives. A STREAMED reply
// has sent its status already: the held events are dropped and the stream
// ends with finish_reason "content_filter" and a guardrail object. Usage
// chunks still go out, so the caller can count what it was charged.
//
// IT SITS OUTSIDE statusCapture, like costWriter: the activity row keeps the
// backend's reply, which is what an operator reviewing a refusal needs.

// guardOutWriter holds a chat reply's tool calls for the guardrails.
type guardOutWriter struct {
	http.ResponseWriter
	// check runs the guardrails on the tool calls, as one document.
	check func(text string) []guardVerdict

	decided bool
	code    int
	sse     bool
	active  bool
	held    []byte // non-streamed: the whole body; streamed: the partial line

	holding bool     // streamed: a tool call was seen, every line from it on is held
	lines   [][]byte // streamed: the held lines
	calls   map[toolKey]*toolCall
	first   []byte // streamed: the first held chunk, for its id and model
	done    bool

	// Refused is the hard fail that replaced the reply, for the activity row.
	Refused *guardVerdict
	// Status is what the caller got instead, on a non-streamed refusal.
	Status int
}

type toolKey struct{ choice, index int }

type toolCall struct {
	name string
	args strings.Builder
}

func (g *guardOutWriter) WriteHeader(code int) {
	if code >= 100 && code < 200 {
		g.ResponseWriter.WriteHeader(code)
		return
	}
	if g.decided {
		return
	}
	g.decided, g.code = true, code
	h := g.Header()
	ct := h.Get("Content-Type")
	if code == http.StatusOK && h.Get("Content-Encoding") == "" {
		switch {
		case strings.HasPrefix(ct, "text/event-stream"):
			g.active, g.sse = true, true
		case isJSONContentType(ct):
			g.active = true
			// The reply may be replaced; its length is decided at finish.
			h.Del("Content-Length")
			return // the status waits for the body
		}
	}
	g.ResponseWriter.WriteHeader(code)
}

func (g *guardOutWriter) Write(b []byte) (int, error) {
	if !g.decided {
		g.WriteHeader(http.StatusOK)
	}
	if !g.active {
		return g.ResponseWriter.Write(b)
	}
	g.held = append(g.held, b...)
	if !g.sse {
		return len(b), nil
	}
	i := bytes.LastIndexByte(g.held, '\n')
	if i < 0 {
		return len(b), nil
	}
	out := g.lineMode(g.held[:i+1])
	g.held = append([]byte(nil), g.held[i+1:]...)
	if len(out) > 0 {
		if _, err := g.ResponseWriter.Write(out); err != nil {
			return 0, err
		}
	}
	return len(b), nil
}

// Flush passes through only what may go out: a held reply must not commit
// its status early.
func (g *guardOutWriter) Flush() {
	if g.active && !g.sse && !g.done {
		return
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// lineMode passes the lines before the first tool call and holds the rest.
// At the stream's [DONE] the held lines are checked and released or refused.
func (g *guardOutWriter) lineMode(b []byte) []byte {
	var out []byte
	for len(b) > 0 {
		line := b
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			line = b[:i+1]
		}
		b = b[len(line):]
		data, isData := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
		data = bytes.TrimSpace(data)
		if isData && bytes.Equal(data, []byte("[DONE]")) && g.holding {
			out = append(out, g.release(line)...)
			continue
		}
		if !g.holding && isData && bytes.Contains(data, []byte(`"tool_calls"`)) && g.addDeltas(data) {
			g.holding, g.first = true, append([]byte(nil), data...)
		} else if g.holding && isData {
			g.addDeltas(data)
		}
		if g.holding {
			g.lines = append(g.lines, append([]byte(nil), line...))
			continue
		}
		out = append(out, line...)
	}
	return out
}

// addDeltas folds one chunk's tool-call deltas into the calls; false when the
// chunk carries none.
func (g *guardOutWriter) addDeltas(data []byte) bool {
	var c struct {
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				ToolCalls []struct {
					Index    int `json:"index"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &c) != nil {
		return false
	}
	found := false
	for _, ch := range c.Choices {
		for _, tc := range ch.Delta.ToolCalls {
			found = true
			g.addCall(toolKey{ch.Index, tc.Index}, tc.Function.Name, tc.Function.Arguments)
		}
	}
	return found
}

func (g *guardOutWriter) addCall(k toolKey, name, args string) {
	if g.calls == nil {
		g.calls = map[toolKey]*toolCall{}
	}
	c := g.calls[k]
	if c == nil {
		c = &toolCall{}
		g.calls[k] = c
	}
	if name != "" {
		c.name = name
	}
	c.args.WriteString(args)
}

// document is the tool calls as the guard reads them, one per line.
func (g *guardOutWriter) document() string {
	keys := make([]toolKey, 0, len(g.calls))
	for k := range g.calls {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].choice != keys[j].choice {
			return keys[i].choice < keys[j].choice
		}
		return keys[i].index < keys[j].index
	})
	var lines []string
	for _, k := range keys {
		c := g.calls[k]
		lines = append(lines, fmt.Sprintf("calls the tool %s with the arguments %s", c.name, c.args.String()))
	}
	return strings.Join(lines, "\n")
}

// refusal is the first hard fail among the verdicts, or a hard guard that
// could not run (it fails closed). Soft fails are logged.
func refusal(vs []guardVerdict) *guardVerdict {
	var hit *guardVerdict
	for i, v := range vs {
		switch {
		case v.Hard && (v.Failed || v.Unavailable != ""):
			if hit == nil {
				hit = &vs[i]
			}
		case v.Unavailable != "":
			slog.Warn("soft guardrail could not run; tool calls passed", "guardrail", v.Name, "err", v.Unavailable)
		case v.Failed:
			slog.Warn("soft guardrail flagged tool calls", "guardrail", v.Name,
				"surface", "tool_calls", "score", v.Score, "threshold", v.Threshold)
		}
	}
	return hit
}

// release checks the held stream and returns what goes out in its place:
// the held lines and the terminator, or a content_filter chunk, the usage
// chunks and the terminator.
func (g *guardOutWriter) release(terminator []byte) []byte {
	g.done = true
	held := g.lines
	g.lines, g.holding = nil, false
	hit := refusal(g.check(g.document()))
	if hit == nil {
		return append(bytes.Join(held, nil), terminator...)
	}
	g.Refused = hit
	var head struct {
		ID      string `json:"id"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
	}
	_ = json.Unmarshal(g.first, &head)
	chunk, _ := json.Marshal(map[string]any{
		"id": head.ID, "object": "chat.completion.chunk", "created": head.Created, "model": head.Model,
		"choices":   []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "content_filter"}},
		"guardrail": hit.object("tool_calls"),
	})
	out := append([]byte("data: "), chunk...)
	out = append(out, "\n\n"...)
	for _, line := range held {
		data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
		if !ok {
			continue
		}
		var c struct {
			Choices []json.RawMessage `json:"choices"`
			Usage   json.RawMessage   `json:"usage"`
		}
		if json.Unmarshal(bytes.TrimSpace(data), &c) == nil && len(c.Choices) == 0 && len(c.Usage) > 0 {
			out = append(out, line...)
			out = append(out, '\n')
		}
	}
	return append(out, terminator...)
}

// finish releases what is held: a non-streamed reply after its check, or a
// stream that ended without [DONE]. Safe on any exit path.
func (g *guardOutWriter) finish() {
	if !g.active || g.done {
		return
	}
	if g.sse {
		g.done = true
		rest := g.held
		g.held = nil
		if len(rest) > 0 {
			rest = g.lineMode(append(rest, '\n'))
		}
		if g.holding {
			rest = append(rest, g.release(nil)...)
		}
		if len(rest) > 0 {
			_, _ = g.ResponseWriter.Write(rest)
		}
		g.Flush()
		return
	}
	g.done = true
	body := g.held
	g.held = nil
	if !g.toolCallsIn(body) {
		g.ResponseWriter.WriteHeader(g.code)
		_, _ = g.ResponseWriter.Write(body)
		return
	}
	vs := g.check(g.document())
	var report []string
	for _, v := range vs {
		report = append(report, v.header())
	}
	g.Header().Add(HeaderGuardrail, strings.Join(report, ", "))
	hit := refusal(vs)
	if hit == nil {
		g.ResponseWriter.WriteHeader(g.code)
		_, _ = g.ResponseWriter.Write(body)
		return
	}
	g.Refused = hit
	g.Status = http.StatusServiceUnavailable
	var b []byte
	if hit.Unavailable != "" {
		b, _ = json.Marshal(map[string]any{"error": map[string]any{
			"message":   fmt.Sprintf("guardrail %q could not check the tool calls: %s", hit.Name, hit.Unavailable),
			"type":      "guardrail",
			"code":      "guardrail_unavailable",
			"guardrail": hit.object("tool_calls"),
		}})
		if hit.Status == http.StatusTooManyRequests {
			g.Status = http.StatusTooManyRequests
			if hit.RetryAfter != "" {
				g.Header().Set("Retry-After", hit.RetryAfter)
			}
		}
	} else {
		g.Status = hit.FailStatus
		_, b = guardrailFailBody(*hit, "tool_calls")
	}
	g.Header().Set("Content-Type", "application/json; charset=utf-8")
	g.ResponseWriter.WriteHeader(g.Status)
	_, _ = g.ResponseWriter.Write(b)
}

// toolCallsIn reads a non-streamed reply's tool calls into the writer.
func (g *guardOutWriter) toolCallsIn(body []byte) bool {
	if !bytes.Contains(body, []byte(`"tool_calls"`)) {
		return false
	}
	var r struct {
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &r) != nil {
		return false
	}
	for _, ch := range r.Choices {
		for i, tc := range ch.Message.ToolCalls {
			g.addCall(toolKey{ch.Index, i}, tc.Function.Name, tc.Function.Arguments)
		}
	}
	return len(g.calls) > 0
}

// responseGuards are the guardrails that run on the reply.
func responseGuards(active []config.ActiveGuard) []config.ActiveGuard {
	var out []config.ActiveGuard
	for _, g := range active {
		if g.Guard.On(config.SurfaceResponses) {
			out = append(out, g)
		}
	}
	return out
}

// checkToolCalls runs each guardrail on the tool calls, as complete content.
func (p *Proxy) checkToolCalls(r *http.Request, guards []config.ActiveGuard) func(string) []guardVerdict {
	return func(text string) []guardVerdict {
		var vs []guardVerdict
		for _, g := range guards {
			v := p.runGuard(r, g, "Assistant", text, nil, true)
			v.FailStatus = g.Guard.FailStatus()
			vs = append(vs, v)
		}
		return vs
	}
}
