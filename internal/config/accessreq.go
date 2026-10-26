package config

import (
	"crypto/subtle"
	"fmt"
	"net"
	"strings"
)

// AccessRequests says who may FILE an access request (P62): an agent or a
// person asking for a key, or for more on the key it has. Filing grants
// nothing — an admin approves each request — but an open door would let
// anyone fill the Approvals page, so it is narrowed here.
//
//	accessRequests:
//	  rules:
//	    - name: lan
//	      cidrs: [192.168.1.0/24, 127.0.0.0/8]
//	    - name: agents
//	      token: ${ACCESS_REQUEST_TOKEN}
//	    - name: people
//	      users: [carl@example.com]
//
// A request is accepted when ANY rule matches; a rule matches when EVERY field
// it sets does. Absent (nil) means the default: a direct connection from this
// machine or a private network. Present with no rules means nobody.
type AccessRequests struct {
	Rules []AccessRequestRule `yaml:"rules,omitempty" json:"rules,omitempty"`
}

// AccessRequestRule is one way in.
type AccessRequestRule struct {
	// Name labels the rule; recorded on each request it admits.
	Name string `yaml:"name,omitempty" json:"name,omitempty"`
	// CIDRs match the TCP peer of a DIRECT connection: one that carries no
	// forwarding header. Traffic through the front proxy never matches, since
	// the proxy's own address is on the LAN and would match for everyone.
	CIDRs []string `yaml:"cidrs,omitempty" json:"cidrs,omitempty"`
	// Token is a shared secret the filer sends as X-Corrallm-Request-Token.
	// ${NAME} resolves from the credential store, then the environment.
	Token string `yaml:"token,omitempty" json:"token,omitempty"`
	// Users are dashboard sign-ins (SSO emails) that may file from the
	// dashboard. They must also be listed under auth.users.
	Users []string `yaml:"users,omitempty" json:"users,omitempty"`
}

// HeaderRequestToken carries a rule's shared token.
const HeaderRequestToken = "X-Corrallm-Request-Token"

// Filer is what a filing request presents to the rules.
type Filer struct {
	Peer      net.IP // the TCP peer, before any forwarding header is read
	Forwarded bool   // a forwarding header is present
	Token     string // X-Corrallm-Request-Token
	User      string // signed-in dashboard user (same-origin only), "" if none
}

// direct reports whether f is a direct connection from a private network or
// this machine.
func (f Filer) direct() bool {
	return !f.Forwarded && f.Peer != nil && (f.Peer.IsLoopback() || f.Peer.IsPrivate() || f.Peer.IsLinkLocalUnicast())
}

// Allows reports whether f may file, and the name of the rule that let it.
// Nil-safe: a nil policy is the default rule.
func (a *AccessRequests) Allows(f Filer) (string, bool) {
	if a == nil {
		return "default (direct from LAN)", f.direct()
	}
	for i, r := range a.Rules {
		if r.matches(f) {
			if r.Name != "" {
				return r.Name, true
			}
			return fmt.Sprintf("rule %d", i+1), true
		}
	}
	return "", false
}

func (r AccessRequestRule) matches(f Filer) bool {
	if len(r.CIDRs) == 0 && r.Token == "" && len(r.Users) == 0 {
		return false
	}
	if len(r.CIDRs) > 0 {
		if f.Forwarded || f.Peer == nil {
			return false
		}
		in := false
		for _, c := range r.CIDRs {
			if _, n, err := net.ParseCIDR(strings.TrimSpace(c)); err == nil && n.Contains(f.Peer) {
				in = true
				break
			}
		}
		if !in {
			return false
		}
	}
	if r.Token != "" {
		want := ExpandSecrets(r.Token)
		if want == "" || strings.Contains(want, "${") ||
			subtle.ConstantTimeCompare([]byte(want), []byte(f.Token)) != 1 {
			return false
		}
	}
	if len(r.Users) > 0 {
		ok := false
		for _, u := range r.Users {
			if f.User != "" && strings.EqualFold(strings.TrimSpace(u), f.User) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// Validate reports a rule that can never match or names a bad subnet.
func (a *AccessRequests) Validate() error {
	if a == nil {
		return nil
	}
	for i, r := range a.Rules {
		if len(r.CIDRs) == 0 && r.Token == "" && len(r.Users) == 0 {
			return fmt.Errorf("accessRequests.rules[%d]: set cidrs, token or users; an empty rule matches nothing", i)
		}
		for _, c := range r.CIDRs {
			if _, _, err := net.ParseCIDR(strings.TrimSpace(c)); err != nil {
				return fmt.Errorf("accessRequests.rules[%d]: %q is not a subnet (want e.g. 192.168.1.0/24)", i, c)
			}
		}
	}
	return nil
}
