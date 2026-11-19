package proxy

import (
	"bytes"
	"cmp"
	"encoding/json"
	"net/http"
	"strings"

	shim "github.com/iodesystems/openai-claude-api-shim"

	"github.com/iodesystems/corrallm/internal/config"
)

// Anthropic Messages shim.
//
// A backend whose proxy declares `api: anthropic` is reached through
// Anthropic's NATIVE Messages API (POST /v1/messages) while the caller keeps
// speaking OpenAI /v1/chat/completions. The request is translated on the way
// out, the reply (JSON or SSE) on the way back.
//
// It exists for prompt caching. Anthropic's OpenAI-compatible layer accepts the
// OpenAI shape but has no cache_control, so an agentic caller that resends a
// 100k-token conversation every turn paid full input price for all of it, every
// turn. Through the native API corrallm places the breakpoints itself (see
// placeCacheBreakpoints), and the cache read comes back as
// prompt_tokens_details.cached_tokens, which extractUsage and the cost model
// already understand.
//
// The translation is openai-claude-api-shim's. This file is the HTTP around it.
//

// anthropicDefaultMaxTokens is the max_tokens a request gets when neither the
// caller nor the model set one. Anthropic requires the field. Kept here rather
// than taking the shim's own default: this one is what a paid request is sized by.
const anthropicDefaultMaxTokens = 16000

// claudeRequest is the shim's translation, with this backend's settings.
func claudeRequest(body []byte, backend config.Model, cacheTTL string) ([]byte, error) {
	return shim.Request(body, shim.Options{
		MaxTokens: cmp.Or(backend.MaxTokens, anthropicDefaultMaxTokens), CacheTTL: cacheTTL,
	})
}

// anthropicWriter sits between the reverse proxy and statusCapture and turns
// Anthropic's reply into OpenAI's, so that everything downstream — usage
// metering, finish_reason, the activity row's body — sees the OpenAI shape the
// caller sees.
//
// A 2xx event stream is translated event by event as it arrives. Anything else
// (a JSON reply, an error) is buffered whole and translated by finish(), which
// the handler calls once the proxy returns. Status and headers pass through
// untouched apart from Content-Length (the body changes size), so a 429's
// Retry-After reaches the caller and the quota code exactly as before.
type anthropicWriter struct {
	http.ResponseWriter
	wroteHeader  bool
	status       int
	streaming    bool
	buf          []byte
	opts         shim.StreamOptions
	stream       *shim.Stream
	claudeUsage  claudeUsage
	usageFound   bool
	// sseUsageBuf accumulates the raw SSE lines that carry usage
	// (message_start, message_delta) so the full Anthropic usage is
	// available after the stream ends. Bounded: usage events are small.
	sseUsageBuf []byte
}

func newAnthropicWriter(w http.ResponseWriter, prices shim.Prices) *anthropicWriter {
	a := &anthropicWriter{ResponseWriter: w, opts: shim.StreamOptions{Prices: prices}}
	a.stream = shim.NewStream(func(b []byte) { _, _ = a.ResponseWriter.Write(b) }, a.opts)
	return a
}

func (a *anthropicWriter) WriteHeader(code int) {
	if code >= 100 && code < 200 {
		a.ResponseWriter.WriteHeader(code)
		return
	}
	if a.wroteHeader {
		return
	}
	a.wroteHeader, a.status = true, code
	h := a.Header()
	h.Del("Content-Length")
	if code >= 200 && code < 300 && strings.HasPrefix(h.Get("Content-Type"), "text/event-stream") {
		a.streaming = true
	} else {
		h.Set("Content-Type", "application/json")
	}
	a.ResponseWriter.WriteHeader(code)
}

func (a *anthropicWriter) Write(b []byte) (int, error) {
	if !a.wroteHeader {
		a.WriteHeader(http.StatusOK)
	}
	if a.streaming {
		a.streamUsage(b)
		a.stream.Feed(b)
	} else {
		a.buf = append(a.buf, b...)
	}
	return len(b), nil
}

// streamUsage accumulates the raw SSE lines that carry usage (message_start,
// message_delta) so the full Anthropic usage is available after the stream
// ends. These events appear once each and are small; content deltas are
// skipped to keep the buffer bounded. It parses immediately so the usage is
// available by the time the translated usage chunk reaches the costWriter.
func (a *anthropicWriter) streamUsage(b []byte) {
	if bytes.Contains(b, []byte("message_start")) || bytes.Contains(b, []byte("message_delta")) {
		a.sseUsageBuf = append(a.sseUsageBuf, b...)
		a.parseUsageFromSSE()
	}
}

func (a *anthropicWriter) Flush() {
	if f, ok := a.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// finish releases a buffered reply, translated, or closes out a stream that
// ended without message_stop.
func (a *anthropicWriter) finish() {
	if a.streaming {
		a.parseUsageFromSSE()
		a.stream.Finish()
		return
	}
	if len(a.buf) == 0 {
		return
	}
	a.parseUsageFromJSON()
	var out []byte
	if a.status >= 200 && a.status < 300 {
		if b, err := shim.Response(a.buf, a.opts); err == nil {
			out = b
		}
	}
	if out == nil {
		out = shim.Error(a.buf)
	}
	_, _ = a.ResponseWriter.Write(out)
}

// claudeUsageJSON returns the full Anthropic usage object as JSON, or nil if
// nothing was measured. Callers read this after the response is complete.
func (a *anthropicWriter) claudeUsageJSON() []byte {
	if !a.usageFound {
		return nil
	}
	return a.claudeUsage.usageJSON()
}

// parseUsageFromJSON extracts the usage object from a non-streamed Messages
// reply body.
func (a *anthropicWriter) parseUsageFromJSON() {
	var r struct {
		Type  string       `json:"type"`
		Usage *claudeUsage `json:"usage"`
	}
	if json.Unmarshal(a.buf, &r) != nil || r.Type != "message" || r.Usage == nil {
		return
	}
	mergeClaudeUsage(&a.claudeUsage, r.Usage)
	a.usageFound = true
}

// parseUsageFromSSE extracts the usage object from the accumulated SSE usage
// lines.
func (a *anthropicWriter) parseUsageFromSSE() {
	if len(a.sseUsageBuf) == 0 {
		return
	}
	for _, line := range bytes.Split(a.sseUsageBuf, []byte("\n")) {
		data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
		if !ok {
			continue
		}
		var e struct {
			Type    string `json:"type"`
			Message struct {
				Usage *claudeUsage `json:"usage"`
			} `json:"message"`
			Usage *claudeUsage `json:"usage"`
		}
		if json.Unmarshal(bytes.TrimSpace(data), &e) != nil {
			continue
		}
		switch e.Type {
		case "message_start":
			if e.Message.Usage != nil {
				mergeClaudeUsage(&a.claudeUsage, e.Message.Usage)
				a.usageFound = true
			}
		case "message_delta":
			if e.Usage != nil {
				mergeClaudeUsage(&a.claudeUsage, e.Usage)
				a.usageFound = true
			}
		}
	}
}

// claudeUsage is the usage object of a Messages reply.
type claudeUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreation            struct {
		Ephemeral5m int `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
	OutputTokensDetails struct {
		ThinkingTokens int `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

// usageJSON marshals the full Anthropic usage object for the extra field in the
// OpenAI response. Returns nil when nothing was measured.
func (u claudeUsage) usageJSON() []byte {
	if u.InputTokens == 0 && u.OutputTokens == 0 &&
		u.CacheCreationInputTokens == 0 && u.CacheReadInputTokens == 0 {
		return nil
	}
	b, _ := json.Marshal(u)
	return b
}

// mergeClaudeUsage folds src into dst, taking the non-zero value from each
// field (Anthropic repeats input fields across message_start and message_delta).
func mergeClaudeUsage(dst *claudeUsage, src *claudeUsage) {
	if src == nil {
		return
	}
	if src.InputTokens > 0 {
		dst.InputTokens = src.InputTokens
	}
	if src.CacheReadInputTokens > 0 {
		dst.CacheReadInputTokens = src.CacheReadInputTokens
	}
	if src.CacheCreationInputTokens > 0 {
		dst.CacheCreationInputTokens = src.CacheCreationInputTokens
	}
	if src.CacheCreation.Ephemeral5m > 0 {
		dst.CacheCreation.Ephemeral5m = src.CacheCreation.Ephemeral5m
	}
	if src.CacheCreation.Ephemeral1h > 0 {
		dst.CacheCreation.Ephemeral1h = src.CacheCreation.Ephemeral1h
	}
	if src.OutputTokensDetails.ThinkingTokens > 0 {
		dst.OutputTokensDetails.ThinkingTokens = src.OutputTokensDetails.ThinkingTokens
	}
	if src.OutputTokens > 0 {
		dst.OutputTokens = src.OutputTokens
	}
}

// claudeStop maps a Messages stop_reason onto finish_reason.
func claudeStop(stop string) string {
	switch stop {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	case "":
		return ""
	}
	return "stop"
}
