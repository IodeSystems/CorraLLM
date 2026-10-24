package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/iodesystems/corrallm/internal/config"
)

// Where a reply's "cost" figure comes from. A caller summing them must be able
// to tell a bill from an estimate: the local figure is watts-per-token, not a
// meter reading.
const (
	costFromProvider = "provider"        // the upstream said so (OpenRouter's usage.cost)
	costFromPrices   = "price-table"     // a paid type's per-token rates
	costFromEnergy   = "energy-estimate" // this box's watts-per-token coefficients
	costFromFree     = "free-tier"       // a declared free tier: $0
	costUnpriced     = "unpriced"        // no rates and no energy figures: $0
)

// classCost splits a text request's $ into cached, processed and generated.
//
// Provider-reported cost wins when present (OpenRouter): treat it as the
// authoritative total, keeping the table-derived class ratio (or a token share
// when the type is unpriced).
func (p *Proxy) classCost(typ string, u usage) (cCost, pCost, gCost float64) {
	proc := max(u.PromptTokens-u.CachedTokens, 0)
	cCost, pCost, gCost = p.cost.RequestUSDByClass(typ, u.CachedTokens, proc, u.CompletionTokens)
	if u.Cost > 0 {
		if s := cCost + pCost + gCost; s > 0 {
			k := u.Cost / s
			cCost, pCost, gCost = cCost*k, pCost*k, gCost*k
		} else if tot := float64(u.PromptTokens + u.CompletionTokens); tot > 0 {
			cCost = u.Cost * float64(u.CachedTokens) / tot
			pCost = u.Cost * float64(proc) / tot
			gCost = u.Cost * float64(u.CompletionTokens) / tot
		} else {
			gCost = u.Cost
		}
	}
	return cCost, pCost, gCost
}

// replyCost is the $ the activity row records for a text request, and its
// source. The same steps as the accounting in serve, in the same order —
// costinject_test holds the two equal.
func (p *Proxy) replyCost(backend config.Model, u usage, loadCost float64) (float64, string) {
	c, pr, g := p.classCost(backend.Type, u)
	usd := c + pr + g
	free := u.Cost <= 0 && p.cost.NoCharge(backend)
	if free {
		usd = 0
	}
	usd += loadCost
	switch {
	case u.Cost > 0:
		return usd, costFromProvider
	case free:
		return usd, costFromFree
	case p.cost.IsPaidType(backend.Type):
		return usd, costFromPrices
	case usd > 0:
		return usd, costFromEnergy
	}
	return usd, costUnpriced
}

// costWindow is how much of a non-streamed body is held back to find its usage
// object in. Usage sits at the end of every shape seen (OpenAI, llama.cpp,
// whose keys are alphabetical) except llama.cpp's rerank, where only "results"
// follows it.
const costWindow = 64 << 10

// costWriter adds corrallm's figure for the request to the usage object the
// caller receives: "cost" in USD and "cost_source". OpenRouter already sends
// usage.cost, so one client parser reads every model's cost.
//
// IT SITS OUTSIDE statusCapture, like the STT writers: the capture keeps the
// BACKEND's body, so metering never reads corrallm's own figure back as a
// provider-reported cost.
//
// An event stream is passed through a line at a time (an SSE data line is one
// whole JSON value), so nothing is delayed past the end of the line it is in.
// A JSON body streams through except its last costWindow bytes, which finish
// rewrites. Anything else — an error, a compressed body, another type — goes
// through untouched.
type costWriter struct {
	http.ResponseWriter
	price func(usage) (float64, string)

	decided bool
	sse     bool // line mode, else window mode
	active  bool
	held    []byte
}

func (c *costWriter) WriteHeader(code int) {
	if code >= 100 && code < 200 {
		c.ResponseWriter.WriteHeader(code)
		return
	}
	if !c.decided {
		c.decided = true
		h := c.Header()
		ct := h.Get("Content-Type")
		if code >= 200 && code < 300 && h.Get("Content-Encoding") == "" {
			switch {
			case strings.HasPrefix(ct, "text/event-stream"):
				c.active, c.sse = true, true
			case isJSONContentType(ct):
				c.active = true
			}
		}
		if c.active {
			// The backend's length described the backend's body.
			h.Del("Content-Length")
		}
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *costWriter) Write(b []byte) (int, error) {
	if !c.decided {
		c.WriteHeader(http.StatusOK)
	}
	if !c.active {
		return c.ResponseWriter.Write(b)
	}
	c.held = append(c.held, b...)
	var out []byte
	if c.sse {
		i := bytes.LastIndexByte(c.held, '\n')
		if i < 0 {
			return len(b), nil
		}
		out = c.costLines(c.held[:i+1])
		c.held = append([]byte(nil), c.held[i+1:]...)
	} else {
		if len(c.held) <= 2*costWindow {
			return len(b), nil
		}
		cut := len(c.held) - costWindow
		out = c.held[:cut]
		c.held = append([]byte(nil), c.held[cut:]...)
	}
	if _, err := c.ResponseWriter.Write(out); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *costWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// finish writes what is held, with the cost added. Safe on any exit path.
func (c *costWriter) finish() {
	if !c.active || len(c.held) == 0 {
		return
	}
	held := c.held
	c.held = nil
	if c.sse {
		held = c.costLines(held)
	} else if u, ok := usageIn(held); ok {
		held = c.withCost(held, u)
	}
	_, _ = c.ResponseWriter.Write(held)
	c.Flush()
}

// costLines adds the cost to each SSE data line that carries a usage object.
func (c *costWriter) costLines(b []byte) []byte {
	if !bytes.Contains(b, []byte(`"usage"`)) {
		return b
	}
	var out []byte
	for len(b) > 0 {
		line := b
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			line = b[:i+1]
		}
		b = b[len(line):]
		if data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:")); ok {
			var r struct {
				Usage   *usage  `json:"usage"`
				Timings timings `json:"timings"`
			}
			if json.Unmarshal(bytes.TrimSpace(data), &r) == nil && r.Usage != nil {
				line = c.withCost(line, mergeTimings(*r.Usage, r.Timings))
			}
		}
		out = append(out, line...)
	}
	return out
}

// usageIn reads the usage (and llama.cpp's sibling timings) from the end of a
// JSON body: the LAST top-level-looking "usage" object, the one extractUsage
// would find.
func usageIn(b []byte) (usage, bool) {
	var u usage
	s, e, ok := objectAfter(b, "usage")
	if !ok || json.Unmarshal(b[s:e], &u) != nil {
		return usage{}, false
	}
	var t timings
	if s, e, ok := objectAfter(b, "timings"); ok {
		_ = json.Unmarshal(b[s:e], &t)
	}
	return mergeTimings(u, t), true
}

// objectAfter finds the last `"key": {…}` in b and returns the object's span.
// A key inside a JSON string cannot match: its quotes would be escaped.
func objectAfter(b []byte, key string) (int, int, bool) {
	i := bytes.LastIndex(b, []byte(`"`+key+`"`))
	if i < 0 {
		return 0, 0, false
	}
	j := i + len(key) + 2
	for j < len(b) && (b[j] == ' ' || b[j] == '\t' || b[j] == '\n' || b[j] == '\r') {
		j++
	}
	if j >= len(b) || b[j] != ':' {
		return 0, 0, false
	}
	j++
	for j < len(b) && (b[j] == ' ' || b[j] == '\t' || b[j] == '\n' || b[j] == '\r') {
		j++
	}
	if j >= len(b) || b[j] != '{' {
		return 0, 0, false
	}
	dec := json.NewDecoder(bytes.NewReader(b[j:]))
	var raw json.RawMessage
	if dec.Decode(&raw) != nil {
		return 0, 0, false
	}
	return j, j + int(dec.InputOffset()), true
}

// withCost inserts "cost" and "cost_source" before the usage object's closing
// brace. An upstream's own "cost" is kept (it is what the figure is); only the
// source is added.
func (c *costWriter) withCost(b []byte, u usage) []byte {
	s, e, ok := objectAfter(b, "usage")
	if !ok {
		return b
	}
	var have map[string]json.RawMessage
	if json.Unmarshal(b[s:e], &have) != nil {
		return b
	}
	usd, source := c.price(u)
	var add []string
	if _, ok := have["cost"]; !ok {
		add = append(add, `"cost":`+strconv.FormatFloat(usd, 'f', -1, 64))
	}
	if _, ok := have["cost_source"]; !ok {
		add = append(add, `"cost_source":"`+source+`"`)
	}
	if len(add) == 0 {
		return b
	}
	ins := strings.Join(add, ",")
	if len(have) > 0 {
		ins = "," + ins
	}
	closeAt := e - 1 // the object's closing brace
	out := make([]byte, 0, len(b)+len(ins))
	out = append(out, b[:closeAt]...)
	out = append(out, ins...)
	return append(out, b[closeAt:]...)
}
