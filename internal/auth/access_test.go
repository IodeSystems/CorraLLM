package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// viewerHarness signs ops@example.com in as a viewer and returns the cookie.
func viewerHarness(t *testing.T) (*harness, *http.Cookie) {
	t.Helper()
	h := newHarness(t)
	h.settings.Users = map[string]Role{"ops@example.com": RoleViewer}
	return h, sessionFrom(t, h.signIn(t))
}

func (h *harness) do(method, path, body string, set func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if set != nil {
		set(r)
	}
	rec := httptest.NewRecorder()
	h.api.ServeHTTP(rec, r)
	return rec
}

func gqlBody(query, opName string) string {
	b, _ := json.Marshal(map[string]any{"query": query, "operationName": opName})
	return string(b)
}

// The role is resolved from the current settings on each request, and rides
// in the context for handlers.
func TestRoleResolution(t *testing.T) {
	h := newHarness(t)
	var got Identity
	h.api = Middleware(adminToken, h.sso)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = IdentityFrom(r.Context())
	}))
	sess := sessionFrom(t, h.signIn(t))
	withSess := func(r *http.Request) { r.AddCookie(sess) }

	h.do("GET", "/api/v1/overview", "", withSess)
	if got != (Identity{Email: "ops@example.com", Role: RoleAdmin}) {
		t.Errorf("admin session identity = %+v", got)
	}
	h.settings.Users = map[string]Role{"ops@example.com": RoleViewer}
	h.do("GET", "/api/v1/overview", "", withSess)
	if got.Role != RoleViewer {
		t.Errorf("demoted session identity = %+v", got)
	}
	got = Identity{}
	h.do("GET", "/api/v1/overview", "", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+adminToken) })
	if got != (Identity{Role: RoleAdmin}) {
		t.Errorf("admin token identity = %+v", got)
	}
}

func TestViewerReads(t *testing.T) {
	h, sess := viewerHarness(t)
	withSess := func(r *http.Request) { r.AddCookie(sess) }
	for _, tc := range []struct{ name, method, path, body string }{
		{"rest get", "GET", "/api/v1/overview", ""},
		{"sse", "GET", "/api/v1/events", ""},
		{"head", "HEAD", "/api/v1/overview", ""},
		{"query", "POST", "/api/graphql", gqlBody(`query Q { corrallm { overview { x } } }`, "")},
		{"shorthand", "POST", "/api/graphql", gqlBody(`{ corrallm { health { version } } }`, "")},
		{"two queries", "POST", "/api/graphql", gqlBody(`query A { a } query B { b }`, "B")},
		{"fragment", "POST", "/api/graphql", gqlBody(`query A { ...F } fragment F on Query { a }`, "")},
		{"word mutation in a comment", "POST", "/api/graphql", gqlBody("# mutation X { unloadModel }\nquery A { a }", "")},
		{"word mutation in a string", "POST", "/api/graphql", gqlBody(`query A { a(x: "mutation { b }") }`, "")},
		{"introspection via schema path", "POST", "/api/schema/graphql", gqlBody(`{ __schema { types { name } } }`, "")},
	} {
		if rec := h.do(tc.method, tc.path, tc.body, withSess); rec.Code != http.StatusOK {
			t.Errorf("%s: %d, want 200: %s", tc.name, rec.Code, rec.Body)
		}
	}
}

// The handler still reads the body the check buffered.
func TestViewerQueryBodyReachesHandler(t *testing.T) {
	h, sess := viewerHarness(t)
	var seen string
	h.api = Middleware(adminToken, h.sso)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		seen = req.Query
	}))
	h.do("POST", "/api/graphql", gqlBody(`{ a }`, ""), func(r *http.Request) { r.AddCookie(sess) })
	if seen != "{ a }" {
		t.Errorf("handler read %q", seen)
	}
}

func TestViewerWritesDenied(t *testing.T) {
	h, sess := viewerHarness(t)
	withSess := func(r *http.Request) { r.AddCookie(sess) }
	big := `query A { a } ` + strings.Repeat("# pad\n", maxGraphQLBody/6+1)
	for _, tc := range []struct{ name, method, path, body string }{
		{"rest post", "POST", "/api/v1/models/x/unload", ""},
		{"rest put", "PUT", "/api/v1/config/models/x", "{}"},
		{"rest delete", "DELETE", "/api/v1/config/models/x", ""},
		{"rest patch", "PATCH", "/api/v1/anything", ""},
		{"options", "OPTIONS", "/api/v1/overview", ""},
		{"mutation", "POST", "/api/graphql", gqlBody(`mutation { corrallm { unloadModel(name: "x") { ok } } }`, "")},
		{"mutation after a query", "POST", "/api/graphql", gqlBody(`query A { a } mutation B { b }`, "A")},
		{"operationName at a mutation", "POST", "/api/graphql", gqlBody(`query A { a } mutation B { b }`, "B")},
		{"mutation before a query", "POST", "/api/graphql", gqlBody(`mutation B { b } query A { a }`, "A")},
		{"subscription", "POST", "/api/graphql", gqlBody(`subscription S { a }`, "")},
		{"query commented out", "POST", "/api/graphql", gqlBody("# query A { a }\nmutation B { b }", "")},
		{"mutation in a fragment-only doc", "POST", "/api/graphql", gqlBody(`fragment F on Mutation { b }`, "")},
		{"schema definition", "POST", "/api/graphql", gqlBody(`schema { query: Q } query A { a }`, "")},
		{"parse error", "POST", "/api/graphql", gqlBody(`query A { a `, "")},
		{"empty", "POST", "/api/graphql", gqlBody(``, "")},
		{"not json", "POST", "/api/graphql", `mutation { b }`},
		{"batched array", "POST", "/api/graphql", `[` + gqlBody(`mutation { b }`, "") + `]`},
		{"later key wins", "POST", "/api/graphql", `{"query":"{ a }","query":"mutation { b }"}`},
		{"case-folded key", "POST", "/api/graphql", `{"QUERY":"mutation { b }"}`},
		{"too big", "POST", "/api/graphql", gqlBody(big, "")},
		{"schema path mutation", "POST", "/api/schema/graphql", gqlBody(`mutation { b }`, "")},
		{"get with mutation query", "GET", "/api/graphql?query=" + "mutation%20%7B%20b%20%7D", ""},
		{"put to graphql", "PUT", "/api/graphql", gqlBody(`{ a }`, "")},
	} {
		rec := h.do(tc.method, tc.path, tc.body, withSess)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"read-only access"`) {
			t.Errorf("%s: %d %s, want 403 read-only", tc.name, rec.Code, rec.Body)
		}
	}
}

func TestAdminTokenUnaffectedByViewerRules(t *testing.T) {
	h, _ := viewerHarness(t)
	bearer := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+adminToken) }
	if rec := h.do("POST", "/api/graphql", gqlBody(`mutation { b }`, ""), bearer); rec.Code != http.StatusOK {
		t.Errorf("admin mutation = %d", rec.Code)
	}
	if rec := h.do("DELETE", "/api/v1/config/models/x", "", bearer); rec.Code != http.StatusOK {
		t.Errorf("admin delete = %d", rec.Code)
	}
}

func TestRevocationInvalidatesOlderSessions(t *testing.T) {
	h := newHarness(t)
	cut := map[string]int64{}
	var recorded []string
	h.sso.NotBefore = func(_ context.Context, email string) (int64, error) { return cut[email], nil }
	h.sso.RecordSignIn = func(_ context.Context, email, ip string, _ int64) error {
		recorded = append(recorded, email+"@"+ip)
		return nil
	}
	old := sessionFrom(t, h.signIn(t))
	if len(recorded) != 1 || !strings.HasPrefix(recorded[0], "ops@example.com@") {
		t.Errorf("sign-in not recorded: %v", recorded)
	}
	withOld := func(r *http.Request) { r.AddCookie(old) }
	if rec := h.do("GET", "/api/v1/x", "", withOld); rec.Code != http.StatusOK {
		t.Fatalf("before revoke = %d", rec.Code)
	}
	cut["ops@example.com"] = time.Now().UnixMilli()
	if rec := h.do("GET", "/api/v1/x", "", withOld); rec.Code != http.StatusUnauthorized {
		t.Errorf("revoked session = %d, want 401", rec.Code)
	}
	// A sign-in after the cutoff works.
	h.sso.Now = func() time.Time { return time.Now().Add(time.Second) }
	fresh := sessionFrom(t, h.signIn(t))
	if rec := h.do("GET", "/api/v1/x", "", func(r *http.Request) { r.AddCookie(fresh) }); rec.Code != http.StatusOK {
		t.Errorf("fresh session after revoke = %d", rec.Code)
	}
	// A session from before the field existed carries no issue time, so any
	// cutoff refuses it.
	legacy := sign(h.sso.Key, "session", map[string]any{"e": "ops@example.com", "x": time.Now().Add(time.Hour).Unix()})
	if rec := h.do("GET", "/api/v1/x", "", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: SessionCookie, Value: legacy}) }); rec.Code != http.StatusUnauthorized {
		t.Errorf("pre-revocation-format session = %d", rec.Code)
	}
	// A failing lookup refuses rather than admits.
	h.sso.NotBefore = func(context.Context, string) (int64, error) { return 0, context.DeadlineExceeded }
	if rec := h.do("GET", "/api/v1/x", "", func(r *http.Request) { r.AddCookie(fresh) }); rec.Code != http.StatusUnauthorized {
		t.Errorf("lookup failure = %d, want 401", rec.Code)
	}
}

func TestWhoAmIRole(t *testing.T) {
	h, sess := viewerHarness(t)
	h.sso.AdminToken = adminToken
	who := func(set func(*http.Request)) map[string]any {
		r := httptest.NewRequest(http.MethodGet, "/auth/whoami", nil)
		set(r)
		w := httptest.NewRecorder()
		h.sso.WhoAmI(w, r)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	if got := who(func(r *http.Request) { r.AddCookie(sess) }); got["role"] != "viewer" || got["email"] != "ops@example.com" {
		t.Errorf("viewer whoami = %v", got)
	}
	if got := who(func(r *http.Request) { r.AddCookie(&http.Cookie{Name: CookieName, Value: adminToken}) }); got["role"] != "admin" || got["email"] != nil {
		t.Errorf("token whoami = %v", got)
	}
	if got := who(func(*http.Request) {}); got["role"] != nil {
		t.Errorf("anonymous whoami = %v", got)
	}
}

// A public client: no secret, so client_id and the PKCE verifier go in the
// form and nothing authenticates the client otherwise.
func TestPKCEOnlyExchange(t *testing.T) {
	h := newHarness(t)
	h.settings.ClientSecret = ""
	sessionFrom(t, h.signIn(t))
	h.idp.mu.Lock()
	form, authz := h.idp.tokenForm, h.idp.tokenAuth
	h.idp.mu.Unlock()
	if form.Get("client_id") != clientID || form.Get("code_verifier") == "" {
		t.Errorf("token form = %v", form)
	}
	if authz != "" || form.Has("client_secret") {
		t.Errorf("a secret was sent: auth %q form %v", authz, form)
	}
}

func TestConfidentialClientSendsSecret(t *testing.T) {
	h := newHarness(t)
	sessionFrom(t, h.signIn(t))
	h.idp.mu.Lock()
	form, authz := h.idp.tokenForm, h.idp.tokenAuth
	h.idp.mu.Unlock()
	sent := form.Get("client_secret") == "shh"
	if strings.HasPrefix(authz, "Basic ") {
		raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(authz, "Basic "))
		sent = sent || string(raw) == clientID+":shh"
	}
	if !sent || form.Get("code_verifier") == "" {
		t.Errorf("secret not sent: auth %q form %v", authz, form)
	}
}

func TestQueriesOnly(t *testing.T) {
	for doc, want := range map[string]bool{
		`{ a }`:                        true,
		`query { a }`:                  true,
		`mutation { a }`:               false,
		"query A { a }\n# mutation":    true,
		`query A { a } mutation B {b}`: false,
		``:                             false,
		`fragment F on Q { a }`:        false,
		`extend type Query { x: Int }`: false,
	} {
		if got := queriesOnly(doc); got != want {
			t.Errorf("queriesOnly(%q) = %v, want %v", doc, got, want)
		}
	}
}

// A disabled admin token is refused by Bearer and by cookie; an SSO admin
// session keeps working, and the agent routes do not care.
func TestAdminTokenDisabled(t *testing.T) {
	h := newHarness(t)
	h.sso.AdminToken = adminToken
	on := false
	h.sso.TokenEnabled = func() bool { return on }
	bearer := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+adminToken) }
	cookie := func(r *http.Request) { r.AddCookie(&http.Cookie{Name: CookieName, Value: adminToken}) }
	if rec := h.do("GET", "/api/v1/x", "", bearer); rec.Code != http.StatusUnauthorized {
		t.Errorf("bearer while disabled = %d", rec.Code)
	}
	if rec := h.do("GET", "/api/v1/x", "", cookie); rec.Code != http.StatusUnauthorized {
		t.Errorf("cookie while disabled = %d", rec.Code)
	}
	if rec := h.do("POST", "/api/v1/agents/heartbeat", "", nil); rec.Code != http.StatusOK {
		t.Errorf("heartbeat while disabled = %d", rec.Code)
	}
	sess := sessionFrom(t, h.signIn(t))
	if rec := h.do("POST", "/api/graphql", gqlBody(`mutation { b }`, ""), func(r *http.Request) { r.AddCookie(sess) }); rec.Code != http.StatusOK {
		t.Errorf("sso admin while token disabled = %d", rec.Code)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/auth/whoami", nil)
	cookie(r)
	h.sso.WhoAmI(w, r)
	var who map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &who)
	if who["adminToken"] != "disabled" || who["role"] != nil {
		t.Errorf("whoami with disabled token = %v", who)
	}
	on = true
	if rec := h.do("GET", "/api/v1/x", "", bearer); rec.Code != http.StatusOK {
		t.Errorf("bearer re-enabled = %d", rec.Code)
	}
}
