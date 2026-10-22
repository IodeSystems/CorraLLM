package auth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeIdP is a minimal OpenID provider: discovery, JWKS and a token endpoint
// that checks the PKCE verifier and returns an RS256 ID token built by claims.
type fakeIdP struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	key       *rsa.PrivateKey
	kid       string
	forge     *rsa.PrivateKey // signs instead of key when set; never published
	challenge string          // code_challenge from the last /auth/login
	nonce     string
	claims    func(issuer, nonce string) map[string]any
	userinfo  map[string]any // served at /userinfo; nil = 404
	tokenForm url.Values     // what the last /token request carried
	tokenAuth string         // its Authorization header
}

const (
	clientID   = "corrallm-test"
	goodCode   = "good-code"
	adminToken = "admin-tok"
)

func newIdP(t *testing.T) *fakeIdP {
	p := &fakeIdP{t: t}
	p.rotate("k1")
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                p.srv.URL,
			"authorization_endpoint":                p.srv.URL + "/authorize",
			"token_endpoint":                        p.srv.URL + "/token",
			"jwks_uri":                              p.srv.URL + "/jwks",
			"userinfo_endpoint":                     p.srv.URL + "/userinfo",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		pub := p.key.PublicKey
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": p.kid,
			"n": b64(pub.N.Bytes()), "e": b64(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		ui := p.userinfo
		p.mu.Unlock()
		if ui == nil || r.Header.Get("Authorization") != "Bearer at" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ui)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		p.mu.Lock()
		challenge, nonce := p.challenge, p.nonce
		p.tokenForm, p.tokenAuth = r.PostForm, r.Header.Get("Authorization")
		p.mu.Unlock()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if r.Form.Get("code") != goodCode || b64(sum[:]) != challenge {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "token_type": "Bearer", "expires_in": 3600,
			"id_token": p.mint(p.claims(p.srv.URL, nonce)),
		})
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	p.claims = func(iss, nonce string) map[string]any { return goodClaims(iss, nonce) }
	return p
}

func goodClaims(iss, nonce string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss": iss, "aud": clientID, "sub": "u1", "nonce": nonce,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		"email": "ops@example.com", "email_verified": true,
	}
}

func (p *fakeIdP) rotate(kid string) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		p.t.Fatal(err)
	}
	p.mu.Lock()
	p.key, p.kid = k, kid
	p.mu.Unlock()
}

func (p *fakeIdP) mint(claims map[string]any) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": p.kid})
	c, _ := json.Marshal(claims)
	in := b64(h) + "." + b64(c)
	sum := sha256.Sum256([]byte(in))
	key := p.key
	if p.forge != nil {
		key = p.forge
	}
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		p.t.Fatal(err)
	}
	return in + "." + b64(sig)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

type harness struct {
	idp      *fakeIdP
	settings *OIDCSettings
	sso      *SSO
	api      http.Handler
}

func newHarness(t *testing.T) *harness {
	h := &harness{idp: newIdP(t)}
	h.settings = &OIDCSettings{
		Issuer: h.idp.srv.URL, ClientID: clientID, ClientSecret: "shh",
		RedirectURL: "http://corrallm.test/auth/callback",
		Scopes:      []string{"openid", "email"},
		Users:       map[string]Role{"Ops@Example.com": RoleAdmin},
	}
	h.sso = &SSO{Key: []byte("0123456789abcdef0123456789abcdef"), Settings: func() *OIDCSettings { return h.settings }}
	h.api = Middleware(adminToken, h.sso)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	return h
}

// login runs /auth/login and returns the state cookie and the state the
// provider would echo back.
func (h *harness) login(t *testing.T) (*http.Cookie, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.sso.Login(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("login status = %d: %s", rec.Code, rec.Body)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := loc.Query()
	if loc.Path != "/authorize" || q.Get("client_id") != clientID || q.Get("redirect_uri") != h.settings.RedirectURL ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("nonce") == "" || q.Get("state") == "" {
		t.Fatalf("authorize URL missing parameters: %s", loc)
	}
	h.idp.mu.Lock()
	h.idp.challenge, h.idp.nonce = q.Get("code_challenge"), q.Get("nonce")
	h.idp.mu.Unlock()
	c := findCookie(rec, stateCookie)
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("state cookie = %+v", c)
	}
	return c, q.Get("state")
}

func (h *harness) callback(state string, c *http.Cookie, extra string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/auth/callback?code="+goodCode+"&state="+url.QueryEscape(state)+extra, nil)
	if c != nil {
		r.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.sso.Callback(rec, r)
	return rec
}

// signIn runs the whole flow and returns the callback response.
func (h *harness) signIn(t *testing.T) *httptest.ResponseRecorder {
	c, state := h.login(t)
	return h.callback(state, c, "")
}

func (h *harness) apiCode(set func(*http.Request)) int {
	r := httptest.NewRequest(http.MethodPost, "/api/graphql", nil)
	if set != nil {
		set(r)
	}
	rec := httptest.NewRecorder()
	h.api.ServeHTTP(rec, r)
	return rec.Code
}

func findCookie(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func sessionFrom(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	c := findCookie(rec, SessionCookie)
	if c == nil || c.Value == "" {
		t.Fatalf("no session cookie (status %d): %s", rec.Code, rec.Body)
	}
	return c
}

func noSession(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Errorf("status = %d, want %d: %s", rec.Code, wantStatus, rec.Body)
	}
	if c := findCookie(rec, SessionCookie); c != nil && c.Value != "" {
		t.Errorf("session cookie issued on a rejected sign-in")
	}
}

func TestSSOHappyPath(t *testing.T) {
	h := newHarness(t)
	rec := h.signIn(t)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("callback = %d → %q: %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	sess := sessionFrom(t, rec)
	if !sess.HttpOnly || sess.SameSite != http.SameSiteLaxMode || sess.Path != "/" || sess.MaxAge != int(sessionTTL.Seconds()) {
		t.Errorf("session cookie attributes = %+v", sess)
	}
	if sess.Secure {
		t.Errorf("Secure set on a plain-http deployment; the browser would drop it")
	}
	if code := h.apiCode(func(r *http.Request) { r.AddCookie(sess) }); code != http.StatusOK {
		t.Errorf("/api with session = %d, want 200", code)
	}
	if code := h.apiCode(nil); code != http.StatusUnauthorized {
		t.Errorf("/api without anything = %d, want 401", code)
	}

	// whoami reports it.
	r := httptest.NewRequest(http.MethodGet, "/auth/whoami", nil)
	r.AddCookie(sess)
	w := httptest.NewRecorder()
	h.sso.WhoAmI(w, r)
	var who struct {
		OIDC  bool   `json:"oidc"`
		Email string `json:"email"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &who)
	if !who.OIDC || who.Email != "ops@example.com" {
		t.Errorf("whoami = %s", w.Body)
	}

	// Logout clears it.
	w = httptest.NewRecorder()
	h.sso.Logout(w, httptest.NewRequest(http.MethodPost, "/auth/logout", nil))
	if c := findCookie(w, SessionCookie); c == nil || c.MaxAge >= 0 {
		t.Errorf("logout did not clear the session cookie: %+v", c)
	}
}

func TestSSOSecureCookieBehindTLS(t *testing.T) {
	h := newHarness(t)
	h.settings.RedirectURL = "https://corrallm.example.com/auth/callback"
	if c := sessionFrom(t, h.signIn(t)); !c.Secure {
		t.Errorf("session cookie not Secure with an https redirect URL")
	}
}

func TestSSOWrongStateRejected(t *testing.T) {
	h := newHarness(t)
	c, _ := h.login(t)
	noSession(t, h.callback("forged-state", c, ""), http.StatusBadRequest)
}

func TestSSOMissingStateCookieRejected(t *testing.T) {
	h := newHarness(t)
	_, state := h.login(t)
	noSession(t, h.callback(state, nil, ""), http.StatusBadRequest)
}

func TestSSOTamperedStateCookieRejected(t *testing.T) {
	h := newHarness(t)
	c, state := h.login(t)
	c.Value = sign([]byte("another key"), "oidc-state", statePayload{State: state, Exp: time.Now().Add(time.Hour).Unix()})
	noSession(t, h.callback(state, c, ""), http.StatusBadRequest)
}

func TestSSOWrongNonceRejected(t *testing.T) {
	h := newHarness(t)
	h.idp.claims = func(iss, _ string) map[string]any { return goodClaims(iss, "someone-elses-nonce") }
	noSession(t, h.signIn(t), http.StatusUnauthorized)
}

func TestSSOWrongAudienceRejected(t *testing.T) {
	h := newHarness(t)
	h.idp.claims = func(iss, nonce string) map[string]any {
		c := goodClaims(iss, nonce)
		c["aud"] = "some-other-client"
		return c
	}
	noSession(t, h.signIn(t), http.StatusUnauthorized)
}

func TestSSOWrongIssuerRejected(t *testing.T) {
	h := newHarness(t)
	h.idp.claims = func(_, nonce string) map[string]any { return goodClaims("https://evil.example.com", nonce) }
	noSession(t, h.signIn(t), http.StatusUnauthorized)
}

func TestSSOExpiredTokenRejected(t *testing.T) {
	h := newHarness(t)
	h.idp.claims = func(iss, nonce string) map[string]any {
		c := goodClaims(iss, nonce)
		c["iat"] = time.Now().Add(-2 * time.Hour).Unix()
		c["exp"] = time.Now().Add(-10 * time.Minute).Unix()
		return c
	}
	noSession(t, h.signIn(t), http.StatusUnauthorized)
}

func TestSSOFutureIssuedAtRejected(t *testing.T) {
	h := newHarness(t)
	h.idp.claims = func(iss, nonce string) map[string]any {
		c := goodClaims(iss, nonce)
		c["iat"] = time.Now().Add(time.Hour).Unix()
		return c
	}
	noSession(t, h.signIn(t), http.StatusUnauthorized)
}

// A token signed by a key the JWKS does not publish is a forgery, even under
// the published kid.
func TestSSOBadSignatureRejected(t *testing.T) {
	h := newHarness(t)
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	h.idp.forge = k
	noSession(t, h.signIn(t), http.StatusUnauthorized)
}

// A provider that rotates keys publishes a new kid; the cached key set must be
// refetched rather than every sign-in failing until a restart.
func TestSSOKeyRotationRefetchesJWKS(t *testing.T) {
	h := newHarness(t)
	sessionFrom(t, h.signIn(t))
	h.idp.rotate("k2")
	sessionFrom(t, h.signIn(t))
}

func TestSSOEmailNotAllowed(t *testing.T) {
	h := newHarness(t)
	h.idp.claims = func(iss, nonce string) map[string]any {
		c := goodClaims(iss, nonce)
		c["email"] = "stranger@example.com"
		return c
	}
	rec := h.signIn(t)
	noSession(t, rec, http.StatusForbidden)
	if !strings.Contains(rec.Body.String(), "stranger@example.com is not allowed") {
		t.Errorf("refusal page does not name the email: %s", rec.Body)
	}
}

func TestSSOUnverifiedEmailRefused(t *testing.T) {
	for name, v := range map[string]any{"false": false, "string false": "false", "missing": nil} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.idp.claims = func(iss, nonce string) map[string]any {
				c := goodClaims(iss, nonce)
				if v == nil {
					delete(c, "email_verified")
				} else {
					c["email_verified"] = v
				}
				return c
			}
			noSession(t, h.signIn(t), http.StatusForbidden)
		})
	}
	// Opted out: admitted.
	h := newHarness(t)
	h.settings.AllowUnverifiedEmail = true
	h.idp.claims = func(iss, nonce string) map[string]any {
		c := goodClaims(iss, nonce)
		delete(c, "email_verified")
		return c
	}
	sessionFrom(t, h.signIn(t))
}

// Zitadel's default: no email in the ID token, only at userinfo. Admitted from
// userinfo for the same subject; a userinfo answer about someone else is not.
func TestSSOEmailFromUserinfo(t *testing.T) {
	noEmail := func(iss, nonce string) map[string]any {
		c := goodClaims(iss, nonce)
		delete(c, "email")
		delete(c, "email_verified")
		return c
	}
	h := newHarness(t)
	h.idp.claims = noEmail
	h.idp.userinfo = map[string]any{"sub": "u1", "email": "ops@example.com", "email_verified": true}
	sessionFrom(t, h.signIn(t))

	h = newHarness(t)
	h.idp.claims = noEmail
	h.idp.userinfo = map[string]any{"sub": "someone-else", "email": "ops@example.com", "email_verified": true}
	if rec := h.signIn(t); findCookie(rec, SessionCookie) != nil && findCookie(rec, SessionCookie).Value != "" {
		t.Error("session issued from userinfo about a different subject")
	}

	h = newHarness(t)
	h.idp.claims = noEmail
	h.idp.userinfo = map[string]any{"sub": "u1", "email": "ops@example.com", "email_verified": false}
	noSession(t, h.signIn(t), http.StatusForbidden)
}

func TestSSOProviderErrorPage(t *testing.T) {
	h := newHarness(t)
	c, state := h.login(t)
	rec := h.callback(state, c, "&error=access_denied&error_description="+url.QueryEscape("<script>x</script>"))
	noSession(t, rec, http.StatusUnauthorized)
	body := rec.Body.String()
	if !strings.Contains(body, "access_denied") || strings.Contains(body, "<script>") || !strings.Contains(body, `href="/"`) {
		t.Errorf("error page = %s", body)
	}
}

func TestSSOTamperedSessionRejected(t *testing.T) {
	h := newHarness(t)
	sess := sessionFrom(t, h.signIn(t))
	body, sig, _ := strings.Cut(sess.Value, ".")
	forged := sessionPayload{Email: "ops@example.com", Exp: time.Now().Add(1000 * time.Hour).Unix()}
	fb, _ := json.Marshal(forged)
	for name, v := range map[string]string{
		"payload swapped":  b64(fb) + "." + sig,
		"sig flipped":      body + "." + strings.Repeat("A", len(sig)),
		"no sig":           body,
		"other key":        sign([]byte("not the server key"), "session", forged),
		"state as session": sign(h.sso.Key, "oidc-state", forged),
	} {
		if code := h.apiCode(func(r *http.Request) { r.AddCookie(&http.Cookie{Name: SessionCookie, Value: v}) }); code != http.StatusUnauthorized {
			t.Errorf("%s: /api = %d, want 401", name, code)
		}
	}
}

func TestSSOSessionExpires(t *testing.T) {
	h := newHarness(t)
	sess := sessionFrom(t, h.signIn(t))
	h.sso.Now = func() time.Time { return time.Now().Add(sessionTTL + time.Minute) }
	if code := h.apiCode(func(r *http.Request) { r.AddCookie(sess) }); code != http.StatusUnauthorized {
		t.Errorf("expired session: /api = %d, want 401", code)
	}
}

// Removing an operator from the allowlist (or SSO altogether) ends their
// session on the next request, not 12 hours later.
func TestSSOSessionFollowsConfig(t *testing.T) {
	h := newHarness(t)
	sess := sessionFrom(t, h.signIn(t))
	h.settings.Users = map[string]Role{"someone-else@example.com": RoleAdmin}
	if code := h.apiCode(func(r *http.Request) { r.AddCookie(sess) }); code != http.StatusUnauthorized {
		t.Errorf("de-listed email: /api = %d, want 401", code)
	}
	h.settings = nil
	if code := h.apiCode(func(r *http.Request) { r.AddCookie(sess) }); code != http.StatusUnauthorized {
		t.Errorf("SSO removed: /api = %d, want 401", code)
	}
}

func TestSSOCrossSiteSessionRefused(t *testing.T) {
	h := newHarness(t)
	sess := sessionFrom(t, h.signIn(t))
	for site, want := range map[string]int{"same-origin": 200, "none": 200, "cross-site": 401, "same-site": 401} {
		code := h.apiCode(func(r *http.Request) {
			r.AddCookie(sess)
			r.Header.Set("Sec-Fetch-Site", site)
		})
		if code != want {
			t.Errorf("Sec-Fetch-Site %s: /api = %d, want %d", site, code, want)
		}
	}
}

func TestSSOAdminTokenStillWorks(t *testing.T) {
	h := newHarness(t)
	if code := h.apiCode(func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+adminToken) }); code != http.StatusOK {
		t.Errorf("bearer = %d, want 200", code)
	}
	if code := h.apiCode(func(r *http.Request) { r.AddCookie(&http.Cookie{Name: CookieName, Value: adminToken}) }); code != http.StatusOK {
		t.Errorf("admin cookie = %d, want 200", code)
	}
}

// /v1 is the inference API: its callers authenticate by their own key inside
// the proxy, and the dashboard gate must neither block nor admit them.
func TestSSOLeavesV1Alone(t *testing.T) {
	h := newHarness(t)
	var reached int
	gate := Middleware(adminToken, h.sso)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached++
		w.WriteHeader(http.StatusOK)
	}))
	for _, p := range []string{"/v1/chat/completions", "/v1/models", "/auth/login", "/auth/callback"} {
		rec := httptest.NewRecorder()
		gate.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, p, nil))
		if rec.Code != http.StatusOK || len(rec.Result().Cookies()) != 0 {
			t.Errorf("%s: status %d, cookies %v — the gate touched it", p, rec.Code, rec.Result().Cookies())
		}
	}
	if reached != 4 {
		t.Errorf("reached = %d, want 4", reached)
	}
}

func TestSSONotConfigured(t *testing.T) {
	sso := &SSO{Key: []byte("k"), Settings: func() *OIDCSettings { return nil }}
	rec := httptest.NewRecorder()
	sso.Login(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("login without config = %d, want 404", rec.Code)
	}
	rec = httptest.NewRecorder()
	sso.WhoAmI(rec, httptest.NewRequest(http.MethodGet, "/auth/whoami", nil))
	if strings.TrimSpace(rec.Body.String()) != `{"oidc":false,"adminToken":"enabled"}` {
		t.Errorf("whoami = %s", rec.Body)
	}
}

// With no override, the callback is the address the browser used — behind a
// TLS-terminating proxy, the forwarded scheme and host (P56 follow-up: the
// --public-base default sent a LAN http callback the provider had never seen).
func TestSSORedirectFromRequest(t *testing.T) {
	h := newHarness(t)
	h.settings.RedirectURL = ""
	login := func(r *http.Request) string {
		rec := httptest.NewRecorder()
		h.sso.Login(rec, r)
		if rec.Code != http.StatusFound {
			t.Fatalf("login = %d: %s", rec.Code, rec.Body)
		}
		loc, _ := url.Parse(rec.Header().Get("Location"))
		return loc.Query().Get("redirect_uri")
	}
	if got := login(httptest.NewRequest(http.MethodGet, "http://box1:8111/auth/login", nil)); got != "http://box1:8111/auth/callback" {
		t.Errorf("direct = %q", got)
	}
	r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8111/auth/login", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "llm.example.com")
	if got := login(r); got != "https://llm.example.com/auth/callback" {
		t.Errorf("proxied = %q", got)
	}
}

// A trailing slash on the configured issuer is not a different issuer.
func TestSSOIssuerTrailingSlash(t *testing.T) {
	h := newHarness(t)
	h.settings.Issuer += "/"
	rec := httptest.NewRecorder()
	h.sso.Login(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if rec.Code != http.StatusFound {
		t.Errorf("login with issuer + slash = %d: %s", rec.Code, rec.Body)
	}
}
