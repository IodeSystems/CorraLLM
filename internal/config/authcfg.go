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
// always works; this block adds ways on top of it.
type Auth struct {
	OIDC *OIDC `yaml:"oidc,omitempty"`
}

// OIDC is single sign-on against one standards-compliant OpenID Connect
// provider, found by discovery at <issuer>/.well-known/openid-configuration.
//
// Admission is an email allowlist, not "anyone the provider knows": a Google
// issuer authenticates every Gmail account on earth, and signing in is not the
// same as being an operator.
type OIDC struct {
	Issuer   string `yaml:"issuer"`
	ClientID string `yaml:"clientId"`
	// ClientSecret should be a ${NAME} reference into the credential store:
	// the config is served verbatim by /api/v1/config/*.
	ClientSecret string `yaml:"clientSecret"`
	// RedirectURL is the callback registered with the provider. Unset, it is
	// <public-base>/auth/callback.
	RedirectURL   string   `yaml:"redirectUrl,omitempty"`
	AllowedEmails []string `yaml:"allowedEmails"`
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
	if a == nil || a.OIDC == nil {
		return nil
	}
	o := a.OIDC
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
	if strings.TrimSpace(o.ClientID) == "" {
		return fmt.Errorf("auth.oidc: clientId is required")
	}
	if strings.TrimSpace(o.ClientSecret) == "" {
		return fmt.Errorf("auth.oidc: clientSecret is required (reference it as ${NAME} from the credential store)")
	}
	if !envRef.MatchString(o.ClientSecret) {
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
	if len(o.AllowedEmails) == 0 {
		return fmt.Errorf("auth.oidc: allowedEmails is empty, so nobody could sign in; list the operators' emails")
	}
	for _, e := range o.AllowedEmails {
		if !strings.Contains(e, "@") {
			return fmt.Errorf("auth.oidc: allowedEmails entry %q is not an email address", e)
		}
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
