package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/store"
)

// Guardrails (P67), input surface: the newest user turn of a chat or
// completion request is checked by each guardrail the caller's root key holds
// it to, before the request is admitted. A hard fail refuses it; a soft fail
// is flagged in a response header and the request goes on.

// Response headers. HeaderGuardrail reports each check that ran
// (`violence=pass;score=0.031, pii=flag;score=0.712`); HeaderGuardrailIgnored
// names asks in X-Corrallm-Guardrails that changed nothing.
const (
	HeaderGuardrail        = "X-Corrallm-Guardrail"
	HeaderGuardrailIgnored = "X-Corrallm-Guardrail-Ignored"
)

// guardSkipKey marks a guard's own call, so it is not checked in turn.
type guardSkipKey struct{}

// guardSystem is the guard's fixed instruction. Shieldstral is trained on it:
// it answers one token, yes (the document meets the query) or no.
const guardSystem = `Judge whether the Document meets the requirements based on the Query ` +
	`and the Instruction provided. Note that the answer can only be "yes" or "no".`

// guardVerdict is the result of one guardrail on one piece of content.
type guardVerdict struct {
	Name      string
	Hard      bool
	Score     float64 // P(yes): the content meets the policy, so it fails
	Threshold float64
	Failed    bool
	// Unavailable: the guard could not be asked. Status and RetryAfter are
	// its answer, for a hard guard to pass on.
	Unavailable string
	Status      int
	RetryAfter  string
	// FailStatus is the status a hard fail replies with.
	FailStatus int
}

func (v guardVerdict) header() string {
	switch {
	case v.Unavailable != "":
		return v.Name + "=unavailable"
	case v.Failed && v.Hard:
		return fmt.Sprintf("%s=block;score=%.3f", v.Name, v.Score)
	case v.Failed:
		return fmt.Sprintf("%s=flag;score=%.3f", v.Name, v.Score)
	}
	return fmt.Sprintf("%s=pass;score=%.3f", v.Name, v.Score)
}

// guardInput is the content a request adds: the newest user message of a chat
// request (its text, and its images as content parts), or the prompt of a
// completion request. Earlier turns were checked when they were sent.
func guardInput(path string, body []byte) (text string, images []json.RawMessage, ok bool) {
	switch path {
	case "/v1/chat/completions":
		var req struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if json.Unmarshal(body, &req) != nil {
			return "", nil, false
		}
		for i := len(req.Messages) - 1; i >= 0; i-- {
			if req.Messages[i].Role != "user" {
				continue
			}
			c := req.Messages[i].Content
			var s string
			if json.Unmarshal(c, &s) == nil {
				return s, nil, s != ""
			}
			var parts []json.RawMessage
			if json.Unmarshal(c, &parts) != nil {
				return "", nil, false
			}
			var texts []string
			for _, raw := range parts {
				var part struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				_ = json.Unmarshal(raw, &part)
				switch part.Type {
				case "text":
					texts = append(texts, part.Text)
				case "image_url":
					images = append(images, raw)
				}
			}
			text = strings.Join(texts, "\n")
			return text, images, text != "" || len(images) > 0
		}
	case "/v1/completions":
		var req struct {
			Prompt json.RawMessage `json:"prompt"`
		}
		if json.Unmarshal(body, &req) != nil {
			return "", nil, false
		}
		var s string
		if json.Unmarshal(req.Prompt, &s) == nil {
			return s, nil, s != ""
		}
		var list []string
		if json.Unmarshal(req.Prompt, &list) == nil {
			s = strings.Join(list, "\n")
			return s, nil, s != ""
		}
	}
	return "", nil, false
}

// guardBody is the guard's request: policy and document in Shieldstral's
// layout, with the document LAST so a guard's prompt cache reuses everything
// before new text. One output token, with its top logprobs for the score.
// label names who wrote the document: "User" or "Assistant".
func guardBody(g config.Guardrail, label, text string, images []json.RawMessage) []byte {
	head := "<Instruct>: " + g.InstructText() + "\n\n<Query>: " + g.Policy + "\n\n<Document>: "
	var content any = head + "[" + label + "] " + text
	if len(images) > 0 {
		parts := []any{map[string]string{"type": "text", "text": head}}
		for _, img := range images {
			parts = append(parts, img)
		}
		content = append(parts, map[string]string{"type": "text", "text": " [" + label + "] " + text})
	}
	b, _ := json.Marshal(map[string]any{
		"model": g.Model,
		"messages": []any{
			map[string]any{"role": "system", "content": guardSystem},
			map[string]any{"role": "user", "content": content},
		},
		"max_tokens":   1,
		"temperature":  0,
		"logprobs":     true,
		"top_logprobs": 20,
		"stream":       false,
	})
	return b
}

// guardScore reads P(yes) from a guard's reply: yes and no renormalised over
// the first token's top logprobs, else the token itself.
func guardScore(reply []byte) (float64, error) {
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Logprobs *struct {
				Content []struct {
					TopLogprobs []struct {
						Token   string  `json:"token"`
						Logprob float64 `json:"logprob"`
					} `json:"top_logprobs"`
				} `json:"content"`
			} `json:"logprobs"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(reply, &out); err != nil || len(out.Choices) == 0 {
		return 0, fmt.Errorf("guard reply is not a chat completion")
	}
	ch := out.Choices[0]
	if ch.Logprobs != nil && len(ch.Logprobs.Content) > 0 {
		yes, no := math.Inf(-1), math.Inf(-1)
		for _, t := range ch.Logprobs.Content[0].TopLogprobs {
			switch strings.Trim(strings.ToLower(strings.TrimSpace(t.Token)), `."'`) {
			case "yes":
				yes = math.Max(yes, t.Logprob)
			case "no":
				no = math.Max(no, t.Logprob)
			}
		}
		if !math.IsInf(yes, -1) || !math.IsInf(no, -1) {
			return math.Exp(yes) / (math.Exp(yes) + math.Exp(no)), nil
		}
	}
	switch strings.Trim(strings.ToLower(strings.TrimSpace(ch.Message.Content)), `."'`) {
	case "yes":
		return 1, nil
	case "no":
		return 0, nil
	}
	return 0, fmt.Errorf("guard answered %q, not yes or no", ch.Message.Content)
}

// bufferedResponse collects an in-process reply.
type bufferedResponse struct {
	h      http.Header
	status int
	buf    bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header         { return b.h }
func (b *bufferedResponse) Write(p []byte) (int, error) { return b.buf.Write(p) }
func (b *bufferedResponse) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}

// runGuard asks one guardrail about the content. The guard is called through
// corrallm itself, with the caller's own credential, so it is scheduled,
// loaded, swapped and charged like any other request by that caller.
func (p *Proxy) runGuard(r *http.Request, g config.ActiveGuard, label, text string, images []json.RawMessage, complete bool) guardVerdict {
	v := guardVerdict{Name: g.Name, Hard: g.Hard, Threshold: g.Guard.CompleteThreshold()}
	if !complete {
		v.Threshold = g.Guard.IncompleteThreshold()
	}
	ctx := context.WithValue(r.Context(), guardSkipKey{}, true)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions",
		bytes.NewReader(guardBody(g.Guard, label, text, images)))
	if err != nil {
		v.Unavailable = err.Error()
		return v
	}
	for _, h := range []string{"Authorization", "X-Corrallm-Key"} {
		if s := r.Header.Get(h); s != "" {
			req.Header.Set(h, s)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = r.RemoteAddr
	rec := &bufferedResponse{h: http.Header{}}
	p.handleInference(rec, req)
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	if rec.status != http.StatusOK {
		v.Unavailable = fmt.Sprintf("guard %s answered %d", g.Guard.Model, rec.status)
		v.Status, v.RetryAfter = rec.status, rec.h.Get("Retry-After")
		return v
	}
	score, err := guardScore(rec.buf.Bytes())
	if err != nil {
		v.Unavailable = err.Error()
		return v
	}
	v.Score = score
	v.Failed = score >= v.Threshold
	return v
}

// checkInput runs the caller's guardrails on what the request adds. It writes
// the reply and returns ok false when a hard guardrail refuses the request.
// active is the guardrails the reply is held to as well.
func (p *Proxy) checkInput(w http.ResponseWriter, r *http.Request, caller config.Caller,
	served string, body []byte, start time.Time, reqBody string) (active []config.ActiveGuard, ok bool) {
	if r.Context().Value(guardSkipKey{}) != nil {
		return nil, true
	}
	root := caller.Key
	if caller.Parent != "" {
		root = caller.Parent
	}
	active, ignored := p.config().ActiveGuardrails(root, r.Header.Get(config.HeaderGuardrails))
	if len(ignored) > 0 {
		w.Header().Set(HeaderGuardrailIgnored, strings.Join(ignored, ", "))
	}
	if len(active) == 0 {
		return nil, true
	}
	text, images, has := guardInput(r.URL.Path, body)
	if !has {
		return active, true
	}
	var report []string
	for _, g := range active {
		v := p.runGuard(r, g, "User", text, images, true)
		report = append(report, v.header())
		w.Header().Set(HeaderGuardrail, strings.Join(report, ", "))
		switch {
		case v.Unavailable != "" && v.Hard:
			p.writeGuardrailUnavailable(w, r, served, caller.Key, v, start, reqBody)
			return nil, false
		case v.Unavailable != "":
			slog.Warn("soft guardrail could not run; request passed", "guardrail", v.Name, "key", caller.Key, "err", v.Unavailable)
		case v.Failed && v.Hard:
			p.writeGuardrailFail(w, r, served, caller.Key, g.Guard.FailStatus(), v, "input", start, reqBody)
			return nil, false
		case v.Failed:
			slog.Warn("soft guardrail flagged a request", "guardrail", v.Name, "key", caller.Key,
				"surface", "input", "score", v.Score, "threshold", v.Threshold)
		}
	}
	return active, true
}

// writeGuardrailFail refuses a request a hard guardrail failed: the
// guardrail's status (422 by default) with an OpenAI-shaped error that names
// the guard, so a caller can tell a policy refusal from a broken request.
func (p *Proxy) writeGuardrailFail(w http.ResponseWriter, r *http.Request, served, key string,
	status int, v guardVerdict, surface string, start time.Time, reqBody string) {
	msg, b := guardrailFailBody(v, surface)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(b)
	p.logReq(r, store.Activity{Served: served, Requested: served, Key: key,
		Path: r.URL.Path, Status: status, DwellMS: time.Since(start).Milliseconds(), Error: msg, ReqBody: reqBody})
}

// guardrailFailBody is a hard fail's message and OpenAI-shaped error body.
func guardrailFailBody(v guardVerdict, surface string) (string, []byte) {
	msg := fmt.Sprintf("refused by guardrail %q on the %s (score %.3f >= %.3f)", v.Name, surface, v.Score, v.Threshold)
	b, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message":   msg,
		"type":      "guardrail",
		"code":      "content_filter",
		"guardrail": v.object(surface),
	}})
	return msg, b
}

// object is the guardrail object of a refusal: in a 422 body, or on the
// content_filter chunk that ends a refused stream.
func (v guardVerdict) object(surface string) map[string]any {
	o := map[string]any{"name": v.Name, "surface": surface, "mode": "hard"}
	if v.Unavailable != "" {
		o["unavailable"] = v.Unavailable
	} else {
		o["score"], o["threshold"] = v.Score, v.Threshold
	}
	return o
}

// writeGuardrailUnavailable refuses a request a hard guardrail could not
// check: it fails closed. A guard that was busy (429) passes its Retry-After
// on; anything else is a 503.
func (p *Proxy) writeGuardrailUnavailable(w http.ResponseWriter, r *http.Request, served, key string,
	v guardVerdict, start time.Time, reqBody string) {
	status := http.StatusServiceUnavailable
	var retryMS int64
	if v.Status == http.StatusTooManyRequests {
		status = http.StatusTooManyRequests
		if v.RetryAfter != "" {
			w.Header().Set("Retry-After", v.RetryAfter)
			if s, err := strconv.Atoi(v.RetryAfter); err == nil {
				retryMS = int64(s) * 1000
			}
		}
	}
	msg := fmt.Sprintf("guardrail %q could not check this request: %s", v.Name, v.Unavailable)
	b, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message":   msg,
		"type":      "guardrail",
		"code":      "guardrail_unavailable",
		"guardrail": map[string]any{"name": v.Name, "mode": "hard"},
	}})
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(b)
	p.logReq(r, store.Activity{Served: served, Requested: served, Key: key,
		Path: r.URL.Path, Status: status, DwellMS: time.Since(start).Milliseconds(), Error: msg,
		ReqBody: reqBody, RetryAfterMS: retryMS})
}
