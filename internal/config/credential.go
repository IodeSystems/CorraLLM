package config

import (
	"fmt"
	"strings"
)

// DefaultCredentialName is the identity a provider's single inline credential
// gets when it declares none explicitly. It exists so that "how many
// credentials does this provider have" has one answer — at least one, always —
// instead of two code paths that drift.
const DefaultCredentialName = "default"

// Credential is ONE set of auth material against a provider.
//
// It exists because a provider is an ENDPOINT, and an endpoint routinely has
// several accounts. p16-free-aggregator.md named this at the outset — "free
// quota is enforced per ACCOUNT and those budgets are independent — across
// providers AND across multiple accounts of the same provider" — and the first
// implementation collapsed it: Provider carried one `proxy`, so two OpenRouter
// keys meant two provider entries pointing at the same host, with nothing
// tying them together and no way to budget them jointly or separately.
//
// The provider owns what is SHARED (host, port, basePath); a credential owns
// what differs (headers, budget, who may use it). That split is the whole
// design: anything a second key would not change belongs upstream of here.
type Credential struct {
	// Name identifies this credential within its provider and is the budget
	// key (see ScopeKey). Stable across restarts by contract — renaming one
	// starts its counters over, because the persisted rows key on it.
	Name string `yaml:"name" json:"name"`

	// Headers are merged OVER the provider's proxy headers, so shared ones
	// (anthropic-version, anthropic-beta) stay declared once and only the auth
	// material repeats. A credential may also override a shared header, which
	// is what makes "same endpoint, different account" expressible without
	// duplicating the endpoint.
	//
	// Values keep ${ENV} references literal, as everywhere else: expansion
	// happens at load, so the stored YAML — which /api/v1/config/* serves —
	// never contains a secret. See p21-provider-credentials.md §9.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// AuthTokenCommand is the per-credential form of the provider-level field:
	// a command whose stdout becomes the bearer token. Lets one account use a
	// rotating credential store while a sibling uses a static key.
	AuthTokenCommand string `yaml:"authTokenCommand,omitempty" json:"authTokenCommand,omitempty"`

	// Limits budget THIS credential — the level at which a provider actually
	// meters. A per-model budget splits one key's spend across N discovered
	// models; a per-provider budget cannot tell two accounts apart.
	Limits LimitSet `yaml:"limits,omitempty" json:"limits,omitempty"`

	// ApprovalRequired gates DISCOVERED models behind an explicit decision
	// before they serve.
	//
	// Off by default, and per credential rather than global: while every
	// discovered model was free, auto-enrolment risked a bad answer. A paid key
	// changes the stakes — auto-enrolling a discovered model there starts
	// spending on something nobody chose. Turning it on for the paid credential
	// leaves a free roster working untouched.
	ApprovalRequired bool `yaml:"approvalRequired,omitempty" json:"approvalRequired,omitempty"`

	// Allow lists the corrallm keys permitted to use this credential. Empty
	// means every caller may. An ALLOWlist rather than a denylist: the failure
	// direction is "denied" rather than "spent someone else's money".
	Allow []string `yaml:"allow,omitempty" json:"allow,omitempty"`
}

// ScopeKey is this credential's budget identity, namespaced by provider so two
// providers may both have a credential called "personal".
func (c Credential) ScopeKey(provider string) string {
	return "cred:" + provider + "/" + c.Name
}

// AllowsKey reports whether the corrallm key may use this credential.
func (c Credential) AllowsKey(key string) bool {
	if len(c.Allow) == 0 {
		return true
	}
	for _, k := range c.Allow {
		if k == key {
			return true
		}
	}
	return false
}

// CredentialList returns the provider's credentials, ALWAYS at least one.
//
// A provider that declares none gets a synthesised DefaultCredentialName
// carrying no headers of its own — the provider's proxy headers already apply,
// and merging an empty map over them changes nothing. That is what makes this
// change additive: every existing config keeps working, and callers get to
// assume the list is non-empty rather than branching on it.
func (p Provider) CredentialList() []Credential {
	if len(p.Credentials) > 0 {
		return p.Credentials
	}
	return []Credential{{Name: DefaultCredentialName}}
}

// CredentialNamed finds one by name, or false.
func (p Provider) CredentialNamed(name string) (Credential, bool) {
	for _, c := range p.CredentialList() {
		if c.Name == name {
			return c, true
		}
	}
	return Credential{}, false
}

// MergedHeaders is the header set this credential presents: the provider's
// shared headers with the credential's merged over. Neither input is mutated.
func (c Credential) MergedHeaders(shared map[string]string) map[string]string {
	if len(shared) == 0 && len(c.Headers) == 0 {
		return nil
	}
	out := make(map[string]string, len(shared)+len(c.Headers))
	for k, v := range shared {
		out[k] = v
	}
	for k, v := range c.Headers {
		out[k] = v
	}
	return out
}

// ProviderTarget resolves where one credential of a provider talks to, for
// callers that need the endpoint OUTSIDE a served model — enumerating a
// catalogue, testing a key.
//
// DiscoverTargets answers the same question but only for providers carrying a
// `discover` block, which is the wrong precondition here: browsing a catalogue
// is how you decide whether to write a filter, so requiring the filter first
// inverts the order the work actually happens in.
func (c *Config) ProviderTarget(provider, credential string) (string, *ProxyTarget, Credential, error) {
	extName, pv, ok := c.findProvider(provider)
	if !ok {
		return "", nil, Credential{}, fmt.Errorf("unknown provider %q", provider)
	}
	cr, ok := pv.CredentialNamed(credential)
	if !ok {
		return "", nil, Credential{}, fmt.Errorf("provider %q has no credential %q", provider, credential)
	}
	base, err := (Model{Proxy: pv.Proxy}).ProxyTarget()
	if err != nil {
		return "", nil, Credential{}, fmt.Errorf("provider %q: %w", provider, err)
	}
	t := *base
	if len(cr.Headers) > 0 || cr.AuthTokenCommand != "" {
		t.Headers = cr.MergedHeaders(base.Headers)
		if cr.AuthTokenCommand != "" {
			t.AuthTokenCommand = cr.AuthTokenCommand
		}
	}
	return extName, &t, cr, nil
}

// validateCredentials checks one provider's credential list.
func validateCredentials(ext, provider string, p Provider) error {
	if len(p.Credentials) == 0 {
		return nil // the synthesised default needs no validation
	}
	seen := map[string]bool{}
	for i, c := range p.Credentials {
		where := fmt.Sprintf("extension %q provider %q credential %d", ext, provider, i)
		name := strings.TrimSpace(c.Name)
		if name == "" {
			return fmt.Errorf("%s: name is required — it is the budget key, and an unnamed credential cannot be referred to or counted", where)
		}
		if seen[name] {
			return fmt.Errorf("%s: duplicate name %q — names key persisted budget counters, so two credentials sharing one would share a budget silently", where, name)
		}
		seen[name] = true
		if err := c.Limits.Validate(fmt.Sprintf("%s (%s)", where, name)); err != nil {
			return err
		}
	}
	return nil
}
