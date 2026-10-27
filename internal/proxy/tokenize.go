package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/iodesystems/corrallm/internal/config"
)

// POST /v1/tokenize and /v1/detokenize (also at /tokenize and /detokenize,
// where vLLM mounts them). Two request shapes, told apart by their fields:
//
//	vLLM:      {model, prompt | messages, add_special_tokens?, return_token_strs?}
//	           → {count, max_model_len, tokens, token_strs?}
//	llama.cpp: {model, content, add_special?, with_pieces?}
//	           → llama.cpp's own reply, unchanged
//
// A local model answers from its own tokenizer: the llama.cpp backend's
// /tokenize, with messages first rendered by its /apply-template, so the count
// includes the chat template. The call loads the model if it is not loaded.
//
// A remote provider has no tokenizer route, so a remote model gets an ESTIMATE
// — about 4 characters a token — marked "estimated": true, with a count and no
// token ids. Detokenize needs real ids and is refused for a remote model.
//
// Untracked, as /v1/messages/count_tokens and /upstream/ are: no admission
// slot, no activity row, no spend. A caller that sizes a prompt before sending
// it must not queue behind the backlog it is measuring against.

// tokenizeReq is both shapes. Pointers tell "absent" from false.
type tokenizeReq struct {
	Model    string            `json:"model"`
	Prompt   *string           `json:"prompt"`
	Messages []json.RawMessage `json:"messages"`
	Content  *string           `json:"content"`
	// vLLM: default true for a prompt, false for messages (the template adds them).
	AddSpecialTokens *bool `json:"add_special_tokens"`
	ReturnTokenStrs  bool  `json:"return_token_strs"`
}

func (q tokenizeReq) vllm() bool { return q.Prompt != nil || q.Messages != nil }

func (p *Proxy) handleTokenize(w http.ResponseWriter, r *http.Request) {
	body, q, ok := readTokenizeBody(w, r)
	if !ok {
		return
	}
	if !q.vllm() && q.Content == nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error",
			`send "prompt" or "messages" (vLLM shape) or "content" (llama.cpp shape)`)
		return
	}
	name, model, ok := p.tokenizerModel(w, q.Model)
	if !ok {
		return
	}
	if isRemote(model) {
		jsonResp(w, http.StatusOK, map[string]any{"count": estimateTokens(q), "estimated": true,
			"note": "remote provider: no tokenizer route; count is characters/4, not exact"})
		return
	}
	target, done, ok := p.tokenizerTarget(w, r, name, model)
	if !ok {
		return
	}
	defer done()

	if !q.vllm() {
		forwardTokenizer(w, r, target, "/tokenize", body)
		return
	}
	text := ""
	addSpecial := q.Messages == nil
	if q.AddSpecialTokens != nil {
		addSpecial = *q.AddSpecialTokens
	}
	if q.Messages != nil {
		// The whole body: apply-template reads tools and template kwargs too.
		var tpl struct {
			Prompt string `json:"prompt"`
		}
		if !callTokenizer(w, r, target, "/apply-template", body, &tpl) {
			return
		}
		text = tpl.Prompt
	} else {
		text = *q.Prompt
	}
	in, _ := json.Marshal(map[string]any{"content": text, "add_special": addSpecial, "with_pieces": q.ReturnTokenStrs})
	var got struct {
		Tokens []json.RawMessage `json:"tokens"`
	}
	if !callTokenizer(w, r, target, "/tokenize", in, &got) {
		return
	}
	ids := make([]int, 0, len(got.Tokens))
	var strs []string
	for _, t := range got.Tokens {
		if !q.ReturnTokenStrs {
			var id int
			_ = json.Unmarshal(t, &id)
			ids = append(ids, id)
			continue
		}
		// with_pieces: {id, piece}; piece is a byte list when not valid UTF-8.
		var pc struct {
			ID    int             `json:"id"`
			Piece json.RawMessage `json:"piece"`
		}
		_ = json.Unmarshal(t, &pc)
		var s string
		_ = json.Unmarshal(pc.Piece, &s)
		ids, strs = append(ids, pc.ID), append(strs, s)
	}
	out := map[string]any{"count": len(ids), "max_model_len": p.contextLen(name, model), "tokens": ids}
	if q.ReturnTokenStrs {
		out["token_strs"] = strs
	}
	jsonResp(w, http.StatusOK, out)
}

// handleDetokenize turns token ids back into text. Both shapes send
// {model, tokens}; the reply carries the text as "prompt" (vLLM) and as
// "content" (llama.cpp), so either client reads it.
func (p *Proxy) handleDetokenize(w http.ResponseWriter, r *http.Request) {
	body, q, ok := readTokenizeBody(w, r)
	if !ok {
		return
	}
	name, model, ok := p.tokenizerModel(w, q.Model)
	if !ok {
		return
	}
	if isRemote(model) {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error",
			"model \""+name+"\" is a remote provider; it has no tokenizer route, so its token ids cannot be turned back into text")
		return
	}
	var t struct {
		Tokens []int `json:"tokens"`
	}
	if err := json.Unmarshal(body, &t); err != nil || t.Tokens == nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error", `send "tokens": a list of token ids`)
		return
	}
	target, done, ok := p.tokenizerTarget(w, r, name, model)
	if !ok {
		return
	}
	defer done()
	in, _ := json.Marshal(map[string]any{"tokens": t.Tokens})
	var got struct {
		Content string `json:"content"`
	}
	if !callTokenizer(w, r, target, "/detokenize", in, &got) {
		return
	}
	jsonResp(w, http.StatusOK, map[string]any{"prompt": got.Content, "content": got.Content})
}

// readTokenizeBody reads and decodes the request.
func readTokenizeBody(w http.ResponseWriter, r *http.Request) ([]byte, tokenizeReq, bool) {
	var q tokenizeReq
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "invalid_request_error", "use POST")
		return nil, q, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error", "unreadable body (limit 32 MiB)")
		return nil, q, false
	}
	if err := json.Unmarshal(body, &q); err != nil || strings.TrimSpace(q.Model) == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error", `body must be JSON with a "model" field`)
		return nil, q, false
	}
	return body, q, true
}

// tokenizerModel resolves one concrete model with text input. A lane is
// refused: its members have different tokenizers, and a count from the first
// would be attributed to a model the caller did not name.
func (p *Proxy) tokenizerModel(w http.ResponseWriter, served string) (string, config.Model, bool) {
	cands, ok := p.config().ResolveServed(served)
	if !ok || len(cands) == 0 || cands[0].Model.Disabled {
		writeJSONError(w, http.StatusNotFound, "invalid_request_error", p.noSuchModel(served))
		return "", config.Model{}, false
	}
	if len(cands) > 1 {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error",
			"tokenize needs a model, not a lane: "+served+" has members with different tokenizers")
		return "", config.Model{}, false
	}
	m := cands[0].Model
	switch config.ModelCapability(m) {
	case "chat", "embeddings", "rerank":
	default:
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error",
			"model \""+cands[0].Name+"\" is a "+config.ModelCapability(m)+" model; it has no text tokenizer")
		return "", config.Model{}, false
	}
	return cands[0].Name, m, true
}

// tokenizerTarget loads the local backend and returns its base URL.
func (p *Proxy) tokenizerTarget(w http.ResponseWriter, r *http.Request, name string, m config.Model) (string, func(), bool) {
	pr, done, _, err := p.mgr.EnsureReady(r.Context(), name, m, nil, nil)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "server_error", "backend unavailable: "+err.Error())
		return "", nil, false
	}
	return strings.TrimRight(pr.Target.URL.String(), "/"), done, true
}

// forwardTokenizer sends body to the backend and copies its reply as it is.
func forwardTokenizer(w http.ResponseWriter, r *http.Request, base, path string, body []byte) {
	resp, err := tokenizerPost(r, base+path, body)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "server_error", "backend "+path+": "+err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// callTokenizer posts body to the backend and decodes a 200 into out. Any
// other reply is relayed and false returned.
func callTokenizer(w http.ResponseWriter, r *http.Request, base, path string, body []byte, out any) bool {
	resp, err := tokenizerPost(r, base+path, body)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "server_error", "backend "+path+": "+err.Error())
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		writeJSONError(w, http.StatusBadGateway, "server_error",
			"backend has no "+path+" route; tokenize needs a llama.cpp backend")
		return false
	}
	if resp.StatusCode != http.StatusOK {
		writeJSONError(w, http.StatusBadGateway, "server_error",
			"backend "+path+": "+resp.Status+": "+strings.TrimSpace(string(raw)))
		return false
	}
	if err := json.Unmarshal(raw, out); err != nil {
		writeJSONError(w, http.StatusBadGateway, "server_error", "backend "+path+": unreadable reply")
		return false
	}
	return true
}

func tokenizerPost(r *http.Request, url string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}

// contextLen is the per-request window when declared, else the loaded
// backend's n_ctx, else 0 (unknown).
func (p *Proxy) contextLen(name string, m config.Model) int {
	if m.ContextPerRequest > 0 {
		return m.ContextPerRequest
	}
	key := m.ProcKey(name)
	for _, rm := range p.mgr.Snapshot().Models {
		if rm.ProcKey == key && rm.NCtx > 0 {
			return rm.NCtx
		}
	}
	return 0
}

// estimateTokens is about 4 characters a token, plus 4 tokens of template per
// message. Rough on purpose; it is labelled an estimate.
func estimateTokens(q tokenizeReq) int {
	chars := 0
	switch {
	case q.Prompt != nil:
		chars = len(*q.Prompt)
	case q.Content != nil:
		chars = len(*q.Content)
	}
	n := 0
	for _, raw := range q.Messages {
		var m struct {
			Content json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(raw, &m)
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			chars += len(s)
		} else {
			var parts []struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(m.Content, &parts)
			for _, pt := range parts {
				chars += len(pt.Text)
			}
		}
		n += 4
	}
	return n + (chars+3)/4
}
