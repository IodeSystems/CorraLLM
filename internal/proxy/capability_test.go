package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// capRig is refusalRig on a store the test owns, so a second Proxy on the same
// store is a restart.
func capRig(t *testing.T, st *store.Store, models map[string]config.Model) (http.Handler, *Proxy) {
	t.Helper()
	mgr := proc.NewManager(&config.Config{})
	t.Cleanup(mgr.Shutdown)
	cfg := &config.Config{
		Models:         models,
		PriorityGroups: map[string]config.PriorityGroup{"default": {Weight: 1}},
		Keys:           map[string]config.KeyPolicy{"k": {Group: "default"}},
	}
	r := chi.NewRouter()
	p := New(cfg, mgr, sched.New(), st)
	p.Mount(r)
	return r, p
}

func capStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func postCapChat(h http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("X-Corrallm-Key", "k")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// glmUpstream answers like Fireworks GLM-5.3: reasoning_effort "none" is a
// 400 (verbatim body, dun-bb 11-07), anything else a 200.
type glmUpstream struct {
	srv    *httptest.Server
	mu     sync.Mutex
	hits   int
	bodies []map[string]any
}

const glmThinkingOnly = `{"error":{"message":"GLM-5.3 is a thinking-only model; disabling thinking (reasoning_effort='none') is not supported.","type":"invalid_request_error","code":400}}`

func newGLMUpstream(t *testing.T) *glmUpstream {
	u := &glmUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		u.mu.Lock()
		u.hits++
		u.bodies = append(u.bodies, b)
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if b["reasoning_effort"] == "none" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, glmThinkingOnly)
			return
		}
		_, _ = io.WriteString(w, `{"id":"c","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *glmUpstream) last() map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.bodies[len(u.bodies)-1]
}

func remoteModel(t *testing.T, urlStr string) config.Model {
	m := modelTo(t, urlStr, "chat")
	m.Upstream = "accounts/fireworks/models/glm-5p3"
	return m
}

func factsOf(t *testing.T, st *store.Store) map[string]store.CapabilityFact {
	t.Helper()
	rows, err := st.Capabilities()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]store.CapabilityFact{}
	for _, r := range rows {
		out[r.Key] = r
	}
	return out
}

// A refusal that teaches the record is stored in the capability table; the
// caller gets the refusal as is, with no resend; the next request is
// translated by what was learned, and its activity row says so.
func TestLearnedRefusalIsStoredAndUsedWithoutResend(t *testing.T) {
	treatAsRemote(t)
	up := newGLMUpstream(t)
	st := capStore(t)
	h, _ := capRig(t, st, map[string]config.Model{"glm": remoteModel(t, up.srv.URL)})
	const body = `{"model":"glm","reasoning_effort":"none","messages":[{"role":"user","content":"hi"}]}`

	rec := postCapChat(h, body)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "thinking-only") {
		t.Fatalf("first request: %d %s, want the upstream's 400", rec.Code, rec.Body)
	}
	if up.hits != 1 {
		t.Fatalf("upstream hits = %d after a taught refusal, want 1 (no resend)", up.hits)
	}
	facts := factsOf(t, st)
	if f := facts[llm.CapReasoningThinkingOnly]; f.Value != `"true"` || f.Source != "refused" || f.Evidence == "" {
		t.Errorf("thinking_only row = %+v, want \"true\" refused with evidence", f)
	}
	if f := facts[llm.CapReasoningEffortsRefused]; f.Value != `"none"` || f.Endpoint != up.srv.URL ||
		f.Model != "accounts/fireworks/models/glm-5p3" {
		t.Errorf("efforts.refused row = %+v, want \"none\" for (%s, the upstream id)", f, up.srv.URL)
	}

	rec = postCapChat(h, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("second request: %d %s", rec.Code, rec.Body)
	}
	if got := up.last()["reasoning_effort"]; got != "minimal" {
		t.Errorf("second request sent reasoning_effort %v, want minimal (lowest not refused)", got)
	}
	if up.hits != 2 {
		t.Errorf("upstream hits = %d, want 2", up.hits)
	}
	rows, _ := st.RecentActivity(store.Window{}, 10, "", "", "")
	if len(rows) != 2 || rows[0].Note != "align: reasoning_effort none → minimal" {
		t.Errorf("rows = %+v, want the newest noted with the translation", rows)
	}
	// The 200 taught "minimal accepted", stored too.
	if f := factsOf(t, st)[llm.CapReasoningEffortsAccepted]; f.Value != `"minimal"` || f.Source != "observed" {
		t.Errorf("efforts.accepted row = %+v", f)
	}
}

// The record outlives the process: a new Proxy on the reopened database file
// seeds its client from the table and translates the first request it sees.
func TestCapabilityRecordsSurviveRestart(t *testing.T) {
	treatAsRemote(t)
	up := newGLMUpstream(t)
	path := t.TempDir() + "/corrallm.db"
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	models := map[string]config.Model{"glm": remoteModel(t, up.srv.URL)}
	h, _ := capRig(t, st, models)
	const body = `{"model":"glm","reasoning_effort":"none","messages":[{"role":"user","content":"hi"}]}`
	if rec := postCapChat(h, body); rec.Code != http.StatusBadRequest {
		t.Fatalf("first: %d", rec.Code)
	}
	_ = st.Close()

	st2, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st2.Close() })
	h2, p2 := capRig(t, st2, models)
	rec, ok := p2.capRecord(capKey{target: up.srv.URL, model: "accounts/fireworks/models/glm-5p3"})
	if !ok || rec.Facts[llm.CapReasoningThinkingOnly].Source != llm.SourceRefused ||
		rec.Facts[llm.CapReasoningThinkingOnly].Evidence == "" {
		t.Fatalf("restarted record = %+v, want the stored refusal with its evidence", rec)
	}
	if rec := postCapChat(h2, body); rec.Code != http.StatusOK {
		t.Fatalf("after restart: %d %s", rec.Code, rec.Body)
	}
	if got := up.last()["reasoning_effort"]; got != "minimal" {
		t.Errorf("after restart sent %v, want minimal", got)
	}
}

// Owner rule: limits only narrow downstream. clamp's cap is the ceiling; a
// smaller record limit lowers it further, and both changes are noted on the
// activity row.
func TestClampAndAlignNotesReachTheRow(t *testing.T) {
	treatAsRemote(t)
	up := newGLMUpstream(t)
	st := capStore(t)
	m := remoteModel(t, up.srv.URL)
	m.MaxTokens = 100
	h, p := capRig(t, st, map[string]config.Model{"glm": m})
	p.capClient(capKey{target: up.srv.URL, model: m.Upstream}).SetCapabilities(llm.Capabilities{Facts: map[string]llm.Fact{
		llm.CapLimitsOutput: {Value: "64", Source: llm.SourceRefused, At: time.Now()},
	}})
	if rec := postCapChat(h, `{"model":"glm","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`); rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	if got := up.last()["max_tokens"]; got != float64(64) {
		t.Errorf("backend max_tokens = %v, want 64 (the smallest)", got)
	}
	rows, _ := st.RecentActivity(store.Window{}, 1, "", "", "")
	want := "clamp: max_tokens 4096 → 100 (the backend's cap); align: max_tokens 100 → 64"
	if len(rows) != 1 || rows[0].Note != want {
		t.Errorf("note = %q, want %q", rows[0].Note, want)
	}
}

// align never raises max_tokens above what clamp left, and never adds a
// second output field beside the caller's max_completion_tokens.
func TestAlignNeverRaisesMaxTokens(t *testing.T) {
	treatAsRemote(t)
	p := &Proxy{}
	k := capKey{target: "https://api.example.com", model: "m"}
	p.capClient(k).SetCapabilities(llm.Capabilities{Facts: map[string]llm.Fact{
		llm.CapLimitsOutput: {Value: "8000", Source: llm.SourceRefused, At: time.Now()},
	}})
	for _, tc := range []struct{ in, want, note string }{
		// Under the record's limit: unchanged.
		{`{"max_tokens":100}`, `{"max_tokens":100}`, ""},
		// The caller's own field stays the only one, at the caller's value.
		{`{"max_completion_tokens":50}`, `{"max_completion_tokens":50}`, ""},
		// No limit at all: the record's limit narrows it.
		{`{}`, `{"max_tokens":8000}`, "align: max_tokens unset → 8000"},
	} {
		c := &chainCtx{p: p, body: []byte(tc.in)}
		c.align(k)
		if string(c.body) != tc.want || strings.Join(c.notes, "; ") != tc.note {
			t.Errorf("%s: body %s notes %q, want %s %q", tc.in, c.body, c.notes, tc.want, tc.note)
		}
	}
	// Both fields set, the record's limit between them: the smallest wins.
	p.capClient(k).SetCapabilities(llm.Capabilities{Facts: map[string]llm.Fact{
		llm.CapLimitsOutput: {Value: "80", Source: llm.SourceRefused, At: time.Now()},
	}})
	c := &chainCtx{p: p, body: []byte(`{"max_tokens":100,"max_completion_tokens":50}`)}
	c.align(k)
	var got map[string]float64
	_ = json.Unmarshal(c.body, &got)
	if got["max_tokens"] > 50 || got["max_completion_tokens"] != 50 {
		t.Errorf("body %s, want nothing above 50", c.body)
	}
}

// align applies to every remote target on chat completions and converted
// /v1/messages, and to nothing else.
func TestAlignAppliesPerPath(t *testing.T) {
	prev := isRemote
	isRemote = func(m config.Model) bool { return m.Type == "remote" }
	t.Cleanup(func() { isRemote = prev })
	u, _ := url.Parse("https://api.example.com")
	p := &Proxy{}
	for _, tc := range []struct {
		name, path, api, typ string
		want                 bool
	}{
		{"native /v1/messages", "/v1/messages", config.APIAnthropic, "remote", false},
		{"converted /v1/messages", "/v1/messages", "", "remote", true},
		{"chat to a local backend", "/v1/chat/completions", "", "local", false},
		{"chat to an anthropic target", "/v1/chat/completions", config.APIAnthropic, "remote", true},
		{"chat to an OpenAI remote", "/v1/chat/completions", "", "remote", true},
		{"embeddings", "/v1/embeddings", "", "remote", false},
	} {
		in := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
		c := &chainCtx{p: p, r: httptest.NewRequest("POST", tc.path, nil), body: []byte(in), name: "m",
			backend: config.Model{Type: tc.typ},
			pr:      &proc.Process{Target: &config.ProxyTarget{URL: u, API: tc.api, Model: "up"}}}
		ran, refused := runBody(candidateStages(), c, false)
		if refused != nil {
			t.Fatalf("%s: refused %s", tc.name, refused.body)
		}
		got := false
		for _, s := range ran {
			got = got || s == "align"
		}
		if got != tc.want || (c.capc != nil) != tc.want {
			t.Errorf("%s: align ran=%v (ran %v), want %v", tc.name, got, ran, tc.want)
		}
	}
}

// The raw tap keeps nothing on a 2xx and at most 8 KiB of an error.
func TestRawTapKeepsOnlyErrors(t *testing.T) {
	ok := &rawTap{ResponseWriter: httptest.NewRecorder(), code: http.StatusOK}
	ok.WriteHeader(http.StatusContinue) // interim: not latched
	ok.WriteHeader(http.StatusOK)
	_, _ = ok.Write(bytes.Repeat([]byte("x"), 100))
	if ok.body != nil || ok.code != http.StatusOK {
		t.Errorf("2xx: code %d body %d bytes, want 200 and nil", ok.code, len(ok.body))
	}
	implicit := &rawTap{ResponseWriter: httptest.NewRecorder(), code: http.StatusOK}
	_, _ = implicit.Write([]byte("x"))
	if implicit.body != nil {
		t.Errorf("implicit 200 kept %q", implicit.body)
	}
	bad := &rawTap{ResponseWriter: httptest.NewRecorder(), code: http.StatusOK}
	bad.WriteHeader(http.StatusBadRequest)
	for i := 0; i < 5; i++ {
		_, _ = bad.Write(bytes.Repeat([]byte("e"), 4<<10))
	}
	if len(bad.body) != 8<<10 || bad.code != http.StatusBadRequest {
		t.Errorf("error: code %d kept %d bytes, want 400 and 8192", bad.code, len(bad.body))
	}
}

// /v1/models gains `capabilities` on a model with a record and on no other;
// every other field stays as it was.
func TestModelsCarriesCapabilities(t *testing.T) {
	treatAsRemote(t)
	up := newGLMUpstream(t)
	other := newGLMUpstream(t)
	models := map[string]config.Model{"glm": remoteModel(t, up.srv.URL), "other": modelTo(t, other.srv.URL, "chat")}
	list := func(st *store.Store) map[string]map[string]json.RawMessage {
		h, _ := capRig(t, st, models)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
		var out struct {
			Data []map[string]json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%v: %s", err, rec.Body)
		}
		byID := map[string]map[string]json.RawMessage{}
		for _, m := range out.Data {
			var id string
			_ = json.Unmarshal(m["id"], &id)
			delete(m, "created")
			byID[id] = m
		}
		return byID
	}
	before := list(capStore(t))

	st := capStore(t)
	at := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC).UnixMilli()
	for _, f := range []store.CapabilityFact{
		{Key: llm.CapReasoningThinkingOnly, Value: `"true"`, Source: "refused", Evidence: "sent thinking off → 400"},
		{Key: llm.CapReasoningEffortsRefused, Value: `"none"`, Source: "refused", Evidence: "sent reasoning_effort=none → 400"},
	} {
		f.Endpoint, f.Model, f.AtMS = up.srv.URL, "accounts/fireworks/models/glm-5p3", at
		if err := st.PutCapability(f); err != nil {
			t.Fatal(err)
		}
	}
	after := list(st)

	want := `{"reasoning.efforts.refused":{"value":"none","source":"refused","at":"2026-11-08T10:00:00Z"},` +
		`"reasoning.thinking_only":{"value":"true","source":"refused","at":"2026-11-08T10:00:00Z"}}`
	if got := string(after["glm"]["capabilities"]); got != want {
		t.Errorf("glm capabilities = %s, want %s", got, want)
	}
	sample, _ := json.Marshal(after["glm"])
	t.Logf("sample /v1/models entry: %s", sample)
	if strings.Contains(string(after["glm"]["capabilities"]), "evidence") {
		t.Error("evidence leaked into /v1/models")
	}
	delete(after["glm"], "capabilities")
	for id := range before {
		if !reflect.DeepEqual(before[id], after[id]) {
			t.Errorf("%s changed:\nbefore %v\nafter  %v", id, before[id], after[id])
		}
	}
	if _, ok := after["other"]["capabilities"]; ok {
		t.Error("a model with no record carries capabilities")
	}
}

// The registry and the table key on agentkit's normalized endpoint: no
// userinfo or query reaches SQLite, and URLs that differ only by host case
// or a trailing slash share one record.
func TestCapabilityKeyIsTheNormalizedEndpoint(t *testing.T) {
	st := capStore(t)
	p := &Proxy{store: st}
	secret := capKey{target: "https://user:hunter2@API.Fireworks.ai/inference/?api_key=sk-secret#frag", model: "glm"}
	sent := map[string]any{"reasoning_effort": "none"}
	p.capClient(secret).LearnResponse(sent, http.StatusBadRequest, []byte(glmThinkingOnly))
	rows, err := st.Capabilities()
	if err != nil || len(rows) == 0 {
		t.Fatalf("rows %v err %v, want the learned facts", rows, err)
	}
	for _, r := range rows {
		if r.Endpoint != "https://api.fireworks.ai/inference" {
			t.Errorf("stored endpoint %q, want https://api.fireworks.ai/inference", r.Endpoint)
		}
		for _, leak := range []string{"user", "hunter2", "api_key", "sk-secret", "frag"} {
			if strings.Contains(r.Endpoint+r.Model+r.Key+r.Value+r.Evidence, leak) {
				t.Errorf("row %+v carries %q", r, leak)
			}
		}
	}
	a := p.capClient(capKey{target: "https://API.Fireworks.ai/inference/", model: "glm"})
	b := p.capClient(capKey{target: "https://api.fireworks.ai/inference", model: "glm"})
	if a != b || a != p.capClient(secret) {
		t.Error("URLs differing by host case, a trailing slash, userinfo or query got separate records")
	}
	if _, ok := p.capRecord(capKey{target: "https://api.fireworks.ai/inference/", model: "glm"}); !ok {
		t.Error("the learned record is not found under the plain URL")
	}
	// Seeding maps a stored row to the same key a raw URL does.
	p2 := &Proxy{store: st}
	p2.loadCapabilities()
	if rec, ok := p2.capRecord(secret); !ok || rec.Facts[llm.CapReasoningThinkingOnly].Value != "true" {
		t.Errorf("seeded record under the raw URL = %+v, %v", rec, ok)
	}
}

// One client serves concurrent requests: translation, learning and the
// /v1/models snapshot race-free (run with -race).
func TestCapabilityClientConcurrent(t *testing.T) {
	p := &Proxy{store: capStore(t)}
	k := capKey{target: "https://api.example.com", model: "m"}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := &chainCtx{p: p, body: []byte(`{"reasoning_effort":"none","max_tokens":10}`)}
			c.align(k)
			c.capc.LearnResponse(c.sent, http.StatusBadRequest, []byte(glmThinkingOnly))
			_, _ = p.capRecord(k)
		}()
	}
	wg.Wait()
}

// A funds 402 corrallm rewrote is corrallm's body, not the backend's: nothing
// is learned from it, even when the backend's own words would have taught a
// fact.
func TestRewrittenFundsRefusalTeachesNothing(t *testing.T) {
	treatAsRemote(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, glmThinkingOnly)
	}))
	t.Cleanup(up.Close)
	st := capStore(t)
	h, _ := capRig(t, st, map[string]config.Model{"glm": remoteModel(t, up.URL)})
	rec := postCapChat(h, `{"model":"glm","reasoning_effort":"none","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusPaymentRequired || !strings.Contains(rec.Body.String(), "upstream_funds_exhausted") {
		t.Fatalf("got %d %s, want corrallm's 402 upstream_funds_exhausted", rec.Code, rec.Body)
	}
	if facts := factsOf(t, st); len(facts) != 0 {
		t.Errorf("learned from corrallm's own body: %+v", facts)
	}
}
