package config

import "sort"

// Model selection: the models an operator chose off a provider's directory, and
// where they put them.
//
// This replaced an approval gate, and dropping the gate is the point. The old
// shape asked a QUESTION — is this discovered model allowed to serve? — with
// three answers and a queue, switched on per credential by `approvalRequired`.
// Against a provider like OpenRouter, which lists four hundred models, a queue
// of questions is not a feature; it is a chore nobody agreed to. The thing
// people actually want is to look at what a provider offers and say "that one,
// in this lane, at this priority".
//
// So a selection is a STATEMENT, not a verdict, and it does two jobs at once:
//
//	enrolment  a selection carrying Upstream contributes the model. Nothing
//	           else knows the provider's own id — ServedName is lossy — so this
//	           row is the only reason the model exists.
//	placement  Lanes/Quality say where it sits. A selection with no Upstream is
//	           placement alone, for a model a discover filter already
//	           contributes: "I did not choose this one, but I do choose where
//	           it goes."
//
// Selections share the discovered overlay rather than sitting beside it,
// because every downstream reader — the served registry, residency, eviction,
// the walk — already knows that overlay and would each need a second case.
// What distinguishes a selection is only its ORIGIN, and no reader acts on
// origin.

// Selection is one chosen model on one credential.
type Selection struct {
	Provider   string
	Credential string
	// Model is the served name (see ServedName), the id every other layer uses.
	Model string
	// Upstream is the provider's own id, set when this selection is what makes
	// the model exist. Empty means the row carries placement only.
	Upstream string
	// Lanes is where it sits, and at what position in each lane's ladder.
	Lanes []LanePlacement
	// Quality is the operator's rank. Zero falls back to the provider's
	// discovery template, then to defaultSelectionQuality.
	Quality float64
}

// LanePlacement is one lane a model was put in, and where in that lane's
// ladder it sits. Mirrors store.LaneRef, kept here so internal/config does not
// depend on the store.
type LanePlacement struct {
	Lane  string
	Order int
}

// defaultSelectionQuality is the rank a selection gets when neither the
// operator nor a discovery template supplies one. Quality is a routing key, and
// leaving it at zero would sort a hand-chosen model below everything on the box
// — the opposite of what choosing it meant.
const defaultSelectionQuality = 3

// SetSelections installs the selection set and applies it immediately.
//
// Called whenever a selection changes and once at startup, with the full set
// each time rather than a delta: the caller already holds every row, and a
// delta protocol here would be a second source of truth for which models exist.
func (c *Config) SetSelections(sel []Selection) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.selections = sel
	c.rebuildDiscoveredLocked()
}

// applySelectionsLocked contributes every selection to the discovered overlay.
// Called only from rebuildDiscoveredLocked, which has just cleared it.
//
// Requires c.mu held for writing. Reads c.Extensions without a lock, as every
// other reader does: the extension tree is immutable after load, and a reload
// builds a whole new Config rather than mutating this one.
func (c *Config) applySelectionsLocked() {
	if len(c.selections) == 0 {
		return
	}
	if c.discovered == nil {
		c.discovered = map[string]Model{}
	}
	if c.discoveredBy == nil {
		c.discoveredBy = map[string]map[string]bool{}
	}
	for _, s := range c.selections {
		if s.Upstream == "" {
			continue // placement only; there is nothing to contribute
		}
		// A declared model always wins, exactly as in SetDiscoveredFor:
		// choosing something the operator already wrote down must not redefine
		// it.
		if _, static := c.Models[s.Model]; static {
			continue
		}
		m, ok := c.modelForSelection(s)
		if !ok {
			continue // provider went away under it; the row is inert, not fatal
		}
		c.discovered[s.Model] = m
		if c.discoveredBy[s.Model] == nil {
			c.discoveredBy[s.Model] = map[string]bool{}
		}
		c.discoveredBy[s.Model][s.Credential] = true
	}
}

// modelForSelection builds the served model a selection stands for: the
// provider's discovery template if it has one (so a selected and a discovered
// model from the same provider are shaped identically), otherwise a plain chat
// model.
func (c *Config) modelForSelection(s Selection) (Model, bool) {
	extName, pv, ok := c.findProvider(s.Provider)
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
	if s.Quality > 0 {
		m.Quality = s.Quality
	} else if m.Quality == 0 {
		m.Quality = defaultSelectionQuality
	}
	m.Extension, m.ProviderName = extName, s.Provider
	m.Proxy = pv.Proxy
	m.Upstream = s.Upstream
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

// selectedLaneMembers returns the candidates a lane gained through selection,
// sorted by the priority chosen when each was placed.
//
// This is the per-model half of lane assignment that a selector cannot express:
// a selector says "everything this provider offers", while a selection says
// "this model, in these lanes, at this position".
func (c *Config) selectedLaneMembers(lane string) []Candidate {
	c.mu.RLock()
	type hit struct {
		name    string
		order   int
		quality float64
	}
	var hits []hit
	for _, s := range c.selections {
		for _, lp := range s.Lanes {
			if lp.Lane == lane {
				hits = append(hits, hit{name: s.Model, order: lp.Order, quality: s.Quality})
			}
		}
	}
	c.mu.RUnlock()

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].order != hits[j].order {
			return hits[i].order < hits[j].order
		}
		return hits[i].name < hits[j].name
	})

	seen := map[string]bool{}
	out := make([]Candidate, 0, len(hits))
	for _, h := range hits {
		// One served name can be selected on several credentials; the lane
		// wants it once, and expandCredentials fans it back out across the
		// accounts that can serve it.
		if seen[h.name] {
			continue
		}
		seen[h.name] = true
		m, ok := c.Models[h.name]
		if !ok {
			c.mu.RLock()
			m, ok = c.discovered[h.name]
			c.mu.RUnlock()
		}
		if !ok {
			continue // selected, then churned away entirely
		}
		if h.quality > 0 {
			// The operator's rank replaces the discovery template's uniform
			// guess, which p16 flagged as "an ASSUMPTION applied to every
			// discovered model, and a wrong one".
			m.Quality = h.quality
		}
		out = append(out, Candidate{Name: h.name, Model: m})
	}
	return out
}

// SelectedUpstream returns the provider id a served name was selected as, if it
// was selected at all. Lets a caller tell a hand-chosen model from a discovered
// one without exposing the selection list.
func (c *Config) SelectedUpstream(model string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, s := range c.selections {
		if s.Model == model && s.Upstream != "" {
			return s.Upstream, true
		}
	}
	return "", false
}
