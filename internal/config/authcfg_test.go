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
		{"no secret", func(o *OIDC) { o.ClientSecret = "" }, "clientSecret"},
		{"no emails", func(o *OIDC) { o.AllowedEmails = nil }, "allowedEmails is empty"},
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
	if err == nil || !strings.Contains(err.Error(), "allowedEmails") {
		t.Fatalf("err = %v, want allowedEmails refusal", err)
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
