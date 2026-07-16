package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// mkConfig builds a one-model config that pure-proxies (no cmd) to the given
// upstream base URL.
func mkConfig(t *testing.T, served, upstream string) *config.Config {
	t.Helper()
	m := modelTo(t, upstream, "local")
	m.Quality = 100
	return &config.Config{
		Models: map[string]config.Model{served: m},
	}
}

func TestInferencePassthroughAndActivityLog(t *testing.T) {
	// Mock OpenAI upstream.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/v1/chat/completions" {
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"mock"`) {
				t.Errorf("upstream did not receive body: %s", body)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"content":"hi"}}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	cfg := mkConfig(t, "mock", upstream.URL)
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)

	// Fire a chat completion through corrallm.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mock","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"content":"hi"`) {
		t.Errorf("unexpected proxied body: %s", rec.Body.String())
	}

	// Activity was logged.
	acts, err := st.RecentActivity(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 1 {
		t.Fatalf("want 1 activity row, got %d", len(acts))
	}
	if acts[0].Served != "mock" || acts[0].Status != http.StatusOK || acts[0].Path != "/v1/chat/completions" {
		t.Errorf("activity = %+v", acts[0])
	}
}

// TestBackpressure429 holds the single slot with one in-flight request and
// asserts a concurrent second request is rejected with 429 + backoff headers.
func TestBackpressure429(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		close(started)
		<-release // hold the slot
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	defer close(release)

	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	r := chi.NewRouter()
	New(mkConfig(t, "mock", upstream.URL), mgr, sched.New(), st).Mount(r)

	// First request occupies the only slot.
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"mock"}`))
		r.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-started

	// Second request: backend at capacity, default group → reject → 429.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mock"}`)))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d (%s)", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" || rec.Header().Get("X-RateLimit-Capacity") != "1" {
		t.Errorf("missing backoff headers: %v", rec.Header())
	}
	if !strings.Contains(rec.Body.String(), `"reason":"rejected"`) {
		t.Errorf("body lacks reason: %s", rec.Body.String())
	}
}

func modelTo(t *testing.T, urlStr, typ string) config.Model {
	t.Helper()
	u, err := url.Parse(urlStr)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(u.Port())
	var pn yaml.Node
	if err := pn.Encode(port); err != nil {
		t.Fatal(err)
	}
	return config.Model{Proxy: pn, Type: typ}
}

func TestOrderCandidates(t *testing.T) {
	cs := []config.Candidate{
		{Model: config.Model{Type: "local"}},
		{Model: config.Model{Type: "local"}},
		{Model: config.Model{Type: "cloud"}},
	}
	cases := []struct {
		rr   uint64
		want []int
	}{
		{0, []int{0, 1, 2}},
		{1, []int{1, 0, 2}}, // rr rotates within the local type; cloud stays last
		{2, []int{0, 1, 2}},
	}
	for _, c := range cases {
		got := orderCandidates(cs, c.rr)
		if len(got) != len(c.want) {
			t.Fatalf("rr=%d len %d", c.rr, len(got))
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("rr=%d: got %v want %v", c.rr, got, c.want)
				break
			}
		}
	}
}

// TestOrderCandidatesQuality: candidates are walked best-quality-tier first;
// within a tier, type-rr is preserved (P7).
func TestOrderCandidatesQuality(t *testing.T) {
	// idx: 0 q40 cloud, 1 q100 local, 2 q100 local, 3 q60 cloud
	cs := []config.Candidate{
		{Model: config.Model{Type: "cloud", Quality: 40}},
		{Model: config.Model{Type: "local", Quality: 100}},
		{Model: config.Model{Type: "local", Quality: 100}},
		{Model: config.Model{Type: "cloud", Quality: 60}},
	}
	// rr=0: tier 100 → [1,2], tier 60 → [3], tier 40 → [0].
	if got := orderCandidates(cs, 0); !equalInts(got, []int{1, 2, 3, 0}) {
		t.Errorf("rr=0 got %v want [1 2 3 0]", got)
	}
	// rr=1 rotates within the 2-backend top tier only.
	if got := orderCandidates(cs, 1); !equalInts(got, []int{2, 1, 3, 0}) {
		t.Errorf("rr=1 got %v want [2 1 3 0]", got)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestClampMaxTokens: a present max_tokens above the cap is reduced; an absent
// one is set to the cap; no cap (or a smaller request) leaves the body intact.
func TestClampMaxTokens(t *testing.T) {
	capped := config.Model{MaxTokens: 128}

	// Above cap → clamped.
	got := clampMaxTokens([]byte(`{"model":"m","max_tokens":4096}`), capped)
	if mt := maxTokensOf(t, got); mt != 128 {
		t.Errorf("clamp above cap: max_tokens = %d, want 128", mt)
	}
	// Absent → set to cap.
	got = clampMaxTokens([]byte(`{"model":"m"}`), capped)
	if mt := maxTokensOf(t, got); mt != 128 {
		t.Errorf("set when absent: max_tokens = %d, want 128", mt)
	}
	// Below cap → unchanged.
	got = clampMaxTokens([]byte(`{"model":"m","max_tokens":16}`), capped)
	if mt := maxTokensOf(t, got); mt != 16 {
		t.Errorf("below cap: max_tokens = %d, want 16", mt)
	}
	// No cap → byte-identical passthrough.
	in := []byte(`{"model":"m","max_tokens":4096}`)
	if got := clampMaxTokens(in, config.Model{}); string(got) != string(in) {
		t.Errorf("no cap altered body: %s", got)
	}
}

func maxTokensOf(t *testing.T, body []byte) int {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	v, ok := m["max_tokens"].(float64)
	if !ok {
		t.Fatalf("no max_tokens in %s", body)
	}
	return int(v)
}

// TestQualityDegradeRouting: a high-quality backend (saturated, spill stage)
// sits above a low-quality one. A non-degrading group must NOT spill onto the
// worse model (terminal 429); a degrading group is served by it.
func TestQualityDegradeRouting(t *testing.T) {
	rel := make(chan struct{})
	started := make(chan struct{})
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		select {
		case <-started:
		default:
			close(started)
		}
		<-rel // hold the top-tier slot
		_, _ = w.Write([]byte(`{"served_by":"big"}`))
	}))
	defer big.Close()
	small := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		_, _ = w.Write([]byte(`{"served_by":"small"}`))
	}))
	defer small.Close()
	defer close(rel)

	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	bigM := modelTo(t, big.URL, "local")
	bigM.Quality = 100
	smallM := modelTo(t, small.URL, "local")
	smallM.Quality = 50

	cfg := &config.Config{
		Models: map[string]config.Model{
			"m-big":   bigM,
			"m-small": smallM,
		},
		Lanes: map[string]config.Lane{
			"m": {Members: []config.LaneMember{{Model: "m-big"}, {Model: "m-small"}}},
		},
		PriorityGroups: map[string]config.PriorityGroup{
			// Both groups spill when the local tier is saturated.
			"strict": {Weight: 1, OnSaturated: map[string]config.Stage{"local": {Spill: true}}},
			"lax": {
				Weight: 1, AcceptDegrade: true,
				OnSaturated: map[string]config.Stage{"local": {Spill: true}},
			},
		},
		Keys: map[string]string{"strict": "strict", "lax": "lax"},
	}
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)

	send := func(key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		req.Header.Set("X-Corrallm-Key", key)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	go send("strict") // occupies the top-tier (big) slot, blocks
	<-started

	// Non-degrading group: top tier saturated, spill stage — but the only lower
	// backend is worse quality, which it won't accept → terminal 429, never small.
	rec := send("strict")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("strict: want 429, got %d (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "small") {
		t.Errorf("strict was served the degraded model: %s", rec.Body.String())
	}

	// Degrading group: accepts the lower tier → served by small.
	rec = send("lax")
	if rec.Code != http.StatusOK {
		t.Fatalf("lax: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"served_by":"small"`) {
		t.Errorf("lax expected degrade to small, got: %s", rec.Body.String())
	}
}

// TestFallThroughSpill: the first (local) backend is saturated with a spill
// stage, so a concurrent request advances to the second (cloud) backend.
func TestFallThroughSpill(t *testing.T) {
	rel := make(chan struct{})
	started := make(chan struct{})
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		close(started)
		<-rel // hold the local slot
		_, _ = w.Write([]byte(`{"served_by":"up1"}`))
	}))
	defer up1.Close()
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		_, _ = w.Write([]byte(`{"served_by":"up2"}`))
	}))
	defer up2.Close()
	// Registered last → runs first (LIFO): unblock the held request before the
	// servers Close (which waits for in-flight connections).
	defer close(rel)

	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	cfg := &config.Config{
		Models: map[string]config.Model{
			"m-1": modelTo(t, up1.URL, "local"),
			"m-2": modelTo(t, up2.URL, "cloud"),
		},
		Lanes: map[string]config.Lane{
			"m": {Members: []config.LaneMember{{Model: "m-1"}, {Model: "m-2"}}},
		},
		PriorityGroups: map[string]config.PriorityGroup{
			"g": {Weight: 1, OnSaturated: map[string]config.Stage{"local": {Spill: true}}},
		},
		Keys: map[string]string{"k": "g"},
	}
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)

	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		req.Header.Set("X-Corrallm-Key", "k")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	go send() // occupies the local slot, blocks in up1
	<-started

	rec := send() // local saturated → spill → served by cloud up2
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"served_by":"up2"`) {
		t.Errorf("expected spill to up2, got: %s", rec.Body.String())
	}
}

// TestExhaustedAllSpill: every backend spills → terminal 429 with reason
// "exhausted".
func TestExhaustedAllSpill(t *testing.T) {
	rel := make(chan struct{})
	started := make(chan struct{}, 1)
	block := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		select {
		case started <- struct{}{}:
		default:
		}
		<-rel
	}))
	defer block.Close()
	defer close(rel) // runs before block.Close() (LIFO) to release the held request

	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	cfg := &config.Config{
		Models: map[string]config.Model{
			"m": modelTo(t, block.URL, "local"),
		},
		PriorityGroups: map[string]config.PriorityGroup{
			"g": {Weight: 1, OnSaturated: map[string]config.Stage{"local": {Spill: true}}},
		},
		Keys: map[string]string{"k": "g"},
	}
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)
	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		req.Header.Set("X-Corrallm-Key", "k")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	go send()
	<-started

	rec := send() // only backend saturated + spill, nothing else → exhausted 429
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"reason":"exhausted"`) {
		t.Errorf("expected reason exhausted, got: %s", rec.Body.String())
	}
}

// TestPreemptionServesHigherGroup: a low, interruptible group holds the only
// slot mid-request; a higher group with a preempt stage cancels it and is served
// once the slot frees. The preempted upstream request observes ctx cancellation.
func TestPreemptionServesHigherGroup(t *testing.T) {
	started := make(chan struct{})
	rel := make(chan struct{}) // unblocks the held upstream handler for clean shutdown
	var once sync.Once
	first := true
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		mu.Lock()
		isFirst := first
		first = false
		mu.Unlock()
		if isFirst {
			once.Do(func() { close(started) })
			// Hold the slot until preempted (proxy aborts on ctx cancel, freeing
			// the slot) or the test ends. The slot frees independent of this
			// handler returning, so `rel` is only for tidy server teardown.
			select {
			case <-r.Context().Done():
			case <-rel:
			}
			return
		}
		_, _ = w.Write([]byte(`{"served_by":"hi"}`))
	}))
	defer upstream.Close()
	defer close(rel) // runs before upstream.Close() (LIFO) so no connection lingers

	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	cfg := &config.Config{
		Models: map[string]config.Model{
			"m": modelTo(t, upstream.URL, "local"),
		},
		PriorityGroups: map[string]config.PriorityGroup{
			"lo": {Weight: 1, Interruptible: true,
				OnSaturated: map[string]config.Stage{"default": {Reject: true}}},
			"hi": {Weight: 10,
				OnSaturated: map[string]config.Stage{"local": {Preempt: true}}},
		},
		Keys: map[string]string{"lokey": "lo", "hikey": "hi"},
	}
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)

	send := func(key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		req.Header.Set("X-Corrallm-Key", key)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	go send("lokey") // occupies the only slot, blocks in upstream
	<-started

	rec := send("hikey") // preempts the low request → served
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for preempting request, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"served_by":"hi"`) {
		t.Errorf("expected hi to be served after preemption, got: %s", rec.Body.String())
	}
}

// meteredConfig is a one-backend (local) config with a cost model so served
// requests resolve to $.
func meteredConfig(t *testing.T, served, upstream string) *config.Config {
	t.Helper()
	c := mkConfig(t, served, upstream)
	c.CostPerKwh = 0.14
	c.CommandCosts = map[string]map[string]any{
		"local": {"generateWattsPerToken": 0.9, "processWattsPerToken": 0.3},
	}
	return c
}

// TestMeterNonStreaming: a non-streaming reply's top-level usage is captured and
// resolved to $ in the activity record.
func TestMeterNonStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}],` +
			`"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer upstream.Close()

	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	r := chi.NewRouter()
	New(meteredConfig(t, "mock", upstream.URL), mgr, sched.New(), st).Mount(r)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mock"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	acts, _ := st.RecentActivity(1)
	if len(acts) != 1 {
		t.Fatalf("want 1 activity, got %d", len(acts))
	}
	a := acts[0]
	if a.PromptTokens != 10 || a.CompletionTokens != 5 {
		t.Errorf("tokens = %d/%d, want 10/5", a.PromptTokens, a.CompletionTokens)
	}
	// (5·0.9 + 10·0.3) = 7.5 Wh = 0.0075 kWh × $0.14 = $0.00105.
	if d := a.CostUSD - 0.00105; d < -1e-9 || d > 1e-9 {
		t.Errorf("cost = %v, want 0.00105", a.CostUSD)
	}
}

// TestMeterStreaming: usage in the trailing SSE event is captured even after
// many preceding chunks, and the stream still reaches the client.
func TestMeterStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f, _ := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"x"}}]}` + "\n\n"))
			if f != nil {
				f.Flush()
			}
		}
		_, _ = w.Write([]byte(`data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":7}}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	r := chi.NewRouter()
	New(meteredConfig(t, "mock", upstream.URL), mgr, sched.New(), st).Mount(r)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mock","stream":true}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Errorf("stream did not reach client: %s", rec.Body.String())
	}

	acts, _ := st.RecentActivity(1)
	if len(acts) != 1 || acts[0].PromptTokens != 3 || acts[0].CompletionTokens != 7 {
		t.Fatalf("streaming usage = %+v, want 3/7", acts)
	}
}

// TestMeterSwapCost: the request that triggers a cold load is billed the load's
// swap energy on top of its token cost.
func TestMeterSwapCost(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":0,"completion_tokens":0}}`))
	}))
	defer upstream.Close()

	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	cfg := meteredConfig(t, "mock", upstream.URL)
	// Attach swap energy to the (only) model: 18s × 300W.
	m := cfg.Models["mock"]
	m.Swap = &config.Swap{LoadSeconds: 18, LoadWatts: 300}
	cfg.Models["mock"] = m

	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mock"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	acts, _ := st.RecentActivity(1)
	if len(acts) != 1 {
		t.Fatalf("want 1 activity, got %d", len(acts))
	}
	// Zero token cost + swap: 18·300 = 5400 Ws = 1.5 Wh = 0.0015 kWh × $0.14.
	if d := acts[0].CostUSD - 0.00021; d < -1e-9 || d > 1e-9 {
		t.Errorf("cost = %v, want 0.00021 (swap energy)", acts[0].CostUSD)
	}
}

// TestMeterStreamingLargeTail: a stream far larger than the capture cap still
// yields the trailing usage event (exercises the lazy 2× tail trim).
func TestMeterStreamingLargeTail(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f, _ := w.(http.Flusher)
		chunk := []byte(`data: {"choices":[{"delta":{"content":"` + strings.Repeat("x", 4096) + `"}}]}` + "\n\n")
		// ~3 MiB of chunks, well past 2× the 1 MiB cap.
		for i := 0; i < 800; i++ {
			_, _ = w.Write(chunk)
		}
		if f != nil {
			f.Flush()
		}
		_, _ = w.Write([]byte(`data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":22}}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	r := chi.NewRouter()
	New(meteredConfig(t, "mock", upstream.URL), mgr, sched.New(), st).Mount(r)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mock","stream":true}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	acts, _ := st.RecentActivity(1)
	if len(acts) != 1 || acts[0].PromptTokens != 11 || acts[0].CompletionTokens != 22 {
		t.Fatalf("large-stream usage = %+v, want 11/22", acts)
	}
}

// TestMeterGzippedUpstream: a compressing upstream still meters real usage —
// the proxy strips Accept-Encoding so the captured body is identity, not gzip.
func TestMeterGzippedUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		payload := []byte(`{"usage":{"prompt_tokens":8,"completion_tokens":4}}`)
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Content-Type", "application/json")
			gz := gzip.NewWriter(w)
			_, _ = gz.Write(payload)
			_ = gz.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
	defer upstream.Close()

	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	r := chi.NewRouter()
	New(meteredConfig(t, "mock", upstream.URL), mgr, sched.New(), st).Mount(r)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mock"}`))
	req.Header.Set("Accept-Encoding", "gzip") // client asks for gzip; proxy must still meter
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}

	acts, _ := st.RecentActivity(1)
	if len(acts) != 1 || acts[0].PromptTokens != 8 || acts[0].CompletionTokens != 4 {
		t.Fatalf("gzipped upstream usage = %+v, want 8/4", acts)
	}
}

func TestUnknownModel404(t *testing.T) {
	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	r := chi.NewRouter()
	New(&config.Config{Models: map[string]config.Model{}}, mgr, sched.New(), st).Mount(r)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"ghost"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 for unknown model, got %d", rec.Code)
	}
}

func TestModelsCatalog(t *testing.T) {
	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	r := chi.NewRouter()
	New(mkConfig(t, "mock", "http://127.0.0.1:1"), proc.NewManager(&config.Config{}), sched.New(), st).Mount(r)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("models catalog: %d", rec.Code)
	}
	var got struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
			State   string `json:"state"`
			Kind    string `json:"kind"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Object != "list" || len(got.Data) != 1 {
		t.Fatalf("catalog shape: %+v", got)
	}
	m := got.Data[0]
	// Standard OpenAI fields + corrallm metadata (no manager residents → absent).
	if m.ID != "mock" || m.Object != "model" || m.OwnedBy != "corrallm" || m.Created == 0 {
		t.Errorf("standard fields: %+v", m)
	}
	if m.State != "absent" || m.Kind != "model" {
		t.Errorf("metadata: state=%q kind=%q, want absent/model", m.State, m.Kind)
	}
}

// TestAudioTranscriptionMultipart exercises the P9a path: a multipart/form-data
// upload to /v1/audio/transcriptions whose "model" is a form field (not JSON).
// corrallm must extract the model to route, then replay the whole multipart body
// — file part intact — to the STT backend, and log the request like any other.
func TestAudioTranscriptionMultipart(t *testing.T) {
	const audioBytes = "RIFF....fake-wav-payload....data"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path != "/v1/audio/transcriptions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// The replayed multipart body must parse and carry both fields intact.
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("upstream parse multipart: %v", err)
		}
		if got := r.FormValue("model"); got != "whisper-mock" {
			t.Errorf("upstream model field = %q", got)
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("upstream file part: %v", err)
		}
		got, _ := io.ReadAll(f)
		if string(got) != audioBytes {
			t.Errorf("upstream file bytes = %q, want %q", got, audioBytes)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"hello world"}`))
	}))
	defer upstream.Close()

	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	cfg := mkConfig(t, "whisper-mock", upstream.URL)
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)

	// Build the multipart body the way an OpenAI audio client does.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "whisper-mock")
	fw, err := mw.CreateFormFile("file", "speech.wav")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write([]byte(audioBytes))
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"text":"hello world"`) {
		t.Errorf("unexpected transcription body: %s", rec.Body.String())
	}

	acts, err := st.RecentActivity(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 1 {
		t.Fatalf("want 1 activity row, got %d", len(acts))
	}
	if acts[0].Served != "whisper-mock" || acts[0].Status != http.StatusOK ||
		acts[0].Path != "/v1/audio/transcriptions" {
		t.Errorf("activity = %+v", acts[0])
	}
}

// TestModelFromMultipart unit-checks field extraction, including the stream flag,
// and that an unparseable/empty boundary yields no model (→ 400 upstream).
func TestModelFromMultipart(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "whisper-mock")
	_ = mw.WriteField("stream", "true")
	fw, _ := mw.CreateFormFile("file", "a.wav")
	_, _ = fw.Write([]byte("0123456789"))
	_ = mw.Close()

	model, streaming := modelFromMultipart(buf.Bytes(), mw.Boundary())
	if model != "whisper-mock" || !streaming {
		t.Errorf("model=%q streaming=%v, want whisper-mock/true", model, streaming)
	}
	if m, _ := modelFromMultipart(buf.Bytes(), ""); m != "" {
		t.Errorf("empty boundary: model=%q, want empty", m)
	}
}

// flushRecorder is an http.ResponseWriter that records the body, status, and how
// many times Flush() propagated — enough to assert that an SSE stream is flushed
// through chunk-by-chunk rather than buffered into one write. httptest's recorder
// also implements Flusher, but doesn't count calls.
type flushRecorder struct {
	header  http.Header
	body    bytes.Buffer
	code    int
	flushes int
}

func (f *flushRecorder) Header() http.Header {
	if f.header == nil {
		f.header = http.Header{}
	}
	return f.header
}
func (f *flushRecorder) Write(b []byte) (int, error) { return f.body.Write(b) }
func (f *flushRecorder) WriteHeader(c int)           { f.code = c }
func (f *flushRecorder) Flush()                      { f.flushes++ }

// TestAudioTranscriptionStreaming exercises the response-streaming path (P9a,
// stream=true on a file transcription): the STT backend emits text/event-stream
// transcript.text.delta events; corrallm must pass them through in order, flushing
// (not buffering), preserving arbitrary fields (e.g. per-token logprob confidence),
// and log the request 200. This is the same SSE passthrough chat uses — previously
// untested for any route.
func TestAudioTranscriptionStreaming(t *testing.T) {
	events := []string{
		`data: {"type":"transcript.text.delta","delta":"Hello","logprobs":[{"token":"Hello","logprob":-0.12}]}`,
		`data: {"type":"transcript.text.delta","delta":" world"}`,
		`data: {"type":"transcript.text.done","text":"Hello world"}`,
		`data: [DONE]`,
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path != "/v1/audio/transcriptions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("upstream ResponseWriter is not a Flusher")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, e := range events {
			_, _ = io.WriteString(w, e+"\n\n")
			fl.Flush() // push each event separately, like a live transcription
		}
	}))
	defer upstream.Close()

	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	r := chi.NewRouter()
	New(mkConfig(t, "whisper-mock", upstream.URL), mgr, sched.New(), st).Mount(r)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "whisper-mock")
	_ = mw.WriteField("stream", "true")
	fw, _ := mw.CreateFormFile("file", "speech.wav")
	_, _ = fw.Write([]byte("fake-wav"))
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := &flushRecorder{}
	r.ServeHTTP(rec, req)

	if rec.code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.code, rec.body.String())
	}
	body := rec.body.String()
	// All events present, in order.
	last := -1
	for _, want := range []string{`"delta":"Hello"`, `"delta":" world"`, `transcript.text.done`, `[DONE]`} {
		i := strings.Index(body, want)
		if i < 0 {
			t.Fatalf("missing %q in stream: %s", want, body)
		}
		if i < last {
			t.Errorf("event out of order: %q at %d after %d", want, i, last)
		}
		last = i
	}
	// Confidence/logprob fields survive the transparent proxy untouched.
	if !strings.Contains(body, `"logprob":-0.12`) {
		t.Errorf("logprob confidence dropped: %s", body)
	}
	// The stream was flushed through, not buffered into one write.
	if rec.flushes == 0 {
		t.Errorf("no Flush propagated — SSE was buffered, not streamed")
	}

	acts, err := st.RecentActivity(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 1 || acts[0].Status != http.StatusOK || acts[0].Path != "/v1/audio/transcriptions" {
		t.Fatalf("activity = %+v", acts)
	}
}

// TestAudioTranscriptionMetering: an audio request carries no token usage, so it
// is metered by request byte size and persisted to audio_bytes (P9c). Cost is
// AudioRequestUSD(type, bytes) — verified against the recorded byte count.
func TestAudioTranscriptionMetering(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"hi"}`)) // no usage block — like a real STT reply
	}))
	defer upstream.Close()

	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	cfg := mkConfig(t, "whisper", upstream.URL)
	m := cfg.Models["whisper"]
	m.Type = "stt"
	cfg.Models["whisper"] = m
	cfg.CostPerKwh = 0.14
	cfg.CommandCosts = map[string]map[string]any{"stt": {"audioWhPerMiB": 10}}

	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "whisper")
	fw, _ := mw.CreateFormFile("file", "speech.wav")
	_, _ = fw.Write(bytes.Repeat([]byte("A"), 4096))
	_ = mw.Close()
	reqBytes := buf.Len()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	acts, _ := st.RecentActivity(1)
	if len(acts) != 1 {
		t.Fatalf("want 1 activity, got %d", len(acts))
	}
	a := acts[0]
	if a.PromptTokens != 0 || a.CompletionTokens != 0 {
		t.Errorf("audio request metered tokens %d/%d, want 0/0", a.PromptTokens, a.CompletionTokens)
	}
	if a.AudioBytes != int64(reqBytes) {
		t.Errorf("audio_bytes = %d, want %d (request body)", a.AudioBytes, reqBytes)
	}
	// Cost is byte-based: bytes/MiB · 10 Wh/MiB → kWh × $0.14.
	want := float64(reqBytes) / (1 << 20) * 10 / 1000 * 0.14
	if d := a.CostUSD - want; d < -1e-12 || d > 1e-12 {
		t.Errorf("cost = %v, want %v", a.CostUSD, want)
	}
	if a.CostUSD <= 0 {
		t.Errorf("audio cost should be > 0, got %v", a.CostUSD)
	}
}

// TestAudioSpeechTTS exercises P9b: /v1/audio/speech is JSON-in, binary-audio-out.
// corrallm must pass the binary through untouched (never parse it as JSON usage)
// and meter it by OUTPUT byte size (the synthesized audio), not the tiny request.
func TestAudioSpeechTTS(t *testing.T) {
	// 64 KiB of non-JSON "audio" bytes (includes a 0x00 byte to catch any text
	// mangling). A leading "ID3" would be a real MP3; arbitrary bytes suffice here.
	audioOut := bytes.Repeat([]byte{0x00, 0x01, 0x02, 0xff}, 16*1024)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path != "/v1/audio/speech" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// The JSON request must arrive intact (model resolves from it).
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"kokoro"`) || !strings.Contains(string(body), `"hello"`) {
			t.Errorf("upstream did not receive the JSON body: %s", body)
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(audioOut)
	}))
	defer upstream.Close()

	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	cfg := mkConfig(t, "kokoro", upstream.URL)
	m := cfg.Models["kokoro"]
	m.Type = "tts"
	cfg.Models["kokoro"] = m
	cfg.CostPerKwh = 0.14
	cfg.CommandCosts = map[string]map[string]any{"tts": {"audioWhPerMiB": 5}}

	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech",
		strings.NewReader(`{"model":"kokoro","input":"hello","voice":"af_sky"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	// Binary audio passed through byte-for-byte.
	if !bytes.Equal(rec.Body.Bytes(), audioOut) {
		t.Errorf("binary audio corrupted: got %d bytes, want %d", rec.Body.Len(), len(audioOut))
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/mpeg" {
		t.Errorf("content-type = %q, want audio/mpeg", ct)
	}

	acts, _ := st.RecentActivity(1)
	if len(acts) != 1 {
		t.Fatalf("want 1 activity, got %d", len(acts))
	}
	a := acts[0]
	if a.PromptTokens != 0 || a.CompletionTokens != 0 {
		t.Errorf("TTS metered tokens %d/%d, want 0/0", a.PromptTokens, a.CompletionTokens)
	}
	// Metered by OUTPUT bytes, not the small request body.
	if a.AudioBytes != int64(len(audioOut)) {
		t.Errorf("audio_bytes = %d, want %d (output size)", a.AudioBytes, len(audioOut))
	}
	want := float64(len(audioOut)) / (1 << 20) * 5 / 1000 * 0.14
	if d := a.CostUSD - want; d < -1e-12 || d > 1e-12 {
		t.Errorf("cost = %v, want %v", a.CostUSD, want)
	}
}

// TestClientCancelLogged499 reproduces the production "502 on qwen" mislabel:
// when the caller (or an upstream front-proxy) drops the connection mid-request,
// corrallm must log it as 499 with the reason captured — NOT a backend 502.
func TestClientCancelLogged499(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		close(started)
		<-release // hang until the client has canceled
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	defer close(release)

	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	r := chi.NewRouter()
	New(mkConfig(t, "mock", upstream.URL), mgr, sched.New(), st).Mount(r)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mock"}`)).WithContext(ctx)
	done := make(chan struct{})
	go func() { r.ServeHTTP(httptest.NewRecorder(), req); close(done) }()

	<-started // backend reached, request in flight
	cancel()  // client/front-proxy drops the connection
	<-done

	acts, _ := st.RecentActivity(1)
	if len(acts) != 1 {
		t.Fatalf("want 1 activity, got %d", len(acts))
	}
	if acts[0].Status != 499 {
		t.Errorf("status = %d, want 499 (client cancel, not 502)", acts[0].Status)
	}
	if !strings.Contains(acts[0].Error, "canceled") {
		t.Errorf("error reason = %q, want it to mention cancellation", acts[0].Error)
	}
}

// TestRequestTimeout504 verifies the configurable cap: a backend slower than the
// timeout yields 504 with the deadline reason, and 0 (default) imposes no cap.
func TestRequestTimeout504(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		<-release // slower than the configured timeout
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()
	defer close(release)

	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	r := chi.NewRouter()
	p := New(mkConfig(t, "mock", upstream.URL), mgr, sched.New(), st)
	p.SetRequestTimeout(150 * time.Millisecond)
	p.Mount(r)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mock"}`)))

	acts, _ := st.RecentActivity(1)
	if len(acts) != 1 || acts[0].Status != http.StatusGatewayTimeout {
		t.Fatalf("status = %v, want 504", acts)
	}
	if !strings.Contains(acts[0].Error, "deadline") {
		t.Errorf("error reason = %q, want deadline-exceeded", acts[0].Error)
	}
}

// TestPayloadCapture (P10b): a text request captures the request body + response
// body (capped) + TTFB; an STT multipart upload is summarized to a size, never
// stored raw; capture can be disabled.
func TestPayloadCapture(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hello"}}]}`))
	}))
	defer upstream.Close()

	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	r := chi.NewRouter()
	New(mkConfig(t, "mock", upstream.URL), mgr, sched.New(), st).Mount(r)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mock","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	acts, _ := st.RecentActivity(1)
	if len(acts) != 1 || acts[0].ID == 0 {
		t.Fatalf("no row/id: %+v", acts)
	}
	full, err := st.ActivityByID(acts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(full.ReqBody, `"content":"hi"`) {
		t.Errorf("req body not captured: %q", full.ReqBody)
	}
	if !strings.Contains(full.RespBody, `"content":"hello"`) {
		t.Errorf("resp body not captured: %q", full.RespBody)
	}

	// Disabled capture stores nothing.
	st2, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st2.Close() }()
	r2 := chi.NewRouter()
	p2 := New(mkConfig(t, "mock", upstream.URL), mgr, sched.New(), st2)
	p2.SetCapturePayloads(false)
	p2.Mount(r2)
	r2.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mock"}`)))
	a2, _ := st2.RecentActivity(1)
	if len(a2) == 1 {
		if d, _ := st2.ActivityByID(a2[0].ID); d.ReqBody != "" || d.RespBody != "" {
			t.Errorf("capture disabled but stored: %q / %q", d.ReqBody, d.RespBody)
		}
	}
}

// TestReqBodyCapAllowsLargeReplay: a request body larger than the 4 KiB response
// cap but under reqBodyCap is stored WHOLE and stays valid JSON — the property
// console replay depends on (a 4 KiB truncation left agentic requests
// unparseable, so replay dumped raw text instead of rendering turns).
func TestReqBodyCapAllowsLargeReplay(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()
	r := chi.NewRouter()
	New(mkConfig(t, "mock", upstream.URL), mgr, sched.New(), st).Mount(r)

	// A ~12 KiB request (well over the 4 KiB payloadCap, under 256 KiB reqBodyCap).
	big := strings.Repeat("x", 12<<10)
	reqJSON := `{"model":"mock","messages":[{"role":"user","content":"` + big + `"}]}`
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqJSON)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	acts, _ := st.RecentActivity(1)
	full, err := st.ActivityByID(acts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(full.ReqBody, "truncated") {
		t.Errorf("12 KiB req body was truncated (reqBodyCap not applied): len=%d", len(full.ReqBody))
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(full.ReqBody), &parsed); err != nil {
		t.Errorf("stored req body is not valid JSON (replay would fail): %v", err)
	}
}

// TestPayloadCaptureBinaryAudio: an STT multipart upload is summarized (size +
// content-type), not stored as raw audio bytes.
func TestPayloadCaptureBinaryAudio(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"hi"}`))
	}))
	defer upstream.Close()

	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()
	r := chi.NewRouter()
	New(mkConfig(t, "whisper", upstream.URL), mgr, sched.New(), st).Mount(r)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "whisper")
	fw, _ := mw.CreateFormFile("file", "a.wav")
	_, _ = fw.Write(bytes.Repeat([]byte{0x00, 0x01}, 4096))
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	r.ServeHTTP(httptest.NewRecorder(), req)

	acts, _ := st.RecentActivity(1)
	full, _ := st.ActivityByID(acts[0].ID)
	if !strings.HasPrefix(full.ReqBody, "<multipart/form-data,") {
		t.Errorf("STT req body should be summarized, got: %q", full.ReqBody)
	}
	if strings.Contains(full.ReqBody, "\x00") {
		t.Errorf("raw audio bytes leaked into req body")
	}
}

// TestRealtimeWebSocketPassthrough (P9e): a /v1/realtime upgrade routes through
// corrallm to the backend, the 101 completes both ways, bytes pass bidirectionally,
// and the session is metered (audio in = client→backend bytes) on close.
func TestRealtimeWebSocketPassthrough(t *testing.T) {
	// Backend: answers /health, else completes a ws upgrade and echoes bytes.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("backend not a hijacker")
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Errorf("backend hijack: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_, _ = io.Copy(conn, buf) // echo client→backend bytes back
	}))
	defer backend.Close()

	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	r := chi.NewRouter()
	New(mkConfig(t, "mock", backend.URL), mgr, sched.New(), st).Mount(r)
	front := httptest.NewServer(r)
	defer front.Close()

	// Raw-conn WebSocket client: write the upgrade, read 101, exchange bytes.
	host := strings.TrimPrefix(front.URL, "http://")
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, "GET /v1/realtime?model=mock HTTP/1.1\r\n"+
		"Host: "+host+"\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n")

	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(statusLine, "101") {
		t.Fatalf("want 101 upgrade, got %q", strings.TrimSpace(statusLine))
	}
	for { // drain upgrade headers to the blank line
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}

	// Bidirectional: send, read it echoed back.
	const msg = "audio-frame-bytes"
	if _, err := io.WriteString(conn, msg); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(got) != msg {
		t.Errorf("echo = %q, want %q", got, msg)
	}
	_ = conn.Close() // ends the session → corrallm releases + logs

	// The session is metered on close (poll — logging follows the copy returning).
	var a store.Activity
	for i := 0; i < 50; i++ {
		if acts, _ := st.RecentActivity(1); len(acts) == 1 {
			a = acts[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if a.Served != "mock" || a.Path != "/v1/realtime" {
		t.Fatalf("activity = %+v", a)
	}
	if a.AudioBytes < int64(len(msg)) {
		t.Errorf("audio_bytes = %d, want >= %d (client→backend bytes)", a.AudioBytes, len(msg))
	}
}

// TestRealtimePreemptAbortsSession (P9e DoD): a low, interruptible realtime
// session holds the only slot; a higher-priority request preempts it. The
// preemption must tear down the *hijacked* WebSocket conn (the flagged risk) —
// freeing the slot for the preemptor and logging the session 499/preempted.
func TestRealtimePreemptAbortsSession(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Header.Get("Upgrade") == "websocket" {
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
			_, _ = io.Copy(conn, buf) // hold until corrallm closes us on preemption
			return
		}
		_, _ = w.Write([]byte(`{"served_by":"hi"}`)) // the preempting chat request
	}))
	defer backend.Close()

	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	cfg := &config.Config{
		Models: map[string]config.Model{
			"m": modelTo(t, backend.URL, "local"),
		},
		PriorityGroups: map[string]config.PriorityGroup{
			"lo": {Weight: 1, Interruptible: true,
				OnSaturated: map[string]config.Stage{"default": {Reject: true}}},
			"hi": {Weight: 10, OnSaturated: map[string]config.Stage{"local": {Preempt: true}}},
		},
		Keys: map[string]string{"lokey": "lo", "hikey": "hi"},
	}
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)
	front := httptest.NewServer(r)
	defer front.Close()

	// Low-priority realtime session occupies the only slot.
	host := strings.TrimPrefix(front.URL, "http://")
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, "GET /v1/realtime?model=m HTTP/1.1\r\nHost: "+host+
		"\r\nX-Corrallm-Key: lokey\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n")
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil || !strings.Contains(line, "101") {
		t.Fatalf("lo session upgrade failed: %q (%v)", line, err)
	}
	// slot is now held by the lo session.

	// Higher-priority chat preempts → must abort the lo ws session and be served.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("X-Corrallm-Key", "hikey")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"served_by":"hi"`) {
		t.Fatalf("preemptor not served: %d %s", rec.Code, rec.Body.String())
	}

	// The aborted realtime session logs 499/preempted.
	var rt store.Activity
	for i := 0; i < 100; i++ {
		acts, _ := st.RecentActivity(10)
		for _, a := range acts {
			if a.Path == "/v1/realtime" {
				rt = a
			}
		}
		if rt.Path != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rt.Path != "/v1/realtime" || rt.Status != 499 || rt.Error != "preempted" {
		t.Errorf("realtime session row = %+v, want 499/preempted", rt)
	}
}

// TestRealtimeIdleReaper (P9e): a realtime session that goes silent past the idle
// timeout is reaped — the conn is torn down, the slot freed, the row logged 408
// with "idle timeout".
func TestRealtimeIdleReaper(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_, _ = io.Copy(conn, buf) // never receives anything → idle
	}))
	defer backend.Close()

	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	r := chi.NewRouter()
	p := New(mkConfig(t, "mock", backend.URL), mgr, sched.New(), st)
	p.SetRealtimeTimeouts(150*time.Millisecond, 0) // reap after 150ms idle
	p.Mount(r)
	front := httptest.NewServer(r)
	defer front.Close()

	host := strings.TrimPrefix(front.URL, "http://")
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, "GET /v1/realtime?model=mock HTTP/1.1\r\nHost: "+host+
		"\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n")
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil || !strings.Contains(line, "101") {
		t.Fatalf("upgrade failed: %q (%v)", line, err)
	}
	// Send nothing — the reaper should close us. A blocking read returns on reap.
	_, _ = io.Copy(io.Discard, br)

	var a store.Activity
	for i := 0; i < 100; i++ {
		if acts, _ := st.RecentActivity(1); len(acts) == 1 && acts[0].Path == "/v1/realtime" {
			a = acts[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if a.Status != http.StatusRequestTimeout || a.Error != "idle timeout" {
		t.Errorf("reaped row = %+v, want 408/idle timeout", a)
	}
}

// TestCapabilitiesManifest: /v1/capabilities is public, groups models by
// capability (inferred from cost class), lists endpoints with examples, and shows
// lanes WITHOUT exposing API keys.
func TestCapabilitiesManifest(t *testing.T) {
	cfg := &config.Config{
		CommandCosts: map[string]map[string]any{
			"stt": {"audioWhPerMiB": 0.03}, "tts": {"audioWhPerMiB": 0.05},
		},
		Models: map[string]config.Model{
			"qwen":     {Type: "chat"},
			"nomic":    {Type: "embed"},
			"parakeet": {Type: "stt"},
			"kokoro":   {Type: "tts"},
		},
		PriorityGroups: map[string]config.PriorityGroup{
			"interactive": {Weight: 10, Interruptible: false},
		},
		Keys: map[string]string{"secret-key-aw3": "interactive"},
	}
	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var m struct {
		Service            string              `json:"service"`
		ModelsByCapability map[string][]string `json:"models_by_capability"`
		Endpoints          []struct {
			Path       string `json:"path"`
			Capability string `json:"capability"`
		} `json:"endpoints"`
		Lanes []struct {
			Name   string `json:"name"`
			Weight int    `json:"weight"`
		} `json:"lanes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m.Service != "corrallm" {
		t.Errorf("service = %q", m.Service)
	}
	if got := m.ModelsByCapability; len(got["chat"]) != 1 || len(got["embeddings"]) != 1 ||
		len(got["audio.stt"]) != 1 || len(got["audio.tts"]) != 1 {
		t.Errorf("capability grouping = %+v", got)
	}
	if len(m.Endpoints) < 8 {
		t.Errorf("want the full endpoint surface, got %d", len(m.Endpoints))
	}
	if len(m.Lanes) != 1 || m.Lanes[0].Name != "interactive" || m.Lanes[0].Weight != 10 {
		t.Errorf("lanes = %+v", m.Lanes)
	}
	// API keys must NEVER appear in the public manifest.
	if strings.Contains(rec.Body.String(), "secret-key-aw3") {
		t.Error("API key leaked into /v1/capabilities")
	}
}

// TestExtractUsageTelemetry covers the backend-measured telemetry (cached
// tokens + tp/s + tg/s) extractUsage derives alongside token accounting, for
// both non-streaming and streaming replies, plus the cache_n fallback.
func TestExtractUsageTelemetry(t *testing.T) {
	// Non-streaming: OpenAI-shape cached_tokens + llama.cpp timings.
	nonStream := []byte(`{
		"usage": {
			"prompt_tokens": 100,
			"completion_tokens": 20,
			"prompt_tokens_details": {"cached_tokens": 64}
		},
		"timings": {"prompt_per_second": 512.5, "predicted_per_second": 88.25, "cache_n": 40}
	}`)
	u := extractUsage(nonStream, false)
	if u.PromptTokens != 100 || u.CompletionTokens != 20 {
		t.Fatalf("token accounting changed: %+v", u)
	}
	if u.CachedTokens != 64 { // prefers prompt_tokens_details over cache_n
		t.Errorf("cachedTokens = %d, want 64", u.CachedTokens)
	}
	if u.PromptPerSec != 512.5 || u.PredictedPerSec != 88.25 {
		t.Errorf("speeds = tp/s %v tg/s %v", u.PromptPerSec, u.PredictedPerSec)
	}

	// cache_n fallback: no prompt_tokens_details.cached_tokens present.
	fallback := []byte(`{
		"usage": {"prompt_tokens": 100, "completion_tokens": 20},
		"timings": {"prompt_per_second": 10, "predicted_per_second": 5, "cache_n": 33}
	}`)
	if u := extractUsage(fallback, false); u.CachedTokens != 33 {
		t.Errorf("cache_n fallback: cachedTokens = %d, want 33", u.CachedTokens)
	}

	// Streaming: usage + timings ride in the final data: event.
	stream := []byte(strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":64}},"timings":{"prompt_per_second":512.5,"predicted_per_second":88.25,"cache_n":40}}`,
		`data: [DONE]`,
		``,
	}, "\n"))
	su := extractUsage(stream, true)
	if su.CachedTokens != 64 || su.PromptPerSec != 512.5 || su.PredictedPerSec != 88.25 {
		t.Errorf("streaming telemetry = %+v", su)
	}

	// Neither present → zeros (existing behavior preserved).
	bare := []byte(`{"usage": {"prompt_tokens": 100, "completion_tokens": 20}}`)
	if u := extractUsage(bare, false); u.CachedTokens != 0 || u.PromptPerSec != 0 || u.PredictedPerSec != 0 {
		t.Errorf("bare body should yield zero telemetry, got %+v", u)
	}
}
