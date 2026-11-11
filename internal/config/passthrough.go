package config

import (
	"sort"
	"strings"
)

// Passthrough opts a provider in to serving every model its /v1/models lists,
// as `<provider>-<id>`, without writing each one down. For an upstream gateway
// (a corp OpenRouter or LiteLLM) whose catalogue is the list of what may be used.
//
// The rows join the discovered overlay, so a declared or selected model of the
// same name wins: that is how one model gets settings of its own.
type Passthrough struct {
	// Template shapes every passthrough model: type, maxConcurrent,
	// scheduler.maxQueueDepth / maxWait, and a price for rows the catalogue does
	// not price.
	Template Model `yaml:"template,omitempty"`
}

// Price is a model's own per-token rates in USD per million tokens. It beats the
// rates of the model's type (commandCosts). Cached falls back to input.
type Price struct {
	InputPerM  float64 `yaml:"inputPerM,omitempty"`
	OutputPerM float64 `yaml:"outputPerM,omitempty"`
	CachedPerM float64 `yaml:"cachedPerM,omitempty"`
}

// ProviderGatePrefix marks a scheduler key that is a provider-wide gate, not a
// model.
const ProviderGatePrefix = "provider:"

// ProviderGateKey is the scheduler key of one provider's gate.
func ProviderGateKey(extension, provider string) string {
	return ProviderGatePrefix + extension + "/" + provider
}

// ProviderGate reports the provider-wide gate a model's requests pass, when its
// provider sets maxConcurrent. queues is true when the provider sets its own
// queue (scheduler): a request at the cap then waits in that line, whatever the
// group's saturation stage, unless the group spills to a fallback.
func (c *Config) ProviderGate(m Model) (key string, capacity int, queues, ok bool) {
	pv, ok := c.providerOf(m.Extension, m.ProviderName)
	if !ok || pv.MaxConcurrent <= 0 {
		return "", 0, false, false
	}
	return ProviderGateKey(m.Extension, m.ProviderName), pv.MaxConcurrent, pv.Scheduler != nil, true
}

// ProviderSchedulerFor resolves a provider gate's queue bounds: the provider's
// own where set, the box-wide values otherwise.
func (c *Config) ProviderSchedulerFor(key string) SchedulerConfig {
	out := c.Scheduler
	ext, pn, ok := strings.Cut(strings.TrimPrefix(key, ProviderGatePrefix), "/")
	if !ok {
		return out
	}
	pv, ok := c.providerOf(ext, pn)
	if !ok || pv.Scheduler == nil {
		return out
	}
	if pv.Scheduler.MaxWait != "" {
		out.MaxWait = pv.Scheduler.MaxWait
	}
	if pv.Scheduler.MaxQueueDepth > 0 {
		out.MaxQueueDepth = pv.Scheduler.MaxQueueDepth
	}
	return out
}

func (c *Config) providerOf(extension, provider string) (Provider, bool) {
	if extension == "" || provider == "" {
		return Provider{}, false
	}
	ext, ok := c.Extensions[extension]
	if !ok {
		return Provider{}, false
	}
	pv, ok := ext.Providers[provider]
	return pv, ok
}

// PassthroughTarget is one (provider, credential) catalogue to fetch.
type PassthroughTarget struct {
	Extension  string
	Provider   string
	Credential string
	Target     *ProxyTarget
}

// PassthroughTargets lists every catalogue the passthrough providers need.
func (c *Config) PassthroughTargets() []PassthroughTarget {
	var out []PassthroughTarget
	exts := make([]string, 0, len(c.Extensions))
	for n := range c.Extensions {
		exts = append(exts, n)
	}
	sort.Strings(exts)
	for _, en := range exts {
		ext := c.Extensions[en]
		provs := make([]string, 0, len(ext.Providers))
		for pn := range ext.Providers {
			provs = append(provs, pn)
		}
		sort.Strings(provs)
		for _, pn := range provs {
			pv := ext.Providers[pn]
			if pv.Passthrough == nil || pv.Proxy.IsZero() {
				continue
			}
			base, err := (Model{Proxy: pv.Proxy}).ProxyTarget()
			if err != nil {
				continue
			}
			for _, cr := range pv.CredentialList() {
				t := *base
				if len(cr.Headers) > 0 || cr.AuthTokenCommand != "" {
					t.Headers = cr.ResolvedHeaders(base.Headers)
					if cr.AuthTokenCommand != "" {
						t.AuthTokenCommand = cr.AuthTokenCommand
					}
				}
				out = append(out, PassthroughTarget{Extension: en, Provider: pn, Credential: cr.Name, Target: &t})
			}
		}
	}
	return out
}

// CatalogRow is what a passthrough model is built from: one /v1/models row.
// Prices are USD per token, as the catalogue reports them; 0 is "not published"
// unless Free.
type CatalogRow struct {
	ID            string
	Free          bool
	PromptUSD     float64
	CompletionUSD float64
}

// PassthroughModelFor builds the served model one catalogue row becomes. ok is
// false when the row has no price and the provider spends against a USD limit:
// an unpriced model there would spend money no limit could see.
func (c *Config) PassthroughModelFor(t PassthroughTarget, row CatalogRow) (string, Model, bool) {
	pv, ok := c.providerOf(t.Extension, t.Provider)
	if !ok || pv.Passthrough == nil {
		return "", Model{}, false
	}
	m := pv.Passthrough.Template // value copy: the template is never mutated
	if m.Type == "" {
		m.Type = "chat"
	}
	m.Extension, m.ProviderName = t.Extension, t.Provider
	m.Proxy = pv.Proxy
	m.SessionAffinityHeader = pv.SessionAffinityHeader
	m.Upstream = row.ID
	switch {
	case row.Free:
		m.Price = &Price{}
	case row.PromptUSD > 0 || row.CompletionUSD > 0:
		m.Price = &Price{InputPerM: row.PromptUSD * 1e6, OutputPerM: row.CompletionUSD * 1e6}
	case m.Price != nil:
		p := *m.Price // the template's, copied so no row shares it
		m.Price = &p
	default:
		if pv.spendsAgainstUSD() && !c.typePriced(m.Type) {
			return "", Model{}, false
		}
	}
	return ServedName(t.Provider, row.ID), m, true
}

// spendsAgainstUSD reports a USD limit on the provider or any of its credentials.
func (pv Provider) spendsAgainstUSD() bool {
	has := func(ls LimitSet) bool {
		for _, l := range ls {
			if l.USD > 0 {
				return true
			}
		}
		return false
	}
	if has(pv.Limits) {
		return true
	}
	for _, cr := range pv.Credentials {
		if has(cr.Limits) {
			return true
		}
	}
	return false
}

// typePriced reports a type whose commandCosts name a per-token rate.
func (c *Config) typePriced(typ string) bool {
	params, ok := c.CommandCosts[typ]
	if !ok {
		return false
	}
	extract, ok := params["extract"].(map[string]any)
	if !ok {
		return false
	}
	for _, k := range []string{"costFactor", "processCostFactor", "generateCostFactor", "cachedCostFactor"} {
		switch v := extract[k].(type) {
		case float64:
			if v > 0 {
				return true
			}
		case int:
			if v > 0 {
				return true
			}
		}
	}
	return false
}

// ServedModel returns a served name's model, declared or from the overlay.
func (c *Config) ServedModel(name string) (Model, bool) {
	if m, ok := c.Models[name]; ok {
		return m, true
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	m, ok := c.discovered[name]
	return m, ok
}

// PassthroughServed counts the models a provider's passthrough serves now,
// across its credentials. 0 before the first refresh.
func (c *Config) PassthroughServed(provider string) int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	names := map[string]bool{}
	for key, con := range c.fetched {
		if !strings.HasPrefix(key, "provider\x00"+provider+"\x00") {
			continue
		}
		for n := range con.models {
			if _, declared := c.Models[n]; !declared {
				names[n] = true
			}
		}
	}
	return len(names)
}
