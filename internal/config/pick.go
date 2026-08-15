package config

// Manual enrolment: models an operator chose from a provider's catalogue by
// hand, rather than ones a discovery filter admitted.
//
// Why this exists. Discovery is a FILTER over a remote catalogue — free-only,
// text-to-text, minimum context, top N by window — and everything it rejects is
// dropped in memory and never named again. That is the right default for a
// churning free roster, and it is useless for "I want that one specific model":
// there was no way to reach a model the filter excluded except to loosen the
// filter and admit a hundred others with it.
//
// A pick is the escape hatch. It contributes exactly one model, from one
// credential's catalogue, and it survives the next refresh — SetDiscoveredFor
// replaces a credential's contribution wholesale, so without re-application a
// hand-picked model would vanish within the refresh interval and look like the
// provider had dropped it.
//
// Picks live in the SAME overlay as discovered models rather than beside it,
// because everything downstream — approval gating, lane placement, the served
// registry, eviction — already knows that overlay and would each need a second
// case otherwise. What distinguishes a pick is only its ORIGIN, and origin is
// not something any of those readers act on.

// Pick is one hand-chosen model on one credential.
type Pick struct {
	Provider   string
	Credential string
	// Model is the served name (see ServedName), the id every other layer uses.
	Model string
	// Upstream is the provider's own id, which is what actually goes on the
	// wire. Kept explicitly because ServedName is lossy — it drops ":free" and
	// rewrites "/" — so the served name cannot be turned back into it.
	Upstream string
	// Quality is the operator's rank. Zero falls back to the provider's
	// discovery template, then to defaultPickQuality.
	Quality float64
}

// defaultPickQuality is the rank a pick gets when neither the operator nor a
// discovery template supplies one. It matches the uniform guess a discovery
// template carries: quality is a routing key, and leaving it at zero would sort
// a hand-picked model below everything on the box — the opposite of what
// picking it meant.
const defaultPickQuality = 3

// SetPicks installs the manual enrolment set and applies it immediately.
//
// Called whenever a decision changes and once at startup, with the full set
// each time rather than a delta: the caller already holds every row, and a
// delta protocol here would be a second source of truth for which models exist.
func (c *Config) SetPicks(picks []Pick) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.picks = picks
	c.applyPicksLocked("", "")
}

// applyPicksLocked contributes picks to the discovered overlay. An empty
// provider/credential means "all of them"; naming one restricts the work to the
// credential a refresh just replaced.
//
// Requires c.mu held for writing. Reads c.Extensions without a lock, as every
// other reader does: the extension tree is immutable after load, and a reload
// builds a whole new Config rather than mutating this one.
func (c *Config) applyPicksLocked(provider, credential string) {
	if len(c.picks) == 0 {
		return
	}
	if c.discovered == nil {
		c.discovered = map[string]Model{}
	}
	if c.discoveredBy == nil {
		c.discoveredBy = map[string]map[string]bool{}
	}
	for _, p := range c.picks {
		if provider != "" && (p.Provider != provider || p.Credential != credential) {
			continue
		}
		// A declared model always wins, exactly as in SetDiscoveredFor: picking
		// something the operator already wrote down must not redefine it.
		if _, static := c.Models[p.Model]; static {
			continue
		}
		m, ok := c.modelForPick(p)
		if !ok {
			continue // provider went away under the pick; the row is inert, not fatal
		}
		c.discovered[p.Model] = m
		if c.discoveredBy[p.Model] == nil {
			c.discoveredBy[p.Model] = map[string]bool{}
		}
		c.discoveredBy[p.Model][p.Credential] = true
	}
}

// modelForPick builds the served model a pick stands for: the provider's
// discovery template if it has one (so a pick and a discovered model from the
// same provider are shaped identically), otherwise a plain chat model.
func (c *Config) modelForPick(p Pick) (Model, bool) {
	extName, pv, ok := c.findProvider(p.Provider)
	if !ok {
		return Model{}, false
	}
	var m Model
	if pv.Discover != nil {
		m = pv.Discover.Template // value copy; the template is never mutated
	}
	if m.Type == "" {
		m.Type = "chat"
	}
	if p.Quality > 0 {
		m.Quality = p.Quality
	} else if m.Quality == 0 {
		m.Quality = defaultPickQuality
	}
	m.Extension, m.ProviderName = extName, p.Provider
	m.Proxy = pv.Proxy
	m.Upstream = p.Upstream
	return m, true
}

// findProvider locates a provider by name across extensions. Provider names are
// unique in practice but not enforced to be; first match by sorted extension
// name keeps the answer stable rather than map-order random.
func (c *Config) findProvider(provider string) (string, Provider, bool) {
	best := ""
	var bestPV Provider
	for en, ext := range c.Extensions {
		pv, ok := ext.Providers[provider]
		if !ok {
			continue
		}
		if best == "" || en < best {
			best, bestPV = en, pv
		}
	}
	return best, bestPV, best != ""
}

// PickedUpstream returns the provider id a served name was picked as, if it was
// picked at all. Lets a caller tell a hand-enrolled model from a discovered one
// without exposing the pick list.
func (c *Config) PickedUpstream(model string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, p := range c.picks {
		if p.Model == model {
			return p.Upstream, true
		}
	}
	return "", false
}
