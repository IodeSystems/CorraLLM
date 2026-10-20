package config

import (
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync"
)

// Auth configures how operators sign in to the dashboard. The admin token
// always works and is always admin; this block adds ways on top of it.
type Auth struct {
	OIDC *OIDC `yaml:"oidc,omitempty"`
	// Users are the people SSO admits, each with a role. Edited from the
	// dashboard's Access tab.
	Users []AuthUser `yaml:"users,omitempty"`
	// AdminToken is enabled (the default, also when absent) or disabled. A
	// disabled token stays on disk but /api stops accepting it, which is the
	// end of "sign in with the token, set up SSO, turn the token off".
	// `corrallm auth admin-token enable` turns it back on without the API.
	AdminToken string `yaml:"adminToken,omitempty"`
}

// AdminToken values.
const (
	AdminTokenEnabled  = "enabled"
	AdminTokenDisabled = "disabled"
)

// AdminTokenOn reports whether /api accepts the admin token. Nil-safe.
func (a *Auth) AdminTokenOn() bool {
	return a == nil || a.AdminToken != AdminTokenDisabled
}

// HasAdminUser reports whether any effective user is an admin.
func (a *Auth) HasAdminUser() bool {
	for _, u := range a.EffectiveUsers() {
		if u.Role == RoleAdmin {
			return true
		}
	}
	return false
}

// Dashboard roles. An admin can change everything; a viewer reads.
const (
	RoleAdmin  = "admin"
	RoleViewer = "viewer"
)

// AuthUser is one person admitted by SSO.
type AuthUser struct {
	Email string `yaml:"email"`
	Role  string `yaml:"role"`
}

// EffectiveUsers is auth.users plus the legacy auth.oidc.allowedEmails, each
// of which is an admin. An email in both keeps its auth.users role. Nil-safe.
func (a *Auth) EffectiveUsers() []AuthUser {
	if a == nil {
		return nil
	}
	out := make([]AuthUser, 0, len(a.Users))
	seen := map[string]bool{}
	for _, u := range a.Users {
		out = append(out, AuthUser{Email: NormEmail(u.Email), Role: u.Role})
		seen[NormEmail(u.Email)] = true
	}
	if a.OIDC != nil {
		for _, e := range a.OIDC.AllowedEmails {
			if n := NormEmail(e); n != "" && !seen[n] {
				out = append(out, AuthUser{Email: n, Role: RoleAdmin})
				seen[n] = true
			}
		}
	}
	return out
}

// OIDCEnabled reports whether SSO sign-in is switched on.
func (a *Auth) OIDCEnabled() bool {
	return a != nil && a.OIDC != nil && !a.OIDC.Disabled
}

// NormEmail is the form emails are compared in.
func NormEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

// OIDC is single sign-on against one standards-compliant OpenID Connect
// provider, found by discovery at <issuer>/.well-known/openid-configuration.
//
// Admission is auth.users, not "anyone the provider knows": a Google issuer
// authenticates every Gmail account on earth, and signing in is not the same
// as being an operator.
type OIDC struct {
	// Disabled keeps the settings but turns sign-in off.
	Disabled bool   `yaml:"disabled,omitempty"`
	Issuer   string `yaml:"issuer"`
	ClientID string `yaml:"clientId"`
	// ClientSecret is optional: a public client (PKCE only, e.g. Zitadel's
	// "PKCE" auth method) has none. When set it should be a ${NAME} reference
	// into the credential store: the config is served verbatim by
	// /api/v1/config/*.
	ClientSecret string `yaml:"clientSecret,omitempty"`
	// RedirectURL is the callback registered with the provider. Unset, it is
	// <public-base>/auth/callback.
	RedirectURL string `yaml:"redirectUrl,omitempty"`
	// AllowedEmails is the legacy admission list, read as admins. Saving
	// through the Access API moves them into auth.users and drops this.
	AllowedEmails []string `yaml:"allowedEmails,omitempty"`
	// Scopes default to openid, email, profile.
	Scopes []string `yaml:"scopes,omitempty"`
	// AllowUnverifiedEmail admits an allowlisted email the provider has not
	// marked email_verified. Off by default: on a provider that lets users type
	// any address, an unverified email claim names nobody.
	AllowUnverifiedEmail bool `yaml:"allowUnverifiedEmail,omitempty"`
}

// DefaultOIDCScopes is what is requested when scopes is unset.
var DefaultOIDCScopes = []string{"openid", "email", "profile"}

// ScopesOrDefault returns the configured scopes, or DefaultOIDCScopes.
func (o *OIDC) ScopesOrDefault() []string {
	if len(o.Scopes) == 0 {
		return DefaultOIDCScopes
	}
	return o.Scopes
}

var literalSecretWarn sync.Once

func (a *Auth) validate() error {
	if a == nil {
		return nil
	}
	seen := map[string]bool{}
	for _, u := range a.Users {
		e := NormEmail(u.Email)
		if !strings.Contains(e, "@") {
			return fmt.Errorf("auth.users: %q is not an email address", u.Email)
		}
		if u.Role != RoleAdmin && u.Role != RoleViewer {
			return fmt.Errorf("auth.users: %s has role %q; want admin or viewer", e, u.Role)
		}
		if seen[e] {
			return fmt.Errorf("auth.users: %s is listed twice", e)
		}
		seen[e] = true
	}
	switch a.AdminToken {
	case "", AdminTokenEnabled:
	case AdminTokenDisabled:
		// Without both there is no way in but the CLI.
		if !a.OIDCEnabled() || !a.HasAdminUser() {
			return fmt.Errorf("auth.adminToken: disabled needs SSO enabled and at least one admin user, or nobody could administer the dashboard")
		}
	default:
		return fmt.Errorf("auth.adminToken: %q; want enabled or disabled", a.AdminToken)
	}
	if a.OIDC == nil {
		return nil
	}
	o := a.OIDC
	for _, e := range o.AllowedEmails {
		if !strings.Contains(e, "@") {
			return fmt.Errorf("auth.oidc: allowedEmails entry %q is not an email address", e)
		}
	}
	// Disabled settings are kept for later, so only what is filled in has to
	// be well-formed.
	if !o.Disabled || strings.TrimSpace(o.Issuer) != "" {
		u, err := url.Parse(strings.TrimSpace(o.Issuer))
		if err != nil || u.Host == "" {
			return fmt.Errorf("auth.oidc: issuer %q is not an absolute URL", o.Issuer)
		}
		// Discovery and the JWKS come from the issuer; over plain http anyone on
		// the path can hand back their own signing keys.
		secure := u.Scheme == "https" || (u.Scheme == "http" && loopbackHost(u.Hostname()))
		if !secure {
			return fmt.Errorf("auth.oidc: issuer %q must be https (plain http is allowed only for localhost)", o.Issuer)
		}
	}
	if !o.Disabled && strings.TrimSpace(o.ClientID) == "" {
		return fmt.Errorf("auth.oidc: clientId is required")
	}
	if o.ClientSecret != "" && !envRef.MatchString(o.ClientSecret) {
		literalSecretWarn.Do(func() {
			slog.Warn("auth.oidc.clientSecret is written literally in config, which /api/v1/config serves verbatim; " +
				"store it with `corrallm config secret set OIDC_CLIENT_SECRET` and reference it as ${OIDC_CLIENT_SECRET}")
		})
	}
	if o.RedirectURL != "" {
		r, err := url.Parse(o.RedirectURL)
		if err != nil || (r.Scheme != "https" && r.Scheme != "http") || r.Host == "" {
			return fmt.Errorf("auth.oidc: redirectUrl %q is not an absolute http(s) URL", o.RedirectURL)
		}
	}
	if !o.Disabled && len(a.EffectiveUsers()) == 0 {
		return fmt.Errorf("auth: SSO is enabled but no users are listed, so nobody could sign in; add at least one under auth.users")
	}
	if len(o.Scopes) > 0 && !containsFold(o.Scopes, "openid") {
		return fmt.Errorf("auth.oidc: scopes %v must include openid", o.Scopes)
	}
	return nil
}

func loopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), s) {
			return true
		}
	}
	return false
}
