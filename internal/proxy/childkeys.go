package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/quota"
	"github.com/iodesystems/corrallm/internal/store"
)

// Child keys: a configured key mints keys of its own (POST /v1/keys) and gives
// each a share of its allocation. aw4 holds one key; each of its projects gets
// a child with, say, $1/day on deepseek.
//
// A child RUNS AS ITS PARENT — group, escalation, thinking default, credential
// allowlists — so it can never reach anything the parent cannot. It is
// RECORDED AS ITSELF: activity rows, the dashboard's per-key split and its own
// allocation all use the child key. Every request is charged to the child's
// scopes AND the parent's, so the parent's allocation bounds all of its
// children together. Shares may add up to more than the parent has
// (oversubscription, owner 10-24): an idle project strands nothing, and the
// parent's own limit is what stops the total.
//
// One level: a child cannot mint. Stored in child_key, not the config, so a
// client minting a key per project adds no config revisions and a config
// restore cannot delete keys in use.
//
// Minting is a PERMISSION (P62): KeyPolicy.Mint for a configured key, CanMint
// for one an admin issued. The same table holds those admin-issued keys
// (parent ""), which run under their own group and credentials, and every
// minted key may carry an expiry, after which it answers 401.

// maxChildKeys bounds one parent's roster. Children cost nothing to the
// budget, but each is a row and a ledger scope.
const maxChildKeys = 1000

type childKey struct {
	store.ChildKey
	limits     config.LimitSet
	keyCreds   []config.KeyCredential // allow patterns; empty = every credential
}

// expired reports whether k's expiry has passed at now.
func (k childKey) expired(now time.Time) bool {
	return k.ExpiresMS > 0 && now.UnixMilli() >= k.ExpiresMS
}

// childRoster is the in-memory copy of child_key, read on every request.
type childRoster struct {
	mu    sync.RWMutex
	byKey map[string]childKey
}

// issued is the roster as config.ResolveCallerWith reads it.
func (c *childRoster) issued(key string) (config.IssuedKey, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	k, ok := c.byKey[key]
	if !ok {
		return config.IssuedKey{}, false
	}
	return config.IssuedKey{Parent: k.Parent,
		Policy: config.KeyPolicy{Group: k.Group, Mint: k.CanMint, Limits: k.limits,
			Credentials: k.keyCreds, Disabled: k.Disabled}}, true
}

func (c *childRoster) get(key string) (childKey, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	k, ok := c.byKey[key]
	return k, ok
}

func (c *childRoster) put(k childKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byKey == nil {
		c.byKey = map[string]childKey{}
	}
	c.byKey[k.Key] = k
}

func (c *childRoster) remove(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.byKey, key)
}

// of lists a parent's children, oldest first; "" lists every child.
func (c *childRoster) of(parent string) []childKey {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []childKey
	for _, k := range c.byKey {
		if parent == "" || k.Parent == parent {
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedMS != out[j].CreatedMS {
			return out[i].CreatedMS < out[j].CreatedMS
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// loadChildKeys fills the roster from the store. A row whose limits no longer
// parse is kept with no limits and logged: refusing to start over one bad row
// would take every caller down with it.
func (p *Proxy) loadChildKeys() {
	if p.store == nil {
		return
	}
	rows, err := p.store.LoadChildKeys()
	if err != nil {
		slog.Warn("child keys: load failed", "err", err)
		return
	}
	for _, r := range rows {
		k := childKey{ChildKey: r}
		if r.Limits != "" {
			if err := json.Unmarshal([]byte(r.Limits), &k.limits); err != nil {
				slog.Warn("child keys: unreadable limits, serving with none", "parent", r.Parent, "label", r.Label, "err", err)
			}
		}
		if r.Credentials != "" {
			if err := json.Unmarshal([]byte(r.Credentials), &k.keyCreds); err != nil {
				slog.Warn("child keys: unreadable allow patterns, allowing none", "label", r.Label, "err", err)
			}
		}
		p.children.put(k)
	}
}

// ChildKeyInfo is what the key roster shows of a child key.
type ChildKeyInfo struct{ Parent, Label string }

// ChildKeys maps every child key to its parent and label, for the key roster.
func (p *Proxy) ChildKeys() map[string]ChildKeyInfo {
	out := map[string]ChildKeyInfo{}
	for _, k := range p.children.of("") {
		out[k.Key] = ChildKeyInfo{Parent: k.Parent, Label: k.Label}
	}
	return out
}

// resolveCaller is config.ResolveCaller, knowing about minted keys.
func (p *Proxy) resolveCaller(cred string) config.Caller {
	return p.config().ResolveCallerWith(cred, p.children.issued)
}

// canMint reports whether key may mint child keys: a configured key granted
// `mint`, or an admin-issued key granted it. Never a child.
func (p *Proxy) canMint(c config.Caller) bool {
	if c.Parent != "" || !c.Recognized {
		return false
	}
	if k, ok := p.children.get(c.Key); ok {
		return k.Parent == "" && k.CanMint
	}
	return p.config().Keys[c.Key].Mint
}

// credentialVerdict decides one candidate's credential for this caller: whether
// the caller may forward with it at all, and WHICH allow pattern admitted it —
// with the key that owns that pattern — so the gate and the charge name the same
// budget scope.
//
// Every policy in the caller's chain must admit the credential: a child narrows
// what its parent may spend, never widens it. No patterns admits everything,
// with no budget of its own — what every key did before patterns existed.
//
// A credential's own legacy `allow:` list is still enforced as an ADDITIONAL
// restriction. It is the pre-patterns way of saying who may spend an account,
// and it is on its way out — the dashboard writes patterns now — but a config
// carrying one must not start serving callers down to nobody on upgrade, so
// dropping it would be a silent widening of access. Kept here rather than
// migrated on load: a migration would have to invent a pattern per key per
// credential and rewrite a rule the operator can still read.
//
// The winning pattern is the most specific one across the whole chain, and it
// belongs to the key that declared it (the child, if the child declares one):
// a budget is kept on the key whose entry spells it out.
func (p *Proxy) credentialVerdict(c config.Caller, cand config.Candidate) credentialAccess {
	v := credentialAccess{allowed: true}
	if cand.Credential == nil {
		return v
	}
	scope := cand.Credential.ScopeKey(cand.Model.ProviderName)
	for _, k := range callerChain(c) {
		pats := p.keyCredentials(k)
		pat, matched := config.MatchCredential(pats, scope)
		if len(pats) > 0 && !matched {
			return credentialAccess{}
		}
		if matched && (v.pattern == "" || config.CredentialPatternWins(pat.Credential, v.pattern)) {
			v.pattern, v.limits, v.owner = pat.Credential, pat.Limits, k
		}
	}
	if !cand.Credential.AllowsKey(c.PolicyKey()) {
		return credentialAccess{}
	}
	return v
}

// credentialAccess is one candidate's credential verdict: permitted or not, and
// which pattern budget (if any) admitted it.
type credentialAccess struct {
	allowed bool
	pattern string // the winning pattern; "" when none matched
	limits  config.LimitSet
	owner   string // the key whose entry declares the winning pattern
}

// scope is the ledger scope the winning pattern's budget is kept under.
func (v credentialAccess) scope() string {
	if v.pattern == "" {
		return ""
	}
	return config.KeyCredentialScopeKey(v.owner, v.pattern)
}

// keyCredentials is one key's allow patterns, from whichever source holds it.
func (p *Proxy) keyCredentials(key string) []config.KeyCredential {
	if ck, ok := p.children.get(key); ok {
		return ck.keyCreds
	}
	return p.config().Keys[key].Credentials
}

// credentialScopesFor is the allow-pattern scopes one SERVED request is charged
// to: the winning pattern under the key that declares it. Empty when that
// pattern carries no budget, and empty when the caller has no patterns at all —
// nothing extra to count.
func (p *Proxy) credentialScopesFor(c config.Caller, cand config.Candidate) []string {
	v := p.credentialVerdict(c, cand)
	if !v.allowed || v.pattern == "" || len(v.limits) == 0 {
		return nil
	}
	return []string{v.scope()}
}

// childKeyPrefix marks a minted key, so a deleted one can be told from a
// stranger.
const childKeyPrefix = "ck-"

// revokedKey says why a minted credential is refused, or "". By default
// corrallm serves any key, so without this a DELETED minted key would keep
// working as a stranger — revoking it would do nothing — a child whose parent
// left the config would run uncapped, and an expiry would mean nothing.
func (p *Proxy) revokedKey(c config.Caller) string {
	now := time.Now()
	k, minted := p.children.get(c.Key)
	switch {
	case !minted && strings.HasPrefix(c.Key, childKeyPrefix):
		return "this key was deleted or never existed"
	case c.Parent != "" && !c.Recognized:
		return "this child key's parent is no longer configured"
	case minted && k.Disabled:
		return "this key is switched off"
	case minted && k.expired(now):
		return "this key expired at " + time.UnixMilli(k.ExpiresMS).UTC().Format(time.RFC3339)
	case c.Disabled:
		return "this key is switched off"
	}
	if c.Parent != "" {
		if pk, ok := p.children.get(c.Parent); ok && pk.Disabled {
			return "the key that made this one is switched off"
		} else if ok && pk.expired(now) {
			return "this key's parent expired at " + time.UnixMilli(pk.ExpiresMS).UTC().Format(time.RFC3339)
		}
	}
	return ""
}

// callerChain is every key whose allocation a request answers to: its own,
// and its parent's.
func callerChain(c config.Caller) []string {
	if c.Parent != "" {
		return []string{c.Key, c.Parent}
	}
	return []string{c.Key}
}

// keyBlocked reports whether key's own hard allocation refuses a request
// model would serve — its key-wide windows, or its share of model ("" checks
// the key-wide windows only) — ignoring value adds, and the wait.
func (p *Proxy) keyBlocked(key, model string) (bool, time.Duration) {
	full, wait := false, time.Duration(0)
	scopes := []string{config.KeyScopeKey(key)}
	if model != "" {
		scopes = append(scopes, config.KeyModelScopeKey(key, model))
	}
	for _, sc := range scopes {
		if b, d := p.quota.HardBlocked(sc); b {
			full = true
			wait = max(wait, d)
		}
	}
	return full, wait
}

// callerBlocked reports whether a key in the caller's chain has a full
// key-wide hard allocation that no value add of its own can cover, and the
// longest wait among them. A model-bound value add counts here: which model
// serves is not decided yet, and filterByCallerShare checks it per candidate.
func (p *Proxy) callerBlocked(c config.Caller) (bool, time.Duration) {
	full, wait := false, time.Duration(0)
	for _, k := range callerChain(c) {
		if b, d := p.keyBlocked(k, ""); b && !p.grants.any(k) {
			full = true
			wait = max(wait, d)
		}
	}
	return full, wait
}

// filterByCallerShare drops candidates that a key in the caller's chain may
// not spend on — its key-wide allocation or its share of that model is full,
// and no value add of its own covers that model — so a lane can still serve
// from another member.
func (p *Proxy) filterByCallerShare(cands []config.Candidate, c config.Caller) ([]config.Candidate, time.Duration) {
	if p.quota == nil {
		return cands, 0
	}
	kept := cands[:0:0]
	wait := time.Duration(0)
	for _, cand := range cands {
		ok := true
		for _, k := range callerChain(c) {
			if b, d := p.keyBlocked(k, cand.Name); b && !p.grants.usable(k, cand.Name) {
				ok = false
				if wait == 0 || d < wait {
					wait = d
				}
			}
		}
		if ok {
			kept = append(kept, cand)
		}
	}
	return kept, wait
}

// chargeCaller charges one served request to every allocation it answers to.
// A key whose own allocation was already full pays the cost from its value
// adds — that is the only way it was admitted — decided before this request
// is added to its windows.
//
// credLimits are the allow pattern's own budgets, when the pattern that
// admitted this request carries any (filterByCredential returned them): they
// are charged beside the key's allocation, under the key-and-pattern scope the
// gate checked, so a pattern that says "$2/day on this account" is counted
// separately from one that says "$20/day on every account".
func (p *Proxy) chargeCaller(c config.Caller, model string, costUSD float64, credScopes []string) {
	if p.quota == nil {
		return
	}
	for _, k := range callerChain(c) {
		if b, _ := p.keyBlocked(k, model); b && costUSD > 0 {
			p.grants.spend(p.store, k, model, costUSD)
		}
		for _, sc := range []string{config.KeyScopeKey(k), config.KeyModelScopeKey(k, model)} {
			p.quota.Charge(sc, config.DimRequests, 1)
			if costUSD > 0 {
				p.quota.Charge(sc, config.DimUSD, costUSD)
			}
		}
	}
	// A pattern budget belongs to the key whose pattern it is. Charging it for
	// every key in the chain would make a child's requests fill its parent's
	// pattern twice over.
	for _, sc := range credScopes {
		p.quota.Charge(sc, config.DimRequests, 1)
		if costUSD > 0 {
			p.quota.Charge(sc, config.DimUSD, costUSD)
		}
	}
}

// seedCallerLimits registers one key's allocation: the key-wide limits under
// its key scope, each model's share under its own, and each allow pattern's
// budget under the key-and-pattern scope the gate checks.
func seedCallerLimits(set func(string, []config.Limit), key string, ls config.LimitSet, creds []config.KeyCredential) {
	all, byModel := ls.ByModel()
	if len(all) > 0 {
		set(config.KeyScopeKey(key), all)
	}
	for m, ml := range byModel {
		set(config.KeyModelScopeKey(key, m), ml)
	}
	for _, pat := range creds {
		if len(pat.Limits) > 0 {
			set(config.KeyCredentialScopeKey(key, pat.Credential), pat.Limits)
		}
	}
}

// ---------------------------------------------------------------------------
// /v1/keys
// ---------------------------------------------------------------------------

// childKeyBody is a child key as the API shows it.
type childKeyBody struct {
	Key     string          `json:"key"`
	Label   string          `json:"label"`
	Limits  config.LimitSet `json:"limits"`
	Created string          `json:"created"`
	Updated string          `json:"updated,omitempty"`
	// Expires is when the key stops working (RFC 3339); empty = never.
	Expires string `json:"expires,omitempty"`
	// Disabled mirrors the admin switch: a key created off answers 401 until it
	// is switched on, and says so here rather than looking broken.
	Disabled bool `json:"disabled"`
	// Windows is the allocation's live state: each limit's used and limit,
	// keyed by scope ("key" or the model name).
	Windows map[string][]quota.Window `json:"windows,omitempty"`
}

// childKeyInput is what POST and PATCH accept. PATCH replaces each field that
// is present; limits: [] removes them all. Expiry: `ttl` ("48h", "7d") from
// now, or `expires` (RFC 3339); "" for either clears it.
type childKeyInput struct {
	Label    *string                 `json:"label"`
	Limits   *config.LimitSet        `json:"limits"`
	TTL      *string                 `json:"ttl"`
	Expires  *string                 `json:"expires"`
	Disabled *bool                   `json:"disabled"`
	Patterns *[]config.KeyCredential `json:"credentials"`
}

// expiryFrom reads an input's ttl/expires into unix millis; set is false when
// neither is present.
func expiryFrom(ttl, expires *string, now time.Time) (ms int64, set bool, err error) {
	switch {
	case ttl != nil && expires != nil:
		return 0, false, fmt.Errorf("send ttl or expires, not both")
	case ttl != nil:
		if strings.TrimSpace(*ttl) == "" {
			return 0, true, nil
		}
		d, err := ParseTTL(*ttl)
		if err != nil {
			return 0, false, err
		}
		return now.Add(d).UnixMilli(), true, nil
	case expires != nil:
		if strings.TrimSpace(*expires) == "" {
			return 0, true, nil
		}
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(*expires))
		if err != nil {
			return 0, false, fmt.Errorf("expires: want RFC 3339, e.g. 2026-11-01T00:00:00Z")
		}
		return t.UnixMilli(), true, nil
	}
	return 0, false, nil
}

// ParseTTL reads a duration that may also be written in days ("7d", "1d12h").
// Bounded to a year: a key meant to live longer needs no expiry.
func ParseTTL(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	var days time.Duration
	if i := strings.Index(s, "d"); i > 0 {
		n, err := strconv.Atoi(s[:i])
		if err != nil {
			return 0, fmt.Errorf("ttl %q: want e.g. 48h, 7d or 1d12h", s)
		}
		days, s = time.Duration(n)*24*time.Hour, s[i+1:]
	}
	var rest time.Duration
	if s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("ttl: want e.g. 48h, 7d or 1d12h")
		}
		rest = d
	}
	d := days + rest
	if d <= 0 || d > 366*24*time.Hour {
		return 0, fmt.Errorf("ttl must be more than 0 and at most 366d")
	}
	return d, nil
}

// handleKeys serves /v1/keys and /v1/keys/{key}.
//
//	GET    /v1/keys          this key's allocation and its children
//	POST   /v1/keys          mint a child: {label, limits}
//	GET    /v1/keys/{key}    one child
//	PATCH  /v1/keys/{key}    change its label and/or limits
//	DELETE /v1/keys/{key}    revoke it
//
// Authenticated by the caller's own key: only a configured key may manage
// keys, and only its own children.
func (p *Proxy) handleKeys(w http.ResponseWriter, r *http.Request) {
	caller := p.resolveCaller(callerKey(r))
	switch {
	case caller.Key == "":
		jsonErr(w, http.StatusUnauthorized, "send your corrallm key as Authorization: Bearer <key>")
		return
	case caller.Parent != "":
		jsonErr(w, http.StatusForbidden, "a child key cannot manage keys; use the key that minted it")
		return
	case p.revokedKey(caller) != "":
		jsonErr(w, http.StatusUnauthorized, p.revokedKey(caller))
		return
	case !p.canMint(caller):
		jsonErr(w, http.StatusForbidden, "this key may not mint child keys; ask for it with POST /v1/access-requests {\"mint\": true, ...}")
		return
	}
	parent := caller.Key
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/keys"), "/")
	if id == "" {
		switch r.Method {
		case http.MethodGet:
			p.listChildKeys(w, parent)
		case http.MethodPost:
			p.createChildKey(w, r, parent)
		default:
			w.Header().Set("Allow", "GET, POST")
			jsonErr(w, http.StatusMethodNotAllowed, "use GET or POST")
		}
		return
	}
	k, ok := p.children.get(id)
	if !ok || k.Parent != parent {
		jsonErr(w, http.StatusNotFound, "no such child key")
		return
	}
	switch r.Method {
	case http.MethodGet:
		jsonResp(w, http.StatusOK, p.childKeyView(k))
	case http.MethodPatch:
		p.updateChildKey(w, r, k)
	case http.MethodDelete:
		if err := p.store.DeleteChildKey(k.Key); err != nil {
			jsonErr(w, http.StatusInternalServerError, "delete: "+err.Error())
			return
		}
		p.children.remove(k.Key)
		p.dropGrants(k.Key)
		p.seedQuota(p.config())
		jsonResp(w, http.StatusOK, map[string]any{"deleted": k.Key})
	default:
		w.Header().Set("Allow", "GET, PATCH, DELETE")
		jsonErr(w, http.StatusMethodNotAllowed, "use GET, PATCH or DELETE")
	}
}

func (p *Proxy) listChildKeys(w http.ResponseWriter, parent string) {
	kids := p.children.of(parent)
	out := make([]childKeyBody, 0, len(kids))
	for _, k := range kids {
		out = append(out, p.childKeyView(k))
	}
	self := map[string]any{
		"limits":    p.config().Keys[parent].Limits,
		"windows":   p.allocationWindows(parent),
		"valueAdds": grantViews(p.grants.of(parent)),
	}
	if k, ok := p.children.get(parent); ok {
		self["limits"] = k.limits
		if k.ExpiresMS > 0 {
			self["expires"] = time.UnixMilli(k.ExpiresMS).UTC().Format(time.RFC3339)
		}
	}
	jsonResp(w, http.StatusOK, map[string]any{"self": self, "keys": out})
}

func (p *Proxy) createChildKey(w http.ResponseWriter, r *http.Request, parent string) {
	if p.store == nil {
		jsonErr(w, http.StatusServiceUnavailable, "child keys need the database")
		return
	}
	var in childKeyInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		jsonErr(w, http.StatusBadRequest, "body: "+err.Error())
		return
	}
	if len(p.children.of(parent)) >= maxChildKeys {
		jsonErr(w, http.StatusConflict, fmt.Sprintf("this key already has %d child keys; delete some first", maxChildKeys))
		return
	}
	var limits config.LimitSet
	if in.Limits != nil {
		limits = *in.Limits
	}
	if err := p.checkChildLimits(limits); err != nil {
		jsonErr(w, http.StatusBadRequest, "limits: "+err.Error())
		return
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		jsonErr(w, http.StatusInternalServerError, "mint: "+err.Error())
		return
	}
	now := time.Now().UnixMilli()
	// A child minted by a PARENT is live at once: the parent is the party that
	// decided, on its own allocation, and it hands the key straight to a project
	// that is waiting for it. The default-off rule belongs where a PERSON makes
	// a key for a caller they have not met yet (Proxy.CreateKey) — not here,
	// where making an agent mint a key and then switch it on is two steps to
	// reach the state it asked for.
	k := childKey{ChildKey: store.ChildKey{Key: childKeyPrefix + hex.EncodeToString(raw), Parent: parent,
		CreatedMS: now, UpdatedMS: now}, limits: limits}
	if in.Label != nil {
		k.Label = strings.TrimSpace(*in.Label)
	}
	exp, _, err := expiryFrom(in.TTL, in.Expires, time.Now())
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	k.ExpiresMS = exp
	if !p.saveChildKey(w, k) {
		return
	}
	jsonResp(w, http.StatusCreated, p.childKeyView(k))
}

func (p *Proxy) updateChildKey(w http.ResponseWriter, r *http.Request, k childKey) {
	var in childKeyInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		jsonErr(w, http.StatusBadRequest, "body: "+err.Error())
		return
	}
	if in.Label != nil {
		k.Label = strings.TrimSpace(*in.Label)
	}
	if in.Limits != nil {
		if err := p.checkChildLimits(*in.Limits); err != nil {
			jsonErr(w, http.StatusBadRequest, "limits: "+err.Error())
			return
		}
		k.limits = *in.Limits
	}
	if exp, set, err := expiryFrom(in.TTL, in.Expires, time.Now()); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	} else if set {
		k.ExpiresMS = exp
	}
	if in.Disabled != nil {
		k.Disabled = *in.Disabled
	}
	if in.Patterns != nil {
		if err := p.checkCredentials(*in.Patterns); err != nil {
			jsonErr(w, http.StatusBadRequest, "credentials: "+err.Error())
			return
		}
		k.keyCreds = clonePatterns(*in.Patterns)
	}
	k.UpdatedMS = time.Now().UnixMilli()
	if !p.saveChildKey(w, k) {
		return
	}
	jsonResp(w, http.StatusOK, p.childKeyView(k))
}

// checkChildLimits is ValidateKey plus: a model a limit names must be one this
// box serves, so a typo is refused instead of being a share nothing charges.
func (p *Proxy) checkChildLimits(ls config.LimitSet) error {
	if err := ls.ValidateKey(); err != nil {
		return err
	}
	for _, l := range ls {
		if _, ok := p.config().Models[l.Model]; l.Model != "" && !ok {
			return fmt.Errorf("no model %q (see GET /v1/models)", l.Model)
		}
	}
	return nil
}

// saveChildKey persists k, updates the roster and re-seeds the ledger. A
// window whose label survives an edit keeps its level, so changing a share
// does not refund what was already spent.
func (p *Proxy) saveChildKey(w http.ResponseWriter, k childKey) bool {
	if err := p.persistChildKey(k); err != nil {
		jsonErr(w, http.StatusInternalServerError, "save: "+err.Error())
		return false
	}
	return true
}

// persistChildKey writes k, updates the roster and re-seeds the ledger.
func (p *Proxy) persistChildKey(k childKey) error {
	k.Limits, k.Credentials = "", ""
	if len(k.limits) > 0 {
		b, _ := json.Marshal(k.limits)
		k.Limits = string(b)
	}
	if len(k.keyCreds) > 0 {
		b, _ := json.Marshal(config.ToKeyCredentials(k.keyCreds))
		k.Credentials = string(b)
	}
	if err := p.store.SaveChildKey(k.ChildKey); err != nil {
		return err
	}
	p.children.put(k)
	p.seedQuota(p.config())
	return nil
}

// dropGrants deletes a removed key's value adds.
func (p *Proxy) dropGrants(key string) {
	for _, g := range p.grants.dropKey(key) {
		if p.store != nil {
			if err := p.store.DeleteKeyGrant(g.ID); err != nil {
				slog.Warn("value add: delete failed", "grant", g.ID, "err", err)
			}
		}
	}
}

func (p *Proxy) childKeyView(k childKey) childKeyBody {
	out := childKeyBody{Key: k.Key, Label: k.Label, Limits: k.limits, Disabled: k.Disabled,
		Created: time.UnixMilli(k.CreatedMS).UTC().Format(time.RFC3339),
		Windows: p.allocationWindows(k.Key)}
	if out.Limits == nil {
		out.Limits = config.LimitSet{}
	}
	if k.UpdatedMS != k.CreatedMS {
		out.Updated = time.UnixMilli(k.UpdatedMS).UTC().Format(time.RFC3339)
	}
	if k.ExpiresMS > 0 {
		out.Expires = time.UnixMilli(k.ExpiresMS).UTC().Format(time.RFC3339)
	}
	return out
}

// allocationWindows is a key's ledger state: "key" for its key-wide windows,
// and each model's share under the model's name.
func (p *Proxy) allocationWindows(key string) map[string][]quota.Window {
	if p.quota == nil {
		return nil
	}
	all := config.KeyScopeKey(key)
	modelPrefix := all + "@"
	out := map[string][]quota.Window{}
	for _, e := range p.quota.Snapshot() {
		switch {
		case e.Backend == all && len(e.Windows) > 0:
			out["key"] = e.Windows
		case strings.HasPrefix(e.Backend, modelPrefix) && len(e.Windows) > 0:
			out[strings.TrimPrefix(e.Backend, modelPrefix)] = e.Windows
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
