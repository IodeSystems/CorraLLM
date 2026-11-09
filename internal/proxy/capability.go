package proxy

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/store"
)

// Capability records (P73): agentkit keeps what one remote (endpoint, model)
// is known to do; corrallm keeps one record-only llm.Client per remote, stores
// what it learns in the capability table, and passes it back at startup.
//
// The align body stage translates a chat body by the record (TranslateBody),
// and the deferred finish learns from the backend's answer (LearnResponse)
// through the raw tap. corrallm never resends after a refusal that taught
// the record something: the caller gets the refusal, and the next request is
// translated.

// capKey names a remote: the backend's proxy target URL as configured and the
// id the upstream knows the model by. The client is built from the raw URL.
type capKey struct{ target, model string }

// capID is the registry and table key: agentkit's normalized endpoint
// (Client.Capabilities().Endpoint: no userinfo, query or fragment; scheme and
// host lowercased; no trailing slash) and the model. So no secret in the URL
// reaches SQLite, and one backend written two ways keeps one record.
type capID struct{ endpoint, model string }

func (k capKey) id() capID {
	return capID{endpoint: llm.NewClient(k.target, "", k.model).Capabilities().Endpoint, model: k.model}
}

// capRegistry holds the record-only clients. It lives on the Proxy, so a
// config reload keeps it. mu guards the map; each client guards its own
// record (agentkit capRecord.mu).
type capRegistry struct {
	mu      sync.Mutex
	clients map[capID]*llm.Client
}

// capKeyFor is the remote a backend is, or false when the backend is not
// remote or has no target URL.
func capKeyFor(name string, backend config.Model, tgt *config.ProxyTarget) (capKey, bool) {
	if tgt == nil || tgt.URL == nil || !isRemote(backend) {
		return capKey{}, false
	}
	model := backend.Upstream
	if model == "" {
		model = firstNonEmpty(tgt.Model, name)
	}
	return capKey{target: tgt.URL.String(), model: model}, true
}

// capClient is the record-only client for k, created on first use. OnLearn
// persists each fact it learns.
func (p *Proxy) capClient(k capKey) *llm.Client {
	cl := llm.NewClient(k.target, "", k.model)
	id := capID{endpoint: cl.Capabilities().Endpoint, model: k.model}
	p.caps.mu.Lock()
	defer p.caps.mu.Unlock()
	if have, ok := p.caps.clients[id]; ok {
		return have
	}
	if p.caps.clients == nil {
		p.caps.clients = map[capID]*llm.Client{}
	}
	cl.OnLearn = func(l llm.Learned) { p.storeFact(id, l.Key, l.Fact) }
	p.caps.clients[id] = cl
	return cl
}

// capRecord is the record for k, or false when there is none. It does not
// create a client.
func (p *Proxy) capRecord(k capKey) (llm.Capabilities, bool) {
	id := k.id()
	p.caps.mu.Lock()
	cl, ok := p.caps.clients[id]
	p.caps.mu.Unlock()
	if !ok {
		return llm.Capabilities{}, false
	}
	c := cl.Capabilities()
	return c, len(c.Facts) > 0
}

func (p *Proxy) storeFact(id capID, key string, f llm.Fact) {
	if p.store == nil {
		return
	}
	v, _ := json.Marshal(f.Value)
	if err := p.store.PutCapability(store.CapabilityFact{Endpoint: id.endpoint, Model: id.model, Key: key,
		Value: string(v), Source: string(f.Source), AtMS: f.At.UnixMilli(), Evidence: f.Evidence}); err != nil {
		slog.Warn("could not store a capability fact", "endpoint", id.endpoint, "model", id.model, "key", key, "err", err)
	}
}

// loadCapabilities seeds a client per stored (endpoint, model) with its
// facts. The stored endpoint is already normalized, so it maps to the same
// registry key as a request's raw target URL.
func (p *Proxy) loadCapabilities() {
	if p.store == nil {
		return
	}
	rows, err := p.store.Capabilities()
	if err != nil {
		slog.Warn("could not load capability records", "err", err)
		return
	}
	recs := map[capKey]map[string]llm.Fact{}
	for _, r := range rows {
		k := capKey{target: r.Endpoint, model: r.Model}
		if recs[k] == nil {
			recs[k] = map[string]llm.Fact{}
		}
		var v string
		if json.Unmarshal([]byte(r.Value), &v) != nil {
			v = r.Value
		}
		recs[k][r.Key] = llm.Fact{Value: v, Source: llm.Source(r.Source), Evidence: r.Evidence, At: time.UnixMilli(r.AtMS)}
	}
	for k, facts := range recs {
		p.capClient(k).SetCapabilities(llm.Capabilities{Facts: facts})
	}
}

// capView is one fact in /v1/models. The evidence is left out.
type capView struct {
	Value  string    `json:"value"`
	Source string    `json:"source"`
	At     time.Time `json:"at"`
}

// capabilitiesOf is a model's `capabilities` in /v1/models; nil when it has
// no record.
func (p *Proxy) capabilitiesOf(name string, mc config.Model) map[string]capView {
	if !isRemote(mc) {
		return nil
	}
	tgt, err := mc.ProxyTarget()
	if err != nil {
		return nil
	}
	k, ok := capKeyFor(name, mc, tgt)
	if !ok {
		return nil
	}
	rec, ok := p.capRecord(k)
	if !ok {
		return nil
	}
	out := make(map[string]capView, len(rec.Facts))
	for key, f := range rec.Facts {
		out[key] = capView{Value: f.Value, Source: string(f.Source), At: f.At.UTC().Truncate(time.Millisecond)}
	}
	return out
}

// align translates the chat body by the backend's capability record. The
// owner's rule: limits only narrow downstream. clamp's max_tokens is the
// ceiling, so align may lower it but never raise it, and the smallest value
// wins. It records the body as sent for LearnResponse, and a note per field
// it changed.
func (c *chainCtx) align(k capKey) {
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(c.body))
	dec.UseNumber()
	if dec.Decode(&m) != nil || m == nil {
		return
	}
	cl := c.p.capClient(k)
	c.capc, c.sent = cl, m

	before := alignFields(m)
	ceiling := tokenCeiling(m)
	_, hadMax := m["max_tokens"]
	var kept []llm.Translation
	for _, t := range cl.TranslateBody(m) {
		if t.Key == "max_tokens" {
			lim := numberOf(m["max_tokens"])
			if mct, ok := m["max_completion_tokens"]; ok && !hadMax {
				// The caller capped with max_completion_tokens: keep one field.
				delete(m, "max_tokens")
				if want := numberOf(mct); lim < want {
					m["max_completion_tokens"] = lim
					kept = append(kept, llm.Translation{Key: "max_completion_tokens",
						Sent: strconv.Itoa(lim), Wanted: strconv.Itoa(want)})
				}
				continue
			}
			if ceiling > 0 && lim > ceiling {
				m["max_tokens"] = ceiling // never above what clamp left
				continue
			}
		}
		kept = append(kept, t)
	}
	after := alignFields(m)
	if after == before {
		return
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(c.body, &raw) != nil {
		return
	}
	for i, f := range alignFieldNames {
		if after[i] == "" {
			delete(raw, f)
		} else {
			raw[f] = json.RawMessage(after[i])
		}
	}
	if b, err := json.Marshal(raw); err == nil {
		c.body = b
	}
	for _, t := range kept {
		wanted, sent := t.Wanted, t.Sent
		if wanted == "" {
			wanted = "unset"
		}
		if sent == "" {
			sent = "dropped"
		}
		c.notes = append(c.notes, "align: "+t.Key+" "+wanted+" → "+sent)
	}
}

// alignFieldNames are the body fields align may change.
var alignFieldNames = [3]string{"reasoning_effort", "max_tokens", "max_completion_tokens"}

// alignFields is each of alignFieldNames as JSON, "" when absent.
func alignFields(m map[string]any) (out [3]string) {
	for i, f := range alignFieldNames {
		if v, ok := m[f]; ok {
			b, _ := json.Marshal(v)
			out[i] = string(b)
		}
	}
	return out
}

// tokenCeiling is the smallest positive output limit the body sets, 0 when
// it sets none.
func tokenCeiling(m map[string]any) int {
	n := 0
	for _, f := range []string{"max_tokens", "max_completion_tokens"} {
		if v := numberOf(m[f]); v > 0 && (n == 0 || v < n) {
			n = v
		}
	}
	return n
}

// numberOf reads a body number: json.Number from a decoded body, int from
// agentkit's translation.
func numberOf(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
		if f, err := n.Float64(); err == nil {
			return int(f)
		}
	}
	return 0
}

// clampNotes names each output limit clamp lowered, comparing the body before
// and after it.
func clampNotes(before, after []byte) []string {
	var b, a map[string]json.RawMessage
	if json.Unmarshal(before, &b) != nil || json.Unmarshal(after, &a) != nil {
		return nil
	}
	var out []string
	for _, f := range []string{"max_tokens", "max_completion_tokens"} {
		var x, y float64
		if json.Unmarshal(b[f], &x) != nil || json.Unmarshal(a[f], &y) != nil {
			continue
		}
		if y < x {
			out = append(out, "clamp: "+f+" "+string(b[f])+" → "+string(a[f])+" (the backend's cap)")
		}
	}
	return out
}

// rawTapLimit is how much of an error body the raw tap keeps.
const rawTapLimit = 8 << 10 // 8 KiB

// rawTap is the innermost writer on every path: the backend writes to it
// first, so it holds the backend's own bytes, before any wire adapter. It
// keeps the first rawTapLimit bytes of the body for an error status (>= 400)
// and nothing for any other. LearnResponse reads it.
type rawTap struct {
	http.ResponseWriter
	code        int
	wroteHeader bool
	body        []byte
}

func (t *rawTap) WriteHeader(code int) {
	if code >= 200 && !t.wroteHeader {
		t.code, t.wroteHeader = code, true
	}
	t.ResponseWriter.WriteHeader(code)
}

func (t *rawTap) Write(b []byte) (int, error) {
	if !t.wroteHeader {
		t.code, t.wroteHeader = http.StatusOK, true
	}
	if t.code >= 400 && len(t.body) < rawTapLimit {
		n := min(len(b), rawTapLimit-len(t.body))
		t.body = append(t.body, b[:n]...)
	}
	return t.ResponseWriter.Write(b)
}

func (t *rawTap) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// learn tells the record what the backend answered to the body align sent.
// Never a resend: the caller already has the answer.
func (c *chainCtx) learn() {
	if c.capc == nil || c.tap == nil || c.rewrote {
		return
	}
	c.capc.LearnResponse(c.sent, c.tap.code, c.tap.body)
}
