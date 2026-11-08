package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	shim "github.com/iodesystems/openai-claude-api-shim"
)

// Guardrails on the tool calls of a native /v1/messages reply: the Messages
// shape of guardOutWriter, with the same verdict (toolCalls.document,
// refusal, toolCallRefusal). Text before the first tool_use block goes out
// as it arrives; from that block's content_block_start on, every event but
// ping is held to message_stop.
//
// At message_stop the guardrails read the held calls. A pass releases the
// held events as they came. A STREAMED refusal drops them, and the stream
// ends with the backend's message_delta (its usage, and a loop object the
// tee put there) with stop_reason "refusal" and a top-level `guardrail`
// object, then message_stop. A NON-STREAMED refusal replaces the reply with
// the guardrail's status (422 by default) and an Anthropic error body, with
// the `guardrail` object top level.
//
// It sits outside statusCapture and the loop tee (chain.go): the activity
// row keeps the backend's reply, and a cut stream's ending reaches the hold
// as any other.
type messagesGuardWriter struct {
	http.ResponseWriter
	// check runs the guardrails on the tool calls, as one document.
	check func(text string) []guardVerdict

	decided bool
	code    int
	sse     bool
	active  bool
	held    []byte // non-streamed: the whole body; streamed: the partial event

	holding bool     // streamed: a tool_use block was seen, every event from it on is held
	events  [][]byte // streamed: the held events
	calls   toolCalls
	done    bool
	errored bool // streamed: an error event ended a held reply; nothing more goes out

	// Refused is the hard fail that replaced the reply, for the activity row.
	Refused *guardVerdict
	// Status is what the caller got instead, on a non-streamed refusal.
	Status int
}

func (g *messagesGuardWriter) WriteHeader(code int) {
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

func (g *messagesGuardWriter) Write(b []byte) (int, error) {
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
	var out []byte
	for {
		i := bytes.Index(g.held, []byte("\n\n"))
		if i < 0 {
			break
		}
		ev := append([]byte(nil), g.held[:i+2]...)
		g.held = g.held[i+2:]
		out = append(out, g.event(ev)...)
	}
	if len(out) > 0 {
		if _, err := g.ResponseWriter.Write(out); err != nil {
			return 0, err
		}
	}
	return len(b), nil
}

// Flush passes through only what may go out: a held reply must not commit
// its status early.
func (g *messagesGuardWriter) Flush() {
	if g.active && !g.sse && !g.done {
		return
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// event passes one event before the first tool_use block and holds the rest.
// At message_stop the held events are checked and released or refused.
func (g *messagesGuardWriter) event(ev []byte) []byte {
	if g.errored {
		return nil
	}
	if g.done {
		return ev
	}
	e := parseMessagesEvent(ev)
	if !g.holding && e.Type == "content_block_start" && e.ContentBlock.Type == "tool_use" {
		g.holding = true
	}
	if !g.holding || e.Type == "ping" {
		return ev // a ping keeps the caller from idling through a long hold
	}
	k := toolKey{0, e.Index}
	switch e.Type {
	case "content_block_start":
		if e.ContentBlock.Type == "tool_use" {
			g.calls.add(k, e.ContentBlock.Name, "")
		}
	case "content_block_delta":
		if c := g.calls[k]; c != nil && e.Delta.Type == "input_json_delta" {
			c.args.WriteString(e.Delta.PartialJSON)
		}
	case "content_block_stop":
		// A call with no arguments streams none; the chat path reads "{}".
		if c := g.calls[k]; c != nil && c.args.Len() == 0 {
			c.args.WriteString("{}")
		}
	case "message_stop":
		return g.release(ev)
	case "error":
		// The backend ended the reply unfinished: the error goes out alone,
		// without the held calls (unchecked), and nothing after it.
		g.done, g.errored = true, true
		g.events, g.holding = nil, false
		return ev
	}
	g.events = append(g.events, ev)
	return nil
}

// release checks the held events and returns what goes out in their place:
// the held events and the terminator, or the refusal's message_delta and the
// terminator.
func (g *messagesGuardWriter) release(terminator []byte) []byte {
	g.done = true
	held := g.events
	g.events, g.holding = nil, false
	hit := refusal(g.check(g.calls.document()))
	if hit == nil {
		return append(bytes.Join(held, nil), terminator...)
	}
	g.Refused = hit
	var delta []byte
	for _, ev := range held {
		if parseMessagesEvent(ev).Type == "message_delta" {
			delta = ev
		}
	}
	if delta == nil {
		delta = bareMessageDelta(0)
	}
	return append(withNote(delta, "guardrail", hit.object("tool_calls"), true), terminator...)
}

// finish releases what is held: a non-streamed reply after its check, or a
// stream that ended without message_stop. Safe on any exit path.
func (g *messagesGuardWriter) finish() {
	if !g.active || g.done {
		return
	}
	if g.sse {
		rest := g.held
		g.held = nil
		if len(rest) > 0 {
			rest = g.event(append(rest, '\n', '\n'))
		}
		if g.holding && !g.done {
			rest = append(rest, g.release(nil)...)
			if g.Refused != nil {
				// corrallm ended the reply: it ends as a Messages stream.
				rest = append(rest, messagesEvent("message_stop", map[string]string{"type": "message_stop"})...)
			}
		}
		g.done = true
		if len(rest) > 0 {
			_, _ = g.ResponseWriter.Write(rest)
		}
		g.Flush()
		return
	}
	g.done = true
	body := g.held
	g.held = nil
	if !g.toolUseIn(body) {
		g.ResponseWriter.WriteHeader(g.code)
		_, _ = g.ResponseWriter.Write(body)
		return
	}
	vs := g.check(g.calls.document())
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
	var msg string
	g.Status, msg = toolCallRefusal(g.Header(), *hit)
	b, _ := json.Marshal(map[string]any{
		"type":      "error",
		"error":     map[string]any{"type": shim.ErrorType(g.Status), "message": msg},
		"guardrail": hit.object("tool_calls"),
	})
	g.Header().Set("Content-Type", "application/json")
	g.ResponseWriter.WriteHeader(g.Status)
	_, _ = g.ResponseWriter.Write(b)
}

// toolUseIn reads a non-streamed Messages reply's tool_use blocks into the
// writer.
func (g *messagesGuardWriter) toolUseIn(body []byte) bool {
	if !bytes.Contains(body, []byte(`"tool_use"`)) {
		return false
	}
	var r struct {
		Content []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if json.Unmarshal(body, &r) != nil {
		return false
	}
	for i, b := range r.Content {
		if b.Type == "tool_use" {
			g.calls.add(toolKey{0, i}, b.Name, string(b.Input))
		}
	}
	return len(g.calls) > 0
}
