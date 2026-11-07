package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// typeUpstreamFunds is error.type when a REMOTE upstream refuses because the
// account corrallm pays with is out of funds or over its billing quota. It is
// not the caller's balance: corrallm's own budget refusals are budget_exhausted,
// allocation_exhausted and allocation_spent. Published in /v1/capabilities.
const typeUpstreamFunds = "upstream_funds_exhausted"

// fundsPeekLimit caps how much of an upstream error body is read to classify it.
const fundsPeekLimit = 8 << 10

// fundsMarkers are lower-case phrases that mark a billing refusal in a body.
// OpenAI: insufficient_quota (code and type), billing_hard_limit_reached.
// Anthropic: "Your credit balance is too low to access the Anthropic API"
// (400 invalid_request_error). A plain rate limit carries none of these.
var fundsMarkers = []string{
	"insufficient_quota",
	"billing_hard_limit_reached",
	"credit balance is too low",
	"exceeded your current quota",
}

// fundsRefusal is a classified upstream billing refusal.
type fundsRefusal struct {
	Provider string
	Status   int    // the upstream's status
	Upstream string // the upstream's own message
}

// Message is the plain-words text for the caller and the activity row.
func (f fundsRefusal) Message() string {
	return fmt.Sprintf("not your balance: the %s account corrallm pays with is out of funds (upstream %d: %s)",
		f.Provider, f.Status, f.Upstream)
}

// classifyFunds reports whether an upstream answer is a billing refusal.
// Any 402 is one. A 400, 403 or 429 is one only when its body carries a
// billing marker, so an ordinary rate limit keeps its cool-down handling.
func classifyFunds(status int, body []byte) bool {
	switch status {
	case http.StatusPaymentRequired:
		return true
	case http.StatusBadRequest, http.StatusForbidden, http.StatusTooManyRequests:
		low := strings.ToLower(string(body))
		for _, m := range fundsMarkers {
			if strings.Contains(low, m) {
				return true
			}
		}
	}
	return false
}

// peekFunds classifies resp without consuming it: the first bytes are read and
// put back in front of the rest. Only statuses that can be a billing refusal
// are read.
func peekFunds(resp *http.Response, provider string) (fundsRefusal, bool) {
	switch resp.StatusCode {
	case http.StatusPaymentRequired, http.StatusBadRequest, http.StatusForbidden, http.StatusTooManyRequests:
	default:
		return fundsRefusal{}, false
	}
	var head []byte
	if resp.Body != nil && resp.Header.Get("Content-Encoding") == "" {
		head, _ = io.ReadAll(io.LimitReader(resp.Body, fundsPeekLimit))
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(head), resp.Body), resp.Body}
	}
	if !classifyFunds(resp.StatusCode, head) {
		return fundsRefusal{}, false
	}
	return fundsRefusal{Provider: provider, Status: resp.StatusCode, Upstream: upstreamErrorText(head)}, true
}

// rewrite turns resp into the 402 the caller gets.
func (f fundsRefusal) rewrite(resp *http.Response) {
	body, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message":         f.Message(),
		"type":            typeUpstreamFunds,
		"provider":        f.Provider,
		"upstream_status": f.Status,
	}})
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	resp.StatusCode = http.StatusPaymentRequired
	resp.Status = "402 Payment Required"
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Retry-After")
}

// alertFunds tells the owner a paid upstream is out of funds, once per provider
// per throttle window.
func (p *Proxy) alertFunds(f fundsRefusal) {
	p.alert.Send("funds/"+f.Provider, f.Provider+" is out of funds",
		fmt.Sprintf("**%s** refused with **%d**: %s. Requests to it fail until the account is topped up.",
			f.Provider, f.Status, f.Upstream), "money_with_wings")
}

// spillNote is the activity row's Error for a refusal that moved the request
// to the next candidate.
func spillNote(status int, funds string) string {
	if funds != "" {
		return funds + "; tried the next model"
	}
	return fmt.Sprintf("upstream %d; tried the next model", status)
}
