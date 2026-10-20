package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
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
// COST-MODEL GAP, named rather than fixed: Anthropic bills cache WRITES
// (cache_creation_input_tokens) at 1.25× the input rate (2× for a 1h TTL).
// corrallm's cost classes are cached / processed / generated, so a write lands
// in "processed" at 1×: requests that write a cache are slightly undercounted.
// Reads are priced correctly through cachedCostFactor.
//
// DELIBERATELY DROPPED request fields — Anthropic has no equivalent, or the
// equivalent changes meaning: n, presence_penalty, frequency_penalty,
// logit_bias, logprobs, top_logprobs, response_format, seed, stream_options,
// user, metadata, store, service_tier, reasoning_effort, modalities, audio,
// prediction, and every llama.cpp extension (min_p, repeat_penalty,
// chat_template_kwargs, …). The body is BUILT from the fields below rather than
// edited, so anything not named here is dropped by construction instead of
// reaching Anthropic as an "extra inputs are not permitted" 400.
//
// Unsupported, and refused with a 400 rather than silently lost: content parts
// other than text / image_url (input_audio, file), image URLs that are neither
// data: nor http(s), and roles other than system/developer/user/assistant/tool.

// anthropicDefaultMaxTokens is the max_tokens sent when the caller set none and
// the model declares no maxTokens. Anthropic REQUIRES the field; OpenAI callers
// routinely omit it. 16000 keeps a non-streaming reply inside the usual HTTP
// timeouts and fits every current Claude model's output cap; a caller that
// needs more says so.
const anthropicDefaultMaxTokens = 16000

// anthropicVersion is the anthropic-version header sent when the config's
// proxy headers do not set one.
const anthropicVersion = "2023-06-01"

// anthropicMaxBreakpoints is Anthropic's per-request cap on cache_control
// markers. A request over it is a 400, so placement stops at it.
const anthropicMaxBreakpoints = 4

// anthropicOpts carries the per-backend settings the translation needs.
type anthropicOpts struct {
	MaxTokens int    // the model's configured cap; the fallback when the caller sent none
	CacheTTL  string // "", "5m" or "1h" — the TTL on breakpoints corrallm places
}

// --- OpenAI request shapes (input) ---

type oaiChatRequest struct {
	Model               string          `json:"model"`
	Messages            []oaiMessage    `json:"messages"`
	Tools               []oaiTool       `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls"`
	MaxTokens           *int            `json:"max_tokens"`
	MaxCompletionTokens *int            `json:"max_completion_tokens"`
	Temperature         *float64        `json:"temperature"`
	TopP                *float64        `json:"top_p"`
	TopK                *int            `json:"top_k"`
	Stop                json.RawMessage `json:"stop"`
	Stream              bool            `json:"stream"`
}

type oaiMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []oaiToolCall   `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// oaiPart is one element of an OpenAI content array. cache_control is not
// OpenAI's, but clients written against OpenRouter put it on parts; it is
// honoured verbatim and counts against the breakpoint cap.
type oaiPart struct {
	Type         string          `json:"type"`
	Text         string          `json:"text"`
	Refusal      string          `json:"refusal"`
	ImageURL     json.RawMessage `json:"image_url"`
	CacheControl json.RawMessage `json:"cache_control"`
}

type oaiTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
		Strict      *bool           `json:"strict"`
	} `json:"function"`
	CacheControl json.RawMessage `json:"cache_control"`
}

// --- Anthropic request shapes (output) ---

type antRequest struct {
	Model         string         `json:"model"`
	MaxTokens     int            `json:"max_tokens"`
	System        []antBlock     `json:"system,omitempty"`
	Messages      []antMessage   `json:"messages"`
	Tools         []antTool      `json:"tools,omitempty"`
	ToolChoice    *antToolChoice `json:"tool_choice,omitempty"`
	Temperature   *float64       `json:"temperature,omitempty"`
	TopP          *float64       `json:"top_p,omitempty"`
	TopK          *int           `json:"top_k,omitempty"`
	StopSequences []string       `json:"stop_sequences,omitempty"`
	Stream        bool           `json:"stream,omitempty"`
}

type antMessage struct {
	Role    string     `json:"role"`
	Content []antBlock `json:"content"`
}

// antBlock is the union of the content blocks corrallm sends. Only the fields
// a given type uses are set; omitempty keeps the others off the wire.
type antBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	Source       *antSource      `json:"source,omitempty"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	Content      []antBlock      `json:"content,omitempty"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

type antSource struct {
	Type      string `json:"type"` // base64 | url
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type antTool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema"`
	Strict       *bool           `json:"strict,omitempty"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

type antToolChoice struct {
	Type                   string `json:"type"` // auto | any | tool | none
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use,omitempty"`
}

// toAnthropicRequest translates an OpenAI chat.completions body into an
// Anthropic Messages body, with cache breakpoints placed. An error is the
// caller's request being untranslatable, and is answered as a 400.
func toAnthropicRequest(body []byte, opts anthropicOpts) ([]byte, error) {
	var in oaiChatRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("request is not a JSON chat completion: %w", err)
	}
	out := antRequest{
		Model:       in.Model,
		Temperature: in.Temperature,
		TopP:        in.TopP,
		TopK:        in.TopK,
		Stream:      in.Stream,
	}

	// max_completion_tokens is OpenAI's current name and wins over the
	// deprecated max_tokens. clampMaxTokens has usually filled one of them from
	// the model's maxTokens already; opts.MaxTokens covers a body it did not.
	switch {
	case in.MaxCompletionTokens != nil && *in.MaxCompletionTokens > 0:
		out.MaxTokens = *in.MaxCompletionTokens
	case in.MaxTokens != nil && *in.MaxTokens > 0:
		out.MaxTokens = *in.MaxTokens
	case opts.MaxTokens > 0:
		out.MaxTokens = opts.MaxTokens
	default:
		out.MaxTokens = anthropicDefaultMaxTokens
	}

	stops, err := stopSequences(in.Stop)
	if err != nil {
		return nil, err
	}
	out.StopSequences = stops

	for i, m := range in.Messages {
		switch m.Role {
		case "system", "developer":
			// A mid-conversation system message is folded into the top-level
			// system in order. Anthropic's own mid-conversation system role is
			// model-dependent, and the top-level field works on every model.
			parts, err := contentParts(m.Content)
			if err != nil {
				return nil, fmt.Errorf("messages[%d]: %w", i, err)
			}
			for _, p := range parts {
				b, ok, err := partToBlock(p)
				if err != nil {
					return nil, fmt.Errorf("messages[%d]: %w", i, err)
				}
				if !ok {
					continue
				}
				if b.Type != "text" {
					return nil, fmt.Errorf("messages[%d]: a %s message may only carry text", i, m.Role)
				}
				out.System = append(out.System, b)
			}
		case "user":
			blocks, err := partsToBlocks(m.Content)
			if err != nil {
				return nil, fmt.Errorf("messages[%d]: %w", i, err)
			}
			out.Messages = appendMessage(out.Messages, "user", blocks)
		case "assistant":
			blocks, err := partsToBlocks(m.Content)
			if err != nil {
				return nil, fmt.Errorf("messages[%d]: %w", i, err)
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, antBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: toolInput(tc.Function.Arguments),
				})
			}
			// reasoning_content on a replayed assistant turn is dropped:
			// Anthropic only accepts thinking blocks it signed, and an OpenAI
			// history carries no signature.
			out.Messages = appendMessage(out.Messages, "assistant", blocks)
		case "tool":
			blocks, err := partsToBlocks(m.Content)
			if err != nil {
				return nil, fmt.Errorf("messages[%d]: %w", i, err)
			}
			// A tool result is a user-turn block in Anthropic's model; the
			// consecutive results of one parallel call merge into one user
			// message via appendMessage, which is what Anthropic requires.
			out.Messages = appendMessage(out.Messages, "user", []antBlock{{
				Type:      "tool_result",
				ToolUseID: m.ToolCallID,
				Content:   blocks,
			}})
		default:
			return nil, fmt.Errorf("messages[%d]: role %q has no Anthropic equivalent", i, m.Role)
		}
	}
	for i := range out.Messages {
		if out.Messages[i].Role == "user" {
			toolResultsFirst(out.Messages[i].Content)
		}
	}

	for _, t := range in.Tools {
		if t.Type != "" && t.Type != "function" {
			return nil, fmt.Errorf("tool type %q is not supported (only function)", t.Type)
		}
		schema := t.Function.Parameters
		if len(bytes.TrimSpace(schema)) == 0 || bytes.Equal(bytes.TrimSpace(schema), []byte("null")) {
			// Anthropic requires an object schema; OpenAI allows a tool with none.
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		at := antTool{
			Name:         t.Function.Name,
			Description:  t.Function.Description,
			InputSchema:  schema,
			CacheControl: t.CacheControl,
		}
		if t.Function.Strict != nil && *t.Function.Strict {
			at.Strict = t.Function.Strict
		}
		out.Tools = append(out.Tools, at)
	}
	tc, err := toolChoice(in.ToolChoice, in.ParallelToolCalls, len(out.Tools) > 0)
	if err != nil {
		return nil, err
	}
	out.ToolChoice = tc

	placeCacheBreakpoints(&out, opts.CacheTTL)
	return json.Marshal(out)
}

// contentParts normalises an OpenAI content field — absent, a string, or an
// array of parts — to parts.
func contentParts(raw json.RawMessage) ([]oaiPart, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return []oaiPart{{Type: "text", Text: s}}, nil
	}
	var parts []oaiPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("content is neither a string nor an array of parts")
	}
	return parts, nil
}

func partsToBlocks(raw json.RawMessage) ([]antBlock, error) {
	parts, err := contentParts(raw)
	if err != nil {
		return nil, err
	}
	var out []antBlock
	for _, p := range parts {
		b, ok, err := partToBlock(p)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, b)
		}
	}
	return out, nil
}

// partToBlock converts one content part. ok=false means the part carries
// nothing and is skipped: Anthropic rejects an empty text block, and an empty
// string is how OpenAI clients spell "no text" next to tool_calls.
func partToBlock(p oaiPart) (antBlock, bool, error) {
	switch p.Type {
	case "text", "input_text", "":
		if p.Text == "" {
			return antBlock{}, false, nil
		}
		return antBlock{Type: "text", Text: p.Text, CacheControl: p.CacheControl}, true, nil
	case "refusal":
		if p.Refusal == "" {
			return antBlock{}, false, nil
		}
		return antBlock{Type: "text", Text: p.Refusal, CacheControl: p.CacheControl}, true, nil
	case "image_url":
		src, err := imageSource(p.ImageURL)
		if err != nil {
			return antBlock{}, false, err
		}
		return antBlock{Type: "image", Source: src, CacheControl: p.CacheControl}, true, nil
	default:
		return antBlock{}, false, fmt.Errorf("content part type %q is not supported by an Anthropic backend", p.Type)
	}
}

// imageSource maps an OpenAI image_url ({url} or, from lax clients, a bare
// string) to an Anthropic image source: a data: URL becomes base64, an http(s)
// URL is passed for Anthropic to fetch.
func imageSource(raw json.RawMessage) (*antSource, error) {
	var u string
	var obj struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.URL != "" {
		u = obj.URL
	} else if json.Unmarshal(raw, &u) != nil || u == "" {
		return nil, fmt.Errorf("image_url part has no url")
	}
	switch {
	case strings.HasPrefix(u, "data:"):
		meta, data, ok := strings.Cut(strings.TrimPrefix(u, "data:"), ",")
		media, enc, _ := strings.Cut(meta, ";")
		if !ok || enc != "base64" || media == "" {
			return nil, fmt.Errorf("image data: URL must be data:<media-type>;base64,<data>")
		}
		return &antSource{Type: "base64", MediaType: media, Data: data}, nil
	case strings.HasPrefix(u, "http://"), strings.HasPrefix(u, "https://"):
		return &antSource{Type: "url", URL: u}, nil
	default:
		return nil, fmt.Errorf("image url must be a data: or http(s) URL")
	}
}

// toolInput turns an OpenAI arguments string into an Anthropic input object.
//
// A non-object (empty, or JSON cut off by max_tokens) becomes {} rather than an
// error: it arrives on a REPLAYED turn, the tool_result that followed it has
// already told the model what happened, and refusing it would make the
// conversation unsendable for good.
func toolInput(args string) json.RawMessage {
	a := bytes.TrimSpace([]byte(args))
	if len(a) > 0 && a[0] == '{' && json.Valid(a) {
		return json.RawMessage(a)
	}
	return json.RawMessage(`{}`)
}

// appendMessage adds a turn, merging it into the previous one when the role
// repeats. Anthropic requires strictly alternating roles; OpenAI histories
// routinely carry adjacent user turns, and always carry adjacent tool results.
// A turn with no content is dropped — Anthropic refuses an empty one.
func appendMessage(msgs []antMessage, role string, blocks []antBlock) []antMessage {
	if len(blocks) == 0 {
		return msgs
	}
	if n := len(msgs); n > 0 && msgs[n-1].Role == role {
		msgs[n-1].Content = append(msgs[n-1].Content, blocks...)
		return msgs
	}
	return append(msgs, antMessage{Role: role, Content: blocks})
}

// toolResultsFirst moves tool_result blocks to the front of a user turn,
// keeping relative order. Anthropic requires them to lead the message that
// answers a tool_use; merging a tool reply with a following user message
// already produces that order, this covers a history that interleaved them.
func toolResultsFirst(blocks []antBlock) {
	var results, rest []antBlock
	for _, b := range blocks {
		if b.Type == "tool_result" {
			results = append(results, b)
		} else {
			rest = append(rest, b)
		}
	}
	copy(blocks, append(results, rest...))
}

// stopSequences reads OpenAI's stop (a string or an array of them).
// Whitespace-only sequences are dropped: Anthropic refuses them, and an OpenAI
// caller sending "\n" means a formatting preference, not a reason to 400.
func stopSequences(raw json.RawMessage) ([]string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var list []string
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		list = []string{s}
	} else if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("stop must be a string or an array of strings")
	}
	out := list[:0]
	for _, s := range list {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

// toolChoice maps OpenAI's tool_choice ("auto" | "none" | "required" |
// {type:function,function:{name}}) and parallel_tool_calls to Anthropic's.
// Without tools it is dropped: Anthropic rejects a tool_choice with nothing to
// choose. Note "required" and a named function become `any` / `tool`, which
// some current models (Opus 5.5, Sonnet 5.5, Fable 5.1) refuse with a 400 —
// that refusal is passed back as it is, not papered over.
func toolChoice(raw json.RawMessage, parallel *bool, haveTools bool) (*antToolChoice, error) {
	if !haveTools {
		return nil, nil
	}
	var tc *antToolChoice
	raw = bytes.TrimSpace(raw)
	switch {
	case len(raw) == 0 || bytes.Equal(raw, []byte("null")):
	case raw[0] == '"':
		var s string
		_ = json.Unmarshal(raw, &s)
		switch s {
		case "auto":
			tc = &antToolChoice{Type: "auto"}
		case "none":
			tc = &antToolChoice{Type: "none"}
		case "required":
			tc = &antToolChoice{Type: "any"}
		default:
			return nil, fmt.Errorf("tool_choice %q is not one of auto, none, required", s)
		}
	default:
		var obj struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil || obj.Function.Name == "" {
			return nil, fmt.Errorf("tool_choice object must name a function")
		}
		tc = &antToolChoice{Type: "tool", Name: obj.Function.Name}
	}
	if parallel != nil && !*parallel {
		if tc == nil {
			tc = &antToolChoice{Type: "auto"}
		}
		if tc.Type != "none" {
			tc.DisableParallelToolUse = true
		}
	}
	return tc, nil
}

// placeCacheBreakpoints marks the cacheable prefix, at most
// anthropicMaxBreakpoints markers in all. Render order is tools → system →
// messages, so in priority order:
//
//  1. the last tool definition (the tool list rarely changes),
//  2. the last system block,
//  3. the last block of the FINAL user turn — writes this turn's prefix,
//  4. the last block of the user turn before it — the prefix the previous
//     request wrote, so this one reads it even when Anthropic's 20-block
//     lookback from (3) would not reach that far back.
//
// Markers the caller placed itself are kept verbatim and count against the
// cap first; a slot whose block already carries one is treated as covered.
// Prefixes shorter than the model's minimum (1024–4096 tokens) are simply not
// cached by Anthropic, so marking them costs nothing.
func placeCacheBreakpoints(req *antRequest, ttl string) {
	cc := json.RawMessage(`{"type":"ephemeral"}`)
	if ttl == "1h" {
		cc = json.RawMessage(`{"type":"ephemeral","ttl":"1h"}`)
	}
	budget := anthropicMaxBreakpoints - countBreakpoints(req)
	mark := func(slot *json.RawMessage) {
		if len(*slot) > 0 || budget <= 0 {
			return
		}
		*slot = cc
		budget--
	}
	if n := len(req.Tools); n > 0 {
		mark(&req.Tools[n-1].CacheControl)
	}
	if n := len(req.System); n > 0 {
		mark(&req.System[n-1].CacheControl)
	}
	turns := 0
	for i := len(req.Messages) - 1; i >= 0 && turns < 2; i-- {
		m := &req.Messages[i]
		if m.Role != "user" || len(m.Content) == 0 {
			continue
		}
		mark(&m.Content[len(m.Content)-1].CacheControl)
		turns++
	}
}

// countBreakpoints counts the cache_control markers already in a request,
// including those nested in tool_result content.
func countBreakpoints(req *antRequest) int {
	n := 0
	for _, t := range req.Tools {
		if len(t.CacheControl) > 0 {
			n++
		}
	}
	var walk func([]antBlock)
	walk = func(bs []antBlock) {
		for _, b := range bs {
			if len(b.CacheControl) > 0 {
				n++
			}
			walk(b.Content)
		}
	}
	walk(req.System)
	for _, m := range req.Messages {
		walk(m.Content)
	}
	return n
}

// --- Anthropic response shapes (input) ---

type antUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

type antContent struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
}

type antResponse struct {
	ID         string       `json:"id"`
	Model      string       `json:"model"`
	Content    []antContent `json:"content"`
	StopReason string       `json:"stop_reason"`
	Usage      antUsage     `json:"usage"`
}

// --- OpenAI response shapes (output) ---

type oaiUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// openAIUsage restates Anthropic usage in OpenAI's terms. Anthropic's
// input_tokens EXCLUDES both cache classes, so the prompt total is the sum of
// all three; cached_tokens is the read share only (see the cost-model gap at
// the top of this file for where writes go).
func openAIUsage(u antUsage) oaiUsage {
	var o oaiUsage
	o.PromptTokens = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	o.CompletionTokens = u.OutputTokens
	o.TotalTokens = o.PromptTokens + o.CompletionTokens
	o.PromptTokensDetails.CachedTokens = u.CacheReadInputTokens
	return o
}

// finishReason maps Anthropic's stop_reason to OpenAI's finish_reason.
func finishReason(stop string) string {
	switch stop {
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default: // end_turn, stop_sequence, pause_turn, unknown
		return "stop"
	}
}

type oaiFunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

type oaiToolCallOut struct {
	Index    *int            `json:"index,omitempty"` // streaming deltas only
	ID       string          `json:"id,omitempty"`
	Type     string          `json:"type,omitempty"`
	Function oaiFunctionCall `json:"function"`
}

type oaiMessageOut struct {
	Role             string           `json:"role"`
	Content          *string          `json:"content"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ToolCalls        []oaiToolCallOut `json:"tool_calls,omitempty"`
}

type oaiChoice struct {
	Index        int           `json:"index"`
	Message      oaiMessageOut `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type oaiCompletion struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Created int64       `json:"created"`
	Model   string      `json:"model"`
	Choices []oaiChoice `json:"choices"`
	Usage   oaiUsage    `json:"usage"`
}

// fromAnthropicResponse translates a non-streaming Messages reply into a
// chat.completion. Text blocks concatenate into content, thinking blocks into
// reasoning_content, tool_use blocks become tool_calls. Server-tool and
// redacted-thinking blocks have no OpenAI form and are dropped.
func fromAnthropicResponse(body []byte) ([]byte, error) {
	var in antResponse
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	var out oaiCompletion
	out.ID, out.Object, out.Created, out.Model = in.ID, "chat.completion", time.Now().Unix(), in.Model
	out.Choices = make([]oaiChoice, 1)
	ch := &out.Choices[0]
	ch.Message.Role = "assistant"
	var text, reasoning strings.Builder
	for _, b := range in.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "thinking":
			reasoning.WriteString(b.Thinking)
		case "tool_use":
			args := string(b.Input)
			if len(bytes.TrimSpace(b.Input)) == 0 {
				args = "{}"
			}
			ch.Message.ToolCalls = append(ch.Message.ToolCalls, oaiToolCallOut{
				ID: b.ID, Type: "function", Function: oaiFunctionCall{Name: b.Name, Arguments: args},
			})
		}
	}
	// OpenAI sends content:null beside tool_calls with no text.
	if s := text.String(); s != "" || len(ch.Message.ToolCalls) == 0 {
		ch.Message.Content = &s
	}
	ch.Message.ReasoningContent = reasoning.String()
	ch.FinishReason = finishReason(in.StopReason)
	out.Usage = openAIUsage(in.Usage)
	return json.Marshal(out)
}

// openAIError restates an Anthropic error body ({type:error, error:{type,
// message}}) as OpenAI's envelope. A body that is not Anthropic's shape is
// returned unchanged — it is still the only account of what went wrong.
func openAIError(body []byte) []byte {
	var in struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &in) != nil || in.Error.Message == "" {
		return body
	}
	out, err := json.Marshal(map[string]any{"error": map[string]any{
		"message": in.Error.Message, "type": in.Error.Type, "code": nil,
	}})
	if err != nil {
		return body
	}
	return out
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
	stream      anthropicStream
}

func newAnthropicWriter(w http.ResponseWriter) *anthropicWriter {
	a := &anthropicWriter{ResponseWriter: w}
	a.stream.emit = func(b []byte) { _, _ = a.ResponseWriter.Write(b) }
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
		a.stream.feed(b)
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
		a.stream.finish()
		return
	}
	if len(a.buf) == 0 {
		return
	}
	var out []byte
	if a.status >= 200 && a.status < 300 {
		if b, err := fromAnthropicResponse(a.buf); err == nil {
			out = b
		}
	}
	if out == nil {
		out = openAIError(a.buf)
	}
	_, _ = a.ResponseWriter.Write(out)
}

// anthropicStream translates Anthropic's SSE events into OpenAI
// chat.completion.chunk events:
//
//	message_start            → first chunk, delta {role: assistant}
//	content_block_start      → tool_use opens a tool_calls entry (id, name)
//	content_block_delta      → text_delta → content, thinking_delta →
//	                           reasoning_content, input_json_delta → arguments
//	message_delta            → stop_reason and final usage, held
//	message_stop             → finish chunk, usage chunk, [DONE]
//	error                    → {"error":…} chunk, [DONE]
//	ping, content_block_stop, signature_delta → nothing
//
// The usage chunk is sent whether or not the caller asked for
// stream_options.include_usage: it is what extractUsage meters from, and a
// chunk with empty choices is part of the OpenAI stream contract.
type anthropicStream struct {
	emit    func([]byte)
	pending []byte
	id      string
	model   string
	created int64
	usage   antUsage
	stop    string
	tools   map[int]int // Anthropic content-block index → OpenAI tool_calls index
	started bool
	done    bool
}

func (s *anthropicStream) feed(b []byte) {
	if s.done {
		return
	}
	// JSON escapes CR inside strings, so a raw CR is only ever a line ending.
	s.pending = append(s.pending, bytes.ReplaceAll(b, []byte("\r"), nil)...)
	for {
		i := bytes.Index(s.pending, []byte("\n\n"))
		if i < 0 {
			return
		}
		ev := s.pending[:i]
		s.pending = s.pending[i+2:]
		s.event(ev)
		if s.done {
			return
		}
	}
}

// event handles one SSE event. The JSON payload's own `type` is authoritative;
// the `event:` line repeats it and is ignored.
func (s *anthropicStream) event(raw []byte) {
	var data []byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if d, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, bytes.TrimSpace(d)...)
		}
	}
	if len(data) == 0 {
		return
	}
	var ev struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message struct {
			ID    string   `json:"id"`
			Model string   `json:"model"`
			Usage antUsage `json:"usage"`
		} `json:"message"`
		ContentBlock antContent `json:"content_block"`
		Delta        struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			PartialJSON string `json:"partial_json"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Usage antUsage        `json:"usage"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	switch ev.Type {
	case "message_start":
		s.id, s.model, s.usage = ev.Message.ID, ev.Message.Model, ev.Message.Usage
		s.created = time.Now().Unix()
		s.started = true
		empty := ""
		s.chunk(oaiDelta{Role: "assistant", Content: &empty}, nil)
	case "content_block_start":
		switch ev.ContentBlock.Type {
		case "text":
			if t := ev.ContentBlock.Text; t != "" {
				s.chunk(oaiDelta{Content: &t}, nil)
			}
		case "thinking":
			if t := ev.ContentBlock.Thinking; t != "" {
				s.chunk(oaiDelta{ReasoningContent: t}, nil)
			}
		case "tool_use":
			if s.tools == nil {
				s.tools = map[int]int{}
			}
			idx := len(s.tools)
			s.tools[ev.Index] = idx
			s.chunk(oaiDelta{ToolCalls: []oaiToolCallOut{{
				Index: &idx, ID: ev.ContentBlock.ID, Type: "function",
				Function: oaiFunctionCall{Name: ev.ContentBlock.Name},
			}}}, nil)
		}
	case "content_block_delta":
		switch ev.Delta.Type {
		case "text_delta":
			if t := ev.Delta.Text; t != "" {
				s.chunk(oaiDelta{Content: &t}, nil)
			}
		case "thinking_delta":
			if t := ev.Delta.Thinking; t != "" {
				s.chunk(oaiDelta{ReasoningContent: t}, nil)
			}
		case "input_json_delta":
			idx, ok := s.tools[ev.Index]
			if ok && ev.Delta.PartialJSON != "" {
				s.chunk(oaiDelta{ToolCalls: []oaiToolCallOut{{
					Index: &idx, Function: oaiFunctionCall{Arguments: ev.Delta.PartialJSON},
				}}}, nil)
			}
		}
	case "message_delta":
		if ev.Delta.StopReason != "" {
			s.stop = ev.Delta.StopReason
		}
		// output_tokens is cumulative here; the input classes are repeated by
		// newer API versions and absent (zero) from older ones.
		s.usage.OutputTokens = ev.Usage.OutputTokens
		if ev.Usage.InputTokens > 0 {
			s.usage.InputTokens = ev.Usage.InputTokens
		}
		if ev.Usage.CacheReadInputTokens > 0 {
			s.usage.CacheReadInputTokens = ev.Usage.CacheReadInputTokens
		}
		if ev.Usage.CacheCreationInputTokens > 0 {
			s.usage.CacheCreationInputTokens = ev.Usage.CacheCreationInputTokens
		}
	case "message_stop":
		fr := finishReason(s.stop)
		s.chunk(oaiDelta{}, &fr)
		s.usageChunk()
		s.emit([]byte("data: [DONE]\n\n"))
		s.done = true
	case "error":
		if b := openAIError(data); b != nil {
			s.emit(append(append([]byte("data: "), b...), '\n', '\n'))
		}
		s.emit([]byte("data: [DONE]\n\n"))
		s.done = true
	}
}

// finish closes a stream that ended without message_stop — the upstream
// dropped, or the request was cancelled. The usage seen so far is sent so the
// input the caller was billed for is metered, but no finish_reason and no
// [DONE]: the caller must still be able to tell the answer was cut off.
func (s *anthropicStream) finish() {
	if s.started && !s.done {
		s.usageChunk()
		s.done = true
	}
}

type oaiDelta struct {
	Role             string           `json:"role,omitempty"`
	Content          *string          `json:"content,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ToolCalls        []oaiToolCallOut `json:"tool_calls,omitempty"`
}

type oaiChunkChoice struct {
	Index        int      `json:"index"`
	Delta        oaiDelta `json:"delta"`
	FinishReason *string  `json:"finish_reason"`
}

type oaiChunk struct {
	ID      string           `json:"id"`
	Object  string           `json:"object"`
	Created int64            `json:"created"`
	Model   string           `json:"model"`
	Choices []oaiChunkChoice `json:"choices"`
	Usage   *oaiUsage        `json:"usage,omitempty"`
}

func (s *anthropicStream) chunk(d oaiDelta, finish *string) {
	s.write(oaiChunk{ID: s.id, Object: "chat.completion.chunk", Created: s.created, Model: s.model,
		Choices: []oaiChunkChoice{{Delta: d, FinishReason: finish}}})
}

func (s *anthropicStream) usageChunk() {
	u := openAIUsage(s.usage)
	s.write(oaiChunk{ID: s.id, Object: "chat.completion.chunk", Created: s.created, Model: s.model,
		Choices: []oaiChunkChoice{}, Usage: &u})
}

func (s *anthropicStream) write(c oaiChunk) {
	b, err := json.Marshal(c)
	if err != nil {
		return
	}
	s.emit(append(append([]byte("data: "), b...), '\n', '\n'))
}
