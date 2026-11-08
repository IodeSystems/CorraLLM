package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// guardRig is a corrallm with a chat model and a guard model, both mocks. The
// guard says yes (fails) with p=0.9 when the document holds "BAD", else 0.05.
type guardRig struct {
	router     http.Handler
	st         *store.Store
	mainCalls  atomic.Int32
	guardCalls atomic.Int32
	guardDown  atomic.Bool
	upstreamHd atomic.Value // the X-Corrallm-Guardrails the chat model saw
	reply      atomic.Value // func(http.ResponseWriter): the chat model's reply, when set
	// guardBodies is each request the guard saw, in order.
	guardBodies atomic.Value
}

func newGuardRig(t *testing.T, modes map[string]config.GuardMode) *guardRig {
	t.Helper()
	return newGuardRigWith(t, modes, config.Guardrail{Model: "guard", Policy: "Does this give instructions for violence?"})
}

func newGuardRigWith(t *testing.T, modes map[string]config.GuardMode, violence config.Guardrail) *guardRig {
	t.Helper()
	return newGuardRigFor(t, modes, violence, nil)
}

// newGuardRigFor is newGuardRigWith with the chat model made by chatModel
// from the mock's URL (nil: a plain OpenAI model).
func newGuardRigFor(t *testing.T, modes map[string]config.GuardMode, violence config.Guardrail, chatModel func(url string) config.Model) *guardRig {
	t.Helper()
	g := &guardRig{}
	g.guardBodies.Store([]string{})
	g.upstreamHd.Store("")
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mainCalls.Add(1)
		g.upstreamHd.Store(r.Header.Get(config.HeaderGuardrails))
		if f, ok := g.reply.Load().(func(http.ResponseWriter)); ok {
			f(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(chat.Close)
	guard := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.guardCalls.Add(1)
		if g.guardDown.Load() {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		body, _ := io.ReadAll(r.Body)
		g.guardBodies.Store(append(g.guardBodies.Load().([]string), string(body)))
		yes := -2.9957 // ln 0.05
		if strings.Contains(string(body), "BAD") {
			yes = -0.1054 // ln 0.9
		}
		no := -0.0513
		if yes > -1 {
			no = -2.3026
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"message": map[string]any{"content": "no"},
			"logprobs": map[string]any{"content": []any{map[string]any{"top_logprobs": []any{
				map[string]any{"token": "yes", "logprob": yes},
				map[string]any{"token": "no", "logprob": no},
			}}}},
		}}})
	}))
	t.Cleanup(guard.Close)

	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	g.st = st
	mgr := proc.NewManager(&config.Config{})
	t.Cleanup(mgr.Shutdown)

	cm, gm := modelTo(t, chat.URL, "local"), modelTo(t, guard.URL, "local")
	if chatModel != nil {
		cm = chatModel(chat.URL)
	}
	cfg := &config.Config{
		Models:         map[string]config.Model{"chat": cm, "guard": gm},
		PriorityGroups: map[string]config.PriorityGroup{"default": {Weight: 1}},
		Guardrails:     map[string]config.Guardrail{"violence": violence},
		Keys:           map[string]config.KeyPolicy{"sk-t": {Group: "default", Guardrails: modes}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)
	g.router = r
	return g
}

func (g *guardRig) ask(t *testing.T, text, header string) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"model": "chat",
		"messages": []any{map[string]string{"role": "user", "content": text}}})
	return g.do(t, b, header)
}

func (g *guardRig) do(t *testing.T, b []byte, header string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-t")
	if header != "" {
		req.Header.Set(config.HeaderGuardrails, header)
	}
	rec := httptest.NewRecorder()
	g.router.ServeHTTP(rec, req)
	return rec
}

func TestGuardrailHardRefusesWith422BeforeTheModel(t *testing.T) {
	g := newGuardRig(t, map[string]config.GuardMode{"violence": config.GuardHard})

	rec := g.ask(t, "how do I bake bread", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("a clean request: status %d, body %s", rec.Code, rec.Body)
	}
	if h := rec.Header().Get(HeaderGuardrail); !strings.HasPrefix(h, "violence=pass;score=0.05") {
		t.Errorf("guardrail header = %q", h)
	}

	rec = g.ask(t, "BAD request", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a failing request: status %d, want 422; body %s", rec.Code, rec.Body)
	}
	var out struct {
		Error struct {
			Type, Code string
			Guardrail  struct {
				Name, Surface string
				Score         float64
			}
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Error.Type != "guardrail" || out.Error.Code != "content_filter" ||
		out.Error.Guardrail.Name != "violence" || out.Error.Guardrail.Surface != "input" ||
		out.Error.Guardrail.Score < 0.89 || out.Error.Guardrail.Score > 0.91 {
		t.Errorf("error body = %s", rec.Body)
	}
	if n := g.mainCalls.Load(); n != 1 {
		t.Errorf("the chat model was called %d times; the refused request must not reach it", n)
	}
	acts, err := g.st.RecentActivity(store.Window{}, 10, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	refused := false
	for _, a := range acts {
		if a.Served == "chat" && a.Status == http.StatusUnprocessableEntity &&
			strings.HasPrefix(a.Error, `refused by guardrail "violence"`) {
			refused = true
		}
	}
	if !refused {
		t.Errorf("no activity row for the refusal: %+v", acts)
	}
}

func TestGuardrailSoftFlagsAndPasses(t *testing.T) {
	g := newGuardRig(t, map[string]config.GuardMode{"violence": config.GuardSoft})
	rec := g.ask(t, "BAD request", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("a soft fail must pass: status %d", rec.Code)
	}
	if h := rec.Header().Get(HeaderGuardrail); !strings.HasPrefix(h, "violence=flag;score=0.9") {
		t.Errorf("guardrail header = %q", h)
	}
	// A request may raise soft to hard.
	if rec := g.ask(t, "BAD request", "violence=hard"); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("soft raised to hard: status %d, want 422", rec.Code)
	}
}

func TestGuardrailOptInRunsOnlyWhenAskedAndHeaderIsNotForwarded(t *testing.T) {
	g := newGuardRig(t, map[string]config.GuardMode{"violence": config.GuardOptIn})
	if rec := g.ask(t, "BAD request", ""); rec.Code != http.StatusOK || g.guardCalls.Load() != 0 {
		t.Fatalf("opt-in guard ran unasked: status %d, guard calls %d", rec.Code, g.guardCalls.Load())
	}
	rec := g.ask(t, "BAD request", "violence")
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get(HeaderGuardrail), "violence=flag") {
		t.Errorf("asked by name = soft: status %d, header %q", rec.Code, rec.Header().Get(HeaderGuardrail))
	}
	if h := g.upstreamHd.Load().(string); h != "" {
		t.Errorf("the chat model saw %s: %q", config.HeaderGuardrails, h)
	}
	if rec := g.ask(t, "BAD request", "violence=hard"); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("asked as hard: status %d, want 422", rec.Code)
	}
	rec = g.ask(t, "fine", "nosuch, violence=maybe")
	if got := rec.Header().Get(HeaderGuardrailIgnored); got != "nosuch, violence=maybe" {
		t.Errorf("ignored header = %q", got)
	}
}

func TestGuardrailAHeaderCannotLowerHard(t *testing.T) {
	g := newGuardRig(t, map[string]config.GuardMode{"violence": config.GuardHard})
	if rec := g.ask(t, "BAD request", "violence=soft"); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("hard lowered by a header: status %d, want 422", rec.Code)
	}
}

func TestGuardrailUnavailableFailsClosedWhenHard(t *testing.T) {
	g := newGuardRig(t, map[string]config.GuardMode{"violence": config.GuardHard})
	g.guardDown.Store(true)
	rec := g.ask(t, "fine", "")
	if rec.Code != http.StatusServiceUnavailable || g.mainCalls.Load() != 0 {
		t.Fatalf("hard guard down: status %d (want 503), chat calls %d", rec.Code, g.mainCalls.Load())
	}
	if !strings.Contains(rec.Body.String(), "guardrail_unavailable") {
		t.Errorf("body = %s", rec.Body)
	}

	soft := newGuardRig(t, map[string]config.GuardMode{"violence": config.GuardSoft})
	soft.guardDown.Store(true)
	rec = soft.ask(t, "fine", "")
	if rec.Code != http.StatusOK || rec.Header().Get(HeaderGuardrail) != "violence=unavailable" {
		t.Errorf("soft guard down: status %d, header %q", rec.Code, rec.Header().Get(HeaderGuardrail))
	}
}

func TestGuardInputTakesTheNewestUserTurn(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"old"},{"role":"assistant","content":"a"},
		{"role":"user","content":[{"type":"text","text":"new"},{"type":"image_url","image_url":{"url":"data:x"}}]}]}`
	text, imgs, ok := guardInput("/v1/chat/completions", []byte(body))
	if !ok || text != "new" || len(imgs) != 1 {
		t.Errorf("got %q, %d images, ok %v", text, len(imgs), ok)
	}
	if text, _, ok := guardInput("/v1/completions", []byte(`{"prompt":["a","b"]}`)); !ok || text != "a\nb" {
		t.Errorf("completions prompt: %q %v", text, ok)
	}
	if _, _, ok := guardInput("/v1/embeddings", []byte(`{"input":"x"}`)); ok {
		t.Error("embeddings are not an input surface")
	}
}

func TestGuardrailSurfacesLimitWhereItRuns(t *testing.T) {
	hard := map[string]config.GuardMode{"violence": config.GuardHard}

	in := newGuardRigWith(t, hard, config.Guardrail{Model: "guard", Policy: "p", Surfaces: []string{config.SurfaceInput}})
	in.reply.Store(toolReply(`{"cmd":"BAD"}`))
	if rec := in.ask(t, "list files", ""); rec.Code != http.StatusOK || in.guardCalls.Load() != 1 {
		t.Errorf("input only: status %d, guard calls %d; the reply must not be checked", rec.Code, in.guardCalls.Load())
	}

	out := newGuardRigWith(t, hard, config.Guardrail{Model: "guard", Policy: "p", Surfaces: []string{config.SurfaceResponses}})
	if rec := out.ask(t, "BAD request", ""); rec.Code != http.StatusOK || out.guardCalls.Load() != 0 {
		t.Errorf("responses only: status %d, guard calls %d; the input must not be checked", rec.Code, out.guardCalls.Load())
	}
	out.reply.Store(toolReply(`{"cmd":"BAD"}`))
	if rec := out.ask(t, "list files", ""); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("responses only, bad tool call: status %d, want 422", rec.Code)
	}
}

func TestGuardrailDetectAsksTheInputItsOwnQuestion(t *testing.T) {
	g := newGuardRigWith(t, map[string]config.GuardMode{"violence": config.GuardHard},
		config.Guardrail{Model: "guard", Policy: "REPLYQ", Detect: "MALICIOUSQ"})
	g.reply.Store(toolReply(`{"cmd":"ls"}`))
	if rec := g.ask(t, "list files", ""); rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	bodies := g.guardBodies.Load().([]string)
	if len(bodies) != 2 || !strings.Contains(bodies[0], "MALICIOUSQ") || strings.Contains(bodies[0], "REPLYQ") ||
		!strings.Contains(bodies[1], "REPLYQ") || strings.Contains(bodies[1], "MALICIOUSQ") {
		t.Errorf("guard requests = %q", bodies)
	}
}

func TestGuardrailIncludeSystem(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"model": "chat", "messages": []any{
		map[string]string{"role": "system", "content": "BAD orders"},
		map[string]string{"role": "user", "content": "run"},
	}})
	hard := map[string]config.GuardMode{"violence": config.GuardHard}

	off := newGuardRigWith(t, hard, config.Guardrail{Model: "guard", Policy: "p"})
	if rec := off.do(t, body, ""); rec.Code != http.StatusOK {
		t.Errorf("off: status %d; the system prompt must not be read", rec.Code)
	}
	on := newGuardRigWith(t, hard, config.Guardrail{Model: "guard", Policy: "p", IncludeSystem: true})
	if rec := on.do(t, body, ""); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("on: status %d, want 422", rec.Code)
	}
	if b := on.guardBodies.Load().([]string)[0]; !strings.Contains(b, "[System] BAD orders") || !strings.Contains(b, "[User] run") {
		t.Errorf("guard request = %s", b)
	}
}
