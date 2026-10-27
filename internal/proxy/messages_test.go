package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/store"
)

// fakeOpenAIChat answers chat completions. It records the last request body.
type fakeOpenAIChat struct {
	srv  *httptest.Server
	mu   sync.Mutex
	body map[string]any
	hdr  http.Header
	hits int
}

func newFakeOpenAIChat(t *testing.T) *fakeOpenAIChat {
	f := &fakeOpenAIChat{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.body, f.hdr = nil, r.Header.Clone()
		_ = json.Unmarshal(raw, &f.body)
		f.hits++
		stream := f.body["stream"] == true
		f.mu.Unlock()
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if !stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"hi there",
				"reasoning_content":"think","tool_calls":[{"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"path\":\"a\"}"}}]},
				"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":60}}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range []string{
			`{"id":"chatcmpl-2","choices":[{"delta":{"reasoning_content":"hm"}}]}`,
			`{"id":"chatcmpl-2","choices":[{"delta":{"content":"Let me look."}}]}`,
			`{"id":"chatcmpl-2","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call.9","type":"function","function":{"name":"Read","arguments":""}}]}}]}`,
			`{"id":"chatcmpl-2","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`,
			`{"id":"chatcmpl-2","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a\"}"}}]}}]}`,
			`{"id":"chatcmpl-2","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"id":"chatcmpl-2","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":60}}}`,
			`[DONE]`,
		} {
			_, _ = io.WriteString(w, "data: "+ev+"\n\n")
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// claudeModel is an `api: anthropic` model pointing at srv.
func claudeModel(t *testing.T, srv string, extra string) config.Model {
	t.Helper()
	u, _ := url.Parse(srv)
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(fmt.Sprintf("host: %s\nport: %s\napi: anthropic\n%s", u.Hostname(), u.Port(), extra)), &doc); err != nil {
		t.Fatal(err)
	}
	return config.Model{Proxy: *doc.Content[0], Type: "chat"}
}

const claudeCodeBody = `{"model":"%s","max_tokens":1000,"stream":%t,
	"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.0; cch=abc123;"},
		{"type":"text","text":"You are Claude Code.","cache_control":{"type":"ephemeral"}}],
	"tools":[{"name":"Read","description":"read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}},
		{"type":"web_search_20250305","name":"web_search"}],
	"messages":[{"role":"user","content":"read a"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"corrallm"},{"type":"text","text":"ok"},
			{"type":"tool_use","id":"toolu_1","name":"Read","input":{"path":"a"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"contents"}]},
			{"type":"text","text":"now summarise"}]},
		{"role":"system","content":[{"type":"text","text":"# Environment"}]}]}`

func postMessages(h http.Handler, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestMessagesTranslatesForAnOpenAIModel(t *testing.T) {
	f := newFakeOpenAIChat(t)
	h, st := refusalRig(t, map[string]config.Model{"chat": modelTo(t, f.srv.URL, "chat")}, nil)
	rec := postMessages(h, fmt.Sprintf(claudeCodeBody, "chat", false), map[string]string{"x-api-key": "k"})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	// The request the backend saw.
	b, _ := json.Marshal(f.body)
	for _, want := range []string{`"content":"You are Claude Code.","role":"system"`, `"content":"contents","role":"tool","tool_call_id":"toolu_1"`,
		`"tool_calls":[{"function":{"arguments":"{\"path\":\"a\"}","name":"Read"}`, `"parameters":{"properties"`, `"content":"now summarise","role":"user"`, `{"content":"# Environment","role":"user"}`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("backend body lacks %s:\n%s", want, b)
		}
	}
	for _, not := range []string{"billing-header", "web_search", "cache_control", `"thinking"`} {
		if strings.Contains(string(b), not) {
			t.Errorf("backend body still has %s:\n%s", not, b)
		}
	}
	if f.hdr.Get("X-Api-Key") != "" || f.hdr.Get("Authorization") != "" {
		t.Errorf("the corrallm key reached the backend: %v", f.hdr)
	}
	// The reply the caller got.
	var out struct {
		Type       string           `json:"type"`
		Model      string           `json:"model"`
		StopReason string           `json:"stop_reason"`
		Content    []map[string]any `json:"content"`
		Usage      map[string]int   `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Type != "message" || out.Model != "chat" || out.StopReason != "tool_use" || len(out.Content) != 3 ||
		out.Content[0]["type"] != "thinking" || out.Content[1]["text"] != "hi there" || out.Content[2]["name"] != "Read" {
		t.Errorf("reply = %s", rec.Body)
	}
	if out.Usage["input_tokens"] != 40 || out.Usage["cache_read_input_tokens"] != 60 || out.Usage["output_tokens"] != 7 {
		t.Errorf("usage = %v", out.Usage)
	}
	rows, _ := st.RecentActivity(store.Window{}, 10, "", "", "")
	if len(rows) != 1 || rows[0].PromptTokens != 100 || rows[0].CompletionTokens != 7 || rows[0].FinishReason != "tool_calls" {
		t.Errorf("activity row = %+v, want the usage metered", rows)
	}
}

func TestMessagesStreamsAnthropicEvents(t *testing.T) {
	f := newFakeOpenAIChat(t)
	h, st := refusalRig(t, map[string]config.Model{"chat": modelTo(t, f.srv.URL, "chat")}, nil)
	rec := postMessages(h, fmt.Sprintf(claudeCodeBody, "chat", true), map[string]string{"Authorization": "Bearer k"})
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d %s: %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
	if so, _ := f.body["stream_options"].(map[string]any); so["include_usage"] != true {
		t.Errorf("stream asked no usage: %v", f.body)
	}
	var events []string
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if ev, ok := strings.CutPrefix(line, "event: "); ok {
			events = append(events, ev)
		}
	}
	want := "message_start content_block_start content_block_delta content_block_delta content_block_stop " +
		"content_block_start content_block_delta content_block_stop " +
		"content_block_start content_block_delta content_block_delta content_block_stop message_delta message_stop"
	if strings.Join(events, " ") != want {
		t.Errorf("events:\n got %s\nwant %s", strings.Join(events, " "), want)
	}
	body := rec.Body.String()
	for _, s := range []string{`"type":"thinking_delta"`, `"text":"Let me look.","type":"text_delta"`, `"id":"call_9"`,
		`"partial_json":"{\"path\":"`, `"stop_reason":"tool_use"`, `"cache_read_input_tokens":60`} {
		if !strings.Contains(body, s) {
			t.Errorf("stream lacks %s", s)
		}
	}
	rows, _ := st.RecentActivity(store.Window{}, 10, "", "", "")
	if len(rows) != 1 || rows[0].PromptTokens != 100 || rows[0].CompletionTokens != 7 {
		t.Errorf("activity row = %+v, want the stream's usage metered", rows)
	}
}

// A Claude backend gets the body as sent; under callerAuth it also gets the
// caller's own credential, but only when the corrallm key came separately.
func TestMessagesPassesThroughToClaude(t *testing.T) {
	treatAsRemote(t)
	var got struct {
		sync.Mutex
		body string
		hdr  http.Header
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.Lock()
		got.body, got.hdr = string(b), r.Header.Clone()
		got.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],
			"stop_reason":"end_turn","usage":{"input_tokens":10,"cache_read_input_tokens":90,"output_tokens":3}}`)
	}))
	t.Cleanup(up.Close)
	h, st := refusalRig(t, map[string]config.Model{"claude": claudeModel(t, up.URL, "callerAuth: true\n")}, nil)

	sent := fmt.Sprintf(claudeCodeBody, "claude", false)
	rec := postMessages(h, sent, map[string]string{"X-Corrallm-Key": "k", "Authorization": "Bearer sub-token",
		"anthropic-beta": "oauth-2025-04-20", "anthropic-version": "2023-06-01"})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"text":"hi"`) {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	got.Lock()
	if got.hdr.Get("Authorization") != "Bearer sub-token" || got.hdr.Get("anthropic-beta") != "oauth-2025-04-20" || got.hdr.Get("X-Corrallm-Key") != "" {
		t.Errorf("upstream headers = %v", got.hdr)
	}
	// Passthrough: cache_control and the billing line reach Claude; only the
	// thinking block corrallm made is gone.
	if !strings.Contains(got.body, "cache_control") || !strings.Contains(got.body, "billing-header") || strings.Contains(got.body, `"corrallm"`) {
		t.Errorf("upstream body = %s", got.body)
	}
	got.Unlock()
	rows, _ := st.RecentActivity(store.Window{}, 10, "", "", "")
	if len(rows) != 1 || rows[0].PromptTokens != 100 || rows[0].CompletionTokens != 3 || rows[0].FinishReason != "stop" {
		t.Errorf("activity row = %+v, want Anthropic's usage metered", rows)
	}

	// The corrallm key alone (no X-Corrallm-Key) is not an Anthropic sign-in:
	// refused here, never forwarded, and Anthropic is not asked.
	got.Lock()
	got.hdr = nil
	got.Unlock()
	for _, hdr := range []map[string]string{{"Authorization": "Bearer k"}, {"x-api-key": "k"}} {
		if rec := postMessages(h, sent, hdr); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "authentication_error") {
			t.Errorf("%v: %d %s, want a 401", hdr, rec.Code, rec.Body)
		}
	}
	got.Lock()
	if got.hdr != nil {
		t.Errorf("Anthropic was asked without the caller's sign-in: %v", got.hdr)
	}
	got.Unlock()
}

// Out of Claude: a 429 moves the same request to the next lane member,
// translated, and the reset keeps Claude cooled for the following requests.
// Through a template lane, so Claude Code needs no model settings.
func TestMessagesFallsBackWhenClaudeIsSpent(t *testing.T) {
	treatAsRemote(t)
	claudeHits := 0
	var mu sync.Mutex
	var sentModel string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		claudeHits++
		sentModel = b.Model
		mu.Unlock()
		w.Header().Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(time.Now().Add(3*time.Hour).Unix(), 10))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"usage limit reached"}}`)
	}))
	t.Cleanup(up.Close)
	f := newFakeOpenAIChat(t)
	h, _ := refusalRig(t, map[string]config.Model{
		"claude-sub-*": claudeModel(t, up.URL, "callerAuth: true\n"),
		"chat":         modelTo(t, f.srv.URL, "chat"),
	}, map[string]config.Lane{"claude-*": {Members: []config.LaneMember{{Model: "claude-sub-*"}, {Model: "chat"}}}})

	for i := range 2 {
		rec := postMessages(h, fmt.Sprintf(claudeCodeBody, "claude-opus-5-5", false), map[string]string{"X-Corrallm-Key": "k", "Authorization": "Bearer sub"})
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"type":"message"`) {
			t.Fatalf("request %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	// Claude Code's own model name, through the template lane, unchanged.
	if sentModel != "claude-opus-5-5" {
		t.Errorf("Anthropic got model %q, want the id the caller sent", sentModel)
	}
	if claudeHits != 1 || f.hits != 2 {
		t.Errorf("claude hits %d, fallback hits %d: want 1 and 2 (Claude cooled after its 429)", claudeHits, f.hits)
	}
}

func TestMessagesErrorsInAnthropicShape(t *testing.T) {
	h, _ := refusalRig(t, map[string]config.Model{}, nil)
	rec := postMessages(h, `{"model":"nope","max_tokens":5,"messages":[]}`, nil)
	var e struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if rec.Code != 404 || json.Unmarshal(rec.Body.Bytes(), &e) != nil || e.Type != "error" || e.Error.Type != "not_found_error" || e.Error.Message == "" {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

func TestStripCorrallmThinkingTurnsThinkingOffForTheLastTurn(t *testing.T) {
	in := `{"thinking":{"type":"enabled","budget_tokens":1000},"messages":[{"role":"user","content":"x"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"corrallm"},{"type":"tool_use","id":"a","name":"R","input":{}}]}]}`
	out := string(stripCorrallmThinking([]byte(in)))
	if strings.Contains(out, "corrallm") || strings.Contains(out, `"budget_tokens"`) || !strings.Contains(out, "tool_use") {
		t.Errorf("got %s", out)
	}
	plain := `{"messages":[{"role":"user","content":"x"}]}`
	if string(stripCorrallmThinking([]byte(plain))) != plain {
		t.Error("a body with nothing to strip must keep its bytes")
	}
}

// An Anthropic sign-in sent without X-Corrallm-Key is not taken as the
// corrallm key: it must never be stored in an activity row.
func TestMessagesNeverLogsAnAnthropicCredentialAsTheKey(t *testing.T) {
	f := newFakeOpenAIChat(t)
	h, st := refusalRig(t, map[string]config.Model{"chat": modelTo(t, f.srv.URL, "chat")}, nil)
	for _, hdr := range []map[string]string{{"Authorization": "Bearer sk-ant-oat01-secret"}, {"x-api-key": "sk-ant-api03-secret"}} {
		if rec := postMessages(h, fmt.Sprintf(claudeCodeBody, "chat", false), hdr); rec.Code != 200 {
			t.Fatalf("%v: %d %s", hdr, rec.Code, rec.Body)
		}
	}
	rows, _ := st.RecentActivity(store.Window{}, 10, "", "", "")
	for _, r := range rows {
		if strings.Contains(r.Key, "sk-ant") {
			t.Errorf("an Anthropic credential was logged as the key: %q", r.Key)
		}
	}
	if len(rows) != 2 {
		t.Errorf("rows = %d, want 2", len(rows))
	}
}
