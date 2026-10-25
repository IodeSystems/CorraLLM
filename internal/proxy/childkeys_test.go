package proxy

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

type childKeyRig struct {
	t   *testing.T
	r   *chi.Mux
	st  *store.Store
	cfg *config.Config
	mgr *proc.Manager
	px  *Proxy
}

// newChildKeyRig serves model "mock" (and "other") at $0.00105 a request,
// with configured keys "parent" and "peer" (both may mint) and "plain".
func newChildKeyRig(t *testing.T, parentLimits config.LimitSet) *childKeyRig {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}],` +
			`"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	t.Cleanup(upstream.Close)
	st, _ := store.Open(context.Background(), ":memory:")
	t.Cleanup(func() { _ = st.Close() })
	mgr := proc.NewManager(&config.Config{})
	t.Cleanup(mgr.Shutdown)

	cfg := meteredConfig(t, "mock", upstream.URL)
	cfg.Models["other"] = cfg.Models["mock"]
	cfg.Keys = map[string]config.KeyPolicy{
		"parent": {Limits: parentLimits, Mint: true},
		"peer":   {Mint: true},
		"plain":  {},
	}
	rig := &childKeyRig{t: t, st: st, cfg: cfg, mgr: mgr}
	rig.mount()
	return rig
}

// mount builds a fresh proxy over the same store, as a restart would.
func (k *childKeyRig) mount() {
	k.r = chi.NewRouter()
	k.px = New(k.cfg, k.mgr, sched.New(), k.st)
	k.px.SetRequestFiler(func(r *http.Request) config.Filer {
		return config.Filer{Peer: net.ParseIP("192.168.1.9"), Forwarded: r.Header.Get("X-Test-Remote") != "",
			User: r.Header.Get("X-Test-User")}
	})
	k.px.Mount(k.r)
}

func (k *childKeyRig) do(method, path, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	k.r.ServeHTTP(rec, req)
	return rec
}

func (k *childKeyRig) chat(key, model string) int {
	return k.do(http.MethodPost, "/v1/chat/completions", key, `{"model":"`+model+`"}`).Code
}

func (k *childKeyRig) mint(body string) childKeyBody {
	k.t.Helper()
	rec := k.do(http.MethodPost, "/v1/keys", "parent", body)
	if rec.Code != http.StatusCreated {
		k.t.Fatalf("mint: %d %s", rec.Code, rec.Body.String())
	}
	var out childKeyBody
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || !strings.HasPrefix(out.Key, "ck-") {
		k.t.Fatalf("mint body: %v %s", err, rec.Body.String())
	}
	return out
}

// A child's share of one model refuses that model once spent, while the child
// is still served by another, and its requests are recorded as its own.
func TestChildKeyModelShare(t *testing.T) {
	k := newChildKeyRig(t, nil)
	child := k.mint(`{"label":"proj-x","limits":[{"req":1,"per":"calendar-day","hard":true,"model":"mock"}]}`)

	if c := k.chat(child.Key, "mock"); c != http.StatusOK {
		t.Fatalf("first: %d", c)
	}
	rec := k.do(http.MethodPost, "/v1/chat/completions", child.Key, `{"model":"mock"}`)
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "allocation_exhausted") {
		t.Fatalf("over share: %d %s", rec.Code, rec.Body.String())
	}
	if c := k.chat(child.Key, "other"); c != http.StatusOK {
		t.Errorf("another model: %d, want served", c)
	}
	if c := k.chat("parent", "mock"); c != http.StatusOK {
		t.Errorf("the parent itself: %d, want served (the share is the child's)", c)
	}
	acts, _ := k.st.RecentActivity(store.Window{}, 10, "", "", "")
	n := 0
	for _, a := range acts {
		if a.Key == child.Key {
			n++
		}
	}
	if n != 3 {
		t.Errorf("rows recorded under the child = %d, want 3", n)
	}
}

// The parent's allocation bounds its children together: a child with no
// limit of its own stops when the parent's is spent.
func TestChildKeyChargesParent(t *testing.T) {
	k := newChildKeyRig(t, config.LimitSet{{USD: 0.002, Per: "calendar-day", Hard: true}})
	a := k.mint(`{"label":"a"}`)
	b := k.mint(`{"label":"b"}`)
	if c := k.chat(a.Key, "mock"); c != http.StatusOK {
		t.Fatalf("a: %d", c)
	}
	if c := k.chat(b.Key, "mock"); c != http.StatusOK {
		t.Fatalf("b: %d", c)
	}
	// $0.0021 spent across both children: the parent's $0.002 is full.
	if c := k.chat(a.Key, "mock"); c != http.StatusTooManyRequests {
		t.Errorf("a after the parent is spent: %d, want 429", c)
	}
	if c := k.chat("parent", "mock"); c != http.StatusTooManyRequests {
		t.Errorf("parent: %d, want 429", c)
	}
	if c := k.chat("peer", "mock"); c != http.StatusOK {
		t.Errorf("an unrelated key: %d, want served", c)
	}
	var list struct {
		Self struct {
			Windows map[string][]struct{ Used float64 } `json:"windows"`
		} `json:"self"`
		Keys []childKeyBody `json:"keys"`
	}
	rec := k.do(http.MethodGet, "/v1/keys", "parent", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list: %v %s", err, rec.Body.String())
	}
	labels := map[string]bool{}
	for _, c := range list.Keys {
		labels[c.Label] = true
	}
	if len(list.Keys) != 2 || !labels["a"] || !labels["b"] {
		t.Errorf("list = %+v", list.Keys)
	}
	if w := list.Self.Windows["key"]; len(w) != 1 || w[0].Used < 0.002 {
		t.Errorf("parent windows = %+v, want the children's spend", list.Self.Windows)
	}
}

// Who may manage what.
func TestChildKeyPermissions(t *testing.T) {
	k := newChildKeyRig(t, nil)
	child := k.mint(`{"label":"x"}`)
	for _, c := range []struct {
		name, method, path, key, body string
		want                          int
	}{
		{"no key", http.MethodGet, "/v1/keys", "", "", http.StatusUnauthorized},
		{"a stranger", http.MethodPost, "/v1/keys", "nobody", `{}`, http.StatusForbidden},
		{"a child minting", http.MethodPost, "/v1/keys", child.Key, `{}`, http.StatusForbidden},
		{"a key without mint", http.MethodPost, "/v1/keys", "plain", `{}`, http.StatusForbidden},
		{"another parent's child", http.MethodGet, "/v1/keys/" + child.Key, "peer", "", http.StatusNotFound},
		{"another parent deleting", http.MethodDelete, "/v1/keys/" + child.Key, "peer", "", http.StatusNotFound},
		{"an unknown model", http.MethodPost, "/v1/keys", "parent", `{"limits":[{"usd":1,"per":"day","model":"nope"}]}`, http.StatusBadRequest},
		{"a sec limit", http.MethodPost, "/v1/keys", "parent", `{"limits":[{"sec":1,"per":"day"}]}`, http.StatusBadRequest},
	} {
		if rec := k.do(c.method, c.path, c.key, c.body); rec.Code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, rec.Code, rec.Body.String(), c.want)
		}
	}
}

// A child survives a restart, an edit keeps what was spent, and a deleted key
// stops working.
func TestChildKeyLifecycle(t *testing.T) {
	k := newChildKeyRig(t, nil)
	child := k.mint(`{"label":"x","limits":[{"req":2,"per":"calendar-day","hard":true}]}`)
	if c := k.chat(child.Key, "mock"); c != http.StatusOK {
		t.Fatalf("first: %d", c)
	}
	k.mount()
	if rec := k.do(http.MethodPatch, "/v1/keys/"+child.Key, "parent", `{"label":"renamed"}`); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"renamed"`) {
		t.Fatalf("patch after restart: %d %s", rec.Code, rec.Body.String())
	}
	if c := k.chat(child.Key, "mock"); c != http.StatusOK {
		t.Fatalf("second: %d", c)
	}
	if c := k.chat(child.Key, "mock"); c != http.StatusTooManyRequests {
		t.Errorf("third: %d, want 429 (2/day, spend kept across the edit)", c)
	}
	if rec := k.do(http.MethodDelete, "/v1/keys/"+child.Key, "parent", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	k.mount()
	if rec := k.do(http.MethodGet, "/v1/keys", "parent", ""); strings.Contains(rec.Body.String(), child.Key) {
		t.Errorf("deleted key still listed: %s", rec.Body.String())
	}
	// corrallm serves strangers by default; a revoked key must not become one.
	if c := k.chat(child.Key, "mock"); c != http.StatusUnauthorized {
		t.Errorf("deleted key: %d, want 401", c)
	}
}
