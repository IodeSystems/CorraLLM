package proxy

import (
	"cmp"
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
	wroteHeader bool
	status      int
	streaming   bool
	buf         []byte
	opts        shim.StreamOptions
	stream      *shim.Stream
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
		a.stream.Feed(b)
	} else {
		a.buf = append(a.buf, b...)
	}
	return len(b), nil
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
		a.stream.Finish()
		return
	}
	if len(a.buf) == 0 {
		return
	}
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

// claudeUsage is the usage object of a Messages reply.
type claudeUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreation            struct {
		Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
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
