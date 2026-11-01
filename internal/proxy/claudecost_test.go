package proxy

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	shim "github.com/iodesystems/openai-claude-api-shim"

	"github.com/iodesystems/corrallm/internal/config"
)

// A Claude reply with a cache write. Sonnet 5.5: $2 in, $2.50 write, $0.10 read,
// $10 out, per million.
const claudeReplyJSON = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5-5",
	"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",
	"usage":{"input_tokens":1000,"cache_creation_input_tokens":2000,"cache_read_input_tokens":3000,"output_tokens":100}}`

var claudeReplyUSD = (1000*2 + 2000*2.5 + 3000*0.10 + 100*10) / 1e6

func fakeClaude(t *testing.T, reply string, seen *map[string]any) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if seen != nil {
			_ = json.Unmarshal(b, seen)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(up.Close)
	return up
}

const passthroughBody = `{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`

func TestAPassthroughIsPricedAsAnEstimate(t *testing.T) {
	treatAsRemote(t)
	up := fakeClaude(t, claudeReplyJSON, nil)
	h, st := refusalRig(t, map[string]config.Model{"claude": claudeModel(t, up.URL, "")}, nil)
	rec := postMessages(h, passthroughBody, map[string]string{"X-Corrallm-Key": "k"})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if row := onlyRow(t, st); math.Abs(row.CostUSD-claudeReplyUSD) > 1e-12 {
		t.Errorf("cost = %g, want %g, cache writes at the write rate", row.CostUSD, claudeReplyUSD)
	}
}

// The chat path gets its estimate from the shim, and the caller is told it is one.
// replyCost must not read it as a provider's bill.
func TestAnEstimateIsNotCalledAProviderCost(t *testing.T) {
	treatAsRemote(t)
	up := fakeClaude(t, claudeReplyJSON, nil)
	h, st := refusalRig(t, map[string]config.Model{"claude": claudeModel(t, up.URL, "")}, nil)
	rec := post(h, "claude")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{`"cost_estimated":true`, `"cost_source":"estimate"`} {
		if !strings.Contains(body, want) {
			t.Errorf("reply lacks %s: %s", want, body)
		}
	}
	if row := onlyRow(t, st); math.Abs(row.CostUSD-claudeReplyUSD) > 1e-12 {
		t.Errorf("cost = %g, want %g", row.CostUSD, claudeReplyUSD)
	}
}

func TestACallerOwnSignInCostsCorrallmNothing(t *testing.T) {
	treatAsRemote(t)
	up := fakeClaude(t, claudeReplyJSON, nil)
	h, st := refusalRig(t, map[string]config.Model{"claude": claudeModel(t, up.URL, "callerAuth: true\n")}, nil)
	rec := postMessages(h, passthroughBody, map[string]string{"X-Corrallm-Key": "k", "Authorization": "Bearer sub-token"})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if row := onlyRow(t, st); row.CostUSD != 0 {
		t.Errorf("cost = %g: the caller's subscription would be charged to a corrallm budget", row.CostUSD)
	}
}

// A local model behind a Claude-named lane answers in Claude's shape under that
// name. It is not Claude and gets no Claude price.
func TestALocalModelUnderAClaudeNameIsNotPriced(t *testing.T) {
	treatAsRemote(t)
	f := newFakeOpenAIChat(t)
	h, st := refusalRig(t, map[string]config.Model{"claude-sonnet-5-5": modelTo(t, f.srv.URL, "chat")}, nil)
	rec := postMessages(h, strings.Replace(passthroughBody, `"claude"`, `"claude-sonnet-5-5"`, 1), map[string]string{"X-Corrallm-Key": "k"})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if row := onlyRow(t, st); row.CostUSD != 0 {
		t.Errorf("cost = %g", row.CostUSD)
	}
}

func TestClaudePricesOverrideTheShim(t *testing.T) {
	own := config.ClaudePrices{"claude-sonnet-5-5": {Input: 1, Write5m: 1, Read: 1, Output: 1,
		Long: &config.ClaudePrice{Input: 9}, LongOver: 5}}.Shim()
	if own["claude-sonnet-5-5"].Long == nil || own["claude-sonnet-5-5"].Long.Input != 9 {
		t.Fatalf("own = %+v", own)
	}
	u := withClaudeEstimate(usage{}, []byte(claudeReplyJSON), false, shim.Prices{"claude-sonnet-5-5": {Input: 1, Write5m: 1, Read: 1, Output: 1}})
	if want := 6100.0 / 1e6; !u.CostEstimated || math.Abs(u.Cost-want) > 1e-12 {
		t.Errorf("usage = %+v, want cost %g", u, want)
	}
	if config.ClaudePrices(nil).Shim() != nil {
		t.Error("no table must give nil")
	}
}

// corrallm's own default, not the shim's: this is what a paid request is sized by.
func TestAClaudeRequestKeepsCorrallmsDefaultMaxTokens(t *testing.T) {
	var seen map[string]any
	up := fakeClaude(t, claudeReplyJSON, &seen)
	treatAsRemote(t)
	h, _ := refusalRig(t, map[string]config.Model{"claude": claudeModel(t, up.URL, "")}, nil)
	if rec := post(h, "claude"); rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if got := seen["max_tokens"]; got != float64(anthropicDefaultMaxTokens) {
		t.Errorf("max_tokens = %v", got)
	}
}

// Live conversations hold blocks with this signature. If it changes they reach
// Anthropic, which refuses the request.
func TestTheThinkingSignatureDoesNotChange(t *testing.T) {
	if thinkingSigCorrallm != "corrallm" {
		t.Fatalf("signature = %q", thinkingSigCorrallm)
	}
}
