package config

import (
	"strings"
	"testing"
)

func TestOIDCValidate(t *testing.T) {
	ok := func() *OIDC {
		return &OIDC{
			Issuer:        "https://accounts.google.com",
			ClientID:      "cid",
			ClientSecret:  "${OIDC_CLIENT_SECRET}",
			AllowedEmails: []string{"a@b.com"},
		}
	}
	for _, tc := range []struct {
		name string
		mod  func(*OIDC)
		want string
	}{
		{"ok", func(*OIDC) {}, ""},
		{"http localhost ok", func(o *OIDC) { o.Issuer = "http://localhost:5556/dex" }, ""},
		{"http loopback ip ok", func(o *OIDC) { o.Issuer = "http://127.0.0.1:9000" }, ""},
		{"http remote refused", func(o *OIDC) { o.Issuer = "http://idp.example.com" }, "must be https"},
		{"not a url", func(o *OIDC) { o.Issuer = "accounts.google.com" }, "absolute URL"},
		{"no client id", func(o *OIDC) { o.ClientID = "" }, "clientId"},
		{"no secret is a public client", func(o *OIDC) { o.ClientSecret = "" }, ""},
		{"no users", func(o *OIDC) { o.AllowedEmails = nil }, "no users are listed"},
		{"disabled needs no users", func(o *OIDC) { o.AllowedEmails, o.Disabled = nil, true }, ""},
		{"disabled half-filled ok", func(o *OIDC) { o.AllowedEmails, o.Disabled, o.Issuer, o.ClientID = nil, true, "", "" }, ""},
		{"disabled bad issuer still refused", func(o *OIDC) { o.Disabled, o.Issuer = true, "http://idp.example.com" }, "must be https"},
		{"bad email", func(o *OIDC) { o.AllowedEmails = []string{"bob"} }, "not an email"},
		{"bad redirect", func(o *OIDC) { o.RedirectURL = "/auth/callback" }, "redirectUrl"},
		{"scopes without openid", func(o *OIDC) { o.Scopes = []string{"email"} }, "openid"},
		{"scopes with openid", func(o *OIDC) { o.Scopes = []string{"openid", "email"} }, ""},
	} {
		o := ok()
		tc.mod(o)
		err := (&Auth{OIDC: o}).validate()
		if (err == nil) != (tc.want == "") || (err != nil && !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: err %v, want %q", tc.name, err, tc.want)
		}
	}
	if err := (*Auth)(nil).validate(); err != nil {
		t.Errorf("nil auth: %v", err)
	}
	if err := (&Auth{}).validate(); err != nil {
		t.Errorf("empty auth: %v", err)
	}
}

// The block is checked by Load like every other section, so a bad one stops
// the config from being accepted rather than failing at the first sign-in.
func TestAuthBlockValidatedAtLoad(t *testing.T) {
	_, err := LoadBytesForTest([]byte(`
auth:
  oidc:
    issuer: https://idp.example.com
    clientId: cid
    clientSecret: ${S}
`))
	if err == nil || !strings.Contains(err.Error(), "no users") {
		t.Fatalf("err = %v, want a no-users refusal", err)
	}
	c, err := LoadBytesForTest([]byte(`
auth:
  oidc:
    issuer: https://idp.example.com
    clientId: cid
    clientSecret: ${S}
    allowedEmails: [ops@example.com]
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Auth.OIDC.ScopesOrDefault(); strings.Join(got, " ") != "openid email profile" {
		t.Errorf("default scopes = %v", got)
	}
}

func TestAuthUsersValidate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		users []AuthUser
		want  string
	}{
		{"ok", []AuthUser{{"a@b.com", "admin"}, {"c@d.com", "viewer"}}, ""},
		{"bad role", []AuthUser{{"a@b.com", "owner"}}, "role"},
		{"no at", []AuthUser{{"bob", "admin"}}, "not an email"},
		{"dup case-insensitive", []AuthUser{{"a@b.com", "admin"}, {" A@B.com", "viewer"}}, "twice"},
	} {
		err := (&Auth{Users: tc.users}).validate()
		if (err == nil) != (tc.want == "") || (err != nil && !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: err %v, want %q", tc.name, err, tc.want)
		}
	}
	// Users alone satisfy an enabled provider.
	a := &Auth{Users: []AuthUser{{"a@b.com", "viewer"}}, OIDC: &OIDC{Issuer: "https://idp.example.com", ClientID: "cid"}}
	if err := a.validate(); err != nil {
		t.Errorf("users without allowedEmails: %v", err)
	}
}

// The legacy allowlist reads as admins; an email in both keeps its users role.
func TestEffectiveUsersMergesLegacy(t *testing.T) {
	a := &Auth{
		Users: []AuthUser{{"Ops@Example.com", "viewer"}},
		OIDC:  &OIDC{AllowedEmails: []string{"ops@example.com", "boss@example.com"}},
	}
	got := a.EffectiveUsers()
	want := []AuthUser{{"ops@example.com", "viewer"}, {"boss@example.com", "admin"}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("effective = %+v, want %+v", got, want)
	}
	if (*Auth)(nil).EffectiveUsers() != nil {
		t.Error("nil auth has users")
	}
}

func TestAdminTokenSetting(t *testing.T) {
	sso := &OIDC{Issuer: "https://idp.example.com", ClientID: "cid"}
	admin := []AuthUser{{"a@b.com", "admin"}}
	viewer := []AuthUser{{"a@b.com", "viewer"}}
	off := &OIDC{Issuer: "https://idp.example.com", ClientID: "cid", Disabled: true}
	for _, tc := range []struct {
		name string
		a    *Auth
		want string
	}{
		{"absent is on", &Auth{}, ""},
		{"enabled", &Auth{AdminToken: "enabled"}, ""},
		{"bad value", &Auth{AdminToken: "off"}, "want enabled or disabled"},
		{"disabled with sso admin", &Auth{AdminToken: "disabled", OIDC: sso, Users: admin}, ""},
		{"disabled without sso", &Auth{AdminToken: "disabled", Users: admin}, "needs SSO"},
		{"disabled with sso switched off", &Auth{AdminToken: "disabled", OIDC: off, Users: admin}, "needs SSO"},
		{"disabled with only viewers", &Auth{AdminToken: "disabled", OIDC: sso, Users: viewer}, "admin user"},
	} {
		err := tc.a.validate()
		if (err == nil) != (tc.want == "") || (err != nil && !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: err %v, want %q", tc.name, err, tc.want)
		}
	}
	if !(*Auth)(nil).AdminTokenOn() || (&Auth{AdminToken: "disabled"}).AdminTokenOn() {
		t.Error("AdminTokenOn")
	}
}
