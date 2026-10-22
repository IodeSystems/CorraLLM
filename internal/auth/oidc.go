package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDC sign-in for the dashboard.
//
// The admin token stays the root credential; this adds a session cookie that a
// person gets by signing in at an OpenID Connect provider with a verified email
// listed in auth.users. The session is an HMAC-signed {email, issued, expiry}
// under a server key. The role is not in it: it is looked up in the current
// config on every request, so a demotion or removal applies at once. Revoking
// one user's sessions sets a per-user cutoff (NotBefore) that every session
// issued before it fails; rotating the key (delete <home>/session.key,
// restart) revokes every session.

const (
	// SessionCookie carries the signed session.
	SessionCookie = "corrallm_session"
	// stateCookie carries state, nonce and the PKCE verifier between
	// /auth/login and /auth/callback.
	stateCookie = "corrallm_oidc"

	sessionTTL = 12 * time.Hour
	stateTTL   = 10 * time.Minute
	// clockSkew is how far a provider's clock may run ahead of ours.
	clockSkew = time.Minute
)

// OIDCSettings is the resolved provider config, secret expanded.
type OIDCSettings struct {
	Issuer               string
	ClientID             string
	ClientSecret         string
	// RedirectURL is the configured override; "" means the callback on the
	// address the browser used (RedirectFor).
	RedirectURL          string
	Scopes               []string
	AllowUnverifiedEmail bool
	// Users maps each admitted email to its role.
	Users map[string]Role
}

// roleOf returns email's role; ok is false when the email is not a user.
func (s *OIDCSettings) roleOf(email string) (Role, bool) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return "", false
	}
	for e, r := range s.Users {
		if strings.ToLower(strings.TrimSpace(e)) == email {
			return r, true
		}
	}
	return "", false
}

// SSO serves /auth/* and checks session cookies.
type SSO struct {
	// Settings returns the current OIDC config, or nil when none is set. Read
	// per request so a config reload applies without a restart, including
	// removing an email from the allowlist.
	Settings func() *OIDCSettings
	// Key signs session and state cookies.
	Key []byte
	// Client is used for discovery, JWKS and the token exchange. Nil means a
	// client with a 15s timeout, so a hung provider cannot hang the callback.
	Client *http.Client
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// AdminToken lets WhoAmI report the token's holder as an admin.
	AdminToken string
	// TokenEnabled reports whether /api accepts the admin token. Nil means
	// always. Read per request, like Settings.
	TokenEnabled func() bool
	// NotBefore returns a user's revocation cutoff in unix ms (0 = none). A
	// session issued at or before it is refused, and so is every session when
	// it fails. Nil means no revocation.
	NotBefore func(ctx context.Context, email string) (int64, error)
	// RecordSignIn notes a successful sign-in. Nil skips it.
	RecordSignIn func(ctx context.Context, email, ip string, atMS int64) error

	mu       sync.Mutex
	issuer   string
	provider *oidc.Provider
}

func (s *SSO) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *SSO) tokenOn() bool {
	return s == nil || s.TokenEnabled == nil || s.TokenEnabled()
}

func (s *SSO) settings() *OIDCSettings {
	if s == nil || s.Settings == nil {
		return nil
	}
	return s.Settings()
}

// ctx attaches the configured HTTP client the way go-oidc and oauth2 expect.
func (s *SSO) ctx(parent context.Context) context.Context {
	c := s.Client
	if c == nil {
		c = defaultClient
	}
	return oidc.ClientContext(parent, c)
}

var defaultClient = &http.Client{Timeout: 15 * time.Second}

// discover returns the provider for issuer, running discovery once per issuer.
// A failure is not cached, so a provider that was down is retried next time.
//
// Background, not the request context: the provider keeps this context for
// its JWKS fetches long after the request ends.
//
// A TRAILING SLASH IS NOT A DIFFERENT ISSUER to a person typing one in, but
// go-oidc requires the configured issuer to equal the provider's byte for
// byte. Zitadel names itself without the slash, Auth0 with one, and the error
// read "provider unreachable" on a provider that answered in 19 ms. So when the
// form typed fails, the other form is tried before giving up.
func (s *SSO) discover(issuer string) (*oidc.Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.provider != nil && s.issuer == issuer {
		return s.provider, nil
	}
	p, err := oidc.NewProvider(s.ctx(context.Background()), issuer)
	if err != nil {
		alt := strings.TrimSuffix(issuer, "/")
		if alt == issuer {
			alt += "/"
		}
		p2, err2 := oidc.NewProvider(s.ctx(context.Background()), alt)
		if err2 != nil {
			return nil, err // the error for what was configured
		}
		p = p2
	}
	s.provider, s.issuer = p, issuer
	return p, nil
}

// RequestOrigin is scheme://host as the browser addressed this server: the
// front proxy's X-Forwarded-Proto and X-Forwarded-Host when it sends them, else
// the connection's own. A client can set those headers; that is acceptable
// where the result is only a callback the provider must already have
// registered — a forged one fails at the provider, not here.
func RequestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := r.Host
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
		host = strings.TrimSpace(strings.Split(fh, ",")[0])
	}
	return scheme + "://" + host
}

// RedirectFor is the callback for a sign-in started by r: the configured
// override, else /auth/callback on the address the browser used. The callback
// request lands on that same address, so both legs of the flow agree.
func (st *OIDCSettings) RedirectFor(r *http.Request) string {
	if st.RedirectURL != "" {
		return st.RedirectURL
	}
	return RequestOrigin(r) + "/auth/callback"
}

func oauthConfig(st *OIDCSettings, p *oidc.Provider, redirect string) *oauth2.Config {
	ep := p.Endpoint()
	if st.ClientSecret == "" {
		// A public client: PKCE is the proof, and client_id rides in the body.
		// Auto-detect would send Basic "id:" first, which a PKCE-only client at
		// Zitadel refuses.
		ep.AuthStyle = oauth2.AuthStyleInParams
	}
	return &oauth2.Config{
		ClientID:     st.ClientID,
		ClientSecret: st.ClientSecret,
		RedirectURL:  redirect,
		Endpoint:     ep,
		Scopes:       st.Scopes,
	}
}

type statePayload struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Exp      int64  `json:"x"`
}

type sessionPayload struct {
	Email string `json:"e"`
	Exp   int64  `json:"x"`
	// Issued is unix ms, checked against NotBefore.
	Issued int64 `json:"i"`
}

// Login starts the authorization-code flow: GET /auth/login.
func (s *SSO) Login(w http.ResponseWriter, r *http.Request) {
	st := s.settings()
	if st == nil {
		errorPage(w, http.StatusNotFound, "Single sign-on is not configured", "This server has no auth.oidc block.")
		return
	}
	p, err := s.discover(st.Issuer)
	if err != nil {
		slog.Warn("sso: provider discovery failed", "issuer", st.Issuer, "err", err)
		errorPage(w, http.StatusBadGateway, "Sign-in provider unreachable",
			"corrallm could not use the provider's configuration: "+err.Error()+". Check the issuer under Setup → Access (Test), or sign in with the admin token.")
		return
	}
	sp := statePayload{
		State:    randomString(),
		Nonce:    randomString(),
		Verifier: oauth2.GenerateVerifier(),
		Exp:      s.now().Add(stateTTL).Unix(),
	}
	http.SetCookie(w, &http.Cookie{
		Name:  stateCookie,
		Value: sign(s.Key, "oidc-state", sp),
		Path:  "/auth/",
		// Lax, not Strict: the callback is a top-level navigation FROM the
		// provider, and a Strict cookie would not be sent on it.
		SameSite: http.SameSiteLaxMode,
		HttpOnly: true,
		Secure:   secureRequest(r, st),
		MaxAge:   int(stateTTL.Seconds()),
	})
	url := oauthConfig(st, p, st.RedirectFor(r)).AuthCodeURL(sp.State, oidc.Nonce(sp.Nonce), oauth2.S256ChallengeOption(sp.Verifier))
	http.Redirect(w, r, url, http.StatusFound)
}

// Callback finishes the flow: GET /auth/callback.
func (s *SSO) Callback(w http.ResponseWriter, r *http.Request) {
	st := s.settings()
	if st == nil {
		errorPage(w, http.StatusNotFound, "Single sign-on is not configured", "This server has no auth.oidc block.")
		return
	}
	// One use only, whatever happens next.
	clearCookie(w, stateCookie, "/auth/", secureRequest(r, st))

	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		msg := e
		if d := q.Get("error_description"); d != "" {
			msg += ": " + d
		}
		slog.Warn("sso: provider returned an error", "error", e)
		errorPage(w, http.StatusUnauthorized, "Sign-in was not completed", "The provider said: "+msg)
		return
	}
	var sp statePayload
	c, err := r.Cookie(stateCookie)
	if err != nil || !verify(s.Key, "oidc-state", c.Value, &sp) || sp.Exp < s.now().Unix() {
		errorPage(w, http.StatusBadRequest, "Sign-in expired",
			"The sign-in attempt was missing, expired or came from another browser. Start again.")
		return
	}
	if !tokenEqual(q.Get("state"), sp.State) {
		slog.Warn("sso: state mismatch")
		errorPage(w, http.StatusBadRequest, "Sign-in rejected", "The response did not match this sign-in attempt. Start again.")
		return
	}
	p, err := s.discover(st.Issuer)
	if err != nil {
		slog.Warn("sso: provider discovery failed", "issuer", st.Issuer, "err", err)
		errorPage(w, http.StatusBadGateway, "Sign-in provider unreachable", "Try again, or sign in with the admin token.")
		return
	}
	ctx := s.ctx(r.Context())
	tok, err := oauthConfig(st, p, st.RedirectFor(r)).Exchange(ctx, q.Get("code"), oauth2.VerifierOption(sp.Verifier))
	if err != nil {
		slog.Warn("sso: code exchange failed", "err", exchangeErr(err))
		errorPage(w, http.StatusBadGateway, "Sign-in failed", "The provider did not accept the sign-in. Start again.")
		return
	}
	email, reason, refused := s.verifyIDToken(ctx, p, st, tok, sp.Nonce)
	if reason != "" {
		slog.Warn("sso: sign-in refused", "reason", reason, "email", email)
		if refused {
			msg := fmt.Sprintf("%s is not allowed to use this dashboard. Ask the operator to add it, or sign in with the admin token.", displayEmail(email))
			if strings.HasPrefix(reason, "email not verified") {
				msg = fmt.Sprintf("The provider has not verified %s, so it cannot be used to sign in here.", email)
			}
			errorPage(w, http.StatusForbidden, "Not allowed", msg)
			return
		}
		errorPage(w, http.StatusUnauthorized, "Sign-in rejected", "The provider's identity token did not verify. Start again.")
		return
	}
	now := s.now()
	if s.RecordSignIn != nil {
		if err := s.RecordSignIn(r.Context(), email, clientIP(r), now.UnixMilli()); err != nil {
			slog.Warn("sso: could not record sign-in", "email", email, "err", err)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    sign(s.Key, "session", sessionPayload{Email: email, Exp: now.Add(sessionTTL).Unix(), Issued: now.UnixMilli()}),
		Path:     "/",
		SameSite: http.SameSiteLaxMode,
		HttpOnly: true,
		Secure:   secureRequest(r, st),
		MaxAge:   int(sessionTTL.Seconds()),
	})
	slog.Info("sso: signed in", "email", email)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// verifyIDToken checks the ID token and admission. reason is empty on success.
// refused means the token verified but the person is not admitted; email is
// set then, for the refusal page.
func (s *SSO) verifyIDToken(ctx context.Context, p *oidc.Provider, st *OIDCSettings, tok *oauth2.Token, nonce string) (email, reason string, refused bool) {
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return "", "no id_token in token response", false
	}
	// Signature (JWKS, refetched on an unknown kid), iss, aud and exp are the
	// library's. Its Now is set back by clockSkew so a token that expired a few
	// seconds ago on a fast provider clock still passes.
	v := p.Verifier(&oidc.Config{ClientID: st.ClientID, Now: func() time.Time { return s.now().Add(-clockSkew) }})
	idt, err := v.Verify(ctx, raw)
	if err != nil {
		return "", "id token: " + err.Error(), false
	}
	if !tokenEqual(idt.Nonce, nonce) {
		return "", "id token: nonce mismatch", false
	}
	if !idt.IssuedAt.IsZero() && idt.IssuedAt.After(s.now().Add(clockSkew)) {
		return "", "id token: issued in the future", false
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified any    `json:"email_verified"`
	}
	if err := idt.Claims(&claims); err != nil {
		return "", "id token claims: " + err.Error(), false
	}
	// Zitadel, among others, leaves profile claims out of the ID token unless
	// the client opts in, and serves them from userinfo instead. Ask there,
	// and only accept an answer about the same subject.
	if strings.TrimSpace(claims.Email) == "" {
		ui, err := p.UserInfo(ctx, oauth2.StaticTokenSource(tok))
		if err != nil {
			return "", "userinfo: " + err.Error(), false
		}
		if ui.Subject != idt.Subject {
			return "", "userinfo: subject does not match the id token", false
		}
		if err := ui.Claims(&claims); err != nil {
			return "", "userinfo claims: " + err.Error(), false
		}
	}
	email = strings.ToLower(strings.TrimSpace(claims.Email))
	if email == "" {
		return "", "no email claim (is the email scope requested?)", true
	}
	if !st.AllowUnverifiedEmail && !truthy(claims.EmailVerified) {
		return email, "email not verified by the provider", true
	}
	if _, ok := st.roleOf(email); !ok {
		return email, "email is not in auth.users", true
	}
	return email, "", false
}

// truthy reads email_verified, which a few providers send as the string "true".
func truthy(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return strings.EqualFold(b, "true")
	}
	return false
}

// Logout clears the session cookie: POST /auth/logout.
func (s *SSO) Logout(w http.ResponseWriter, r *http.Request) {
	clearCookie(w, SessionCookie, "/", secureRequest(r, s.settings()))
	w.WriteHeader(http.StatusNoContent)
}

// WhoAmI tells the dashboard whether SSO is configured, who the session
// belongs to and their role: GET /auth/whoami. Public, so the login screen can
// ask before it has any credential. The admin token reports role admin and no
// email.
func (s *SSO) WhoAmI(w http.ResponseWriter, r *http.Request) {
	out := struct {
		OIDC bool `json:"oidc"`
		// AdminToken is enabled or disabled; the login screen hides the token
		// field when disabled.
		AdminToken string `json:"adminToken"`
		Email      string `json:"email,omitempty"`
		Role       Role   `json:"role,omitempty"`
	}{OIDC: s.settings() != nil, AdminToken: "enabled"}
	if !s.tokenOn() {
		out.AdminToken = "disabled"
	}
	if s != nil && s.AdminToken != "" && s.tokenOn() && authorized(r, s.AdminToken) {
		out.Role = RoleAdmin
	} else if id, ok := s.Session(r); ok {
		out.Email, out.Role = id.Email, id.Role
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

// Session returns who holds a valid session on r: signature good, not
// expired, issued after the user's revocation cutoff, SSO still enabled and
// the email still a user. The role comes from the current settings.
func (s *SSO) Session(r *http.Request) (Identity, bool) {
	st := s.settings()
	if st == nil {
		return Identity{}, false
	}
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return Identity{}, false
	}
	var sp sessionPayload
	if !verify(s.Key, "session", c.Value, &sp) || sp.Exp < s.now().Unix() {
		return Identity{}, false
	}
	role, ok := st.roleOf(sp.Email)
	if !ok {
		return Identity{}, false
	}
	if s.NotBefore != nil {
		nb, err := s.NotBefore(r.Context(), sp.Email)
		if err != nil {
			slog.Warn("sso: revocation lookup failed; refusing the session", "email", sp.Email, "err", err)
			return Identity{}, false
		}
		if nb > 0 && sp.Issued <= nb {
			return Identity{}, false
		}
	}
	return Identity{Email: sp.Email, Role: role}, true
}

// clientIP is the address recorded for a sign-in: RemoteAddr without the
// port, after the router's RealIP has run.
func clientIP(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

// sign encodes v as base64url(JSON).base64url(HMAC-SHA256). purpose is mixed
// into the MAC so a state cookie can never pass as a session.
func sign(key []byte, purpose string, v any) string {
	b, _ := json.Marshal(v)
	body := base64.RawURLEncoding.EncodeToString(b)
	return body + "." + base64.RawURLEncoding.EncodeToString(mac(key, purpose, body))
}

func verify(key []byte, purpose, value string, out any) bool {
	body, sig, ok := strings.Cut(value, ".")
	if !ok || len(key) == 0 {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, mac(key, purpose, body)) {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return false
	}
	return json.Unmarshal(b, out) == nil
}

func mac(key []byte, purpose, body string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(purpose + "\x00" + body))
	return m.Sum(nil)
}

func randomString() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// secureRequest mirrors refreshCookie's rule, plus an https redirect URL: the
// callback is served wherever that URL points.
func secureRequest(r *http.Request, st *OIDCSettings) bool {
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		return true
	}
	return st != nil && strings.HasPrefix(st.RedirectURL, "https://")
}

func clearCookie(w http.ResponseWriter, name, path string, secure bool) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: path, MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secure})
}

// exchangeErr keeps the provider's error code and drops the rest: the body of
// a token-endpoint response is not something to copy into a log.
func exchangeErr(err error) string {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		if re.ErrorCode != "" {
			return re.ErrorCode
		}
		if re.Response != nil {
			return re.Response.Status
		}
	}
	return "request failed"
}

func displayEmail(e string) string {
	if e == "" {
		return "This account"
	}
	return e
}

var errorTmpl = template.Must(template.New("err").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}} · corrallm</title></head>
<body style="font-family:system-ui,sans-serif;max-width:34em;margin:4em auto;padding:0 16px;line-height:1.5">
<h1 style="font-size:1.3em">{{.Title}}</h1>
<p>{{.Msg}}</p>
<p><a href="/">Back to the sign-in screen</a></p>
</body></html>
`))

// errorPage renders a plain page with a way back. Everything is escaped: msg
// can carry the provider's error_description, which arrives in the URL.
func errorPage(w http.ResponseWriter, status int, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = errorTmpl.Execute(w, struct{ Title, Msg string }{title, msg})
}
