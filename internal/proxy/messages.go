package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

// POST /v1/messages: Anthropic's Messages API, in front of every model.
//
// Claude Code (and anything else that speaks only Anthropic) points
// ANTHROPIC_BASE_URL at corrallm. The request then goes through the same walk
// as /v1/chat/completions — keys, limits, lanes, admission, the activity log —
// and each candidate gets it in its own dialect:
//
//   - a backend whose proxy says `api: anthropic` gets the body AS SENT
//     (passthrough): cache_control, thinking and betas reach Anthropic intact.
//     With `callerAuth: true` the caller's own credential goes with it — a
//     Claude Code signed in to a subscription — and the caller names its
//     corrallm key in X-Corrallm-Key.
//   - every other backend gets OpenAI chat completions, translated here, and
//     its reply (JSON or SSE) is translated back.
//
// That is what makes running out of Claude seamless: a lane [claude, chat]
// serves Claude until Anthropic answers 429 (or 503/529), and the walk then
// gives the SAME request to the next member, translated. Anthropic's 429 cools
// the backend until its Retry-After (or the subscription's reset), so the next
// requests go straight to the fallback without asking Anthropic again.
//
// Translated requests lose nothing a local model can use. They drop:
// cache_control (llama.cpp caches the prefix by itself), thinking settings,
// metadata, server tools (web search &c.), and Claude Code's per-request
// billing line in the system prompt — which changes on every request and,
// left in, put a different first line on every prompt so a local backend
// could never reuse its prompt cache.
//
// Thinking a translated model streams back is sent as thinking blocks signed
// with thinkingSigCorrallm. Anthropic would refuse those signatures, so the
// passthrough strips them (see stripCorrallmThinking).

// thinkingSigCorrallm marks a thinking block corrallm made from a non-Anthropic
// model's reasoning. It is not a valid Anthropic signature, on purpose.
const thinkingSigCorrallm = "corrallm"

// nativeMessagesKey marks an upstream request as /v1/messages passthrough, for
// the Director (callerAuth).
type nativeMessagesKey struct{}

func (p *Proxy) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	// Anthropic clients send the key as x-api-key. corrallm reads its key from
	// Authorization; move it there, and off x-api-key, so it can never be
	// forwarded as an Anthropic credential.
	if k := r.Header.Get("X-Api-Key"); k != "" && r.Header.Get("X-Corrallm-Key") == "" && r.Header.Get("Authorization") == "" {
		r.Header.Set("Authorization", "Bearer "+k)
		r.Header.Del("X-Api-Key")
	}
	ew := &anthropicErrorWriter{ResponseWriter: w}
	p.handleInference(ew, r)
	ew.finish()
}

// --- errors ---

// anthropicErrorType is Anthropic's error type for a status.
func anthropicErrorType(code int) string {
	switch code {
	case http.StatusBadRequest, http.StatusMethodNotAllowed, http.StatusUnprocessableEntity:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusPaymentRequired:
		return "billing_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable, 529:
		return "overloaded_error"
	}
	return "api_error"
}

func writeAnthropicError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(anthropicErrorBody(code, msg))
}

func anthropicErrorBody(code int, msg string) []byte {
	b, _ := json.Marshal(map[string]any{"type": "error",
		"error": map[string]string{"type": anthropicErrorType(code), "message": msg}})
	return b
}

// anthropicErrorWriter restates every error reply in Anthropic's envelope:
// corrallm's own (OpenAI-shaped JSON or plain text) and an OpenAI backend's.
// An error already in Anthropic's shape (the passthrough's) is left alone.
// Successful replies pass straight through, streams included.
type anthropicErrorWriter struct {
	http.ResponseWriter
	code int
	held []byte
}

func (e *anthropicErrorWriter) WriteHeader(code int) {
	if e.code != 0 {
		return
	}
	e.code = code
	if code < 400 {
		e.ResponseWriter.WriteHeader(code)
	}
}

func (e *anthropicErrorWriter) Write(b []byte) (int, error) {
	if e.code == 0 {
		e.WriteHeader(http.StatusOK)
	}
	if e.code >= 400 {
		e.held = append(e.held, b...)
		return len(b), nil
	}
	return e.ResponseWriter.Write(b)
}

func (e *anthropicErrorWriter) Flush() {
	if e.code < 400 {
		if f, ok := e.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
	}
}

func (e *anthropicErrorWriter) finish() {
	if e.code < 400 {
		return
	}
	body := e.held
	var probe struct {
		Type  string          `json:"type"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &probe) != nil || probe.Type != "error" {
		msg := strings.TrimSpace(string(body))
		var oai struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			Budgets json.RawMessage `json:"budgets"`
		}
		if json.Unmarshal(body, &oai) == nil && oai.Error.Message != "" {
			msg = oai.Error.Message
		} else {
			var s struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(body, &s) == nil && s.Error != "" {
				msg = s.Error
			}
		}
		if msg == "" {
			msg = http.StatusText(e.code)
		}
		body = anthropicErrorBody(e.code, msg)
	}
	h := e.Header()
	h.Set("Content-Type", "application/json")
	h.Del("Content-Length")
	e.ResponseWriter.WriteHeader(e.code)
	_, _ = e.ResponseWriter.Write(body)
}

// --- request: Anthropic → OpenAI ---

type antInBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Source    *antInSource    `json:"source"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	Thinking  string          `json:"thinking"`
	Signature string          `json:"signature"`
}

type antInSource struct {
	Type      string `json:"type"` // base64 | url | text
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
}

type antInRequest struct {
	Model      string            `json:"model"`
	MaxTokens  *int              `json:"max_tokens"`
	System     json.RawMessage   `json:"system"`
	Messages   []json.RawMessage `json:"messages"`
	Tools      []json.RawMessage `json:"tools"`
	ToolChoice *struct {
		Type                   string `json:"type"`
		Name                   string `json:"name"`
		DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
	} `json:"tool_choice"`
	StopSequences []string `json:"stop_sequences"`
	Stream        bool     `json:"stream"`
	Temperature   *float64 `json:"temperature"`
	TopP          *float64 `json:"top_p"`
	TopK          *int     `json:"top_k"`
}

// blocksOf reads content that is a string or a list of blocks.
func blocksOf(raw json.RawMessage) ([]antInBlock, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []antInBlock{{Type: "text", Text: s}}, nil
	}
	var bs []antInBlock
	if err := json.Unmarshal(raw, &bs); err != nil {
		return nil, fmt.Errorf("content is neither a string nor a list of blocks")
	}
	return bs, nil
}

// billingLine is Claude Code's per-request attribution line in the system
// prompt ("x-anthropic-billing-header: cc_version=…; cch=…;").
var billingLine = regexp.MustCompile(`(?m)^x-anthropic-billing-header:[^\n]*\n?`)

// oaiPartOf turns a text or image block into an OpenAI content part.
func oaiPartOf(b antInBlock) (map[string]any, error) {
	switch b.Type {
	case "text":
		return map[string]any{"type": "text", "text": b.Text}, nil
	case "image":
		if b.Source == nil {
			return nil, fmt.Errorf("image block has no source")
		}
		u := b.Source.URL
		if b.Source.Type == "base64" {
			u = "data:" + b.Source.MediaType + ";base64," + b.Source.Data
		}
		if u == "" {
			return nil, fmt.Errorf("image source type %q is not supported", b.Source.Type)
		}
		return map[string]any{"type": "image_url", "image_url": map[string]string{"url": u}}, nil
	case "document":
		if b.Source != nil && b.Source.Type == "text" {
			return map[string]any{"type": "text", "text": b.Source.Data}, nil
		}
		return nil, fmt.Errorf("document blocks other than plain text are not supported for non-Anthropic models")
	}
	return nil, nil // thinking, redacted_thinking, server tool blocks: dropped
}

// userContent is OpenAI message content: a plain string when it is one text
// part (what every chat template handles best), else the part list.
func userContent(parts []map[string]any) any {
	if len(parts) == 1 && parts[0]["type"] == "text" {
		return parts[0]["text"]
	}
	return parts
}

// anthropicToOpenAI translates an Anthropic Messages request into an OpenAI
// chat completions request.
func anthropicToOpenAI(body []byte) ([]byte, error) {
	var in antInRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("body is not an Anthropic Messages request: %v", err)
	}
	out := map[string]any{"model": in.Model}
	var msgs []map[string]any

	sys, err := blocksOf(in.System)
	if err != nil {
		return nil, fmt.Errorf("system: %v", err)
	}
	var sysText []string
	for _, b := range sys {
		if t := strings.TrimSpace(billingLine.ReplaceAllString(b.Text, "")); b.Type == "text" && t != "" {
			sysText = append(sysText, t)
		}
	}
	if len(sysText) > 0 {
		msgs = append(msgs, map[string]any{"role": "system", "content": strings.Join(sysText, "\n\n")})
	}

	for i, raw := range in.Messages {
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("messages[%d]: %v", i, err)
		}
		blocks, err := blocksOf(m.Content)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %v", i, err)
		}
		switch m.Role {
		case "assistant":
			var text []string
			var calls []map[string]any
			for _, b := range blocks {
				switch b.Type {
				case "text":
					text = append(text, b.Text)
				case "tool_use":
					args := string(b.Input)
					if args == "" || args == "null" {
						args = "{}"
					}
					calls = append(calls, map[string]any{"id": b.ID, "type": "function",
						"function": map[string]string{"name": b.Name, "arguments": args}})
				}
			}
			am := map[string]any{"role": "assistant", "content": strings.Join(text, "")}
			if len(calls) > 0 {
				am["tool_calls"] = calls
			}
			msgs = append(msgs, am)
		case "user":
			// Tool results first, each its own tool message; the rest of the
			// turn follows as one user message.
			var parts, toolImages []map[string]any
			for _, b := range blocks {
				if b.Type != "tool_result" {
					pt, err := oaiPartOf(b)
					if err != nil {
						return nil, fmt.Errorf("messages[%d]: %v", i, err)
					}
					if pt != nil {
						parts = append(parts, pt)
					}
					continue
				}
				inner, err := blocksOf(b.Content)
				if err != nil {
					return nil, fmt.Errorf("messages[%d] tool_result: %v", i, err)
				}
				var text []string
				for _, ib := range inner {
					switch ib.Type {
					case "text":
						text = append(text, ib.Text)
					case "image":
						if pt, err := oaiPartOf(ib); err == nil && pt != nil {
							toolImages = append(toolImages, pt)
						}
					}
				}
				content := strings.Join(text, "\n")
				if b.IsError && !strings.HasPrefix(content, "Error") {
					content = "Error: " + content
				}
				msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": b.ToolUseID, "content": content})
			}
			// An image a tool returned rides in the user turn: a tool message
			// carries text only in most chat templates.
			parts = append(toolImages, parts...)
			if len(parts) > 0 {
				msgs = append(msgs, map[string]any{"role": "user", "content": userContent(parts)})
			}
		case "system":
			// Claude Code sends its environment block as a system message
			// AFTER the first user turn. Most chat templates refuse a system
			// message that is not first (Qwen's raises), so it goes as a user
			// message in the same place: the prefix before it stays the same,
			// which is what a prompt cache keys on.
			var text []string
			for _, b := range blocks {
				if b.Type == "text" {
					text = append(text, b.Text)
				}
			}
			msgs = append(msgs, map[string]any{"role": "user", "content": strings.Join(text, "\n\n")})
		default:
			return nil, fmt.Errorf("messages[%d]: role %q is not user, assistant or system", i, m.Role)
		}
	}
	out["messages"] = msgs

	var tools []map[string]any
	for _, raw := range in.Tools {
		var t struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		}
		if json.Unmarshal(raw, &t) != nil || (t.Type != "" && t.Type != "custom") || len(t.InputSchema) == 0 {
			continue // a server tool (web_search_…) runs at Anthropic only
		}
		tools = append(tools, map[string]any{"type": "function",
			"function": map[string]any{"name": t.Name, "description": t.Description, "parameters": t.InputSchema}})
	}
	if len(tools) > 0 {
		out["tools"] = tools
		if tc := in.ToolChoice; tc != nil {
			switch tc.Type {
			case "auto":
				out["tool_choice"] = "auto"
			case "any":
				out["tool_choice"] = "required"
			case "none":
				out["tool_choice"] = "none"
			case "tool":
				out["tool_choice"] = map[string]any{"type": "function", "function": map[string]string{"name": tc.Name}}
			}
			if tc.DisableParallelToolUse {
				out["parallel_tool_calls"] = false
			}
		}
	}
	if in.MaxTokens != nil {
		out["max_tokens"] = *in.MaxTokens
	}
	if len(in.StopSequences) > 0 {
		out["stop"] = in.StopSequences
	}
	if in.Temperature != nil {
		out["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		out["top_p"] = *in.TopP
	}
	if in.TopK != nil {
		out["top_k"] = *in.TopK
	}
	if in.Stream {
		out["stream"] = true
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	return json.Marshal(out)
}

// stripCorrallmThinking removes the thinking blocks corrallm made (signed
// thinkingSigCorrallm) before a conversation goes back to Anthropic, which
// would refuse their signatures. When the LAST assistant turn loses one,
// thinking is switched off for this request: Anthropic requires that turn to
// keep its thinking while thinking is on, and the real one does not exist.
// The body is returned unchanged when there is nothing to strip, so the
// common case keeps its exact bytes.
func stripCorrallmThinking(body []byte) []byte {
	if !bytes.Contains(body, []byte(`"`+thinkingSigCorrallm+`"`)) {
		return body
	}
	var req map[string]json.RawMessage
	if json.Unmarshal(body, &req) != nil {
		return body
	}
	var msgs []map[string]json.RawMessage
	if json.Unmarshal(req["messages"], &msgs) != nil {
		return body
	}
	lastAssistant := -1
	for i, m := range msgs {
		if string(m["role"]) == `"assistant"` {
			lastAssistant = i
		}
	}
	changed, offLast := false, false
	for i, m := range msgs {
		var blocks []map[string]json.RawMessage
		if string(m["role"]) != `"assistant"` || json.Unmarshal(m["content"], &blocks) != nil {
			continue
		}
		kept := blocks[:0]
		for _, b := range blocks {
			if string(b["type"]) == `"thinking"` && string(b["signature"]) == `"`+thinkingSigCorrallm+`"` {
				changed = true
				offLast = offLast || i == lastAssistant
				continue
			}
			kept = append(kept, b)
		}
		if len(kept) == 0 {
			kept = append(kept, map[string]json.RawMessage{"type": json.RawMessage(`"text"`), "text": json.RawMessage(`"…"`)})
		}
		m["content"], _ = json.Marshal(kept)
	}
	if !changed {
		return body
	}
	req["messages"], _ = json.Marshal(msgs)
	if offLast {
		delete(req, "thinking")
	}
	out, err := json.Marshal(req)
	if err != nil {
		return body
	}
	return out
}

// --- reply: OpenAI → Anthropic ---

type oaiToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiReplyUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (u *oaiReplyUsage) anthropic() map[string]int {
	if u == nil {
		return map[string]int{"input_tokens": 0, "output_tokens": 0}
	}
	cached := u.PromptTokensDetails.CachedTokens
	return map[string]int{"input_tokens": max(u.PromptTokens-cached, 0), "output_tokens": u.CompletionTokens,
		"cache_read_input_tokens": cached, "cache_creation_input_tokens": 0}
}

// stopReason maps OpenAI's finish_reason to Anthropic's stop_reason.
func stopReason(finish string, sawTool bool) string {
	switch {
	case finish == "length":
		return "max_tokens"
	case finish == "tool_calls" || sawTool:
		return "tool_use"
	case finish == "content_filter":
		return "refusal"
	}
	return "end_turn"
}

var badToolID = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// toolID is an id Anthropic accepts: a translated model's ids can carry
// other characters, and the conversation may go back to Anthropic.
func toolID(id string) string {
	if id == "" {
		return "toolu_corrallm"
	}
	return badToolID.ReplaceAllString(id, "_")
}

func msgID(id string) string {
	if id == "" {
		return "msg_corrallm"
	}
	return "msg_" + badToolID.ReplaceAllString(strings.TrimPrefix(id, "chatcmpl-"), "_")
}

// openAIToAnthropic translates a non-streamed chat completion.
func openAIToAnthropic(body []byte, model string) ([]byte, error) {
	var r struct {
		ID      string `json:"id"`
		Choices []struct {
			Message struct {
				Content          string             `json:"content"`
				ReasoningContent string             `json:"reasoning_content"`
				ToolCalls        []oaiToolCallDelta `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *oaiReplyUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	content := []map[string]any{}
	finish := ""
	if len(r.Choices) > 0 {
		c := r.Choices[0]
		finish = c.FinishReason
		if c.Message.ReasoningContent != "" {
			content = append(content, map[string]any{"type": "thinking", "thinking": c.Message.ReasoningContent, "signature": thinkingSigCorrallm})
		}
		if c.Message.Content != "" {
			content = append(content, map[string]any{"type": "text", "text": c.Message.Content})
		}
		for _, tc := range c.Message.ToolCalls {
			input := json.RawMessage(tc.Function.Arguments)
			if !json.Valid(input) {
				input = json.RawMessage("{}")
			}
			content = append(content, map[string]any{"type": "tool_use", "id": toolID(tc.ID), "name": tc.Function.Name, "input": input})
		}
	}
	sawTool := len(r.Choices) > 0 && len(r.Choices[0].Message.ToolCalls) > 0
	return json.Marshal(map[string]any{
		"id": msgID(r.ID), "type": "message", "role": "assistant", "model": model, "content": content,
		"stop_reason": stopReason(finish, sawTool), "stop_sequence": nil, "usage": r.Usage.anthropic(),
	})
}

// messagesStream turns an OpenAI chat-completions SSE stream into Anthropic's
// event sequence: message_start, then per block content_block_start / _delta
// / _stop, then message_delta (stop_reason, usage) and message_stop.
type messagesStream struct {
	emit    func([]byte)
	model   string
	pending []byte
	started bool
	done    bool
	open    string // type of the open block, "" if none
	index   int    // the open block's index; next block is index+1
	tool    int    // the open tool_use's OpenAI index
	sawTool bool
	finish  string
	usage   *oaiReplyUsage
}

func (s *messagesStream) send(event string, v any) {
	b, _ := json.Marshal(v)
	s.emit([]byte("event: " + event + "\ndata: " + string(b) + "\n\n"))
}

func (s *messagesStream) feed(b []byte) {
	s.pending = append(s.pending, b...)
	for {
		i := bytes.IndexByte(s.pending, '\n')
		if i < 0 {
			return
		}
		line := bytes.TrimSpace(s.pending[:i])
		s.pending = s.pending[i+1:]
		if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			s.event(bytes.TrimSpace(data))
		}
	}
}

func (s *messagesStream) start(id string) {
	if s.started {
		return
	}
	s.started = true
	s.index = -1
	s.send("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": msgID(id), "type": "message", "role": "assistant", "model": s.model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0},
	}})
}

func (s *messagesStream) closeBlock() {
	if s.open == "" {
		return
	}
	if s.open == "thinking" {
		s.send("content_block_delta", map[string]any{"type": "content_block_delta", "index": s.index,
			"delta": map[string]string{"type": "signature_delta", "signature": thinkingSigCorrallm}})
	}
	s.send("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.index})
	s.open = ""
}

func (s *messagesStream) openBlock(typ string, block map[string]any) {
	s.closeBlock()
	s.index++
	s.open = typ
	s.send("content_block_start", map[string]any{"type": "content_block_start", "index": s.index, "content_block": block})
}

func (s *messagesStream) delta(d map[string]string) {
	s.send("content_block_delta", map[string]any{"type": "content_block_delta", "index": s.index, "delta": d})
}

func (s *messagesStream) event(data []byte) {
	if s.done || len(data) == 0 {
		return
	}
	if bytes.Equal(data, []byte("[DONE]")) {
		s.end()
		return
	}
	var c struct {
		ID      string `json:"id"`
		Choices []struct {
			Delta struct {
				Content          string             `json:"content"`
				ReasoningContent string             `json:"reasoning_content"`
				ToolCalls        []oaiToolCallDelta `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *oaiReplyUsage `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &c) != nil {
		return
	}
	s.start(c.ID)
	if c.Error != nil {
		s.send("error", map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": c.Error.Message}})
		s.done = true
		return
	}
	if c.Usage != nil {
		s.usage = c.Usage
	}
	for _, ch := range c.Choices {
		d := ch.Delta
		if d.ReasoningContent != "" {
			if s.open != "thinking" {
				s.openBlock("thinking", map[string]any{"type": "thinking", "thinking": ""})
			}
			s.delta(map[string]string{"type": "thinking_delta", "thinking": d.ReasoningContent})
		}
		if d.Content != "" {
			if s.open != "text" {
				s.openBlock("text", map[string]any{"type": "text", "text": ""})
			}
			s.delta(map[string]string{"type": "text_delta", "text": d.Content})
		}
		for _, tc := range d.ToolCalls {
			if s.open != "tool_use" || tc.Index != s.tool || (tc.ID != "" && tc.Function.Name != "") {
				s.openBlock("tool_use", map[string]any{"type": "tool_use", "id": toolID(tc.ID), "name": tc.Function.Name, "input": map[string]any{}})
				s.tool = tc.Index
				s.sawTool = true
			}
			if tc.Function.Arguments != "" {
				s.delta(map[string]string{"type": "input_json_delta", "partial_json": tc.Function.Arguments})
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			s.finish = *ch.FinishReason
		}
	}
}

// end closes the message. Safe to call more than once, and on a stream that
// ended without [DONE].
func (s *messagesStream) end() {
	if s.done {
		return
	}
	s.start("")
	s.closeBlock()
	s.send("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": stopReason(s.finish, s.sawTool), "stop_sequence": nil},
		"usage": s.usage.anthropic()})
	s.send("message_stop", map[string]string{"type": "message_stop"})
	s.done = true
}

// messagesWriter sits between the reverse proxy and statusCapture on the
// translated path, so the capture, the activity row and the caller all see
// Anthropic's shape. A 200 stream is translated as it arrives; a 200 JSON body
// is held and translated by finish(); anything else passes through for
// anthropicErrorWriter to restate.
type messagesWriter struct {
	http.ResponseWriter
	model  string
	status int
	mode   int // 0 undecided, 1 stream, 2 json, 3 passthrough
	held   []byte
	stream messagesStream
}

func newMessagesWriter(w http.ResponseWriter, model string) *messagesWriter {
	m := &messagesWriter{ResponseWriter: w, model: model}
	m.stream.model = model
	m.stream.emit = func(b []byte) { _, _ = m.ResponseWriter.Write(b) }
	return m
}

func (m *messagesWriter) WriteHeader(code int) {
	if m.mode != 0 {
		return
	}
	m.status = code
	h := m.Header()
	switch {
	case code != http.StatusOK:
		m.mode = 3
		m.ResponseWriter.WriteHeader(code)
	case strings.HasPrefix(h.Get("Content-Type"), "text/event-stream"):
		m.mode = 1
		h.Del("Content-Length")
		m.ResponseWriter.WriteHeader(code)
	default:
		m.mode = 2 // header written by finish, with the new length
	}
}

func (m *messagesWriter) Write(b []byte) (int, error) {
	if m.mode == 0 {
		m.WriteHeader(http.StatusOK)
	}
	switch m.mode {
	case 1:
		m.stream.feed(b)
		return len(b), nil
	case 2:
		m.held = append(m.held, b...)
		return len(b), nil
	}
	return m.ResponseWriter.Write(b)
}

func (m *messagesWriter) Flush() {
	if m.mode == 2 {
		return
	}
	if f, ok := m.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (m *messagesWriter) finish() {
	switch m.mode {
	case 1:
		if m.stream.started {
			m.stream.end()
		}
		m.Flush()
	case 2:
		out, err := openAIToAnthropic(m.held, m.model)
		h := m.Header()
		h.Del("Content-Length")
		if err != nil {
			h.Set("Content-Type", "application/json")
			m.ResponseWriter.WriteHeader(http.StatusBadGateway)
			_, _ = m.ResponseWriter.Write(anthropicErrorBody(http.StatusBadGateway, "backend reply is not a chat completion"))
			return
		}
		h.Set("Content-Type", "application/json")
		h.Set("Content-Length", strconv.Itoa(len(out)))
		m.ResponseWriter.WriteHeader(http.StatusOK)
		_, _ = m.ResponseWriter.Write(out)
	}
}

// --- usage in Anthropic's shape, for metering ---

// anthropicReplyUsage reads usage and stop_reason from an Anthropic reply,
// whole or streamed, for extractUsage and extractFinishReason: the passthrough
// and the translated path both leave the capture holding Anthropic's shape.
func anthropicReplyUsage(buf []byte, streaming bool) (usage, string, bool) {
	var au antUsage
	stop, found := "", false
	merge := func(u *antUsage) {
		if u == nil {
			return
		}
		found = true
		if u.InputTokens > 0 {
			au.InputTokens = u.InputTokens
		}
		if u.CacheReadInputTokens > 0 {
			au.CacheReadInputTokens = u.CacheReadInputTokens
		}
		if u.CacheCreationInputTokens > 0 {
			au.CacheCreationInputTokens = u.CacheCreationInputTokens
		}
		if u.OutputTokens > 0 {
			au.OutputTokens = u.OutputTokens
		}
	}
	if !streaming {
		var r struct {
			Type       string    `json:"type"`
			StopReason string    `json:"stop_reason"`
			Usage      *antUsage `json:"usage"`
		}
		if json.Unmarshal(buf, &r) != nil || r.Type != "message" {
			return usage{}, "", false
		}
		merge(r.Usage)
		stop = r.StopReason
	} else {
		for _, line := range bytes.Split(buf, []byte("\n")) {
			data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
			if !ok {
				continue
			}
			var e struct {
				Type    string `json:"type"`
				Message struct {
					Usage *antUsage `json:"usage"`
				} `json:"message"`
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
				Usage *antUsage `json:"usage"`
			}
			if json.Unmarshal(bytes.TrimSpace(data), &e) != nil {
				continue
			}
			switch e.Type {
			case "message_start":
				merge(e.Message.Usage)
			case "message_delta":
				merge(e.Usage)
				if e.Delta.StopReason != "" {
					stop = e.Delta.StopReason
				}
			}
		}
	}
	if !found {
		return usage{}, "", false
	}
	o := openAIUsage(au)
	var u usage
	u.PromptTokens, u.CompletionTokens = o.PromptTokens, o.CompletionTokens
	u.PromptTokensDetails.CachedTokens = o.PromptTokensDetails.CachedTokens
	u.CachedTokens = o.PromptTokensDetails.CachedTokens
	return u, finishReason(stop), true
}

// --- count_tokens for models Anthropic does not serve ---

// estimateAnthropicTokens is about 4 characters a token over the system
// prompt, the messages and the tool definitions.
func estimateAnthropicTokens(body []byte) int {
	var in antInRequest
	_ = json.Unmarshal(body, &in)
	chars := 0
	if sys, err := blocksOf(in.System); err == nil {
		for _, b := range sys {
			chars += len(b.Text)
		}
	}
	for _, raw := range in.Messages {
		var m struct {
			Content json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(raw, &m)
		chars += len(m.Content) // JSON of the blocks: close enough for text, rough for tool calls
	}
	for _, t := range in.Tools {
		chars += len(t)
	}
	return (chars+3)/4 + 4*len(in.Messages)
}

// statusOverloaded is Anthropic's 529: the API is overloaded. A refusal like
// 503, so the walk tries the next model.
const statusOverloaded = 529

// anthropicResetAsRetryAfter gives a 429 from a Claude subscription a
// Retry-After when it has none. Its limit resets hours later and says when in
// anthropic-ratelimit-unified-reset (Unix seconds); without this the backend
// cooled for the 1-minute fallback and every minute one request asked again.
func anthropicResetAsRetryAfter(h http.Header) {
	if h.Get("Retry-After") != "" {
		return
	}
	reset, err := strconv.ParseInt(h.Get("anthropic-ratelimit-unified-reset"), 10, 64)
	if err != nil {
		return
	}
	if secs := reset - time.Now().Unix(); secs > 0 {
		h.Set("Retry-After", strconv.FormatInt(secs, 10))
	}
}

// filterByCallerAuth drops a callerAuth candidate when the caller brought no
// Anthropic credential of its own. Tried anyway, Anthropic answers 401, and a
// free-tier backend is marked down on a 401 for EVERY caller: one caller with
// no Claude sign-in would switch Claude off for the one who has it.
func filterByCallerAuth(r *http.Request, cands []config.Candidate) []config.Candidate {
	has := r.URL.Path == "/v1/messages" && r.Header.Get("X-Corrallm-Key") != "" &&
		(r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "")
	if has {
		return cands
	}
	kept := cands[:0:0]
	for _, c := range cands {
		if t, err := c.Model.ProxyTarget(); err == nil && t != nil && t.CallerAuth {
			continue
		}
		kept = append(kept, c)
	}
	return kept
}
