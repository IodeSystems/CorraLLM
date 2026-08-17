package config

import (
	"fmt"
	"log/slog"
	"sort"
)

// Top-level `providers:` — a provider whose models own their own process.
//
// The asymmetry this fixes. A remote provider is an ENDPOINT: one host, one
// key, a catalogue of models reached through it. corrallm modelled that
// properly (extensions.<x>.providers.<p>). A local model is the other kind — it
// owns a process, a GPU budget and a port — and it had no provider at all. It
// sat in a top-level `models:` map with no owner, which meant the Providers
// page could show every remote you talk to and nothing about the six processes
// actually running on the box.
//
//	providers:
//	  local:
//	    models:
//	      Qwen3.8-27B:
//	        cmd: ...        # each model owns its process, unlike an extension
//	        server: box1    # where that process runs
//	        proxy: {host: 127.0.0.1, port: 5801}
//
// Why not an extension. `extensions.<x>.provides` shares ONE cmd across
// everything it provides — that is the oidio shape, one process serving four
// models — and Effective() overlays it. Six models with six different GGUFs,
// context windows and ports cannot share a cmd. Per-model lifecycle is exactly
// what the old top-level `models:` expressed, so this keeps that and gives it
// an owner.
//
// Served names are PREFIXED, `local-Qwen3.8-27B`, the same rule every remote
// provider follows. That is a breaking rename and it is deliberate: the point
// of the refactor is that local stops being the special case. Old callers keep
// working through BARE PRECEDENCE (below) rather than a hand-written alias per
// model — an unprefixed name resolves to the highest-precedence provider that
// offers it, and local claims highest by default.

// LocalProvider is a provider whose models each own a process.
type LocalProvider struct {
	// Models are declared here rather than fetched: there is no catalogue
	// endpoint to enumerate, because the "catalogue" is whatever the operator
	// has put on the disk.
	Models map[string]Model `yaml:"models,omitempty"`

	// BarePrecedence is how strongly this provider claims an UNPREFIXED name.
	//
	// Prefixing served names is what makes local stop being a special case, and
	// it is also a breaking rename: every caller asking for `Qwen3.8-27B` would
	// get a 404. Precedence is the answer, and a better one than writing an
	// alias per model — a bare name that matches nothing else resolves to the
	// highest-precedence provider offering that id, so `Qwen3.8-27B` keeps
	// working and `local-Qwen3.8-27B` is its explicit spelling.
	//
	// It also states a policy worth stating: when a bare name is ambiguous —
	// the same id offered locally and by a remote — the local process wins,
	// because it is free and on this box. Raise a remote's precedence above
	// local's to invert that.
	//
	// Nil means the default, which is ON at defaultBarePrecedence for a local
	// provider. Zero, written explicitly, turns it off: only the prefixed name
	// resolves.
	BarePrecedence *int `yaml:"barePrecedence,omitempty"`

	// Notes is free text kept about the provider, shown in the UI beside it.
	Notes string `yaml:"notes,omitempty"`
}

// defaultBarePrecedence is what a top-level provider claims when it says
// nothing. High, because the only providers declared here are ones whose models
// run on this box, and a bare name should prefer the process you already paid
// for over anybody's API.
const defaultBarePrecedence = 100

// BarePrecedenceOf resolves a provider's effective claim strength, applying the
// default. Exported so the dashboard reports what is IN FORCE rather than what
// was written, which for an unset field are different things.
func BarePrecedenceOf(lp LocalProvider) int {
	if lp.BarePrecedence == nil {
		return defaultBarePrecedence
	}
	return *lp.BarePrecedence
}

// bareClaim is one provider's claim on an unprefixed model id.
type bareClaim struct {
	served     string
	provider   string
	precedence int
}

// foldLocalProviders folds every top-level provider's models into the served
// registry under their prefixed names.
//
// Folding rather than teaching the rest of the system a second registry: these
// ARE declared models — they differ from the old top-level ones only in where
// they are written and what they are called. Resolution, residency, eviction,
// lanes and the tune cache keep working unchanged because they still see one
// map of declared models. A parallel registry would mean auditing every reader
// of c.Models for a case it has never had.
func (c *Config) foldLocalProviders() error {
	if len(c.Providers) == 0 {
		return nil
	}
	names := make([]string, 0, len(c.Providers))
	for n := range c.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	if c.Models == nil {
		c.Models = map[string]Model{}
	}
	for _, pn := range names {
		lp := c.Providers[pn]
		if len(lp.Models) == 0 {
			return fmt.Errorf("provider %q: declares no models — a local provider is its models", pn)
		}
		ids := make([]string, 0, len(lp.Models))
		for id := range lp.Models {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			m := lp.Models[id]
			served := ServedName(pn, id)
			if _, clash := c.Models[served]; clash {
				return fmt.Errorf("provider %q model %q: served name %q collides with a model declared elsewhere", pn, id, served)
			}
			if _, clash := c.Lanes[served]; clash {
				return fmt.Errorf("provider %q model %q: served name %q collides with a lane", pn, id, served)
			}
			// A local model owns its process, so it must say which box runs it
			// and where to reach it. Caught here because the message can name
			// the provider; the generic model check cannot.
			if m.Cmd == "" && m.Proxy.IsZero() {
				return fmt.Errorf("provider %q model %q: needs a cmd (a process to run) or a proxy (something already listening)", pn, id)
			}
			m.ProviderName = pn
			c.Models[served] = m
		}
	}
	return nil
}

// buildBareIndex indexes every provider's UNPREFIXED model ids.
//
// One function for both kinds deliberately. The claim is the same mechanism
// wherever it comes from, and the whole point of precedence is that claims from
// different providers COMPETE — which they cannot do if each kind maintains its
// own index.
//
//	top-level providers (local)   default ON at defaultBarePrecedence
//	providers inside an extension default OFF
//
// The asymmetry is not an accident: a local model runs on this box and its
// unprefixed name is what callers used before the prefix rename, while a remote
// provider answering a bare name would route a request off the box on the
// strength of a coincidence.
func (c *Config) buildBareIndex() {
	c.bare = map[string][]bareClaim{}
	claim := func(id, served, provider string, prec int) {
		if prec <= 0 || id == "" {
			return
		}
		c.bare[id] = append(c.bare[id], bareClaim{served: served, provider: provider, precedence: prec})
	}
	for pn, lp := range c.Providers {
		prec := defaultBarePrecedence
		if lp.BarePrecedence != nil {
			prec = *lp.BarePrecedence
		}
		for id := range lp.Models {
			claim(id, ServedName(pn, id), pn, prec)
		}
	}
	for _, ext := range c.Extensions {
		for pn, pv := range ext.Providers {
			if pv.BarePrecedence == nil {
				continue // off by default for a remote provider
			}
			for id := range pv.Provides {
				claim(id, ServedName(pn, id), pn, *pv.BarePrecedence)
			}
		}
	}
	// Strongest claim first, ties broken by provider name so the winner is
	// reproducible rather than map-order random.
	for id := range c.bare {
		claims := c.bare[id]
		sort.SliceStable(claims, func(i, j int) bool {
			if claims[i].precedence != claims[j].precedence {
				return claims[i].precedence > claims[j].precedence
			}
			return claims[i].provider < claims[j].provider
		})
		c.bare[id] = claims
	}
}

// resolveBare answers an unprefixed request, or reports that nothing claims it.
//
// Deliberately the LAST thing tried. It must never shadow an exact model, an
// alias, a lane or a discovered name — a fallback that outranks something
// written down is not a fallback, it is a surprise. By the time this runs the
// caller has asked for something no explicit mechanism recognises, and the only
// question left is which provider offers an id by that name.
func (c *Config) resolveBare(name string) (string, bool) {
	claims := c.bare[name]
	if len(claims) == 0 {
		return "", false
	}
	return claims[0].served, true
}

// BareClaims reports what an unprefixed name would resolve to, strongest first.
// For the dashboard, so "why did `Qwen3.8-27B` go there" is answerable without
// reading the config.
func (c *Config) BareClaims(name string) []string {
	out := make([]string, 0, len(c.bare[name]))
	for _, cl := range c.bare[name] {
		out = append(out, cl.served)
	}
	return out
}

// warnLegacyTopLevelModels tells an operator their models are in the retired
// place, once, at load.
//
// They still work: a top-level `models:` entry is a declared model exactly as
// before, unprefixed and unowned. Refusing to load would strand anyone who
// upgrades before editing, and silently rewriting names would be far worse —
// every caller asking for `Qwen3.8-27B` would start getting a 404 with nothing
// on screen explaining why.
func warnLegacyTopLevelModels(n int) {
	if n == 0 {
		return
	}
	slog.Warn("top-level `models:` is the retired shape — these models have no provider and no prefix; move them under `providers.<name>.models` and add `aliases:` for their old names to keep existing callers working",
		"count", n)
}
