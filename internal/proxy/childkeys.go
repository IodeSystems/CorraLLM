package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
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

// maxChildKeys bounds one parent's roster. Children cost nothing to the
// budget, but each is a row and a ledger scope.
const maxChildKeys = 1000

type childKey struct {
	store.ChildKey
	limits config.LimitSet
}

// childRoster is the in-memory copy of child_key, read on every request.
type childRoster struct {
	mu    sync.RWMutex
	byKey map[string]childKey
}

func (c *childRoster) parentOf(key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	k, ok := c.byKey[key]
	return k.Parent, ok
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

// resolveCaller is config.ResolveCaller, knowing about child keys.
func (p *Proxy) resolveCaller(cred string) config.Caller {
	return p.config().ResolveCallerWith(cred, p.children.parentOf)
}

// childKeyPrefix marks a minted key, so a deleted one can be told from a
// stranger.
const childKeyPrefix = "ck-"

// revokedChild says why a child-shaped credential is refused, or "". By
// default corrallm serves any key, so without this a DELETED child key would
// keep working as a stranger — revoking it would do nothing — and a child whose
// parent left the config would run uncapped.
func revokedChild(c config.Caller) string {
	switch {
	case c.Parent == "" && strings.HasPrefix(c.Key, childKeyPrefix):
		return "this child key was deleted or never existed"
	case c.Parent != "" && !c.Recognized:
		return "this child key's parent is no longer configured"
	}
	return ""
}

// callerScopes are the key-wide allocation scopes a request is held to and
// charged against: its own, and its parent's.
func callerScopes(c config.Caller) []string {
	out := []string{config.KeyScopeKey(c.Key)}
	if c.Parent != "" {
		out = append(out, config.KeyScopeKey(c.Parent))
	}
	return out
}

// callerModelScopes are the per-model shares a request served by model is
// held to and charged against.
func callerModelScopes(c config.Caller, model string) []string {
	out := []string{config.KeyModelScopeKey(c.Key, model)}
	if c.Parent != "" {
		out = append(out, config.KeyModelScopeKey(c.Parent, model))
	}
	return out
}

// callerBlocked reports whether a hard allocation in any of scopes is full,
// and the longest wait among them.
func (p *Proxy) callerBlocked(scopes []string) (bool, time.Duration) {
	full, wait := false, time.Duration(0)
	for _, sc := range scopes {
		if b, d := p.quota.HardBlocked(sc); b {
			full = true
			wait = max(wait, d)
		}
	}
	return full, wait
}

// filterByCallerShare drops candidates whose model the caller (or its parent)
// has used up its share of, so a lane can still serve from another member.
func (p *Proxy) filterByCallerShare(cands []config.Candidate, c config.Caller) ([]config.Candidate, time.Duration) {
	if p.quota == nil {
		return cands, 0
	}
	kept := cands[:0:0]
	wait := time.Duration(0)
	for _, cand := range cands {
		if b, d := p.callerBlocked(callerModelScopes(c, cand.Name)); b {
			if wait == 0 || d < wait {
				wait = d
			}
			continue
		}
		kept = append(kept, cand)
	}
	return kept, wait
}

// chargeCaller charges one served request to every allocation it answers to.
func (p *Proxy) chargeCaller(c config.Caller, model string, costUSD float64) {
	if p.quota == nil {
		return
	}
	scopes := append(callerScopes(c), callerModelScopes(c, model)...)
	for _, sc := range scopes {
		p.quota.Charge(sc, config.DimRequests, 1)
		if costUSD > 0 {
			p.quota.Charge(sc, config.DimUSD, costUSD)
		}
	}
}

// seedCallerLimits registers one key's allocation: the key-wide limits under
// its key scope, each model's share under its own.
func seedCallerLimits(set func(string, []config.Limit), key string, ls config.LimitSet) {
	all, byModel := ls.ByModel()
	if len(all) > 0 {
		set(config.KeyScopeKey(key), all)
	}
	for m, ml := range byModel {
		set(config.KeyModelScopeKey(key, m), ml)
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
	// Windows is the allocation's live state: each limit's used and limit,
	// keyed by scope ("key" or the model name).
	Windows map[string][]quota.Window `json:"windows,omitempty"`
}

// childKeyInput is what POST and PATCH accept. PATCH replaces each field that
// is present; limits: [] removes them all.
type childKeyInput struct {
	Label  *string          `json:"label"`
	Limits *config.LimitSet `json:"limits"`
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
	case !caller.Recognized:
		jsonErr(w, http.StatusForbidden, "only a configured key can mint child keys")
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
	jsonResp(w, http.StatusOK, map[string]any{
		"self": map[string]any{
			"limits":  p.config().Keys[parent].Limits,
			"windows": p.allocationWindows(parent),
		},
		"keys": out,
	})
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
	k := childKey{ChildKey: store.ChildKey{Key: childKeyPrefix + hex.EncodeToString(raw), Parent: parent,
		CreatedMS: now, UpdatedMS: now}, limits: limits}
	if in.Label != nil {
		k.Label = strings.TrimSpace(*in.Label)
	}
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
	b, _ := json.Marshal(k.limits)
	k.Limits = string(b)
	if len(k.limits) == 0 {
		k.Limits = ""
	}
	if err := p.store.SaveChildKey(k.ChildKey); err != nil {
		jsonErr(w, http.StatusInternalServerError, "save: "+err.Error())
		return false
	}
	p.children.put(k)
	p.seedQuota(p.config())
	return true
}

func (p *Proxy) childKeyView(k childKey) childKeyBody {
	out := childKeyBody{Key: k.Key, Label: k.Label, Limits: k.limits,
		Created: time.UnixMilli(k.CreatedMS).UTC().Format(time.RFC3339),
		Windows: p.allocationWindows(k.Key)}
	if out.Limits == nil {
		out.Limits = config.LimitSet{}
	}
	if k.UpdatedMS != k.CreatedMS {
		out.Updated = time.UnixMilli(k.UpdatedMS).UTC().Format(time.RFC3339)
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
