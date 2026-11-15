package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/sched"
)

// KV-POOL ADMISSION, proxy side (sched/kvpool.go has the rule).
//
// kvAsk sizes a request's KV need before admission; the reply's KV-full
// failure is re-queued once (kvFull).

// kvAsk is the request's expected KV need on a model with kvPool, nil
// without one. Prompt: the larger of the body at bytesPerTokenEstimate and
// the conversation's previous turn (prompt + completion: a next turn starts
// at least about there). Output reserve: min(max_tokens or
// max_completion_tokens when sent, kvPool.reserveTokens). The sum is capped
// at contextPerRequest, which llama-server enforces per slot
// (--kv-unified-per-slot), so no request needs more.
func (p *Proxy) kvAsk(r *http.Request, backend config.Model, body []byte) *sched.KVAsk {
	k := backend.KVPool
	if k == nil {
		return nil
	}
	prompt := len(body) / bytesPerTokenEstimate
	if c, ok := r.Context().Value(conversationCtxKey{}).(conversation); ok {
		if prev := p.cacheTracker.LastContext(c.id, time.Now().UnixMilli()); prev > prompt {
			prompt = prev
		}
	}
	reserve := k.Reserve()
	if mt := requestMaxTokens(body); mt > 0 && mt < reserve {
		reserve = mt
	}
	need := prompt + reserve
	if backend.ContextPerRequest > 0 && need > backend.ContextPerRequest {
		need = backend.ContextPerRequest
	}
	return &sched.KVAsk{Tokens: need, Pool: k.Tokens, Bypass: k.Bypass()}
}

// requestMaxTokens is the body's max_tokens, else max_completion_tokens; 0
// when neither is a positive number.
func requestMaxTokens(body []byte) int {
	var q struct {
		MaxTokens           *int `json:"max_tokens"`
		MaxCompletionTokens *int `json:"max_completion_tokens"`
	}
	if json.Unmarshal(body, &q) != nil {
		return 0
	}
	switch {
	case q.MaxTokens != nil && *q.MaxTokens > 0:
		return *q.MaxTokens
	case q.MaxCompletionTokens != nil && *q.MaxCompletionTokens > 0:
		return *q.MaxCompletionTokens
	}
	return 0
}

// kvFullText is the error llama-server sends every request in a batch it
// cannot fit in the KV cache even at batch size 1, after clearing idle slots
// (server-context.cpp decode(): n_batch == 1 && ret == 1). It is a
// server_error, HTTP 500, as a JSON body before the first token, or as an SSE
// error event once a stream started (that one is passed through).
const kvFullText = "Context size has been exceeded."

// errKVFull is returned from ModifyResponse for a local backend's KV-full 500
// that may be retried: nothing reached the caller, so the walk re-queues the
// request once on the same backend.
var errKVFull = errors.New("backend KV cache full")

// kvFullPeekLimit bounds how much of a 500 body is read to classify it.
const kvFullPeekLimit = 4 << 10

// peekKVFull reports whether resp is llama-server's KV-full failure. The
// peeked bytes are put back, so a reply that is not one passes through whole.
func peekKVFull(resp *http.Response) bool {
	if resp.StatusCode != http.StatusInternalServerError || resp.Body == nil || resp.Header.Get("Content-Encoding") != "" {
		return false
	}
	head, _ := io.ReadAll(io.LimitReader(resp.Body, kvFullPeekLimit))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), resp.Body), resp.Body}
	return strings.Contains(upstreamErrorText(head), kvFullText)
}

// kvNeed is a's tokens for a log line; 0 without an ask.
func kvNeed(a *sched.KVAsk) int {
	if a == nil {
		return 0
	}
	return a.Tokens
}
