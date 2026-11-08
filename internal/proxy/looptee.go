package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	shim "github.com/iodesystems/openai-claude-api-shim"

	"github.com/iodesystems/corrallm/internal/config"
)

// Loop detection on native /v1/messages: a translated tee. The backend's
// Messages stream goes to the caller unchanged. A copy goes through the
// shim's reply translator (Messages → chat, the one anthropicWriter uses)
// into a loopWriter that writes nowhere: it only detects.
//
// From the first tool_use block on, every event but ping is held to
// message_stop, as the guard hold does: a cut must not hand the caller a
// tool call with half its input. A reply that ends is released unchanged.
//
// At a cut corrallm ends the stream itself, in the Messages shape: the held
// events are dropped, so a cut reply carries no tool call, the open text or
// thinking block is closed, then a message_delta with stop_reason "refusal"
// and the `loop` object, then message_stop — the ending messagesWriter gives
// a cut converted stream. Then the backend is cancelled. A finding under the
// tolerance goes onto the final message_delta and stop_reason stays. For
// that, message_delta waits for message_stop: the detector reads the tool
// calls whole at the end of the reply, which the translator signals at
// message_stop.
//
// It sits outside statusCapture and inside the native guard-out hold
// (chain.go).
type loopTee struct {
	http.ResponseWriter
	lw     *loopWriter  // the detector; it writes to discardSSE
	tr     *shim.Stream // Messages → chat, into lw
	cancel func()

	pending   []byte   // the partial event
	delta     []byte   // message_delta, held until message_stop
	holding   bool     // a tool_use block was seen, every event from it on is held
	held      [][]byte // the held events; a cut drops them
	open      int      // the open content block's index, -1 when none
	openType  string
	outTokens int // output_tokens of message_start, for a cut before message_delta
	ended     bool
}

func newLoopTee(w http.ResponseWriter, cfg *config.LoopDetection, chatBody []byte, cancel func()) *loopTee {
	t := &loopTee{ResponseWriter: w, cancel: cancel, open: -1}
	// The tee cancels after it has ended the stream, so the detector's own
	// cancel does nothing.
	t.lw = newLoopWriter(discardSSE{http.Header{"Content-Type": {"text/event-stream"}}}, cfg, chatBody, func() {})
	t.tr = shim.NewStream(func(b []byte) { _, _ = t.lw.Write(b) }, shim.StreamOptions{})
	return t
}

func (t *loopTee) Write(b []byte) (int, error) {
	if t.ended {
		return len(b), nil
	}
	if !strings.HasPrefix(t.Header().Get("Content-Type"), "text/event-stream") {
		return t.ResponseWriter.Write(b)
	}
	t.pending = append(t.pending, b...)
	for !t.ended {
		i := bytes.Index(t.pending, []byte("\n\n"))
		if i < 0 {
			break
		}
		ev := append([]byte(nil), t.pending[:i+2]...)
		t.pending = t.pending[i+2:]
		if err := t.event(ev); err != nil {
			return 0, err
		}
	}
	return len(b), nil
}

func (t *loopTee) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// event feeds one Messages event to the detector and sends it on.
func (t *loopTee) event(ev []byte) error {
	e := parseMessagesEvent(ev)
	t.tr.Feed(ev)
	if !t.holding && e.Type == "content_block_start" && e.ContentBlock.Type == "tool_use" {
		t.holding = true
	}
	switch {
	case e.Type == "message_delta":
		t.delta = ev
	case e.Type == "message_stop":
		if t.lw.cut == nil {
			d := t.delta
			t.delta = nil
			if f := t.lw.session.Finding(); f != nil && d != nil {
				d = withNote(d, "loop", f, false)
			}
			out := append(bytes.Join(t.held, nil), d...)
			t.held = nil
			_, err := t.ResponseWriter.Write(append(out, ev...))
			return err
		}
	case t.holding && e.Type == "error":
		// The backend ended the reply unfinished: the error goes out alone,
		// without the held call, and nothing after it.
		t.ended = true
		t.held, t.delta = nil, nil
		_, err := t.ResponseWriter.Write(ev)
		return err
	case t.holding && e.Type != "ping":
		t.held = append(t.held, ev)
	default:
		// The event that tripped the detector goes out too, as on the chat path.
		switch e.Type {
		case "message_start":
			t.outTokens = e.Message.Usage.OutputTokens
		case "content_block_start":
			t.open, t.openType = e.Index, e.ContentBlock.Type
		case "content_block_stop":
			t.open = -1
		}
		if _, err := t.ResponseWriter.Write(ev); err != nil {
			return err
		}
	}
	if t.lw.cut != nil {
		t.end()
	}
	return nil
}

// end closes the cut stream for the caller, then stops the backend.
func (t *loopTee) end() {
	t.ended = true
	var out []byte
	if t.open >= 0 {
		if t.openType == "thinking" {
			// Signed as corrallm's, so the passthrough strips it from the next
			// turn: Anthropic refuses a thinking block with no signature.
			out = append(out, messagesEvent("content_block_delta", map[string]any{"type": "content_block_delta", "index": t.open,
				"delta": map[string]string{"type": "signature_delta", "signature": thinkingSigCorrallm}})...)
		}
		out = append(out, messagesEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": t.open})...)
	}
	d := t.delta
	if d == nil {
		d = bareMessageDelta(t.outTokens)
	}
	out = append(out, withNote(d, "loop", t.lw.cut, true)...)
	out = append(out, messagesEvent("message_stop", map[string]string{"type": "message_stop"})...)
	_, _ = t.ResponseWriter.Write(out)
	t.Flush()
	t.cancel()
}

// finish sends what a stream that ended without message_stop left: a held
// message_delta and the partial event.
func (t *loopTee) finish() {
	if t.ended {
		return
	}
	t.ended = true
	rest := append(bytes.Join(t.held, nil), t.delta...)
	if rest = append(rest, t.pending...); len(rest) > 0 {
		_, _ = t.ResponseWriter.Write(rest)
	}
	t.held, t.delta, t.pending = nil, nil, nil
}

// discardSSE is a writer for a detector: an event stream that goes nowhere.
type discardSSE struct{ h http.Header }

func (d discardSSE) Header() http.Header       { return d.h }
func (discardSSE) Write(b []byte) (int, error) { return len(b), nil }
func (discardSSE) WriteHeader(int)             {}

// messagesEventInfo is the parts of a Messages stream event that the tee and
// the hold read.
type messagesEventInfo struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		PartialJSON string `json:"partial_json"`
	} `json:"delta"`
	Message struct {
		Usage struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// eventData is the JSON of one SSE event's data lines.
func eventData(ev []byte) []byte {
	var data []byte
	for _, line := range bytes.Split(ev, []byte("\n")) {
		if d, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:")); ok {
			data = append(data, bytes.TrimSpace(d)...)
		}
	}
	return data
}

func parseMessagesEvent(ev []byte) messagesEventInfo {
	var e messagesEventInfo
	_ = json.Unmarshal(eventData(ev), &e)
	return e
}

func messagesEvent(name string, v any) []byte {
	b, _ := json.Marshal(v)
	return []byte("event: " + name + "\ndata: " + string(b) + "\n\n")
}

// bareMessageDelta is the message_delta of a stream corrallm ends before the
// backend sent one.
func bareMessageDelta(outTokens int) []byte {
	return messagesEvent("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": nil, "stop_sequence": nil},
		"usage": map[string]int{"output_tokens": outTokens}})
}

// withNote puts corrallm's object (loop, guardrail) on a message_delta event,
// top level, and with refuse sets its stop_reason to "refusal". An event it
// cannot read is returned as it is.
func withNote(ev []byte, key string, v any, refuse bool) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(eventData(ev), &m) != nil {
		return ev
	}
	if refuse {
		var d map[string]json.RawMessage
		if json.Unmarshal(m["delta"], &d) != nil || d == nil {
			d = map[string]json.RawMessage{}
		}
		d["stop_reason"] = json.RawMessage(`"refusal"`)
		m["delta"], _ = json.Marshal(d)
	}
	var err error
	if m[key], err = json.Marshal(v); err != nil {
		return ev
	}
	return messagesEvent("message_delta", m)
}
