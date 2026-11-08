package proxy

import (
	"net/http"
	"strings"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	shim "github.com/iodesystems/openai-claude-api-shim"
)

// The candidate chain (P73): what handleInference does to a request's body
// before it goes to a backend, and which writers the backend's reply passes
// through on its way to the caller. The order is the two slices in
// candidateChain and nowhere else; TestCandidateChainOrder pins it.
//
// Each stage has an applies predicate, evaluated at the stage's own position:
// the runner asks it just before it would run, so it sees the body (and the
// chainCtx) as the previous stages left them.
//
// Body stages, in order:
//
//   - guard-in: the caller's guardrails on what the request adds (P67). It
//     can end the request with a stageError. It is the one REQUEST stage: it
//     runs once, on the request body, BEFORE admission, so a refused request
//     does not wait behind a saturated backend to find that out. runBody with
//     request=true runs it from handleInference ahead of the candidate walk;
//     runBody with request=false (per candidate) skips it. Request stages
//     must come first in the slice, so the slice order is still the order
//     they run in.
//   - wire-in: /v1/messages to a non-Anthropic backend, translated to chat
//     completions (shim.ReverseRequest). First of the candidate stages, so
//     every stage after it sees a chat body on that path.
//   - clamp: max_tokens down to the backend's cap (P7).
//   - model: the body's `model` set to the upstream's own id (P16).
//   - sampling: the backend's sampling profile and reasoning budget.
//   - (align: commit 2 of P73 puts the capability-record translation here.)
//   - wire-out: chat completions to an `api: anthropic` backend becomes a
//     Messages body (claudeRequest); a native /v1/messages body loses the
//     thinking blocks corrallm signed (shim.StripThinking).
//
// The native /v1/messages gap: a /v1/messages request to an `api: anthropic`
// backend passes through in Anthropic shape. wire-in skips it, so a stage
// that assumes the OpenAI shape (sampling today, align later) must not apply
// to it. clamp and model work on both shapes because max_tokens and model are
// the same fields in both.
//
// The converted /v1/messages gap: loop and guard-out apply to the chat path
// only. On converted /v1/messages they would sit outside messagesWriter and
// read the Messages shape, so that path runs without them, as before P73.
//
// Writer stages, backend outward: wire adapter → statusCapture → loop →
// guard-out → cost → stt remap → stt SSE. wrapWriters builds them from the
// caller's writer inward, so the first stage here is the one the backend
// writes to.
//
// PDF conversion and audio transcode stay outside the chain: they run once
// per request before the walk, and choose the body a candidate starts from.

// stageError ends a request: the status and body the caller gets, and the
// activity row's error.
type stageError struct {
	status       int
	body         []byte
	msg          string      // the activity row's Error
	header       http.Header // extra response headers (Retry-After); may be nil
	retryAfterMS int64
}

// write sends the refusal to the caller.
func (e *stageError) write(w http.ResponseWriter) {
	for k, v := range e.header {
		w.Header()[k] = v
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(e.status)
	_, _ = w.Write(e.body)
}

// invalidRequest is the 400 writeJSONError sends for a body a stage could not
// translate; msg is the activity row's Error.
func invalidRequest(prefix string, err error) *stageError {
	return &stageError{
		status: http.StatusBadRequest,
		body:   []byte(`{"error":{"message":` + jsonString(err.Error()) + `,"type":"invalid_request_error"}}`),
		msg:    prefix + err.Error(),
		header: http.Header{"X-Content-Type-Options": {"nosniff"}},
	}
}

// chainCtx is what the stages read and set for one request on one candidate.
// guard-in runs before there is a candidate, so it reads only the request
// fields.
type chainCtx struct {
	p      *Proxy
	w      http.ResponseWriter // the caller's writer
	r      *http.Request
	caller config.Caller
	served string
	body   []byte // as the previous stage left it

	guards []config.ActiveGuard // set by guard-in

	// The candidate.
	name      string
	backend   config.Model
	pr        *proc.Process
	candBody  []byte // the candidate's body before any candidate stage
	streaming bool
	image     bool
	audio     bool
	loadCost  float64
	sttReq    sttRequest
	sttJoin   sttDiarizeJoin
	live      *inflightEntry
	cancelReq func(cause error)

	// The writers the writer stages built; nil when a stage did not apply.
	aw       *anthropicWriter
	mw       *messagesWriter
	sc       *statusCapture
	lw       *loopWriter
	gw       *guardOutWriter
	cw       *costWriter
	sttRemap *sttRemapWriter
	sttSSE   *sttStreamWriter
}

func (c *chainCtx) chat() bool     { return c.r.URL.Path == "/v1/chat/completions" }
func (c *chainCtx) messages() bool { return c.r.URL.Path == "/v1/messages" }

func (c *chainCtx) anthropicTarget() bool {
	return c.pr != nil && c.pr.Target != nil && c.pr.Target.API == config.APIAnthropic
}

// anthropic: a chat completions request to an `api: anthropic` backend.
func (c *chainCtx) anthropic() bool { return c.anthropicTarget() && c.chat() }

// nativeMessages: a /v1/messages request to an `api: anthropic` backend.
func (c *chainCtx) nativeMessages() bool { return c.messages() && c.anthropicTarget() }

// convertedMessages: a /v1/messages request to any other backend.
func (c *chainCtx) convertedMessages() bool { return c.messages() && !c.anthropicTarget() }

// BodyStage changes the body bound for a backend, or ends the request.
type BodyStage struct {
	name    string
	request bool // runs once per request, before admission (guard-in)
	applies func(c *chainCtx) bool
	run     func(c *chainCtx) *stageError
}

// WriterStage wraps the writer the backend's reply goes through.
type WriterStage struct {
	name    string
	applies func(c *chainCtx) bool
	wrap    func(c *chainCtx, outer http.ResponseWriter) http.ResponseWriter
}

func always(*chainCtx) bool { return true }

// candidateChain is the order of the stages. See the comment at the top.
func candidateChain() ([]BodyStage, []WriterStage) {
	body := []BodyStage{
		{name: "guard-in", request: true, applies: always, run: func(c *chainCtx) *stageError {
			var refused *stageError
			c.guards, refused = c.p.checkInput(c.w, c.r, c.caller, c.body)
			return refused
		}},
		{name: "wire-in", applies: (*chainCtx).convertedMessages, run: func(c *chainCtx) *stageError {
			ob, err := shim.ReverseRequest(c.body)
			if err != nil {
				return invalidRequest("messages translation: ", err)
			}
			c.body = ob
			return nil
		}},
		{name: "clamp", applies: always, run: func(c *chainCtx) *stageError {
			c.body = clampMaxTokens(c.body, c.backend)
			return nil
		}},
		// Rewrite the body's `model` to the upstream's own id for a remote that
		// declares one (P16): corrallm routed on the served name, but the remote
		// does not know it. A local backend leaves Target.Model empty and the
		// body forwards unchanged.
		{name: "model", applies: func(c *chainCtx) bool { return upstreamID(c.backend, c.pr) != "" }, run: func(c *chainCtx) *stageError {
			upstream := upstreamID(c.backend, c.pr)
			// JSON bodies (chat/embeddings) and multipart bodies (audio) carry the
			// model in different places, and rewriting only the former silently
			// forwarded the served name upstream on every audio request.
			if ct := c.r.Header.Get("Content-Type"); strings.HasPrefix(ct, "multipart/form-data") {
				if nb, nct, ok := rewriteModelMultipart(c.body, ct, upstream); ok {
					c.body = nb
					c.r.Header.Set("Content-Type", nct)
				}
			} else {
				c.body = rewriteModelField(c.body, upstream)
			}
			return nil
		}},
		{name: "sampling", applies: func(c *chainCtx) bool { return c.chat() || c.convertedMessages() }, run: func(c *chainCtx) *stageError {
			c.body = c.p.withSampling(c.body, c.backend, c.name, c.caller)
			return nil
		}},
		// align (P73 commit 2) goes here.
		//
		// Anthropic Messages shim (anthropic.go): a backend whose proxy says
		// `api: anthropic` gets the native Messages body — with prompt-cache
		// breakpoints — and its reply is translated back by the wire adapter.
		// After the model rewrite and the max_tokens clamp, because both are
		// properties of THIS backend and the translation reads the result.
		{name: "wire-out", applies: func(c *chainCtx) bool { return c.anthropic() || c.nativeMessages() }, run: func(c *chainCtx) *stageError {
			if c.nativeMessages() {
				c.body = shim.StripThinking(c.body, thinkingSigCorrallm)
				return nil
			}
			ab, err := claudeRequest(c.body, c.backend, c.pr.Target.CacheTTL)
			if err != nil {
				return invalidRequest("anthropic translation: ", err)
			}
			c.body = ab
			return nil
		}},
	}
	writers := []WriterStage{
		// The Anthropic shim's writer sits OUTSIDE statusCapture, so the capture
		// — usage, finish_reason, the activity row's body — reads the OpenAI
		// shape the caller gets. The /v1/messages writer (messages.go) sits in
		// the same place.
		{name: "wire", applies: func(c *chainCtx) bool { return c.anthropic() || c.convertedMessages() },
			wrap: func(c *chainCtx, out http.ResponseWriter) http.ResponseWriter {
				if c.anthropic() {
					c.aw = newAnthropicWriter(out, c.p.config().ClaudePrices.Shim())
					return c.aw
				}
				c.mw = newMessagesWriter(out, c.served)
				return c.mw
			}},
		{name: "status", applies: always, wrap: func(c *chainCtx, out http.ResponseWriter) http.ResponseWriter {
			c.sc = &statusCapture{ResponseWriter: out, code: http.StatusOK, streaming: c.streaming, live: c.live}
			if c.image {
				// Nothing reads an image reply's body for usage — it has none — and
				// the only body worth retaining is a small JSON error. Retaining
				// the default 1 MiB would hold a megabyte of base64 per in-flight
				// generation for no reader.
				c.sc.bufLimit = errorCaptureLimit
			}
			return c.sc
		}},
		// Before the guardrails: a cut stream ends in events they read as any other.
		{name: "loop", applies: func(c *chainCtx) bool {
			return c.p.config().LoopDetectionFor(c.backend) != nil && c.streaming && c.chat()
		}, wrap: func(c *chainCtx, out http.ResponseWriter) http.ResponseWriter {
			c.lw = newLoopWriter(out, c.p.config().LoopDetectionFor(c.backend), c.candBody, func() { c.cancelReq(errLoopCut) })
			return c.lw
		}},
		// The tool calls of a chat reply wait for the guardrails (guardout.go).
		{name: "guard-out", applies: func(c *chainCtx) bool { return len(responseGuards(c.guards)) > 0 && c.chat() },
			wrap: func(c *chainCtx, out http.ResponseWriter) http.ResponseWriter {
				c.gw = &guardOutWriter{ResponseWriter: out, check: c.p.checkToolCalls(c.r, responseGuards(c.guards))}
				return c.gw
			}},
		// The caller's copy of usage gets corrallm's figure (costinject.go).
		// Text routes only: image and audio replies carry no usage object.
		// Not on /v1/messages: Anthropic's usage objects are not OpenAI's, and
		// its clients do not read a cost.
		{name: "cost", applies: func(c *chainCtx) bool { return !c.image && !c.audio && !c.messages() },
			wrap: func(c *chainCtx, out http.ResponseWriter) http.ResponseWriter {
				backend, loadCost := c.backend, c.loadCost
				c.cw = &costWriter{ResponseWriter: out, price: func(u usage) (float64, string) {
					return c.p.replyCost(backend, u, loadCost)
				}}
				return c.cw
			}},
		// The canonical remap (P35 §12) sits BETWEEN the two stt writers, and the
		// order is the design rather than an accident. Inside the SSE writer, so
		// the events are synthesised from corrallm's canonical body and carry the
		// canonical speaker labels — "A", not a UUID a client must not key on.
		// Outside statusCapture, so the activity row still records the
		// BACKEND's body and metering is untouched, exactly as §9.1 arranged.
		{name: "stt-remap", applies: func(c *chainCtx) bool { return c.sttReq.remaps() },
			wrap: func(c *chainCtx, out http.ResponseWriter) http.ResponseWriter {
				c.sttRemap = &sttRemapWriter{ResponseWriter: out, req: c.sttReq, model: c.served,
					streaming: c.streaming, join: c.sttJoin}
				return c.sttRemap
			}},
		// Remap-out contract 2 (P35 §9.1): the caller asked for a stream on an
		// STT route and the backend answers with a finished JSON body, because
		// oidio's diarize path never reads the flag. corrallm emits the OpenAI
		// transcription event sequence itself rather than handing a client that
		// is parsing SSE a lump of JSON.
		//
		// The latency benefit is a FICTION and sttstream.go says so at length:
		// the body is complete before the first event goes out. What this buys
		// is one client code path, and nothing else.
		//
		// OUTSIDE statusCapture, so the capture still sees the BACKEND's body:
		// the activity row records what was actually served, metering is
		// untouched, and /v1/audio/speech is excluded by audioUploadRoute.
		{name: "stt-sse", applies: func(c *chainCtx) bool { return c.streaming && audioUploadRoute(c.r.URL.Path) },
			wrap: func(c *chainCtx, out http.ResponseWriter) http.ResponseWriter {
				c.sttSSE = &sttStreamWriter{ResponseWriter: out}
				return c.sttSSE
			}},
	}
	return body, writers
}

// runBody runs the body stages of one scope in order: the request stages
// (request true) or the candidate stages. It stops at the first stageError.
// ran names the stages that ran.
func runBody(stages []BodyStage, c *chainCtx, request bool) (ran []string, refused *stageError) {
	for _, s := range stages {
		if s.request != request || !s.applies(c) {
			continue
		}
		ran = append(ran, s.name)
		if refused = s.run(c); refused != nil {
			return ran, refused
		}
	}
	return ran, nil
}

// wrapWriters builds the writer stages around w, from the caller's writer
// inward, and returns the writer the backend writes to.
func wrapWriters(stages []WriterStage, c *chainCtx, w http.ResponseWriter) http.ResponseWriter {
	for i := len(stages) - 1; i >= 0; i-- {
		if stages[i].applies(c) {
			w = stages[i].wrap(c, w)
		}
	}
	return w
}
