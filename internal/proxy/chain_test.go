package proxy

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
)

// The order of the chain is the design: a writer moved one place changes
// what the activity row records, and a body stage moved one place changes
// what the next stage reads.
func TestCandidateChainOrder(t *testing.T) {
	body, writers := candidateChain()
	var bn []string
	for _, s := range body {
		bn = append(bn, s.name)
	}
	if want := []string{"guard-in", "wire-in", "clamp", "model", "sampling", "align", "wire-out"}; !reflect.DeepEqual(bn, want) {
		t.Errorf("body stages = %v, want %v", bn, want)
	}
	// Backend outward. The raw tap is innermost on every path, so it holds
	// the backend's own bytes. loop and guard-out read the chat shape:
	// outside anthropicWriter, inside messagesWriter.
	if want := []string{"tap", "wire", "status", "loop", "guard-out", "cost", "stt-remap", "stt-sse"}; !reflect.DeepEqual(stageNames(writers.chat), want) {
		t.Errorf("chat writer stages = %v, want %v", stageNames(writers.chat), want)
	}
	if want := []string{"tap", "status", "loop", "guard-out", "wire", "cost", "stt-remap", "stt-sse"}; !reflect.DeepEqual(stageNames(writers.converted), want) {
		t.Errorf("converted /v1/messages writer stages = %v, want %v", stageNames(writers.converted), want)
	}
	// Native /v1/messages is Messages-shaped end to end: the capture records
	// the backend's reply, the tee watches a translated copy, and the hold
	// sees the tee's ending as any other.
	if want := []string{"tap", "status", "loop-tee", "guard-hold"}; !reflect.DeepEqual(stageNames(writers.native), want) {
		t.Errorf("native /v1/messages writer stages = %v, want %v", stageNames(writers.native), want)
	}
	// The request stages come first, so the slice is the order they run in.
	seenCand := false
	for _, s := range body {
		if !s.request {
			seenCand = true
		} else if seenCand {
			t.Errorf("request stage %q after a candidate stage", s.name)
		}
	}
}

// A native /v1/messages request skips the stages that assume the OpenAI
// shape; a converted one reaches them, after wire-in, so clamp reads the chat
// body.
func TestChainAppliesAtEachPosition(t *testing.T) {
	const in = `{"model":"m","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`
	backend := config.Model{MaxTokens: 100}
	run := func(api string) (*chainCtx, []string) {
		c := &chainCtx{
			r:       httptest.NewRequest("POST", "/v1/messages", strings.NewReader(in)),
			body:    []byte(in),
			backend: backend,
			pr:      &proc.Process{Target: &config.ProxyTarget{API: api}},
		}
		ran, refused := runBody(candidateStages(), c, false)
		if refused != nil {
			t.Fatalf("%s: refused %d %s", api, refused.status, refused.body)
		}
		return c, ran
	}

	_, ran := run(config.APIAnthropic)
	if want := []string{"clamp", "wire-out"}; !reflect.DeepEqual(ran, want) {
		t.Errorf("native /v1/messages ran %v, want %v", ran, want)
	}

	c, ran := run("")
	if want := []string{"wire-in", "clamp", "sampling"}; !reflect.DeepEqual(ran, want) {
		t.Errorf("converted /v1/messages ran %v, want %v", ran, want)
	}
	var got map[string]any
	if err := json.Unmarshal(c.body, &got); err != nil {
		t.Fatal(err)
	}
	if got["max_tokens"] != float64(100) {
		t.Errorf("max_tokens = %v, want the cap 100: %s", got["max_tokens"], c.body)
	}
	if fmt.Sprint(got["messages"]) != "[map[content:hi role:user]]" {
		t.Errorf("body is not the chat shape: %s", c.body)
	}
}

func stageNames(stages []WriterStage) []string {
	var n []string
	for _, s := range stages {
		n = append(n, s.name)
	}
	return n
}

// The writers that apply on each path, backend outward, with loop detection
// and a response guardrail on and a streamed text request.
func TestWriterOrderPerPath(t *testing.T) {
	_, writers := candidateChain()
	p := &Proxy{}
	p.cfg.Store(&config.Config{LoopDetection: &config.LoopDetection{}})
	for _, tc := range []struct {
		name, path, api string
		want            []string
	}{
		{"chat to OpenAI", "/v1/chat/completions", "", []string{"tap", "status", "loop", "guard-out", "cost"}},
		{"chat to anthropic", "/v1/chat/completions", config.APIAnthropic, []string{"tap", "wire", "status", "loop", "guard-out", "cost"}},
		{"converted /v1/messages", "/v1/messages", "", []string{"tap", "status", "loop", "guard-out", "wire"}},
		{"native /v1/messages", "/v1/messages", config.APIAnthropic, []string{"tap", "status", "loop-tee", "guard-hold"}},
	} {
		c := &chainCtx{p: p, r: httptest.NewRequest("POST", tc.path, nil), streaming: true,
			guards: []config.ActiveGuard{{Name: "g", Hard: true}},
			pr:     &proc.Process{Target: &config.ProxyTarget{API: tc.api}}}
		var got []string
		for _, s := range writers.forPath(c) {
			if s.applies(c) {
				got = append(got, s.name)
			}
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: writers %v, want %v", tc.name, got, tc.want)
		}
	}
}

func candidateStages() []BodyStage {
	body, _ := candidateChain()
	return body
}

// clamp still lands on a /v1/messages body translated for an OpenAI backend:
// the body the backend gets carries the cap.
func TestClampLandsOnConvertedMessages(t *testing.T) {
	f := newFakeOpenAIChat(t)
	m := modelTo(t, f.srv.URL, "chat")
	m.MaxTokens = 64
	h, _ := refusalRig(t, map[string]config.Model{"chat": m}, nil)
	rec := postMessages(h, fmt.Sprintf(claudeCodeBody, "chat", false), map[string]string{"x-api-key": "k"})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.body["max_tokens"] != float64(64) {
		t.Errorf("backend max_tokens = %v, want 64 (body asked 1000)", f.body["max_tokens"])
	}
	if _, ok := f.body["messages"]; !ok || f.body["system"] != nil {
		t.Errorf("backend body is not the chat shape: %v", f.body)
	}
}
