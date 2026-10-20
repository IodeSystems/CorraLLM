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
// person gets by signing in at an OpenID Connect provider with an allowlisted,
// verified email. The session is stateless: an HMAC-signed {email, expiry}
// under a server key. Nothing is stored server-side, so sign-out clears the
// cookie but cannot revoke a copy of it; rotating the key (delete
// <home>/session.key, restart) revokes every session.

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

// OIDCSettings is the resolved provider config: secret expanded, redirect URL
// defaulted.
type OIDCSettings struct {
	Issuer               string
	ClientID             string
	ClientSecret         string
	RedirectURL          string
	Scopes               []string
	AllowedEmails        []string
	AllowUnverifiedEmail bool
}

func (s *OIDCSettings) allows(email string) bool {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return false
	}
	for _, e := range s.AllowedEmails {
		if strings.ToLower(strings.TrimSpace(e)) == email {
			return true
		}
	}
	return false
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
func (s *SSO) discover(issuer string) (*oidc.Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.provider != nil && s.issuer == issuer {
		return s.provider, nil
	}
	p, err := oidc.NewProvider(s.ctx(context.Background()), issuer)
	if err != nil {
		return nil, err
	}
	s.provider, s.issuer = p, issuer
	return p, nil
}

func oauthConfig(st *OIDCSettings, p *oidc.Provider) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     st.ClientID,
		ClientSecret: st.ClientSecret,
		RedirectURL:  st.RedirectURL,
		Endpoint:     p.Endpoint(),
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
}

// Login starts the authorization-code flow: GET /auth/login.
func (s *SSO) Login(w http.ResponseWriter, r *http.Request) {
	st := s.settings()
	if st == nil {
		errorPage(w, http.StatusNotFound, "Single sign-on is not configured", "This server has no auth.oidc block.")
		return
	}
	if st.RedirectURL == "" {
		errorPage(w, http.StatusInternalServerError, "Single sign-on is not set up",
			"No callback URL: set auth.oidc.redirectUrl, or start corrallm with --public-base.")
		return
	}
	p, err := s.discover(st.Issuer)
	if err != nil {
		slog.Warn("sso: provider discovery failed", "issuer", st.Issuer, "err", err)
		errorPage(w, http.StatusBadGateway, "Sign-in provider unreachable",
			"corrallm could not read the provider's configuration. Try again, or sign in with the admin token.")
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
	url := oauthConfig(st, p).AuthCodeURL(sp.State, oidc.Nonce(sp.Nonce), oauth2.S256ChallengeOption(sp.Verifier))
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
	tok, err := oauthConfig(st, p).Exchange(ctx, q.Get("code"), oauth2.VerifierOption(sp.Verifier))
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
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    sign(s.Key, "session", sessionPayload{Email: email, Exp: s.now().Add(sessionTTL).Unix()}),
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
	email = strings.ToLower(strings.TrimSpace(claims.Email))
	if email == "" {
		return "", "no email claim (is the email scope requested?)", true
	}
	if !st.AllowUnverifiedEmail && !truthy(claims.EmailVerified) {
		return email, "email not verified by the provider", true
	}
	if !st.allows(email) {
		return email, "email is not in auth.oidc.allowedEmails", true
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

// WhoAmI tells the dashboard whether SSO is configured and who the session
// belongs to: GET /auth/whoami. Public, so the login screen can ask before it
// has any credential.
func (s *SSO) WhoAmI(w http.ResponseWriter, r *http.Request) {
	out := struct {
		OIDC  bool   `json:"oidc"`
		Email string `json:"email,omitempty"`
	}{OIDC: s.settings() != nil}
	out.Email, _ = s.SessionEmail(r)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

// SessionEmail returns the email of a valid session on r: signature good, not
// expired, SSO still configured and the email still allowlisted.
func (s *SSO) SessionEmail(r *http.Request) (string, bool) {
	st := s.settings()
	if st == nil {
		return "", false
	}
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return "", false
	}
	var sp sessionPayload
	if !verify(s.Key, "session", c.Value, &sp) || sp.Exp < s.now().Unix() || !st.allows(sp.Email) {
		return "", false
	}
	return sp.Email, true
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
