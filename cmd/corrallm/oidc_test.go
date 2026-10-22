package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/auth"
	"github.com/iodesystems/corrallm/internal/config"
)

func TestOIDCSettings(t *testing.T) {
	if oidcSettings(&config.Config{}) != nil {
		t.Fatal("settings without an auth.oidc block")
	}
	t.Setenv("CORRALLM_TEST_OIDC_SECRET", "shh")
	c := &config.Config{Auth: &config.Auth{OIDC: &config.OIDC{
		Issuer: " https://idp.example.com ", ClientID: "cid", ClientSecret: "${CORRALLM_TEST_OIDC_SECRET}",
		AllowedEmails: []string{"ops@example.com"},
	}}}
	st := oidcSettings(c)
	// No override: the callback comes from the request (auth.RedirectFor).
	if st.RedirectURL != "" {
		t.Errorf("default redirect = %q, want none", st.RedirectURL)
	}
	if st.ClientSecret != "shh" || st.Issuer != "https://idp.example.com" || len(st.Scopes) != 3 {
		t.Errorf("settings = %+v", st)
	}
	c.Auth.OIDC.RedirectURL = "https://other/cb"
	if got := oidcSettings(c).RedirectURL; got != "https://other/cb" {
		t.Errorf("explicit redirect overridden: %q", got)
	}
}

func TestOIDCSettingsUsersAndDisabled(t *testing.T) {
	c := &config.Config{Auth: &config.Auth{
		Users: []config.AuthUser{{Email: "Eve@Example.com", Role: "viewer"}},
		OIDC:  &config.OIDC{Issuer: "https://idp.example.com", ClientID: "cid", AllowedEmails: []string{"ops@example.com"}},
	}}
	st := oidcSettings(c)
	if st.Users["eve@example.com"] != auth.RoleViewer || st.Users["ops@example.com"] != auth.RoleAdmin {
		t.Errorf("users = %v", st.Users)
	}
	c.Auth.OIDC.Disabled = true
	if oidcSettings(c) != nil {
		t.Error("settings while disabled")
	}
}

// The recovery path: enable and disable round-trip through the config store
// with no daemon involved.
func TestAuthAdminTokenCommand(t *testing.T) {
	ctx := context.Background()
	db, src, err := openConfigDB(filepath.Join(t.TempDir(), "corrallm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := src.Save(ctx, &config.Config{Auth: &config.Auth{
		Users: []config.AuthUser{{Email: "ops@example.com", Role: "admin"}},
		OIDC:  &config.OIDC{Issuer: "https://idp.example.com", ClientID: "cid"},
	}}); err != nil {
		t.Fatal(err)
	}
	status := func() string {
		var b strings.Builder
		if err := adminTokenStatus(ctx, db, &b); err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(b.String())
	}
	var out strings.Builder
	if err := setAdminToken(ctx, src, config.AdminTokenDisabled, &out); err != nil {
		t.Fatal(err)
	}
	if status() != "disabled" || !strings.Contains(out.String(), "systemctl --user reload corrallm") {
		t.Errorf("after disable: %s / %s", status(), out.String())
	}
	if err := setAdminToken(ctx, src, config.AdminTokenEnabled, &out); err != nil {
		t.Fatal(err)
	}
	if status() != "enabled" {
		t.Errorf("after enable: %s", status())
	}
	c, _ := src.Load(ctx)
	if c.Auth.AdminToken != "" || c.Auth.OIDC.ClientID != "cid" || len(c.Auth.Users) != 1 {
		t.Errorf("auth after round trip = %+v", c.Auth)
	}
}
