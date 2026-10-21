package api

import "testing"

func TestThinkingLevel(t *testing.T) {
	for _, c := range []struct{ body, cmd, want string }{
		{`{"reasoning_effort":"low"}`, "", "effort low"},
		{`{"reasoning":{"effort":"high"}}`, "", "effort high"},
		{`{"thinking":{"type":"enabled","budget_tokens":2048}}`, "", "budget 2048 tokens"},
		{`{"thinking":{"type":"disabled"}}`, "", "off"},
		{`{"chat_template_kwargs":{"enable_thinking":false}}`, "", "off"},
		{`{"messages":[]}`, "llama-server --reasoning-budget 0 -m x", "off (--reasoning-budget 0)"},
		{"", `llama-server --chat-template-kwargs '{"enable_thinking": false}'`, "off (enable_thinking false)"},
		{`{"messages":[]}`, "llama-server -m x", "model default"},
	} {
		if got := thinkingLevel(c.body, c.cmd); got != c.want {
			t.Errorf("thinkingLevel(%s, %s) = %q, want %q", c.body, c.cmd, got, c.want)
		}
	}
}

func TestBenchRunStartIsLocalTime(t *testing.T) {
	s, ok := benchRunStart("20261020-081756")
	if !ok || s.Hour() != 8 || s.Minute() != 17 || s.Second() != 56 {
		t.Errorf("start = %v, %v", s, ok)
	}
	if _, ok := benchRunStart("not-a-run"); ok {
		t.Error("parsed garbage")
	}
}
