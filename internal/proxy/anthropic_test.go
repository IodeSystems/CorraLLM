package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	shim "github.com/iodesystems/openai-claude-api-shim"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
	"gopkg.in/yaml.v3"
)

// oaiChunk and oaiCompletion read what the caller receives.
type oaiChunk struct {
	Object  string `json:"object"`
	Usage   *usage `json:"usage"`
	Choices []struct {
		Delta struct {
			Role             string  `json:"role"`
			Content          *string `json:"content"`
			ReasoningContent string  `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    *int   `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

type oaiCompletion struct {
	Object  string `json:"object"`
	Choices []struct {
		Message struct {
			Content *string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// anthropicSSE is a realistic stream: thinking, text, a tool call whose
// arguments arrive in pieces, a ping, and usage split across message_start
// and message_delta.
const anthropicSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_s","type":"message","role":"assistant","model":"claude-x","content":[],"stop_reason":null,"usage":{"input_tokens":12,"cache_creation_input_tokens":100,"cache_read_input_tokens":5000,"output_tokens":1}}}` + "\n\n" +
	"event: ping\ndata: {\"type\":\"ping\"}\n\n" +
	`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
	`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"let me see"}}` + "\n\n" +
	`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"abc"}}` + "\n\n" +
	`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n" +
	`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}` + "\n\n" +
	`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hello"}}` + "\n\n" +
	`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":" world"}}` + "\n\n" +
	`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":1}` + "\n\n" +
	`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_9","name":"wx","input":{}}}` + "\n\n" +
	`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":""}}` + "\n\n" +
	`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}` + "\n\n" +
	`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"Oslo\"}"}}` + "\n\n" +
	`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":2}` + "\n\n" +
	`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":57}}` + "\n\n" +
	`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n"

type parsedStream struct {
	role, content, reasoning string
	toolID, toolName, args   string
	finish                   string
	usage                    *usage
	done                     bool
	errBody                  string
}

func parseOpenAIStream(t *testing.T, b []byte) parsedStream {
	t.Helper()
	var p parsedStream
	for _, ev := range strings.Split(string(b), "\n\n") {
		ev = strings.TrimSpace(ev)
		if ev == "" {
			continue
		}
		data, ok := strings.CutPrefix(ev, "data: ")
		if !ok {
			t.Fatalf("event without data: %q", ev)
		}
		if p.done {
			t.Fatalf("event after [DONE]: %q", ev)
		}
		if data == "[DONE]" {
			p.done = true
			continue
		}
		if strings.HasPrefix(data, `{"error"`) {
			p.errBody = data
			continue
		}
		var c oaiChunk
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			t.Fatalf("chunk not JSON: %v: %s", err, data)
		}
		if c.Object != "chat.completion.chunk" {
			t.Errorf("object = %q", c.Object)
		}
		if c.Usage != nil {
			p.usage = c.Usage
		}
		for _, ch := range c.Choices {
			if ch.Delta.Role != "" {
				p.role = ch.Delta.Role
			}
			if ch.Delta.Content != nil {
				p.content += *ch.Delta.Content
			}
			p.reasoning += ch.Delta.ReasoningContent
			for _, tc := range ch.Delta.ToolCalls {
				if tc.Index == nil || *tc.Index != 0 {
					t.Errorf("tool call delta index = %v, want 0", tc.Index)
				}
				if tc.ID != "" {
					p.toolID, p.toolName = tc.ID, tc.Function.Name
				}
				p.args += tc.Function.Arguments
			}
			if ch.FinishReason != nil {
				p.finish = *ch.FinishReason
			}
		}
	}
	return p
}

type fakeAnthropic struct {
	srv     *httptest.Server
	path    string
	header  http.Header
	body    map[string]any
	status  int
	errBody string
}

func newFakeAnthropic(t *testing.T) *fakeAnthropic {
	f := &fakeAnthropic{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.path, f.header = r.URL.Path, r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		f.body = nil
		_ = json.Unmarshal(b, &f.body)
		if f.status != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(f.status)
			_, _ = io.WriteString(w, f.errBody)
			return
		}
		if stream, _ := f.body["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, ev := range strings.SplitAfter(anthropicSSE, "\n\n") {
				_, _ = io.WriteString(w, ev)
				w.(http.Flusher).Flush()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-up",
			"content":[{"type":"text","text":"hi there"}],"stop_reason":"end_turn",
			"usage":{"input_tokens":10,"cache_creation_input_tokens":20,"cache_read_input_tokens":3000,"output_tokens":5}}`)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func anthropicRig(t *testing.T, f *fakeAnthropic) (*chi.Mux, *store.Store) {
	t.Helper()
	u, _ := url.Parse(f.srv.URL)
	var doc yaml.Node
	src := fmt.Sprintf("host: %s\nport: %s\napi: anthropic\ncacheTTL: 1h\nmodel: claude-up\nheaders:\n  x-api-key: test-key\n",
		u.Hostname(), u.Port())
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr := proc.NewManager(&config.Config{})
	t.Cleanup(mgr.Shutdown)
	cfg := &config.Config{
		Models: map[string]config.Model{"claude": {Proxy: *doc.Content[0], Type: "anthropic-test"}},
		CommandCosts: map[string]map[string]any{"anthropic-test": {"extract": map[string]any{
			"processCostFactor": 1e-6, "cachedCostFactor": 1e-7, "generateCostFactor": 5e-6,
		}}},
		PriorityGroups: map[string]config.PriorityGroup{"default": {Weight: 1}},
		Keys:           map[string]config.KeyPolicy{"k": {Group: "default"}},
	}
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)
	return r, st
}

const anthropicChatBody = `{"model":"claude","messages":[
	{"role":"system","content":"sys"},
	{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"f"}}]%s}`

func postAnthropicChat(r *chi.Mux, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// checkUpstream asserts what reached the fake Anthropic: the native path, the
// configured key and not the caller's, a version, and a translated body with
// the upstream model id and breakpoints.
func checkUpstream(t *testing.T, f *fakeAnthropic) {
	t.Helper()
	if f.path != "/v1/messages" {
		t.Errorf("upstream path = %q, want /v1/messages", f.path)
	}
	if f.header.Get("x-api-key") != "test-key" || f.header.Get("Authorization") != "" ||
		f.header.Get("anthropic-version") != shim.APIVersion {
		t.Errorf("upstream headers: x-api-key=%q authorization=%q version=%q",
			f.header.Get("x-api-key"), f.header.Get("Authorization"), f.header.Get("anthropic-version"))
	}
	if f.body["model"] != "claude-up" || f.body["max_tokens"] == nil || f.body["messages"] == nil {
		t.Errorf("upstream body = %v", f.body)
	}
	if b, _ := json.Marshal(f.body); strings.Count(string(b), `"ttl":"1h"`) != 3 {
		t.Errorf("want 3 one-hour breakpoints (tool, system, user turn): %s", b)
	}
}

func TestAnthropicProxyNonStreaming(t *testing.T) {
	f := newFakeAnthropic(t)
	r, st := anthropicRig(t, f)
	rec := postAnthropicChat(r, fmt.Sprintf(anthropicChatBody, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	checkUpstream(t, f)
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
	var c oaiCompletion
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatalf("reply is not JSON: %v: %s", err, rec.Body)
	}
	if c.Object != "chat.completion" || *c.Choices[0].Message.Content != "hi there" || c.Choices[0].FinishReason != "stop" {
		t.Errorf("reply = %s", rec.Body)
	}
	row := onlyRow(t, st)
	if row.PromptTokens != 3030 || row.CachedTokens != 3000 || row.CompletionTokens != 5 || row.FinishReason != "stop" {
		t.Errorf("activity row: prompt=%d cached=%d completion=%d finish=%q",
			row.PromptTokens, row.CachedTokens, row.CompletionTokens, row.FinishReason)
	}
	// 3000 cached at 1e-7, 30 processed at 1e-6, 5 generated at 5e-6.
	if want := 3000*1e-7 + 30*1e-6 + 5*5e-6; math.Abs(row.CostUSD-want) > 1e-12 {
		t.Errorf("cost = %g, want %g (cache reads priced as cached)", row.CostUSD, want)
	}
}

func TestAnthropicProxyStreaming(t *testing.T) {
	f := newFakeAnthropic(t)
	r, st := anthropicRig(t, f)
	rec := postAnthropicChat(r, fmt.Sprintf(anthropicChatBody, `,"stream":true`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	checkUpstream(t, f)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content-type = %q", ct)
	}
	p := parseOpenAIStream(t, rec.Body.Bytes())
	if p.content != "Hello world" || p.args != `{"city":"Oslo"}` || p.finish != "tool_calls" || !p.done {
		t.Errorf("stream = %+v", p)
	}
	row := onlyRow(t, st)
	if row.PromptTokens != 5112 || row.CachedTokens != 5000 || row.CompletionTokens != 57 || row.FinishReason != "tool_calls" {
		t.Errorf("activity row: prompt=%d cached=%d completion=%d finish=%q",
			row.PromptTokens, row.CachedTokens, row.CompletionTokens, row.FinishReason)
	}
}

func TestAnthropicProxyErrorKeepsStatusAndRetryAfter(t *testing.T) {
	f := newFakeAnthropic(t)
	f.status = http.StatusTooManyRequests
	f.errBody = `{"type":"error","error":{"type":"rate_limit_error","message":"Number of request tokens has exceeded your rate limit"}}`
	r, st := anthropicRig(t, f)
	rec := postAnthropicChat(r, fmt.Sprintf(anthropicChatBody, `,"stream":true`))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "7" {
		t.Errorf("status=%d retry-after=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
	var e struct {
		Error struct{ Message, Type string } `json:"error"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &e) != nil || e.Error.Type != "rate_limit_error" || !strings.Contains(e.Error.Message, "rate limit") {
		t.Errorf("error body = %s", rec.Body)
	}
	if row := onlyRow(t, st); row.Status != http.StatusTooManyRequests {
		t.Errorf("row status = %d", row.Status)
	}
}

func TestAnthropicProxyUntranslatableIs400(t *testing.T) {
	f := newFakeAnthropic(t)
	r, st := anthropicRig(t, f)
	rec := postAnthropicChat(r, `{"model":"claude","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{}}]}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if f.path != "" {
		t.Errorf("an untranslatable request must not reach the upstream")
	}
	if row := onlyRow(t, st); row.Status != http.StatusBadRequest || !strings.Contains(row.Error, "input_audio") {
		t.Errorf("row = %d %q", row.Status, row.Error)
	}
}

// TestAnthropicUsageFieldNonStreaming: the caller's usage carries the full
// Anthropic usage in an anthropic_usage field, so a meter that prices cache
// writes (5m/1h) and thinking tokens can read them from the OpenAI response.
func TestAnthropicUsageFieldNonStreaming(t *testing.T) {
	f := newFakeAnthropic(t)
	r, _ := anthropicRig(t, f)
	rec := postAnthropicChat(r, fmt.Sprintf(anthropicChatBody, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var resp struct {
		Usage struct {
			AnthropicUsage *struct {
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
			} `json:"anthropic_usage"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("reply is not JSON: %v: %s", err, rec.Body)
	}
	au := resp.Usage.AnthropicUsage
	if au == nil {
		t.Fatalf("anthropic_usage missing from usage: %s", rec.Body)
	}
	// The fake Anthropic returns input_tokens=10, cache_creation_input_tokens=20,
	// cache_read_input_tokens=3000, output_tokens=5.
	if au.InputTokens != 10 {
		t.Errorf("input_tokens = %d, want 10", au.InputTokens)
	}
	if au.CacheCreationInputTokens != 20 {
		t.Errorf("cache_creation_input_tokens = %d, want 20", au.CacheCreationInputTokens)
	}
	if au.CacheReadInputTokens != 3000 {
		t.Errorf("cache_read_input_tokens = %d, want 3000", au.CacheReadInputTokens)
	}
	if au.OutputTokens != 5 {
		t.Errorf("output_tokens = %d, want 5", au.OutputTokens)
	}
	// Zero-value fields are present in the struct but must be 0 (no thinking,
	// no 5m/1h cache creation in the fake).
	if au.CacheCreation.Ephemeral5m != 0 || au.CacheCreation.Ephemeral1h != 0 {
		t.Errorf("cache_creation = %+v, want zeros", au.CacheCreation)
	}
	if au.OutputTokensDetails.ThinkingTokens != 0 {
		t.Errorf("thinking_tokens = %d, want 0", au.OutputTokensDetails.ThinkingTokens)
	}
}

// TestAnthropicUsageFieldStreaming: the usage chunk of an SSE stream carries
// anthropic_usage with the merged message_start + message_delta values.
func TestAnthropicUsageFieldStreaming(t *testing.T) {
	f := newFakeAnthropic(t)
	r, _ := anthropicRig(t, f)
	rec := postAnthropicChat(r, fmt.Sprintf(anthropicChatBody, `,"stream":true`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	// Find the SSE data line that carries usage.
	var au *struct {
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
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.Contains(line, `"usage"`) {
			continue
		}
		data, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
		if !ok {
			continue
		}
		var chunk struct {
			Usage struct {
				AnthropicUsage *struct {
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
				} `json:"anthropic_usage"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if chunk.Usage.AnthropicUsage != nil {
			au = chunk.Usage.AnthropicUsage
		}
	}
	if au == nil {
		t.Fatalf("anthropic_usage missing from stream usage: %s", rec.Body)
	}
	// message_start: input=12, cache_creation=100, cache_read=5000, output=1
	// message_delta: output=57
	// Merged: input=12, cache_creation=100, cache_read=5000, output=57
	if au.InputTokens != 12 {
		t.Errorf("input_tokens = %d, want 12", au.InputTokens)
	}
	if au.CacheCreationInputTokens != 100 {
		t.Errorf("cache_creation_input_tokens = %d, want 100", au.CacheCreationInputTokens)
	}
	if au.CacheReadInputTokens != 5000 {
		t.Errorf("cache_read_input_tokens = %d, want 5000", au.CacheReadInputTokens)
	}
	if au.OutputTokens != 57 {
		t.Errorf("output_tokens = %d, want 57", au.OutputTokens)
	}
}

// TestAnthropicUsageFieldAbsentOnLocalBackend: a local (non-Anthropic)
// backend's usage has no anthropic_usage field.
func TestAnthropicUsageFieldAbsentOnLocalBackend(t *testing.T) {
	rec, _ := costCase(t, nil, `{"model":"mock"}`, "application/json", 200,
		`{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	var r struct {
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("body not JSON: %v: %s", err, rec.Body.String())
	}
	if _, ok := r.Usage["anthropic_usage"]; ok {
		t.Errorf("anthropic_usage present on a local backend: %s", rec.Body.String())
	}
}
