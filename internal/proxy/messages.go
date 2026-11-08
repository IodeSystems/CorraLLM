package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	shim "github.com/iodesystems/openai-claude-api-shim"

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
// model's reasoning. It is not a valid Anthropic signature, on purpose. It must
// not change: live conversations hold blocks signed with it, and one that stops
// matching goes to Anthropic, which refuses the request.
const thinkingSigCorrallm = "corrallm"

// anthropicCredPrefix starts every Anthropic credential: API keys
// (sk-ant-api…) and Claude subscription sign-ins (sk-ant-oat…).
const anthropicCredPrefix = "sk-ant-"

// nativeMessagesKey marks an upstream request as /v1/messages passthrough, for
// the Director (callerAuth).
type nativeMessagesKey struct{}

func (p *Proxy) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	// An Anthropic credential (a subscription sign-in, an API key) is never a
	// corrallm key. Without X-Corrallm-Key, corrallm would read it AS the key
	// and store it in every activity row. Dropped: the caller is served as
	// unkeyed, and it is not forwarded (callerAuth needs X-Corrallm-Key).
	if r.Header.Get("X-Corrallm-Key") == "" {
		if strings.HasPrefix(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), anthropicCredPrefix) {
			r.Header.Del("Authorization")
		}
		if strings.HasPrefix(r.Header.Get("X-Api-Key"), anthropicCredPrefix) {
			r.Header.Del("X-Api-Key")
		}
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

func writeAnthropicError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(shim.ReverseError(code, msg))
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
		body = shim.ReverseError(e.code, msg)
	}
	h := e.Header()
	h.Set("Content-Type", "application/json")
	h.Del("Content-Length")
	e.ResponseWriter.WriteHeader(e.code)
	_, _ = e.ResponseWriter.Write(body)
}

// --- request: Anthropic → OpenAI ---

// messagesWriter sits between the chat-shape writers (loop, guard-out) and
// statusCapture on the translated path, so the capture, the activity row and
// the caller all see Anthropic's shape. A 200 stream is translated as it
// arrives; a 200 JSON body is held and translated by finish(); anything else
// passes through for anthropicErrorWriter to restate.
//
// A stream that loopWriter or guardOutWriter ended (finish_reason
// content_filter with a `loop` or `guardrail` object) ends with stop_reason
// "refusal", even when a tool call had started, and its message_delta
// carries the same object. A `loop` object on a stream that was not cut
// goes onto message_delta as well. With a loop writer on, a cut stream
// drops its tool_use blocks (holdTools).
type messagesWriter struct {
	http.ResponseWriter
	model  string
	status int
	mode   int // 0 undecided, 1 stream, 2 json, 3 passthrough
	held   []byte
	stream *shim.ReverseStream

	line  []byte                     // stream: the partial line, for notes
	notes map[string]json.RawMessage // corrallm's loop / guardrail objects
	cut   bool                       // a note came with finish_reason content_filter

	// holdTools: a loop writer watches the chat stream, so from the first
	// tool_use block on every event is held to message_delta, and a cut
	// drops them: the caller never gets a tool call with half its input. An
	// error event drops them too.
	holdTools bool
	holding   bool
	toolHeld  [][]byte
}

func newMessagesWriter(w http.ResponseWriter, model string) *messagesWriter {
	m := &messagesWriter{ResponseWriter: w, model: model}
	m.stream = shim.NewReverseStream(func(b []byte) {
		if b = m.hold(m.withNotes(b)); len(b) > 0 {
			_, _ = m.ResponseWriter.Write(b)
		}
	}, model, thinkingSigCorrallm)
	return m
}

// hold returns what of one translated event goes out now (see holdTools).
func (m *messagesWriter) hold(ev []byte) []byte {
	if !m.holdTools {
		return ev
	}
	e := parseMessagesEvent(ev)
	if !m.holding && e.Type == "content_block_start" && e.ContentBlock.Type == "tool_use" {
		m.holding = true
	}
	if !m.holding {
		return ev
	}
	if e.Type != "message_delta" && e.Type != "error" {
		m.toolHeld = append(m.toolHeld, ev)
		return nil
	}
	held := m.toolHeld
	m.toolHeld, m.holding = nil, false
	// An error ends the reply unfinished (a newer shim says so for a stream
	// with no finish_reason): it goes out alone, without the held call.
	if m.cut || e.Type == "error" {
		return ev
	}
	return append(bytes.Join(held, nil), ev...)
}

// note reads corrallm's loop and guardrail objects off the complete lines of
// a chat stream. Before the shim reads them: their line comes before [DONE].
func (m *messagesWriter) note(b []byte) {
	m.line = append(m.line, b...)
	i := bytes.LastIndexByte(m.line, '\n')
	if i < 0 {
		return
	}
	for _, line := range bytes.Split(m.line[:i], []byte("\n")) {
		data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
		if !ok || !(bytes.Contains(data, []byte(`"loop"`)) || bytes.Contains(data, []byte(`"guardrail"`))) {
			continue
		}
		var c struct {
			Choices []struct {
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Loop      json.RawMessage `json:"loop"`
			Guardrail json.RawMessage `json:"guardrail"`
		}
		if json.Unmarshal(bytes.TrimSpace(data), &c) != nil || (c.Loop == nil && c.Guardrail == nil) {
			continue
		}
		if m.notes == nil {
			m.notes = map[string]json.RawMessage{}
		}
		if c.Loop != nil {
			m.notes["loop"] = c.Loop
		}
		if c.Guardrail != nil {
			m.notes["guardrail"] = c.Guardrail
		}
		if len(c.Choices) > 0 && c.Choices[0].FinishReason == "content_filter" {
			m.cut = true
		}
	}
	m.line = append(m.line[:0], m.line[i+1:]...)
}

// withNotes adds the notes to the shim's message_delta event, and makes a cut
// stream's stop_reason "refusal": the shim says "tool_use" once a tool call
// started, and a client would run a call the cut left half-written.
func (m *messagesWriter) withNotes(b []byte) []byte {
	const prefix = "event: message_delta\ndata: "
	if m.notes == nil || !bytes.HasPrefix(b, []byte(prefix)) {
		return b
	}
	var ev map[string]json.RawMessage
	if json.Unmarshal(bytes.TrimSpace(b[len(prefix):]), &ev) != nil {
		return b
	}
	if m.cut {
		var d map[string]json.RawMessage
		if json.Unmarshal(ev["delta"], &d) == nil {
			d["stop_reason"] = json.RawMessage(`"refusal"`)
			ev["delta"], _ = json.Marshal(d)
		}
	}
	for k, v := range m.notes {
		ev[k] = v
	}
	out, err := json.Marshal(ev)
	if err != nil {
		return b
	}
	return append(append([]byte(prefix), out...), "\n\n"...)
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
		m.note(b)
		m.stream.Feed(b)
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
		// An error here means nothing was a chat completion. The 200 has gone,
		// so the stream ends empty, and the row says what the backend sent.
		_ = m.stream.End()
		m.Flush()
	case 2:
		out, err := shim.ReverseResponse(m.held, m.model, thinkingSigCorrallm)
		h := m.Header()
		h.Del("Content-Length")
		if err != nil {
			h.Set("Content-Type", "application/json")
			m.ResponseWriter.WriteHeader(http.StatusBadGateway)
			_, _ = m.ResponseWriter.Write(shim.ReverseError(http.StatusBadGateway, "backend reply is not a chat completion"))
			return
		}
		h.Set("Content-Type", "application/json")
		h.Set("Content-Length", strconv.Itoa(len(out)))
		m.ResponseWriter.WriteHeader(http.StatusOK)
		_, _ = m.ResponseWriter.Write(out)
	}
}

// --- usage in Anthropic's shape, for metering ---

// claudeReply reads usage, model and stop_reason from a Messages reply, whole or
// streamed.
func claudeReply(buf []byte, streaming bool) (au claudeUsage, model, stop string, ok bool) {
	found := false
	merge := func(u *claudeUsage) {
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
		if u.CacheCreation.Ephemeral1h > 0 {
			au.CacheCreation.Ephemeral1h = u.CacheCreation.Ephemeral1h
		}
		if u.OutputTokens > 0 {
			au.OutputTokens = u.OutputTokens
		}
	}
	if !streaming {
		var r struct {
			Type       string       `json:"type"`
			Model      string       `json:"model"`
			StopReason string       `json:"stop_reason"`
			Usage      *claudeUsage `json:"usage"`
		}
		if json.Unmarshal(buf, &r) != nil || r.Type != "message" {
			return au, "", "", false
		}
		merge(r.Usage)
		model, stop = r.Model, r.StopReason
	} else {
		for _, line := range bytes.Split(buf, []byte("\n")) {
			data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
			if !ok {
				continue
			}
			var e struct {
				Type    string `json:"type"`
				Message struct {
					Model string       `json:"model"`
					Usage *claudeUsage `json:"usage"`
				} `json:"message"`
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
				Usage *claudeUsage `json:"usage"`
			}
			if json.Unmarshal(bytes.TrimSpace(data), &e) != nil {
				continue
			}
			switch e.Type {
			case "message_start":
				model = e.Message.Model
				merge(e.Message.Usage)
			case "message_delta":
				merge(e.Usage)
				if e.Delta.StopReason != "" {
					stop = e.Delta.StopReason
				}
			}
		}
	}
	return au, model, stop, found
}

// anthropicReplyUsage reads usage and finish_reason from a Messages reply, for
// extractUsage and extractFinishReason: the passthrough and the translated path
// both leave the capture holding Anthropic's shape.
func anthropicReplyUsage(buf []byte, streaming bool) (usage, string, bool) {
	au, _, stop, ok := claudeReply(buf, streaming)
	if !ok {
		return usage{}, "", false
	}
	var u usage
	u.PromptTokens = au.InputTokens + au.CacheReadInputTokens + au.CacheCreationInputTokens
	u.CompletionTokens = au.OutputTokens
	u.PromptTokensDetails.CachedTokens = au.CacheReadInputTokens
	u.CachedTokens = au.CacheReadInputTokens
	return u, claudeStop(stop), true
}

// withClaudeEstimate prices the reply of a passthrough to Anthropic, which reports
// tokens and no money. Only that path: a model translated to from /v1/messages
// answers in the same shape, under whatever name the caller asked for, and a local
// model behind a Claude-named lane is not Claude.
func withClaudeEstimate(u usage, buf []byte, streaming bool, prices shim.Prices) usage {
	au, model, _, ok := claudeReply(buf, streaming)
	if !ok {
		return u
	}
	usd, ok := shim.Estimate(prices, model, shim.Usage{
		Input: au.InputTokens, Output: au.OutputTokens,
		CacheRead: au.CacheReadInputTokens, CacheWrite: au.CacheCreationInputTokens,
		CacheWrite1h: au.CacheCreation.Ephemeral1h,
	})
	if ok {
		u.Cost, u.CostEstimated = usd, true
	}
	return u
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
