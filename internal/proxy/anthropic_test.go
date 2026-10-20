package proxy

import (
	"bytes"
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
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
	"gopkg.in/yaml.v3"
)

// translate runs toAnthropicRequest and decodes the result for inspection.
func translate(t *testing.T, body string, opts anthropicOpts) antRequest {
	t.Helper()
	out, err := toAnthropicRequest([]byte(body), opts)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var req antRequest
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("translated body is not JSON: %v\n%s", err, out)
	}
	return req
}

func TestAnthropicRequestBasics(t *testing.T) {
	req := translate(t, `{
		"model":"claude-x","stream":true,"temperature":0.2,"top_p":0.9,
		"stop":["END","  \n"],"n":2,"presence_penalty":1,"response_format":{"type":"json_object"},
		"stream_options":{"include_usage":true},"min_p":0.05,
		"messages":[
			{"role":"system","content":"be terse"},
			{"role":"developer","content":[{"type":"text","text":"and kind"}]},
			{"role":"user","content":"hi"}
		]}`, anthropicOpts{})

	if req.Model != "claude-x" || !req.Stream {
		t.Errorf("model/stream = %q/%v", req.Model, req.Stream)
	}
	if req.MaxTokens != anthropicDefaultMaxTokens {
		t.Errorf("max_tokens = %d, want the default %d when nothing is set", req.MaxTokens, anthropicDefaultMaxTokens)
	}
	if req.Temperature == nil || *req.Temperature != 0.2 || req.TopP == nil || *req.TopP != 0.9 {
		t.Errorf("sampling not carried: temp=%v top_p=%v", req.Temperature, req.TopP)
	}
	if len(req.StopSequences) != 1 || req.StopSequences[0] != "END" {
		t.Errorf("stop_sequences = %q, want [END] (whitespace-only dropped)", req.StopSequences)
	}
	if len(req.System) != 2 || req.System[0].Text != "be terse" || req.System[1].Text != "and kind" {
		t.Errorf("system = %+v", req.System)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" || req.Messages[0].Content[0].Text != "hi" {
		t.Errorf("messages = %+v", req.Messages)
	}

	// Dropped fields must not reach Anthropic at all.
	raw, _ := toAnthropicRequest([]byte(`{"model":"m","n":2,"seed":1,"user":"u","logit_bias":{},
		"messages":[{"role":"user","content":"x"}]}`), anthropicOpts{})
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	for _, k := range []string{"n", "seed", "user", "logit_bias", "stream_options", "response_format", "min_p"} {
		if _, ok := m[k]; ok {
			t.Errorf("field %q leaked into the Anthropic body", k)
		}
	}
}

func TestAnthropicRequestMaxTokens(t *testing.T) {
	cases := []struct {
		name string
		body string
		opts anthropicOpts
		want int
	}{
		{"default constant", `{}`, anthropicOpts{}, anthropicDefaultMaxTokens},
		{"model cap fallback", `{}`, anthropicOpts{MaxTokens: 777}, 777},
		{"max_tokens", `{"max_tokens":100}`, anthropicOpts{MaxTokens: 777}, 100},
		{"max_completion_tokens wins", `{"max_tokens":100,"max_completion_tokens":200}`, anthropicOpts{}, 200},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := strings.Replace(c.body, "{", `{"model":"m","messages":[{"role":"user","content":"x"}],`, 1)
			body = strings.Replace(body, ",}", "}", 1)
			if got := translate(t, body, c.opts).MaxTokens; got != c.want {
				t.Errorf("max_tokens = %d, want %d", got, c.want)
			}
		})
	}
}

// A full tool round trip: the assistant's tool_calls become tool_use blocks,
// the tool replies become ONE user turn of tool_result blocks, and the user's
// follow-up merges into that same turn after them.
func TestAnthropicRequestToolRoundTrip(t *testing.T) {
	req := translate(t, `{"model":"m","messages":[
		{"role":"user","content":"weather in two cities"},
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"wx","arguments":"{\"city\":\"Oslo\"}"}},
			{"id":"call_2","type":"function","function":{"name":"wx","arguments":"{\"city\":"}}
		]},
		{"role":"tool","tool_call_id":"call_1","content":"cold"},
		{"role":"tool","tool_call_id":"call_2","content":[{"type":"text","text":"bad args"}]},
		{"role":"user","content":"thanks"}
	],"tools":[{"type":"function","function":{"name":"wx","description":"weather",
		"parameters":{"type":"object","properties":{"city":{"type":"string"}}}}},
		{"type":"function","function":{"name":"noargs"}}]}`, anthropicOpts{})

	if len(req.Messages) != 3 {
		t.Fatalf("want user/assistant/user, got %d messages: %+v", len(req.Messages), req.Messages)
	}
	a := req.Messages[1]
	if a.Role != "assistant" || len(a.Content) != 2 {
		t.Fatalf("assistant turn = %+v", a)
	}
	if a.Content[0].Type != "tool_use" || a.Content[0].ID != "call_1" || a.Content[0].Name != "wx" ||
		string(a.Content[0].Input) != `{"city":"Oslo"}` {
		t.Errorf("tool_use 1 = %+v", a.Content[0])
	}
	if string(a.Content[1].Input) != `{}` {
		t.Errorf("truncated arguments must replay as {}, got %s", a.Content[1].Input)
	}
	u := req.Messages[2]
	if u.Role != "user" || len(u.Content) != 3 {
		t.Fatalf("merged user turn = %+v", u)
	}
	if u.Content[0].Type != "tool_result" || u.Content[0].ToolUseID != "call_1" || u.Content[0].Content[0].Text != "cold" ||
		u.Content[1].Type != "tool_result" || u.Content[1].ToolUseID != "call_2" ||
		u.Content[2].Type != "text" || u.Content[2].Text != "thanks" {
		t.Errorf("merged user turn content = %+v", u.Content)
	}
	if len(req.Tools) != 2 || req.Tools[0].Name != "wx" || req.Tools[0].Description != "weather" ||
		!strings.Contains(string(req.Tools[0].InputSchema), `"city"`) {
		t.Errorf("tools = %+v", req.Tools)
	}
	if string(req.Tools[1].InputSchema) != `{"type":"object","properties":{}}` {
		t.Errorf("a tool with no parameters needs an empty object schema, got %s", req.Tools[1].InputSchema)
	}
}

func TestAnthropicRequestRoleMerging(t *testing.T) {
	req := translate(t, `{"model":"m","messages":[
		{"role":"user","content":"a"},
		{"role":"user","content":[{"type":"text","text":"b"},{"type":"text","text":""}]},
		{"role":"assistant","content":""},
		{"role":"user","content":"c"},
		{"role":"assistant","content":"d"},
		{"role":"assistant","content":[{"type":"text","text":"e"}]},
		{"role":"tool","tool_call_id":"x","content":"r"},
		{"role":"user","content":"f"}
	]}`, anthropicOpts{})
	var got []string
	for _, m := range req.Messages {
		var parts []string
		for _, b := range m.Content {
			if b.Type == "tool_result" {
				parts = append(parts, "result")
			} else {
				parts = append(parts, b.Text)
			}
		}
		got = append(got, m.Role+":"+strings.Join(parts, ","))
	}
	want := []string{"user:a,b,c", "assistant:d,e", "user:result,f"}
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Errorf("merged = %q, want %q (empty assistant dropped, empty text skipped)", got, want)
	}
}

func TestAnthropicRequestImages(t *testing.T) {
	req := translate(t, `{"model":"m","messages":[{"role":"user","content":[
		{"type":"text","text":"what is this"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}},
		{"type":"image_url","image_url":{"url":"https://example.com/cat.jpg","detail":"high"}}
	]}]}`, anthropicOpts{})
	c := req.Messages[0].Content
	if len(c) != 3 {
		t.Fatalf("content = %+v", c)
	}
	if s := c[1].Source; c[1].Type != "image" || s == nil || s.Type != "base64" || s.MediaType != "image/png" || s.Data != "iVBORw0KGgo=" {
		t.Errorf("data URL image = %+v %+v", c[1], c[1].Source)
	}
	if s := c[2].Source; s == nil || s.Type != "url" || s.URL != "https://example.com/cat.jpg" {
		t.Errorf("https image = %+v", c[2].Source)
	}
}

func TestAnthropicRequestRefusals(t *testing.T) {
	cases := map[string]string{
		"audio part":        `{"model":"m","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{}}]}]}`,
		"unknown role":      `{"model":"m","messages":[{"role":"function","content":"x"}]}`,
		"non-base64 data":   `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png,xyz"}}]}]}`,
		"ftp image":         `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"ftp://x/y.png"}}]}]}`,
		"image in system":   `{"model":"m","messages":[{"role":"system","content":[{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}]}`,
		"bad tool_choice":   `{"model":"m","tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":"sometimes","messages":[]}`,
		"not json":          `nope`,
		"non-function tool": `{"model":"m","tools":[{"type":"web_search"}],"messages":[]}`,
	}
	for name, body := range cases {
		if _, err := toAnthropicRequest([]byte(body), anthropicOpts{}); err == nil {
			t.Errorf("%s: want an error, got none", name)
		}
	}
}

func TestAnthropicToolChoice(t *testing.T) {
	tools := `"tools":[{"type":"function","function":{"name":"f"}}],`
	cases := []struct {
		name  string
		extra string
		tools bool
		want  string // JSON of tool_choice, "" for absent
	}{
		{"absent", ``, true, ``},
		{"auto", `"tool_choice":"auto",`, true, `{"type":"auto"}`},
		{"none", `"tool_choice":"none",`, true, `{"type":"none"}`},
		{"required", `"tool_choice":"required",`, true, `{"type":"any"}`},
		{"named", `"tool_choice":{"type":"function","function":{"name":"f"}},`, true, `{"type":"tool","name":"f"}`},
		{"parallel off alone", `"parallel_tool_calls":false,`, true, `{"type":"auto","disable_parallel_tool_use":true}`},
		{"parallel off required", `"tool_choice":"required","parallel_tool_calls":false,`, true, `{"type":"any","disable_parallel_tool_use":true}`},
		{"parallel off none", `"tool_choice":"none","parallel_tool_calls":false,`, true, `{"type":"none"}`},
		{"no tools drops it", `"tool_choice":"required",`, false, ``},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := `{"model":"m",` + c.extra
			if c.tools {
				body += tools
			}
			body += `"messages":[{"role":"user","content":"x"}]}`
			req := translate(t, body, anthropicOpts{})
			got := ""
			if req.ToolChoice != nil {
				b, _ := json.Marshal(req.ToolChoice)
				got = string(b)
			}
			if got != c.want {
				t.Errorf("tool_choice = %s, want %s", got, c.want)
			}
		})
	}
}

// markers lists where cache_control landed, as "tools[i]", "system[i]" and
// "msg[i].content[j]".
func markers(req antRequest) []string {
	var out []string
	for i, t := range req.Tools {
		if len(t.CacheControl) > 0 {
			out = append(out, fmt.Sprintf("tools[%d]", i))
		}
	}
	for i, b := range req.System {
		if len(b.CacheControl) > 0 {
			out = append(out, fmt.Sprintf("system[%d]", i))
		}
	}
	for i, m := range req.Messages {
		for j, b := range m.Content {
			if len(b.CacheControl) > 0 {
				out = append(out, fmt.Sprintf("msg[%d].content[%d]", i, j))
			}
		}
	}
	return out
}

func TestAnthropicCacheBreakpoints(t *testing.T) {
	conv := `"messages":[
		{"role":"system","content":"sys one"},
		{"role":"system","content":"sys two"},
		{"role":"user","content":"u1"},
		{"role":"assistant","content":"a1"},
		{"role":"user","content":"u2"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c","content":"out"},
		{"role":"user","content":[{"type":"text","text":"u3a"},{"type":"text","text":"u3b"}]}
	]`
	tools := `"tools":[{"type":"function","function":{"name":"e"}},{"type":"function","function":{"name":"f"}}]`

	cases := []struct {
		name string
		body string
		ttl  string
		want []string
	}{
		{
			name: "tools, system, last two user turns",
			body: `{"model":"m",` + tools + `,` + conv + `}`,
			want: []string{"tools[1]", "system[1]", "msg[2].content[0]", "msg[4].content[2]"},
		},
		{
			name: "no tools: system and two turns",
			body: `{"model":"m",` + conv + `}`,
			want: []string{"system[1]", "msg[2].content[0]", "msg[4].content[2]"},
		},
		{
			name: "single user turn",
			body: `{"model":"m","messages":[{"role":"user","content":"only"}]}`,
			want: []string{"msg[0].content[0]"},
		},
		{
			// The caller already spent three of the four: only the
			// highest-priority automatic slot (the tool list) still fits.
			name: "caller markers count against the cap",
			body: `{"model":"m",` + tools + `,"messages":[
				{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}]},
				{"role":"assistant","content":"x"},
				{"role":"user","content":[{"type":"text","text":"b","cache_control":{"type":"ephemeral"}}]},
				{"role":"assistant","content":"y"},
				{"role":"user","content":[{"type":"text","text":"c","cache_control":{"type":"ephemeral"}}]}
			]}`,
			want: []string{"tools[1]", "msg[0].content[0]", "msg[2].content[0]", "msg[4].content[0]"},
		},
		{
			name: "caller already at the cap: nothing added",
			body: `{"model":"m",` + tools + `,"messages":[
				{"role":"system","content":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}]},
				{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}]},
				{"role":"assistant","content":"x"},
				{"role":"user","content":[{"type":"text","text":"b","cache_control":{"type":"ephemeral"}},{"type":"text","text":"b2","cache_control":{"type":"ephemeral"}}]}
			]}`,
			want: []string{"system[0]", "msg[0].content[0]", "msg[2].content[0]", "msg[2].content[1]"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := translate(t, c.body, anthropicOpts{CacheTTL: c.ttl})
			got := markers(req)
			if strings.Join(got, " ") != strings.Join(c.want, " ") {
				t.Errorf("breakpoints = %v, want %v", got, c.want)
			}
			if n := countBreakpoints(&req); n > anthropicMaxBreakpoints {
				t.Errorf("%d breakpoints, cap is %d", n, anthropicMaxBreakpoints)
			}
		})
	}
}

func TestAnthropicCacheTTL(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"x"}]}`
	if cc := string(translate(t, body, anthropicOpts{}).Messages[0].Content[0].CacheControl); cc != `{"type":"ephemeral"}` {
		t.Errorf("default TTL marker = %s", cc)
	}
	if cc := string(translate(t, body, anthropicOpts{CacheTTL: "1h"}).Messages[0].Content[0].CacheControl); cc != `{"type":"ephemeral","ttl":"1h"}` {
		t.Errorf("1h TTL marker = %s", cc)
	}
}

func TestAnthropicResponseTranslation(t *testing.T) {
	out, err := fromAnthropicResponse([]byte(`{
		"id":"msg_1","type":"message","role":"assistant","model":"claude-x",
		"content":[
			{"type":"thinking","thinking":"hmm","signature":"sig"},
			{"type":"text","text":"Checking "},
			{"type":"text","text":"now."},
			{"type":"tool_use","id":"toolu_1","name":"wx","input":{"city":"Oslo"}}
		],
		"stop_reason":"tool_use",
		"usage":{"input_tokens":10,"cache_creation_input_tokens":200,"cache_read_input_tokens":3000,"output_tokens":42}}`))
	if err != nil {
		t.Fatal(err)
	}
	var c oaiCompletion
	if err := json.Unmarshal(out, &c); err != nil {
		t.Fatal(err)
	}
	if c.Object != "chat.completion" || c.ID != "msg_1" || c.Model != "claude-x" || len(c.Choices) != 1 {
		t.Fatalf("envelope = %s", out)
	}
	m := c.Choices[0].Message
	if m.Content == nil || *m.Content != "Checking now." || m.ReasoningContent != "hmm" {
		t.Errorf("content/reasoning = %v/%q", m.Content, m.ReasoningContent)
	}
	if len(m.ToolCalls) != 1 || m.ToolCalls[0].ID != "toolu_1" || m.ToolCalls[0].Type != "function" ||
		m.ToolCalls[0].Function.Name != "wx" || m.ToolCalls[0].Function.Arguments != `{"city":"Oslo"}` {
		t.Errorf("tool_calls = %+v", m.ToolCalls)
	}
	if c.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q", c.Choices[0].FinishReason)
	}
	if c.Usage.PromptTokens != 3210 || c.Usage.CompletionTokens != 42 || c.Usage.TotalTokens != 3252 ||
		c.Usage.PromptTokensDetails.CachedTokens != 3000 {
		t.Errorf("usage = %+v", c.Usage)
	}
	// What the proxy meters from must agree.
	if u := extractUsage(out, false); u.PromptTokens != 3210 || u.CachedTokens != 3000 || u.CompletionTokens != 42 {
		t.Errorf("extractUsage = %+v", u)
	}

	// Tool call with no text: content is null, as OpenAI sends it.
	out, _ = fromAnthropicResponse([]byte(`{"id":"m","content":[{"type":"tool_use","id":"t","name":"f","input":{}}],"stop_reason":"tool_use"}`))
	if !strings.Contains(string(out), `"content":null`) {
		t.Errorf("tool-only reply should carry content:null: %s", out)
	}
}

func TestAnthropicFinishReason(t *testing.T) {
	for in, want := range map[string]string{
		"end_turn": "stop", "stop_sequence": "stop", "max_tokens": "length",
		"tool_use": "tool_calls", "refusal": "content_filter", "pause_turn": "stop", "": "stop",
	} {
		if got := finishReason(in); got != want {
			t.Errorf("finishReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAnthropicErrorTranslation(t *testing.T) {
	out := openAIError([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
	var e struct {
		Error struct{ Message, Type string } `json:"error"`
	}
	if json.Unmarshal(out, &e) != nil || e.Error.Message != "slow down" || e.Error.Type != "rate_limit_error" {
		t.Errorf("error = %s", out)
	}
	if got := string(openAIError([]byte("upstream exploded"))); got != "upstream exploded" {
		t.Errorf("a non-Anthropic body must pass through, got %q", got)
	}
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

// parsedStream is an OpenAI stream reassembled the way a client would.
type parsedStream struct {
	role, content, reasoning string
	toolID, toolName, args   string
	finish                   string
	usage                    *oaiUsage
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

func TestAnthropicStreamTranslation(t *testing.T) {
	// Fed one byte at a time and all at once: event boundaries must not
	// depend on how the transport chunked the body.
	for _, step := range []int{1, 7, len(anthropicSSE)} {
		var out bytes.Buffer
		s := anthropicStream{emit: func(b []byte) { out.Write(b) }}
		for i := 0; i < len(anthropicSSE); i += step {
			s.feed([]byte(anthropicSSE[i:min(i+step, len(anthropicSSE))]))
		}
		s.finish()
		p := parseOpenAIStream(t, out.Bytes())
		if p.role != "assistant" || p.content != "Hello world" || p.reasoning != "let me see" {
			t.Errorf("step %d: role/content/reasoning = %q/%q/%q", step, p.role, p.content, p.reasoning)
		}
		if p.toolID != "toolu_9" || p.toolName != "wx" || p.args != `{"city":"Oslo"}` {
			t.Errorf("step %d: tool = %q %q %q", step, p.toolID, p.toolName, p.args)
		}
		if p.finish != "tool_calls" || !p.done {
			t.Errorf("step %d: finish=%q done=%v", step, p.finish, p.done)
		}
		if p.usage == nil || p.usage.PromptTokens != 5112 || p.usage.CompletionTokens != 57 ||
			p.usage.PromptTokensDetails.CachedTokens != 5000 {
			t.Errorf("step %d: usage = %+v", step, p.usage)
		}
		// The proxy's own metering reads the same bytes.
		u := extractUsage(out.Bytes(), true)
		if u.PromptTokens != 5112 || u.CachedTokens != 5000 || u.CompletionTokens != 57 {
			t.Errorf("step %d: extractUsage = %+v", step, u)
		}
		if fr := extractFinishReason(out.Bytes(), true); fr != "tool_calls" {
			t.Errorf("step %d: extractFinishReason = %q", step, fr)
		}
	}
}

func TestAnthropicStreamError(t *testing.T) {
	var out bytes.Buffer
	s := anthropicStream{emit: func(b []byte) { out.Write(b) }}
	s.feed([]byte(strings.SplitAfter(anthropicSSE, "\n\n")[0]))
	s.feed([]byte("event: error\r\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\r\n\r\n"))
	s.finish()
	p := parseOpenAIStream(t, out.Bytes())
	if !strings.Contains(p.errBody, `"message":"Overloaded"`) || !strings.Contains(p.errBody, `"type":"overloaded_error"`) || !p.done {
		t.Errorf("error = %q done=%v\n%s", p.errBody, p.done, out.String())
	}
}

func TestAnthropicStreamTruncated(t *testing.T) {
	// Upstream drops after the first text delta: usage so far is reported,
	// but no finish_reason and no [DONE], so the cut-off stays visible.
	cut := strings.Index(anthropicSSE, `"text":" world"`)
	var out bytes.Buffer
	s := anthropicStream{emit: func(b []byte) { out.Write(b) }}
	s.feed([]byte(anthropicSSE[:cut]))
	s.finish()
	p := parseOpenAIStream(t, out.Bytes())
	if p.done || p.finish != "" || p.usage == nil || p.usage.PromptTokensDetails.CachedTokens != 5000 {
		t.Errorf("truncated stream: done=%v finish=%q usage=%+v", p.done, p.finish, p.usage)
	}
}

// --- full proxy path against a fake Anthropic ---

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
		f.header.Get("anthropic-version") != anthropicVersion {
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
