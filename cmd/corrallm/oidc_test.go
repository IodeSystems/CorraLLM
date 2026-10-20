package main

import (
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

func TestOIDCSettings(t *testing.T) {
	if oidcSettings(&config.Config{}, "http://box:8111") != nil {
		t.Fatal("settings without an auth.oidc block")
	}
	t.Setenv("CORRALLM_TEST_OIDC_SECRET", "shh")
	c := &config.Config{Auth: &config.Auth{OIDC: &config.OIDC{
		Issuer: " https://idp.example.com ", ClientID: "cid", ClientSecret: "${CORRALLM_TEST_OIDC_SECRET}",
		AllowedEmails: []string{"ops@example.com"},
	}}}
	st := oidcSettings(c, "https://corrallm.example.com/")
	if st.RedirectURL != "https://corrallm.example.com/auth/callback" {
		t.Errorf("default redirect = %q", st.RedirectURL)
	}
	if st.ClientSecret != "shh" || st.Issuer != "https://idp.example.com" || len(st.Scopes) != 3 {
		t.Errorf("settings = %+v", st)
	}
	c.Auth.OIDC.RedirectURL = "https://other/cb"
	if got := oidcSettings(c, "https://corrallm.example.com").RedirectURL; got != "https://other/cb" {
		t.Errorf("explicit redirect overridden: %q", got)
	}
	c.Auth.OIDC.RedirectURL = ""
	if got := oidcSettings(c, "").RedirectURL; got != "" {
		t.Errorf("redirect invented without a public base: %q", got)
	}
}
