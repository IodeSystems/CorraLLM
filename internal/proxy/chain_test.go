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
	var bn, wn []string
	for _, s := range body {
		bn = append(bn, s.name)
	}
	for _, s := range writers {
		wn = append(wn, s.name)
	}
	if want := []string{"guard-in", "wire-in", "clamp", "model", "sampling", "wire-out"}; !reflect.DeepEqual(bn, want) {
		t.Errorf("body stages = %v, want %v", bn, want)
	}
	// Backend outward.
	if want := []string{"wire", "status", "loop", "guard-out", "cost", "stt-remap", "stt-sse"}; !reflect.DeepEqual(wn, want) {
		t.Errorf("writer stages = %v, want %v", wn, want)
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
