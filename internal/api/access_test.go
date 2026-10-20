package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/auth"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/store"
)

var (
	ctxToken  = auth.WithIdentity(context.Background(), auth.Identity{Role: auth.RoleAdmin})
	ctxAdmin  = auth.WithIdentity(context.Background(), auth.Identity{Email: "ops@example.com", Role: auth.RoleAdmin})
	ctxViewer = auth.WithIdentity(context.Background(), auth.Identity{Email: "eve@example.com", Role: auth.RoleViewer})
)

// accessHandlers is a store-backed Handlers whose config has SSO on, with the
// given users and legacy allowlist.
func accessHandlers(t *testing.T, users []config.AuthUser, legacy ...string) *Handlers {
	t.Helper()
	if err := config.LoadCredentials(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(managedConfig(t, localProviderYAML))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Auth = &config.Auth{Users: users, OIDC: &config.OIDC{
		Issuer: "https://id.example.com", ClientID: "cid", ClientSecret: "${OIDC_SECRET}", AllowedEmails: legacy,
	}}
	h := storeBackedHandlers(t, cfg)
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h.Store = st
	h.PublicBase = "https://corrallm.example.com/"
	return h
}

func TestLegacyAllowedEmailsReadAsAdminsAndDroppedOnSave(t *testing.T) {
	h := accessHandlers(t, []config.AuthUser{{Email: "ops@example.com", Role: "viewer"}}, "ops@example.com", "boss@example.com")
	out, err := h.ListAccess(ctxToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]AccessUser{}
	for _, u := range out.Body.Users {
		got[u.Email] = u
	}
	if got["ops@example.com"].Role != "viewer" || got["ops@example.com"].Legacy {
		t.Errorf("users list must win: %+v", got["ops@example.com"])
	}
	if got["boss@example.com"].Role != "admin" || !got["boss@example.com"].Legacy {
		t.Errorf("legacy email: %+v", got["boss@example.com"])
	}

	in := &UpsertUserInput{}
	in.Body.Email, in.Body.Role = "new@example.com", "viewer"
	if _, err := h.UpsertUser(ctxToken, in); err != nil {
		t.Fatal(err)
	}
	c := reloadStored(t, h, "after upsert")
	if len(c.Auth.OIDC.AllowedEmails) != 0 {
		t.Errorf("allowedEmails kept: %v", c.Auth.OIDC.AllowedEmails)
	}
	if len(c.Auth.Users) != 3 {
		t.Errorf("users = %+v", c.Auth.Users)
	}
}

func TestLastAdminProtected(t *testing.T) {
	h := accessHandlers(t, []config.AuthUser{{Email: "ops@example.com", Role: "admin"}, {Email: "eve@example.com", Role: "viewer"}})
	demote := &UpsertUserInput{}
	demote.Body.Email, demote.Body.Role = "ops@example.com", "viewer"
	if _, err := h.UpsertUser(ctxToken, demote); err == nil || !strings.Contains(err.Error(), "last admin") {
		t.Errorf("demote last admin: %v", err)
	}
	rm := &UserEmailInput{}
	rm.Body.Email = "OPS@example.com"
	if _, err := h.RemoveUser(ctxToken, rm); err == nil || !strings.Contains(err.Error(), "last admin") {
		t.Errorf("remove last admin: %v", err)
	}
	// With a second admin both go through.
	promote := &UpsertUserInput{}
	promote.Body.Email, promote.Body.Role = "eve@example.com", "admin"
	if _, err := h.UpsertUser(ctxToken, promote); err != nil {
		t.Fatal(err)
	}
	if _, err := h.RemoveUser(ctxToken, rm); err != nil {
		t.Fatalf("remove with another admin: %v", err)
	}
	// Removal revokes, so re-adding later does not revive an old cookie.
	if nb, _ := h.Store.AuthNotBefore(context.Background(), "ops@example.com"); nb == 0 {
		t.Error("removal did not revoke sessions")
	}
	bad := &UpsertUserInput{}
	bad.Body.Email, bad.Body.Role = "x@example.com", "owner"
	if _, err := h.UpsertUser(ctxToken, bad); err == nil || !strings.Contains(err.Error(), "role") {
		t.Errorf("bad role: %v", err)
	}
}

func TestRevokeUserSessions(t *testing.T) {
	h := accessHandlers(t, []config.AuthUser{{Email: "ops@example.com", Role: "admin"}})
	in := &UserEmailInput{}
	in.Body.Email = "ops@example.com"
	if _, err := h.RevokeUserSessions(ctxToken, in); err != nil {
		t.Fatal(err)
	}
	if nb, _ := h.Store.AuthNotBefore(context.Background(), "ops@example.com"); nb == 0 {
		t.Error("no cutoff recorded")
	}
}

func TestViewerRefusedByAccessHandlers(t *testing.T) {
	h := accessHandlers(t, []config.AuthUser{{Email: "ops@example.com", Role: "admin"}})
	if _, err := h.ListAccess(ctxViewer, nil); err == nil {
		t.Error("viewer listed access")
	}
	if _, err := h.UpsertUser(ctxViewer, &UpsertUserInput{}); err == nil {
		t.Error("viewer upserted a user")
	}
}

// listAccess reports the reference and whether it resolves, never the value.
func TestListAccessNeverReturnsSecretValues(t *testing.T) {
	h := accessHandlers(t, []config.AuthUser{{Email: "ops@example.com", Role: "admin"}})
	in := &SetIdentityProviderInput{}
	in.Body.Issuer, in.Body.ClientID, in.Body.Enabled = "https://id.example.com", "cid", true
	in.Body.SecretRef, in.Body.SecretValue = "OIDC_SECRET", "very-secret-value"
	if out, err := h.SetIdentityProvider(ctxToken, in); err != nil || !out.Body.OK {
		t.Fatalf("set provider: %v %+v", err, out)
	}
	out, err := h.ListAccess(ctxToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out.Body)
	if strings.Contains(string(raw), "very-secret-value") {
		t.Fatalf("secret value served: %s", raw)
	}
	p := out.Body.Provider
	if p.SecretRef != "OIDC_SECRET" || !p.HasSecret || !p.Enabled || p.RedirectURI != "https://corrallm.example.com/auth/callback" {
		t.Errorf("provider = %+v", p)
	}
	if c := reloadStored(t, h, "after provider"); c.Auth.OIDC.ClientSecret != "${OIDC_SECRET}" {
		t.Errorf("stored secret = %q, want a reference", c.Auth.OIDC.ClientSecret)
	}

	// A public client: no secret at all.
	in.Body.SecretRef, in.Body.SecretValue = "", ""
	if _, err := h.SetIdentityProvider(ctxToken, in); err != nil {
		t.Fatal(err)
	}
	if c := reloadStored(t, h, "public client"); c.Auth.OIDC.ClientSecret != "" {
		t.Errorf("public client stored secret %q", c.Auth.OIDC.ClientSecret)
	}
}

func TestTestIdentityProvider(t *testing.T) {
	h := accessHandlers(t, nil, "ops@example.com")
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" || r.Header.Get("Authorization") != "" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": srv.URL, "authorization_endpoint": srv.URL + "/authorize", "token_endpoint": srv.URL + "/token",
			"jwks_uri": srv.URL + "/keys", "code_challenge_methods_supported": []string{"S256"},
		})
	}))
	defer srv.Close()
	in := &TestIdentityProviderInput{}
	in.Body.Issuer = srv.URL + "/"
	out, err := h.TestIdentityProvider(ctxToken, in)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Body.OK || out.Body.TokenEndpoint != srv.URL+"/token" || len(out.Body.CodeChallengeMethods) != 1 {
		t.Errorf("test = %+v", out.Body)
	}
	in.Body.Issuer = srv.URL + "/nope"
	if out, _ := h.TestIdentityProvider(ctxToken, in); out.Body.OK || !strings.Contains(out.Body.Message, "404") {
		t.Errorf("missing document = %+v", out.Body)
	}
	in.Body.Issuer = "id.example.com"
	if out, _ := h.TestIdentityProvider(ctxToken, in); out.Body.OK {
		t.Errorf("relative issuer accepted")
	}
}

func TestSetAdminTokenGuards(t *testing.T) {
	off := &SetAdminTokenInput{}
	h := accessHandlers(t, []config.AuthUser{{Email: "ops@example.com", Role: "admin"}})
	if out, _ := h.SetAdminToken(ctxToken, off); out.Body.OK || !strings.Contains(out.Body.Message, "sign in with SSO") {
		t.Errorf("disable via admin token: %+v", out.Body)
	}
	viewersOnly := accessHandlers(t, []config.AuthUser{{Email: "eve@example.com", Role: "viewer"}})
	if out, _ := viewersOnly.SetAdminToken(ctxAdmin, off); out.Body.OK || !strings.Contains(out.Body.Message, "admin") {
		t.Errorf("disable with no admin user: %+v", out.Body)
	}
	ssoOff := accessHandlers(t, []config.AuthUser{{Email: "ops@example.com", Role: "admin"}})
	p := &SetIdentityProviderInput{}
	p.Body.Issuer, p.Body.ClientID, p.Body.Enabled = "https://id.example.com", "cid", false
	if _, err := ssoOff.SetIdentityProvider(ctxToken, p); err != nil {
		t.Fatal(err)
	}
	if out, _ := ssoOff.SetAdminToken(ctxAdmin, off); out.Body.OK || !strings.Contains(out.Body.Message, "not enabled") {
		t.Errorf("disable with SSO off: %+v", out.Body)
	}

	// From an SSO admin session it goes through, and re-enabling works.
	if out, err := h.SetAdminToken(ctxAdmin, off); err != nil || !out.Body.OK {
		t.Fatalf("disable from sso admin: %v %+v", err, out)
	}
	if c := reloadStored(t, h, "disabled"); c.Auth.AdminTokenOn() {
		t.Error("token still on")
	}
	// SSO cannot then be switched off: nothing would be left to sign in with.
	if _, err := h.SetIdentityProvider(ctxAdmin, p); err == nil || !strings.Contains(err.Error(), "adminToken") {
		t.Errorf("disable SSO with token off: %v", err)
	}
	on := &SetAdminTokenInput{}
	on.Body.Enabled = true
	if out, err := h.SetAdminToken(ctxAdmin, on); err != nil || !out.Body.OK {
		t.Fatalf("re-enable: %v %+v", err, out)
	}
	if c := reloadStored(t, h, "re-enabled"); !c.Auth.AdminTokenOn() {
		t.Error("token still off")
	}
}
