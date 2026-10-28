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
	models  []string          // selectors: what it may see and use; empty = every model
	budgets config.KeyBudgets // by selector
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
		Policy: config.KeyPolicy{Group: k.Group, Mint: k.CanMint, Models: k.models,
			Budgets: k.budgets, Disabled: k.Disabled}}, true
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

// loadChildKeys fills the roster from the store. A row whose budgets no
// longer parse is kept with none and logged: refusing to start over one bad
// row would take every caller down with it. A row whose MODELS do not parse is
// switched off instead — an unreadable filter must not become "every model".
//
// A row still in the pre-P65 shape (limits, credentials) is converted with
// config.LegacyAccess and written back in the new one.
func (p *Proxy) loadChildKeys() {
	if p.store == nil {
		return
	}
	rows, err := p.store.LoadChildKeys()
	if err != nil {
		slog.Warn("child keys: load failed", "err", err)
		return
	}
	var migrate []childKey
	for _, r := range rows {
		k := childKey{ChildKey: r}
		if r.Models != "" {
			if err := json.Unmarshal([]byte(r.Models), &k.models); err != nil {
				slog.Warn("child keys: unreadable models, switching the key off", "label", r.Label, "err", err)
				k.Disabled = true
			}
		}
		if r.Budgets != "" {
			if err := json.Unmarshal([]byte(r.Budgets), &k.budgets); err != nil {
				slog.Warn("child keys: unreadable budgets, serving with none", "label", r.Label, "err", err)
			}
		}
		if r.Limits != "" || r.Credentials != "" {
			k.models, k.budgets = p.legacyChildAccess(r)
			migrate = append(migrate, k)
		}
		p.children.put(k)
	}
	for _, k := range migrate {
		if err := p.persistChildKey(k); err != nil {
			slog.Warn("child keys: could not write the migrated shape", "label", k.Label, "err", err)
		}
	}
	if len(migrate) > 0 {
		slog.Info("child keys: limits and credentials moved into models and budgets (P65)", "keys", len(migrate))
	}
}

// legacyChildAccess converts a pre-P65 row. Its credentials column held
// either allow patterns ({credential, limits}) or bare credential scopes.
func (p *Proxy) legacyChildAccess(r store.ChildKey) ([]string, config.KeyBudgets) {
	var limits config.LimitSet
	if r.Limits != "" {
		if err := json.Unmarshal([]byte(r.Limits), &limits); err != nil {
			slog.Warn("child keys: unreadable limits, migrating with none", "label", r.Label, "err", err)
		}
	}
	var creds []config.KeyCredential
	if r.Credentials != "" {
		if json.Unmarshal([]byte(r.Credentials), &creds) != nil {
			var scopes []string
			if err := json.Unmarshal([]byte(r.Credentials), &scopes); err != nil {
				slog.Warn("child keys: unreadable allow patterns, migrating as no paid account", "label", r.Label, "err", err)
				scopes = []string{config.NoCredentialPattern}
			}
			for _, sc := range scopes {
				creds = append(creds, config.KeyCredential{Credential: sc})
			}
		}
	}
	models, budgets := config.LegacyAccess(limits, creds, p.config().OpenRemoteProviders())
	return models, budgets.Normalized()
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

// ---------------------------------------------------------------------------
// /v1/keys
// ---------------------------------------------------------------------------

// childKeyBody is a child key as the API shows it.
type childKeyBody struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// Models are the selectors naming what it may see and use; empty = every
	// model its parent may use.
	Models []string `json:"models"`
	// Budgets are what it may spend, by selector.
	Budgets config.KeyBudgets `json:"budgets"`
	Created string            `json:"created"`
	Updated string            `json:"updated,omitempty"`
	// Expires is when the key stops working (RFC 3339); empty = never.
	Expires string `json:"expires,omitempty"`
	// Disabled mirrors the admin switch: a key created off answers 401 until it
	// is switched on, and says so here rather than looking broken.
	Disabled bool `json:"disabled"`
	// Windows is each budget's live state, keyed by selector.
	Windows map[string][]quota.Window `json:"windows,omitempty"`
	// Warnings say where subkey budgets add up to more than what they sit
	// under. Oversubscription is allowed; the parent's own budget stops the
	// total.
	Warnings []string `json:"warnings,omitempty"`
}

// childKeyInput is what POST and PATCH accept. PATCH replaces each field that
// is present; budgets: {} removes them all, models: [] allows every model.
// Expiry: `ttl` ("48h", "7d") from now, or `expires` (RFC 3339); "" for either
// clears it.
//
// `limits` and `credentials` are the pre-P65 shape, still accepted from
// clients written against it and converted (config.LegacyAccess).
type childKeyInput struct {
	Label    *string            `json:"label"`
	Models   *[]string          `json:"models"`
	Budgets  *config.KeyBudgets `json:"budgets"`
	TTL      *string            `json:"ttl"`
	Expires  *string            `json:"expires"`
	Disabled *bool              `json:"disabled"`

	Limits   *config.LimitSet        `json:"limits"`
	Patterns *[]config.KeyCredential `json:"credentials"`
}

// access resolves the input's models and budgets, converting the legacy
// fields; a nil result leaves that part of the key as it is.
func (in childKeyInput) access(cfg *config.Config) (*[]string, *config.KeyBudgets) {
	models, budgets := in.Models, in.Budgets
	if in.Limits != nil || in.Patterns != nil {
		var creds []config.KeyCredential
		if in.Patterns != nil {
			creds = *in.Patterns
		}
		var limits config.LimitSet
		if in.Limits != nil {
			limits = *in.Limits
		}
		lm, lb := config.LegacyAccess(limits, creds, cfg.OpenRemoteProviders())
		if in.Patterns != nil && models == nil {
			models = &lm
		}
		if budgets == nil {
			lb = lb.Normalized()
			if lb == nil {
				lb = config.KeyBudgets{}
			}
			budgets = &lb
		}
	}
	if models != nil {
		n := config.NormalizeSelectors(*models)
		models = &n
	}
	if budgets != nil {
		n := budgets.Normalized()
		budgets = &n
	}
	return models, budgets
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
//	GET    /v1/keys          this key's models, budgets and spend, and its
//	                         children (any key; one that cannot mint has none)
//	POST   /v1/keys          mint a child: {label, models, budgets, ttl}
//	GET    /v1/keys/{key}    one child
//	PATCH  /v1/keys/{key}    change its label, models, budgets, expiry, on/off
//	DELETE /v1/keys/{key}    revoke it
//
// Authenticated by the caller's own key: only a configured key may manage
// keys, and only its own children.
func (p *Proxy) handleKeys(w http.ResponseWriter, r *http.Request) {
	caller := p.resolveCaller(callerKey(r))
	// Any key may read its own models, budgets and spend (GET /v1/keys with no
	// subkeys listed): a subkey or a key without mint must be able to check
	// what it has before it asks for more.
	if r.Method == http.MethodGet && strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/keys"), "/") == "" &&
		caller.Key != "" && caller.Recognized && p.revokedKey(caller) == "" && !p.canMint(caller) {
		p.listChildKeys(w, caller.Key)
		return
	}
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
	models, budgets := p.keyAccess(parent)
	self := map[string]any{
		"models":    nonNilSelectors(models),
		"budgets":   nonNilBudgets(budgets),
		"windows":   p.budgetWindows(parent),
		"valueAdds": grantViews(p.grants.of(parent)),
	}
	if w := p.oversubscription(parent); len(w) > 0 {
		self["warnings"] = w
	}
	if k, ok := p.children.get(parent); ok {
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
	models, budgets := in.access(p.config())
	if err := p.checkAccess(models, budgets); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
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
		CreatedMS: now, UpdatedMS: now}}
	if models != nil {
		k.models = *models
	}
	if budgets != nil {
		k.budgets = *budgets
	}
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
	models, budgets := in.access(p.config())
	if err := p.checkAccess(models, budgets); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if models != nil {
		k.models = *models
	}
	if budgets != nil {
		k.budgets = *budgets
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
	k.UpdatedMS = time.Now().UnixMilli()
	if !p.saveChildKey(w, k) {
		return
	}
	jsonResp(w, http.StatusOK, p.childKeyView(k))
}

// checkAccess validates a key's new models and budgets, and refuses a literal
// model or provider name this box does not have: a typo would otherwise be a
// lockout, or a budget nothing charges. nil = unchanged.
func (p *Proxy) checkAccess(models *[]string, budgets *config.KeyBudgets) error {
	if models != nil {
		if err := p.config().CheckSelectorNames(*models); err != nil {
			return fmt.Errorf("models: %w", err)
		}
	}
	if budgets != nil {
		if err := budgets.Validate(); err != nil {
			return fmt.Errorf("budgets: %w", err)
		}
		if err := p.config().CheckSelectorNames(budgets.Selectors()); err != nil {
			return fmt.Errorf("budgets: %w", err)
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
	k.Limits, k.Credentials, k.Models, k.Budgets = "", "", "", ""
	if len(k.models) > 0 {
		b, _ := json.Marshal(k.models)
		k.Models = string(b)
	}
	if len(k.budgets) > 0 {
		b, _ := json.Marshal(k.budgets)
		k.Budgets = string(b)
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
	out := childKeyBody{Key: k.Key, Label: k.Label, Models: nonNilSelectors(k.models),
		Budgets: nonNilBudgets(k.budgets), Disabled: k.Disabled,
		Created: time.UnixMilli(k.CreatedMS).UTC().Format(time.RFC3339),
		Windows: p.budgetWindows(k.Key)}
	if k.Parent != "" {
		out.Warnings = p.oversubscription(k.Parent)
	}
	if k.UpdatedMS != k.CreatedMS {
		out.Updated = time.UnixMilli(k.UpdatedMS).UTC().Format(time.RFC3339)
	}
	if k.ExpiresMS > 0 {
		out.Expires = time.UnixMilli(k.ExpiresMS).UTC().Format(time.RFC3339)
	}
	return out
}
