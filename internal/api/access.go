package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/iodesystems/corrallm/internal/auth"
	"github.com/iodesystems/corrallm/internal/config"
)

// Access: who may use the dashboard, with what role, and the one identity
// provider they sign in through. Admin only. The viewer gate in the /api
// middleware already refuses every write; requireAdmin is here for the one
// read (listAccess) and as a second check on the writes.

// requireAdmin refuses a viewer. No identity at all means --insecure, which
// has no gate to enforce.
func requireAdmin(ctx context.Context) error {
	if auth.IsViewer(ctx) {
		return huma.Error403Forbidden("admin only")
	}
	return nil
}

// OIDCRedirectURL is the callback the provider must have registered: the
// configured one, else <public-base>/auth/callback, else empty.
func OIDCRedirectURL(o *config.OIDC, publicBase string) string {
	if o != nil && o.RedirectURL != "" {
		return o.RedirectURL
	}
	if publicBase == "" {
		return ""
	}
	return strings.TrimRight(publicBase, "/") + "/auth/callback"
}

// AccessUser is one dashboard user.
type AccessUser struct {
	Email        string `json:"email"`
	Role         string `json:"role" doc:"admin | viewer"`
	LastSignInMS int64  `json:"lastSignInMs" doc:"Unix ms of the last SSO sign-in; 0 = never."`
	LastSignInIP string `json:"lastSignInIp"`
	// Legacy rows come from auth.oidc.allowedEmails and move to auth.users on
	// the next save through this API.
	Legacy bool `json:"legacy" doc:"Listed in the legacy auth.oidc.allowedEmails; moved to auth.users on the next save."`
}

// IdentityProviderView is the provider as the form edits it. Never the secret.
type IdentityProviderView struct {
	Configured    bool   `json:"configured" doc:"An auth.oidc block exists."`
	Enabled       bool   `json:"enabled" doc:"Sign-in is switched on."`
	Issuer        string `json:"issuer"`
	ClientID      string `json:"clientId"`
	SecretRef     string `json:"secretRef" doc:"Credential-store entry holding the client secret; empty for a public (PKCE-only) client."`
	HasSecret     bool   `json:"hasSecret" doc:"Whether that entry exists. The value is never returned."`
	LiteralSecret bool   `json:"literalSecret" doc:"The config holds a literal secret instead of a reference; saving from the form drops it."`
	RedirectURL   string `json:"redirectUrl" doc:"Configured callback override; empty = derived."`
	RedirectURI   string `json:"redirectUri" doc:"The callback to register with the provider, as corrallm will send it. Empty when it cannot be derived (no --public-base)."`
}

type AccessOutput struct {
	Body struct {
		Users      []AccessUser         `json:"users"`
		Provider   IdentityProviderView `json:"provider"`
		Secrets    []string             `json:"secrets" doc:"Names in the credential store, for picking a stored client secret. Names only."`
		AdminToken string               `json:"adminToken" doc:"enabled | disabled: whether /api accepts the admin token."`
		// Who is asking, so the form can say why disabling the token is refused.
		CallerEmail string `json:"callerEmail" doc:"Email of the SSO session making this request; empty for the admin token."`
	}
}

// ListAccess reports users, the identity provider and the admin token setting.
func (h *Handlers) ListAccess(ctx context.Context, _ *struct{}) (*AccessOutput, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	out := &AccessOutput{}
	out.Body.Users = []AccessUser{}
	out.Body.Secrets = config.SecretNames()
	sort.Strings(out.Body.Secrets)
	if id, ok := auth.IdentityFrom(ctx); ok {
		out.Body.CallerEmail = id.Email
	}
	cfg := h.config()
	var a *config.Auth
	if cfg != nil {
		a = cfg.Auth
	}
	out.Body.AdminToken = config.AdminTokenEnabled
	if !a.AdminTokenOn() {
		out.Body.AdminToken = config.AdminTokenDisabled
	}
	states := map[string]struct {
		ms int64
		ip string
	}{}
	if h.Store != nil {
		if all, err := h.Store.AuthUserStates(ctx); err == nil {
			for e, s := range all {
				states[e] = struct {
					ms int64
					ip string
				}{s.LastSignInMS, s.LastSignInIP}
			}
		}
	}
	listed := map[string]bool{}
	if a != nil {
		for _, u := range a.Users {
			listed[config.NormEmail(u.Email)] = true
		}
	}
	for _, u := range a.EffectiveUsers() {
		st := states[u.Email]
		out.Body.Users = append(out.Body.Users, AccessUser{
			Email: u.Email, Role: u.Role, LastSignInMS: st.ms, LastSignInIP: st.ip, Legacy: !listed[u.Email],
		})
	}
	sort.Slice(out.Body.Users, func(i, j int) bool { return out.Body.Users[i].Email < out.Body.Users[j].Email })

	p := &out.Body.Provider
	if a != nil && a.OIDC != nil {
		o := a.OIDC
		p.Configured, p.Enabled = true, !o.Disabled
		p.Issuer, p.ClientID, p.RedirectURL = o.Issuer, o.ClientID, o.RedirectURL
		if ref := secretRefOfValue(o.ClientSecret); ref != "" {
			p.SecretRef = ref
			for _, n := range out.Body.Secrets {
				if n == ref {
					p.HasSecret = true
				}
			}
		} else if o.ClientSecret != "" {
			p.LiteralSecret = true
		}
	}
	p.RedirectURI = OIDCRedirectURL(cfgOIDC(a), h.PublicBase)
	return out, nil
}

func cfgOIDC(a *config.Auth) *config.OIDC {
	if a == nil {
		return nil
	}
	return a.OIDC
}

// secretRefOfValue returns NAME when v is exactly ${NAME}.
func secretRefOfValue(v string) string {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "${") && strings.HasSuffix(v, "}") && !strings.Contains(v[2:len(v)-1], "${") {
		return v[2 : len(v)-1]
	}
	return ""
}

// editAuth runs fn on the auth block with the legacy allowlist folded into
// auth.users, so every save through this API writes only auth.users.
func (h *Handlers) editAuth(ctx context.Context, what string, fn func(a *config.Auth) error) error {
	return h.mutateConfig(ctx, what, func(c *config.Config) error {
		a := config.Auth{}
		if c.Auth != nil {
			a = *c.Auth
		}
		a.Users = a.EffectiveUsers()
		if a.OIDC != nil {
			o := *a.OIDC
			o.AllowedEmails = nil
			a.OIDC = &o
		}
		if err := fn(&a); err != nil {
			return err
		}
		c.Auth = &a
		return nil
	})
}

// lastAdminGuard refuses an edit that takes SSO from having an admin user to
// having none. The admin token would still get in, but an SSO-only team would
// find itself locked out of every setting without being told.
func lastAdminGuard(hadAdmin bool, a *config.Auth, email string) error {
	if hadAdmin && a.OIDCEnabled() && !a.HasAdminUser() {
		return huma.Error409Conflict(fmt.Sprintf(
			"%s is the last admin; add another admin first so somebody can still change settings through SSO", email))
	}
	return nil
}

type UpsertUserInput struct {
	Body struct {
		Email string `json:"email"`
		Role  string `json:"role" doc:"admin | viewer"`
	}
}

// UpsertUser adds a user or changes their role.
func (h *Handlers) UpsertUser(ctx context.Context, in *UpsertUserInput) (*ConfigMutationOutput, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	email := config.NormEmail(in.Body.Email)
	err := h.editAuth(ctx, "user "+email+" set to "+in.Body.Role, func(a *config.Auth) error {
		hadAdmin, found := a.HasAdminUser(), false
		for i, u := range a.Users {
			if u.Email == email {
				a.Users[i].Role, found = in.Body.Role, true
			}
		}
		if !found {
			a.Users = append(a.Users, config.AuthUser{Email: email, Role: in.Body.Role})
		}
		return lastAdminGuard(hadAdmin, a, email)
	})
	if err != nil {
		return nil, err
	}
	out := &ConfigMutationOutput{}
	out.Body.OK, out.Body.Message = true, "saved "+email
	return out, nil
}

type UserEmailInput struct {
	Body struct {
		Email string `json:"email"`
	}
}

// RemoveUser removes a user and revokes their sessions, so adding them back
// later does not revive a cookie from before.
func (h *Handlers) RemoveUser(ctx context.Context, in *UserEmailInput) (*ConfigMutationOutput, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	email := config.NormEmail(in.Body.Email)
	err := h.editAuth(ctx, "user "+email+" removed", func(a *config.Auth) error {
		hadAdmin := a.HasAdminUser()
		kept := a.Users[:0:0]
		for _, u := range a.Users {
			if u.Email != email {
				kept = append(kept, u)
			}
		}
		if len(kept) == len(a.Users) {
			return huma.Error404NotFound("no such user: " + email)
		}
		a.Users = kept
		return lastAdminGuard(hadAdmin, a, email)
	})
	if err != nil {
		return nil, err
	}
	out := &ConfigMutationOutput{}
	out.Body.OK, out.Body.Message = true, "removed "+email
	if h.Store != nil {
		if err := h.Store.AuthRevoke(ctx, email, time.Now().UnixMilli()); err != nil {
			out.Body.Message += "; revoking their sessions failed: " + err.Error()
		}
	}
	return out, nil
}

// RevokeUserSessions signs a user out everywhere: every session issued until
// now stops working on its next request.
func (h *Handlers) RevokeUserSessions(ctx context.Context, in *UserEmailInput) (*ConfigMutationOutput, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if h.Store == nil {
		return nil, huma.Error503ServiceUnavailable("no database to record the revocation in")
	}
	email := config.NormEmail(in.Body.Email)
	if !strings.Contains(email, "@") {
		return nil, huma.Error400BadRequest("not an email address: " + in.Body.Email)
	}
	if err := h.Store.AuthRevoke(ctx, email, time.Now().UnixMilli()); err != nil {
		return nil, huma.Error500InternalServerError("could not record the revocation", err)
	}
	out := &ConfigMutationOutput{}
	out.Body.OK, out.Body.Message = true, "signed "+email+" out everywhere"
	return out, nil
}

type SetIdentityProviderInput struct {
	Body struct {
		Issuer      string `json:"issuer"`
		ClientID    string `json:"clientId"`
		SecretRef   string `json:"secretRef" required:"false" doc:"Credential-store entry for the client secret. Empty = public client (PKCE only)."`
		SecretValue string `json:"secretValue" required:"false" doc:"Stored under secretRef first when set. Write-only."`
		RedirectURL string `json:"redirectUrl" required:"false" doc:"Callback override; empty derives it from --public-base."`
		Enabled     bool   `json:"enabled"`
	}
}

// SetIdentityProvider writes auth.oidc. Scopes and allowUnverifiedEmail are
// kept as they were: the form does not model them.
func (h *Handlers) SetIdentityProvider(ctx context.Context, in *SetIdentityProviderInput) (*ConfigMutationOutput, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	b := in.Body
	ref := strings.TrimSpace(b.SecretRef)
	out := &ConfigMutationOutput{}
	if b.SecretValue != "" {
		if ref == "" {
			out.Body.Message = "name the stored key the secret goes in"
			return out, nil
		}
		if err := config.SetSecret(ref, b.SecretValue); err != nil {
			out.Body.Message = err.Error()
			return out, nil
		}
	}
	err := h.editAuth(ctx, "identity provider saved", func(a *config.Auth) error {
		o := config.OIDC{}
		if a.OIDC != nil {
			o = *a.OIDC
		}
		o.Issuer, o.ClientID = strings.TrimSpace(b.Issuer), strings.TrimSpace(b.ClientID)
		o.RedirectURL = strings.TrimSpace(b.RedirectURL)
		o.Disabled = !b.Enabled
		o.ClientSecret = ""
		if ref != "" {
			// A REFERENCE: the config is served verbatim by /api/v1/config.
			o.ClientSecret = "${" + ref + "}"
		}
		a.OIDC = &o
		return nil
	})
	if err != nil {
		return nil, err
	}
	out.Body.OK = true
	out.Body.Message = "identity provider saved"
	if !b.Enabled {
		out.Body.Message += " (sign-in off)"
	}
	return out, nil
}

type TestIdentityProviderInput struct {
	Body struct {
		Issuer string `json:"issuer"`
	}
}

type TestIdentityProviderOutput struct {
	Body struct {
		OK                    bool     `json:"ok"`
		Message               string   `json:"message"`
		Issuer                string   `json:"issuer" doc:"The issuer the discovery document names. Must equal the one configured."`
		AuthorizationEndpoint string   `json:"authorizationEndpoint"`
		TokenEndpoint         string   `json:"tokenEndpoint"`
		UserinfoEndpoint      string   `json:"userinfoEndpoint"`
		JwksURI               string   `json:"jwksUri"`
		CodeChallengeMethods  []string `json:"codeChallengeMethods"`
		TokenAuthMethods      []string `json:"tokenAuthMethods"`
	}
}

// discoveryClient fetches discovery documents; short, so a dead issuer
// answers the form quickly.
var discoveryClient = &http.Client{Timeout: 10 * time.Second}

// TestIdentityProvider fetches <issuer>/.well-known/openid-configuration and
// reports what it found. It sends no credentials.
func (h *Handlers) TestIdentityProvider(ctx context.Context, in *TestIdentityProviderInput) (*TestIdentityProviderOutput, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	out := &TestIdentityProviderOutput{}
	out.Body.CodeChallengeMethods, out.Body.TokenAuthMethods = []string{}, []string{}
	issuer := strings.TrimRight(strings.TrimSpace(in.Body.Issuer), "/")
	u, err := url.Parse(issuer)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		out.Body.Message = "the issuer must be an absolute https URL"
		return out, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		out.Body.Message = err.Error()
		return out, nil
	}
	resp, err := discoveryClient.Do(req)
	if err != nil {
		out.Body.Message = "could not reach the issuer: " + err.Error()
		return out, nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		out.Body.Message = "discovery document: " + resp.Status
		return out, nil
	}
	var doc struct {
		Issuer        string   `json:"issuer"`
		Authorization string   `json:"authorization_endpoint"`
		Token         string   `json:"token_endpoint"`
		Userinfo      string   `json:"userinfo_endpoint"`
		JWKS          string   `json:"jwks_uri"`
		Challenge     []string `json:"code_challenge_methods_supported"`
		AuthMethods   []string `json:"token_endpoint_auth_methods_supported"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		out.Body.Message = "discovery document is not JSON: " + err.Error()
		return out, nil
	}
	b := &out.Body
	b.Issuer, b.AuthorizationEndpoint, b.TokenEndpoint = doc.Issuer, doc.Authorization, doc.Token
	b.UserinfoEndpoint, b.JwksURI = doc.Userinfo, doc.JWKS
	if doc.Challenge != nil {
		b.CodeChallengeMethods = doc.Challenge
	}
	if doc.AuthMethods != nil {
		b.TokenAuthMethods = doc.AuthMethods
	}
	switch {
	case strings.TrimRight(doc.Issuer, "/") != issuer:
		b.Message = fmt.Sprintf("the document names issuer %q, not %q; sign-in would fail the issuer check", doc.Issuer, issuer)
	case doc.Authorization == "" || doc.Token == "" || doc.JWKS == "":
		b.Message = "the document is missing the authorization, token or JWKS endpoint"
	default:
		b.OK = true
		b.Message = "discovery document found"
	}
	return out, nil
}

type SetAdminTokenInput struct {
	Body struct {
		Enabled bool `json:"enabled"`
	}
}

// SetAdminToken turns /api's acceptance of the admin token on or off.
//
// Disabling is accepted only from an SSO admin session: that request is the
// proof SSO works end to end. A request made WITH the admin token cannot turn
// it off. `corrallm auth admin-token enable` is the way back when SSO breaks.
func (h *Handlers) SetAdminToken(ctx context.Context, in *SetAdminTokenInput) (*ConfigMutationOutput, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	out := &ConfigMutationOutput{}
	if !in.Body.Enabled {
		id, ok := auth.IdentityFrom(ctx)
		if !ok || id.Email == "" || id.Role != auth.RoleAdmin {
			out.Body.Message = "sign in with SSO as an admin to disable the admin token; that sign-in is what proves SSO works"
			return out, nil
		}
		cfg := h.config()
		var a *config.Auth
		if cfg != nil {
			a = cfg.Auth
		}
		if !a.OIDCEnabled() {
			out.Body.Message = "SSO is not enabled; enable the identity provider first"
			return out, nil
		}
		if !a.HasAdminUser() {
			out.Body.Message = "no SSO user is an admin; make one an admin first"
			return out, nil
		}
	}
	v := config.AdminTokenEnabled
	if !in.Body.Enabled {
		v = config.AdminTokenDisabled
	}
	err := h.editAuth(ctx, "admin token "+v, func(a *config.Auth) error {
		a.AdminToken = v
		if in.Body.Enabled {
			a.AdminToken = ""
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out.Body.OK, out.Body.Message = true, "admin token "+v
	return out, nil
}
