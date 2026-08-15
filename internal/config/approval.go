package config

import "sort"

// LanePlacement is one lane a model was approved into, and where in that lane's
// ladder it sits. Mirrors store.LaneRef, kept here so internal/config does not
// depend on the store.
type LanePlacement struct {
	Lane  string
	Order int
}

// ApprovalView is the routing-relevant half of a decision.
type ApprovalView struct {
	State   string
	Lanes   []LanePlacement
	Quality float64
}

// Approval states, duplicated from the store for the same
// no-dependency reason.
const (
	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalRejected = "rejected"
)

// SetApprovals installs the decision table, keyed provider/credential/model.
//
// Empty means "no approval policy in force", which is every deployment that has
// not opted in: nothing is gated and discovery behaves exactly as it did. The
// gate turns on per credential via approvalRequired, not globally, so adding a
// paid key does not suddenly hide a working free roster.
func (c *Config) SetApprovals(v map[string]ApprovalView) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.approvals = v
}

// ApprovalKey identifies one decision.
func ApprovalKey(provider, credential, model string) string {
	return provider + "\x00" + credential + "\x00" + model
}

// approvalFor returns the decision for a candidate, and whether one exists.
func (c *Config) approvalFor(provider, credential, model string) (ApprovalView, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.approvals[ApprovalKey(provider, credential, model)]
	return v, ok
}

// servesUnderApproval reports whether a candidate may serve.
//
// Only DISCOVERED models are gated: a declared model is already an operator
// decision, and asking someone to approve what they just wrote down would be
// theatre. A credential that does not require approval serves everything its
// catalogue offers, which keeps a free roster working untouched.
func (c *Config) servesUnderApproval(cand Candidate) bool {
	if cand.Credential == nil || !cand.Credential.ApprovalRequired {
		return true
	}
	c.mu.RLock()
	_, discovered := c.discovered[cand.Name]
	c.mu.RUnlock()
	if !discovered {
		return true
	}
	v, ok := c.approvalFor(cand.Model.ProviderName, cand.Credential.Name, cand.Name)
	return ok && v.State == ApprovalApproved
}

// approvedLaneMembers returns candidates a lane gained through approval, sorted
// by the order chosen at approval time.
//
// This is the per-model half of lane assignment that a selector cannot express:
// a selector says "everything this provider offers", while an approval says
// "this model, in these lanes, at this position".
func (c *Config) approvedLaneMembers(lane string) []Candidate {
	c.mu.RLock()
	type hit struct {
		name  string
		order int
		view  ApprovalView
	}
	var hits []hit
	for key, v := range c.approvals {
		if v.State != ApprovalApproved {
			continue
		}
		for _, lp := range v.Lanes {
			if lp.Lane != lane {
				continue
			}
			_, _, model := splitApprovalKey(key)
			hits = append(hits, hit{name: model, order: lp.Order, view: v})
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
		// One served name can be approved on several credentials; the lane wants
		// it once, and expandCredentials fans it back out across the accounts
		// that can serve it.
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
			continue // approved then churned away entirely
		}
		if h.view.Quality > 0 {
			// The operator's rank replaces the discovery template's uniform
			// guess, which p16 flagged as "an ASSUMPTION applied to every
			// discovered model, and a wrong one".
			m.Quality = h.view.Quality
		}
		out = append(out, Candidate{Name: h.name, Model: m})
	}
	return out
}

func splitApprovalKey(k string) (provider, credential, model string) {
	first := -1
	second := -1
	for i := 0; i < len(k); i++ {
		if k[i] == 0 {
			if first < 0 {
				first = i
			} else {
				second = i
				break
			}
		}
	}
	if first < 0 || second < 0 {
		return "", "", k
	}
	return k[:first], k[first+1 : second], k[second+1:]
}
