package config

import (
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
)

// A VIRTUAL extension: an extension that satisfies the provider contract.
//
// The layering. A provider is an endpoint with a catalogue. An extension groups
// providers. A virtual extension is an extension that is ALSO a provider — same
// contract, different implementation: its catalogue is the union of its
// members' catalogues, narrowed by a filter, and it holds no key of its own
// because the models it offers are reached with the key of whichever member
// actually serves them.
//
// `free` is the motivating case and explains the shape. The free tier was never
// a property of OpenRouter; it is a property of the SET of providers that give
// something away, and it churns — a model that was free last week is billable
// today, and one provider withdrawing it should not take the pool down. Writing
// that as a per-provider `discover` filter got it backwards twice over: the
// filter enrolled models nobody asked for, and it could only ever see one
// provider's catalogue at a time.
//
// What it deliberately does NOT do: rename anything. A model drawn into the
// pool keeps the served name of the provider that serves it
// (`openrouter-moonshotai-kimi-k2`), because quota, credential scoping and cost
// already key on that provider, and a second name for one upstream model would
// split its metrics, its residency and its budget between two identities.
// Membership in the pool is a fact ABOUT the model, not a new model.

// Virtual makes an extension act as a provider over its own members.
type Virtual struct {
	// Filter narrows the union. For `free` this is `{free: true}` plus the
	// modality and window constraints that keep a music model out of a chat
	// lane. An empty filter would admit every member's entire catalogue, which
	// is not a pool, it is a mess.
	Filter DiscoverFilter `yaml:"filter,omitempty"`
	// Template shapes each contributed model — cost class, quality, free-tier
	// budget — exactly as a discovery template did.
	Template Model `yaml:"template,omitempty"`
	// Limit caps the POOL, not each member: this is one catalogue assembled
	// from several, and a per-member cap would let one verbose provider crowd
	// out the rest. Ordered by context length descending, so a cap keeps the
	// most useful.
	Limit int `yaml:"limit,omitempty"`
	// Lanes place every member of the pool into a lane at a priority. This is
	// how the pool becomes reachable as one thing: a caller asks for the lane,
	// and the walk tries the pool's members in order. Without it the models are
	// servable only by their own names, which is a list, not a tier.
	Lanes []LanePlacement `yaml:"lanes,omitempty"`
}

// VirtualTarget is one (virtual extension, source provider, credential) to
// fetch. Three axes because they vary independently: a pool spans providers,
// and each provider's catalogue differs by key.
type VirtualTarget struct {
	Virtual    string // the extension acting as a provider
	Source     string // the member provider whose catalogue this is
	Credential string
	Spec       *Virtual
	Target     *ProxyTarget
	ProxyNode  yaml.Node
}

// VirtualTargets lists every fetch the virtual extensions require.
//
// Sources are the providers declared INSIDE the extension — being in the pool
// is an explicit act of putting the provider there, so a paid provider added
// elsewhere can never leak in on the strength of listing one free model.
func (c *Config) VirtualTargets() []VirtualTarget {
	var out []VirtualTarget
	exts := make([]string, 0, len(c.Extensions))
	for n := range c.Extensions {
		exts = append(exts, n)
	}
	sort.Strings(exts)
	for _, en := range exts {
		ext := c.Extensions[en]
		if ext.Virtual == nil {
			continue
		}
		provs := make([]string, 0, len(ext.Providers))
		for pn := range ext.Providers {
			provs = append(provs, pn)
		}
		sort.Strings(provs)
		for _, pn := range provs {
			pv := ext.Providers[pn]
			if pv.Proxy.IsZero() {
				continue // a member with no endpoint contributes no catalogue
			}
			base, err := (Model{Proxy: pv.Proxy}).ProxyTarget()
			if err != nil {
				continue
			}
			for _, cr := range pv.CredentialList() {
				t := *base
				if len(cr.Headers) > 0 || cr.AuthTokenCommand != "" {
					t.Headers = cr.MergedHeaders(base.Headers)
					if cr.AuthTokenCommand != "" {
						t.AuthTokenCommand = cr.AuthTokenCommand
					}
				}
				out = append(out, VirtualTarget{
					Virtual: en, Source: pn, Credential: cr.Name,
					Spec: ext.Virtual, Target: &t, ProxyNode: pv.Proxy,
				})
			}
		}
	}
	return out
}

// VirtualModelFor builds the served model one pooled catalogue row becomes.
//
// The served name is the SOURCE provider's, deliberately — see the type comment.
func (c *Config) VirtualModelFor(t VirtualTarget, upstreamID string) (string, Model) {
	m := t.Spec.Template // value copy: the template is never mutated
	if m.Type == "" {
		m.Type = "chat"
	}
	m.Extension, m.ProviderName = t.Virtual, t.Source
	m.Proxy = t.ProxyNode
	m.Upstream = upstreamID
	return ServedName(t.Source, upstreamID), m
}

// virtualLaneMembers returns the pool's models as lane candidates.
//
// Read from the recorded membership rather than re-derived, because the
// predicate is not reconstructible after the fact: a served Model keeps no
// context length and no free flag, so "which models did this filter admit"
// cannot be answered from the overlay alone.
func (c *Config) virtualLaneMembers(lane string) []Candidate {
	c.mu.RLock()
	type hit struct {
		name  string
		order int
	}
	var hits []hit
	for en, ext := range c.Extensions {
		if ext.Virtual == nil {
			continue
		}
		for _, lp := range ext.Virtual.Lanes {
			if lp.Lane != lane {
				continue
			}
			for _, name := range c.virtualMembers[en] {
				hits = append(hits, hit{name: name, order: lp.Order})
			}
		}
	}
	models := make(map[string]Model, len(hits))
	for _, h := range hits {
		if m, ok := c.discovered[h.name]; ok {
			models[h.name] = m
		}
	}
	c.mu.RUnlock()

	// Stable order: the pool's declared priority first, then by name. The pool
	// is assembled from a map, and a lane whose ladder reshuffled between
	// refreshes would make routing irreproducible.
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].order != hits[j].order {
			return hits[i].order < hits[j].order
		}
		return hits[i].name < hits[j].name
	})
	seen := map[string]bool{}
	out := make([]Candidate, 0, len(hits))
	for _, h := range hits {
		if seen[h.name] {
			continue
		}
		seen[h.name] = true
		m, ok := models[h.name]
		if !ok {
			// Declared in the pool but not currently contributed — the provider
			// withdrew it, or a refresh has not run yet. Skipping is the point
			// of a churning pool.
			continue
		}
		out = append(out, Candidate{Name: h.name, Model: m})
	}
	return out
}

// validateVirtual checks one extension's virtual block.
func validateVirtual(name string, ext Extension) error {
	v := ext.Virtual
	if v == nil {
		return nil
	}
	if len(ext.Providers) == 0 {
		return fmt.Errorf("extension %q: virtual draws on the providers declared inside it, and there are none", name)
	}
	endpoints := 0
	for _, pv := range ext.Providers {
		if !pv.Proxy.IsZero() {
			endpoints++
		}
	}
	if endpoints == 0 {
		return fmt.Errorf("extension %q: virtual needs at least one member provider with a proxy to draw a catalogue from", name)
	}
	// An unfiltered pool is every member's entire catalogue — hundreds of
	// models, most of them wrong for it. Caught here rather than left to
	// surprise someone at the next refresh.
	f := v.Filter
	if !f.Free && f.InputModality == "" && f.OutputModality == "" && f.MinContext == 0 && len(f.Exclude) == 0 && v.Limit == 0 {
		return fmt.Errorf("extension %q: virtual has no filter and no limit — it would pool every model every member offers", name)
	}
	for _, lp := range v.Lanes {
		if lp.Lane == "" {
			return fmt.Errorf("extension %q: virtual lane placement needs a lane name", name)
		}
	}
	return nil
}
