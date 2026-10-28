package proxy

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/store"
)

// Access requests (P62): an agent asks for a key — or for more on the key it
// has — and an admin approves it on the dashboard's API Keys page, editing
// what is granted. The agent never sees a secret pass through a person: it
// gets a claim secret when filing and polls with it, and the approved key is
// handed over ONCE, on the first poll after approval.
//
// Who may file is the config's accessRequests rules (by subnet, shared token
// or dashboard user); the default is a direct LAN or loopback connection.
// Public traffic arrives through the front proxy, and a public endpoint that
// accepted requests from anyone would let strangers fill the page.

// Request bounds: per source IP and in all, counting pending requests only.
const (
	maxPendingPerIP  = 3
	maxPendingTotal  = 50
	accessIDPrefix   = "ar-"
	accessClaimPrefx = "cl-"
)

// AccessAsk is what a request asks for, and — edited by the admin — what an
// approval grants. Every field is optional; for a request about an existing
// key an absent field leaves that part of the key as it is.
type AccessAsk struct {
	// Label names the new key ("mcpshell-bench"). Required for a new key.
	Label string `json:"label,omitempty"`
	// Purpose is why, in the requester's words. Required.
	Purpose string `json:"purpose,omitempty"`
	// From names who is asking ("mcpshell-ea"), so the owner can tell the
	// request they asked an assistant to file from the others. Self-reported.
	From string `json:"from,omitempty"`
	// Models are selectors (config/keyaccess.go) naming what the key may see
	// and use: local, proxy, provider:<name>, cred:<provider>/<name>,
	// model:<name>, or a bare model name. Empty list = every model; absent =
	// unchanged (for a request about an existing key).
	Models *[]string `json:"models,omitempty"`
	// Budgets are what it may spend, by selector: recurring, or `per: total`
	// for a one-off amount.
	Budgets *config.KeyBudgets `json:"budgets,omitempty"`
	// Group is the priority group a new key runs in; "" = the fallback group.
	Group *string `json:"group,omitempty"`
	// Credentials and Limits are the pre-P65 shape, still accepted from
	// requests filed before it; upgrade converts them into Models and Budgets.
	Credentials *[]config.KeyCredential `json:"credentials,omitempty"`
	Limits      *config.LimitSet        `json:"limits,omitempty"`
	// ValueAdds are one-time amounts, spent once the limits refuse.
	ValueAdds []ValueAddAsk `json:"valueAdds,omitempty"`
	// TTL ("48h", "7d") or Expires (RFC 3339): when the key stops working.
	TTL     string `json:"ttl,omitempty"`
	Expires string `json:"expires,omitempty"`
	// Mint asks for the permission to mint child keys.
	Mint *bool `json:"mint,omitempty"`
}

// ValueAddAsk is one value add asked for or granted.
type ValueAddAsk struct {
	USD     float64 `json:"usd"`
	Model   string  `json:"model,omitempty"`
	TTL     string  `json:"ttl,omitempty"`
	Expires string  `json:"expires,omitempty"`
	Note    string  `json:"note,omitempty"`
}

// expiresAt resolves a ttl/expires pair against now; 0 = never.
func expiresAt(ttl, expires string, now time.Time) (int64, error) {
	var t, e *string
	if ttl != "" {
		t = &ttl
	}
	if expires != "" {
		e = &expires
	}
	ms, _, err := expiryFrom(t, e, now)
	return ms, err
}

// upgrade converts the pre-P65 fields into Models and Budgets
// (config.LegacyAccess) and clears them. A pre-P65 `models` list was
// informational bare names; it is kept as a filter of those models, which the
// admin edits on approval.
func (a *AccessAsk) upgrade(cfg *config.Config) {
	if a.Credentials == nil && a.Limits == nil {
		return
	}
	var creds []config.KeyCredential
	if a.Credentials != nil {
		creds = *a.Credentials
	}
	var limits config.LimitSet
	if a.Limits != nil {
		limits = *a.Limits
	}
	models, budgets := config.LegacyAccess(limits, creds, cfg.OpenRemoteProviders())
	if a.Credentials != nil && a.Models == nil {
		a.Models = &models
	}
	if a.Limits != nil || len(budgets) > 0 {
		merged := config.KeyBudgets{}
		if a.Budgets != nil {
			for s, ls := range *a.Budgets {
				merged[s] = append(merged[s], ls...)
			}
		}
		for s, ls := range budgets {
			merged[s] = append(merged[s], ls...)
		}
		a.Budgets = &merged
	}
	a.Credentials, a.Limits = nil, nil
}

// normalize upgrades the legacy fields and normalizes selectors.
func (a *AccessAsk) normalize(cfg *config.Config) {
	a.upgrade(cfg)
	if a.Models != nil {
		n := config.NormalizeSelectors(*a.Models)
		if n == nil {
			n = []string{}
		}
		a.Models = &n
	}
	if a.Budgets != nil {
		n := a.Budgets.Normalized()
		if n == nil {
			n = config.KeyBudgets{}
		}
		a.Budgets = &n
	}
}

// check validates an ask against cfg. forNewKey also requires a label.
func (a AccessAsk) check(cfg *config.Config, forNewKey bool) error {
	if strings.TrimSpace(a.Purpose) == "" {
		return errors.New("purpose: say what the access is for")
	}
	if len(a.Purpose) > 2000 || len(a.Label) > 80 || len(a.From) > 80 {
		return errors.New("label and from are at most 80 characters, purpose at most 2000")
	}
	if forNewKey && strings.TrimSpace(a.Label) == "" {
		return errors.New("label: name the key you are asking for")
	}
	if a.Models != nil {
		if err := cfg.CheckSelectorNames(*a.Models); err != nil {
			return fmt.Errorf("models: %w", err)
		}
	}
	if a.Budgets != nil {
		if err := a.Budgets.Validate(); err != nil {
			return fmt.Errorf("budgets: %w", err)
		}
		if err := cfg.CheckSelectorNames(a.Budgets.Selectors()); err != nil {
			return fmt.Errorf("budgets: %w", err)
		}
	}
	if a.Group != nil && *a.Group != "" {
		if _, ok := cfg.PriorityGroups[*a.Group]; !ok {
			return fmt.Errorf("group: no priority group %q", *a.Group)
		}
	}
	if _, err := expiresAt(a.TTL, a.Expires, time.Now()); err != nil {
		return err
	}
	if len(a.ValueAdds) > 10 {
		return errors.New("valueAdds: at most 10")
	}
	for _, v := range a.ValueAdds {
		if v.USD <= 0 || v.USD > 10000 {
			return errors.New("valueAdds: usd must be more than 0 and at most 10000")
		}
		if _, ok := cfg.Models[v.Model]; v.Model != "" && !ok {
			return fmt.Errorf("valueAdds: no model %q", v.Model)
		}
		if _, err := expiresAt(v.TTL, v.Expires, time.Now()); err != nil {
			return fmt.Errorf("valueAdds: %w", err)
		}
	}
	return nil
}

// SetRequestFiler sets how a filing request is described to the
// accessRequests rules: its TCP peer before any forwarding header, whether
// such a header is present, and the signed-in dashboard user. The token is
// read here. nil = nobody may file.
func (p *Proxy) SetRequestFiler(f func(r *http.Request) config.Filer) { p.requestFiler = f }

func hashClaim(c string) string {
	s := sha256.Sum256([]byte(c))
	return hex.EncodeToString(s[:])
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// handleAccessRequests serves /v1/access-requests and /v1/access-requests/{id}.
//
//	POST /v1/access-requests        file one; body AccessAsk. With your key in
//	                                Authorization it asks for more on THAT key;
//	                                without, for a new key.
//	GET  /v1/access-requests/{id}   its status, with the claim secret as
//	                                Authorization: Bearer <claim>; the issued
//	                                key once, on the first read after approval.
func (p *Proxy) handleAccessRequests(w http.ResponseWriter, r *http.Request) {
	if p.store == nil {
		jsonErr(w, http.StatusServiceUnavailable, "access requests need the database")
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/access-requests"), "/")
	switch {
	case id == "" && r.Method == http.MethodPost:
		p.fileAccessRequest(w, r)
	case id != "" && r.Method == http.MethodGet:
		p.pollAccessRequest(w, r, id)
	case id == "":
		w.Header().Set("Allow", "POST")
		jsonErr(w, http.StatusMethodNotAllowed, "use POST to file a request")
	default:
		w.Header().Set("Allow", "GET")
		jsonErr(w, http.StatusMethodNotAllowed, "use GET with your claim secret")
	}
}

func (p *Proxy) fileAccessRequest(w http.ResponseWriter, r *http.Request) {
	if p.requestFiler == nil {
		jsonErr(w, http.StatusForbidden, "access requests are not accepted here")
		return
	}
	filer := p.requestFiler(r)
	filer.Token = r.Header.Get(config.HeaderRequestToken)
	via, ok := p.config().AccessRequests.Allows(filer)
	if !ok {
		jsonErr(w, http.StatusForbidden, "this connection may not file access requests (see the accessRequests rules: a subnet, "+
			"a shared token in "+config.HeaderRequestToken+", or a dashboard sign-in)")
		return
	}
	var ask AccessAsk
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&ask); err != nil {
		jsonErr(w, http.StatusBadRequest, "body: "+err.Error())
		return
	}
	ask.normalize(p.config())
	requester := ""
	if cred := callerKey(r); cred != "" {
		c := p.resolveCaller(cred)
		if !c.Recognized {
			jsonErr(w, http.StatusForbidden, "unknown key; leave out Authorization to ask for a new key")
			return
		}
		if msg := p.revokedKey(c); msg != "" && !strings.Contains(msg, "expired") {
			jsonErr(w, http.StatusUnauthorized, msg)
			return
		}
		requester = c.Key
	}
	if err := ask.check(p.config(), requester == ""); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ip := clientIP(r)
	total, fromIP, err := p.store.PendingAccessRequests(ip)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if fromIP >= maxPendingPerIP || total >= maxPendingTotal {
		jsonErr(w, http.StatusTooManyRequests, fmt.Sprintf(
			"too many pending requests (%d from this address, %d in all; limits %d and %d) — wait for a decision",
			fromIP, total, maxPendingPerIP, maxPendingTotal))
		return
	}
	idHex, err1 := randomHex(8)
	claimHex, err2 := randomHex(24)
	if err := errors.Join(err1, err2); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	askJSON, _ := json.Marshal(ask)
	ar := store.AccessRequest{ID: accessIDPrefix + idHex, ClaimHash: hashClaim(accessClaimPrefx + claimHex),
		Status: "pending", RequesterKey: requester, SourceIP: ip, Asked: string(askJSON),
		Via: via, FiledBy: filer.User, CreatedMS: time.Now().UnixMilli()}
	if err := p.store.SaveAccessRequest(ar); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if filer.User != "" {
		ip = ip + " (" + filer.User + ")"
	}
	what := "a new key **" + ask.Label + "**"
	if requester != "" {
		what = "more for key **" + requester + "**"
	}
	if ask.From != "" {
		what += " (says it is " + ask.From + ")"
	}
	p.alert.Send("access-request/"+ar.ID, "Access request: "+firstNonEmpty(ask.Label, requester),
		fmt.Sprintf("%s asks for %s.\n\n%s\n\nApprove or deny on the API Keys page (/approvals).", ip, what, ask.Purpose), "key")
	jsonResp(w, http.StatusCreated, map[string]any{
		"id": ar.ID, "claim": accessClaimPrefx + claimHex, "status": ar.Status,
		"poll": "/v1/access-requests/" + ar.ID,
		"note": "Keep the claim: poll GET " + "/v1/access-requests/" + ar.ID +
			" with Authorization: Bearer <claim>. After approval the key is returned once.",
	})
}

func (p *Proxy) pollAccessRequest(w http.ResponseWriter, r *http.Request, id string) {
	ar, ok, err := p.store.AccessRequestByID(id)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	claim := callerKey(r)
	if !ok || claim == "" || subtle.ConstantTimeCompare([]byte(hashClaim(claim)), []byte(ar.ClaimHash)) != 1 {
		jsonErr(w, http.StatusNotFound, "no such request for this claim")
		return
	}
	out := map[string]any{"id": ar.ID, "status": ar.Status}
	if ar.Reason != "" {
		out["reason"] = ar.Reason
	}
	if ar.Granted != "" {
		out["granted"] = json.RawMessage(ar.Granted)
	}
	if ar.Status == "approved" && ar.IssuedKey != "" && ar.RequesterKey == "" {
		if first, err := p.store.MarkAccessDelivered(ar.ID); err == nil && first {
			out["key"] = ar.IssuedKey
		} else {
			out["note"] = "the key was already handed over on an earlier read"
		}
	}
	jsonResp(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// Admin side (internal/api calls these).
// ---------------------------------------------------------------------------

// AccessRequestView is one request as the admin page shows it.
type AccessRequestView struct {
	ID           string    `json:"id"`
	Status       string    `json:"status"`
	RequesterKey string    `json:"requesterKey,omitempty" doc:"The key that asked for more; empty = a request for a new key."`
	SourceIP     string    `json:"sourceIp"`
	Via          string    `json:"via" doc:"The accessRequests rule that admitted the filing."`
	FiledBy      string    `json:"filedBy,omitempty" doc:"Dashboard user who filed it, if any."`
	Asked        AccessAsk `json:"asked"`
	Granted      AccessAsk `json:"granted"`
	IssuedKey    string    `json:"issuedKey,omitempty"`
	Delivered    bool      `json:"delivered"`
	Reason       string    `json:"reason,omitempty"`
	Created      string    `json:"created"`
	Decided      string    `json:"decided,omitempty"`
	DecidedBy    string    `json:"decidedBy,omitempty"`
}

func (p *Proxy) accessView(ar store.AccessRequest) AccessRequestView {
	v := AccessRequestView{ID: ar.ID, Status: ar.Status, RequesterKey: ar.RequesterKey, SourceIP: ar.SourceIP,
		Via: ar.Via, FiledBy: ar.FiledBy,
		IssuedKey: ar.IssuedKey, Delivered: ar.Delivered, Reason: ar.Reason, DecidedBy: ar.DecidedBy,
		Created: time.UnixMilli(ar.CreatedMS).UTC().Format(time.RFC3339)}
	_ = json.Unmarshal([]byte(ar.Asked), &v.Asked)
	v.Asked.normalize(p.config())
	if ar.Granted != "" {
		_ = json.Unmarshal([]byte(ar.Granted), &v.Granted)
		v.Granted.normalize(p.config())
	}
	if ar.DecidedMS > 0 {
		v.Decided = time.UnixMilli(ar.DecidedMS).UTC().Format(time.RFC3339)
	}
	return v
}

// AccessRequests lists requests, pending first.
func (p *Proxy) AccessRequests() ([]AccessRequestView, error) {
	if p.store == nil {
		return nil, nil
	}
	rows, err := p.store.AccessRequests(500)
	if err != nil {
		return nil, err
	}
	out := make([]AccessRequestView, 0, len(rows))
	for _, r := range rows {
		out = append(out, p.accessView(r))
	}
	return out, nil
}

// ErrNotPending is returned for a decision on a request already decided.
var ErrNotPending = errors.New("this request was already decided")

// DenyAccess refuses a pending request.
func (p *Proxy) DenyAccess(id, by, reason string) error {
	ar, ok, err := p.store.AccessRequestByID(id)
	switch {
	case err != nil:
		return err
	case !ok:
		return errors.New("no such request")
	case ar.Status != "pending":
		return ErrNotPending
	}
	ar.Status, ar.Reason, ar.DecidedBy, ar.DecidedMS = "denied", reason, by, time.Now().UnixMilli()
	return p.store.SaveAccessRequest(ar)
}

// ConfigEditor applies an edit to the stored config (api.Handlers.UpdateConfig).
type ConfigEditor func(note string, fn func(*config.Config) error) error

// ApproveAccess grants a pending request as granted (the admin's edit of what
// was asked) and returns the issued key for a request for a new one.
//
// A request about a configured key changes its limits and mint permission in
// the config, through edit; one about a minted key changes its row. Value adds
// go to key_grant either way.
func (p *Proxy) ApproveAccess(id, by, reason string, g AccessAsk, edit ConfigEditor) (string, error) {
	ar, ok, err := p.store.AccessRequestByID(id)
	switch {
	case err != nil:
		return "", err
	case !ok:
		return "", errors.New("no such request")
	case ar.Status != "pending":
		return "", ErrNotPending
	}
	var asked AccessAsk
	_ = json.Unmarshal([]byte(ar.Asked), &asked)
	asked.normalize(p.config())
	g.normalize(p.config())
	if g.Purpose == "" {
		g.Purpose = asked.Purpose
	}
	if ar.RequesterKey != "" {
		g = onlyAsked(g, asked)
	}
	if err := g.check(p.config(), ar.RequesterKey == ""); err != nil {
		return "", err
	}
	now := time.Now()
	key := ar.RequesterKey
	if key == "" {
		if key, err = p.IssueKey(g, now); err != nil {
			return "", err
		}
		ar.IssuedKey = key
	} else if err := p.applyToKey(key, g, edit, "access request "+id, now); err != nil {
		return "", err
	}
	for _, v := range g.ValueAdds {
		if _, err := p.AddValueAdd(key, v, now); err != nil {
			return "", err
		}
	}
	gj, _ := json.Marshal(g)
	ar.Status, ar.Granted, ar.Reason, ar.DecidedBy, ar.DecidedMS = "approved", string(gj), reason, by, now.UnixMilli()
	return key, p.store.SaveAccessRequest(ar)
}

// onlyAsked narrows a grant on an EXISTING key to the parts its request asked
// to change. The dashboard sends a grant as the full state of a form, and a
// form for "$10 more" carries an empty budget list and mint off — applied
// whole, approving a top-up would strip the key's own cap and permissions.
// What was not asked for stays as the key has it; the admin changes that with
// Edit, not by approving someone else's request.
func onlyAsked(g, asked AccessAsk) AccessAsk {
	if asked.Label == "" {
		g.Label = ""
	}
	if asked.Group == nil {
		g.Group = nil
	}
	if asked.Models == nil {
		g.Models = nil
	}
	if asked.Budgets == nil {
		g.Budgets = nil
	}
	if asked.Mint == nil {
		g.Mint = nil
	}
	if asked.TTL == "" && asked.Expires == "" {
		g.TTL, g.Expires = "", ""
	}
	return g
}

// CreateKey mints a key on an admin's order, without an access request behind
// it.
//
// It is the same row IssueKey writes, and it differs in exactly one way: the key
// is created DISABLED (owner, 10-26). An admin who makes a key has decided what
// it may do, not that its first caller may start spending immediately — and a
// key handed over a chat window may be pasted somewhere before anyone has
// checked the budgets on it. Switching it on is one row's toggle.
func (p *Proxy) CreateKey(g AccessAsk, now time.Time) (string, error) {
	if p.store == nil {
		return "", errors.New("keys need the database")
	}
	g.normalize(p.config())
	if err := g.check(p.config(), true); err != nil {
		return "", err
	}
	if g.Group != nil && *g.Group != "" {
		if _, ok := p.config().PriorityGroups[*g.Group]; !ok {
			return "", fmt.Errorf("no priority group %q", *g.Group)
		}
	}
	key, err := p.issueKey(g, now, true)
	if err != nil {
		return "", err
	}
	// Value adds are part of the key being created, so they are written here
	// rather than left to the caller: a top-up with no key is a row nobody can
	// find.
	for _, v := range g.ValueAdds {
		if _, err := p.AddValueAdd(key, v, now); err != nil {
			return key, err
		}
	}
	return key, nil
}

// IssueKey mints an admin-issued key (parent "") from an approved grant.
//
// An approved key is LIVE: the owner read a request, decided what it may do and
// said yes, and making them switch it on afterwards would answer a question
// they have just answered.
func (p *Proxy) IssueKey(g AccessAsk, now time.Time) (string, error) {
	return p.issueKey(g, now, false)
}

// issueKey is the row both paths write; disabled is the one field they differ on.
func (p *Proxy) issueKey(g AccessAsk, now time.Time, disabled bool) (string, error) {
	raw, err := randomHex(24)
	if err != nil {
		return "", err
	}
	exp, err := expiresAt(g.TTL, g.Expires, now)
	if err != nil {
		return "", err
	}
	k := childKey{ChildKey: store.ChildKey{Key: childKeyPrefix + raw, Label: strings.TrimSpace(g.Label),
		CreatedMS: now.UnixMilli(), UpdatedMS: now.UnixMilli(), ExpiresMS: exp, Disabled: disabled}}
	if g.Group != nil {
		k.Group = *g.Group
	}
	if g.Models != nil {
		k.models = append([]string(nil), *g.Models...)
	}
	if g.Budgets != nil {
		k.budgets = g.Budgets.Normalized()
	}
	if g.Mint != nil {
		k.CanMint = *g.Mint
	}
	if err := p.persistChildKey(k); err != nil {
		return "", err
	}
	return k.Key, nil
}

// SetKeyDisabled switches a key off or on, keeping it and its budgets: the
// deliberate pause, as opposed to an expiry that runs out on its own.
//
// A configured key is edited through edit, because its entry lives in the
// config; a minted one is a row. Both end up answering 401 when off (see
// revokedKey), which is the only thing a caller can observe.
func (p *Proxy) SetKeyDisabled(key string, off bool, edit ConfigEditor) error {
	if k, ok := p.children.get(key); ok {
		k.Disabled, k.UpdatedMS = off, time.Now().UnixMilli()
		return p.persistChildKey(k)
	}
	if _, ok := p.config().Keys[key]; !ok {
		return errors.New("no such key")
	}
	if edit == nil {
		return errors.New("config is not editable here")
	}
	verbed := "enable"
	if off {
		verbed = "disable"
	}
	return edit(verbed+" key "+key, func(c *config.Config) error {
		kp := c.Keys[key]
		kp.Disabled = off
		c.Keys[key] = kp
		return nil
	})
}

// applyToKey changes an existing key as g says: a minted key's row, or a
// configured key's limits and mint permission through edit.
func (p *Proxy) applyToKey(key string, g AccessAsk, edit ConfigEditor, note string, now time.Time) error {
	if k, ok := p.children.get(key); ok {
		if g.Label != "" {
			k.Label = strings.TrimSpace(g.Label)
		}
		if g.Models != nil {
			k.models = append([]string(nil), *g.Models...)
		}
		if g.Budgets != nil {
			k.budgets = g.Budgets.Normalized()
		}
		if g.TTL != "" || g.Expires != "" {
			exp, err := expiresAt(g.TTL, g.Expires, now)
			if err != nil {
				return err
			}
			k.ExpiresMS = exp
		}
		if k.Parent == "" { // a child runs as its parent: these are the parent's
			if g.Group != nil {
				k.Group = *g.Group
			}
			if g.Mint != nil {
				k.CanMint = *g.Mint
			}
		}
		k.UpdatedMS = now.UnixMilli()
		return p.persistChildKey(k)
	}
	if _, ok := p.config().Keys[key]; !ok {
		return fmt.Errorf("key %q is neither configured nor minted any more", key)
	}
	if g.Models == nil && g.Budgets == nil && g.Mint == nil && g.Group == nil {
		return nil
	}
	if g.TTL != "" || g.Expires != "" {
		return errors.New("a configured key has no expiry; remove it from the config instead")
	}
	if edit == nil {
		return errors.New("config is not editable here")
	}
	return edit(note, func(c *config.Config) error {
		kp := c.Keys[key]
		if g.Models != nil {
			kp.Models = append([]string(nil), *g.Models...)
		}
		if g.Budgets != nil {
			kp.Budgets = g.Budgets.Normalized()
		}
		if g.Mint != nil {
			kp.Mint = *g.Mint
		}
		if g.Group != nil && *g.Group != "" {
			kp.Group = *g.Group
		}
		c.Keys[key] = kp
		return nil
	})
}

// AddValueAdd stores a value add on key.
func (p *Proxy) AddValueAdd(key string, v ValueAddAsk, now time.Time) (GrantView, error) {
	if p.store == nil {
		return GrantView{}, errors.New("value adds need the database")
	}
	if v.USD <= 0 {
		return GrantView{}, errors.New("usd must be more than 0")
	}
	if _, ok := p.config().Models[v.Model]; v.Model != "" && !ok {
		return GrantView{}, fmt.Errorf("no model %q", v.Model)
	}
	exp, err := expiresAt(v.TTL, v.Expires, now)
	if err != nil {
		return GrantView{}, err
	}
	g := store.KeyGrant{Key: key, USD: v.USD, Model: v.Model, ExpiresMS: exp, CreatedMS: now.UnixMilli(), Note: v.Note}
	if g.ID, err = p.store.AddKeyGrant(g); err != nil {
		return GrantView{}, err
	}
	p.grants.put(g)
	return grantViews([]store.KeyGrant{g})[0], nil
}

// UpdateValueAdd changes a value add's amount, expiry and note.
func (p *Proxy) UpdateValueAdd(id int64, usd float64, ttl, expires, note string) (GrantView, error) {
	g, ok := p.grants.get(id)
	if !ok {
		return GrantView{}, errors.New("no such value add")
	}
	if usd <= 0 {
		return GrantView{}, errors.New("usd must be more than 0")
	}
	exp := g.ExpiresMS
	if ttl != "" || expires != "" {
		var err error
		if exp, err = expiresAt(ttl, expires, time.Now()); err != nil {
			return GrantView{}, err
		}
	}
	if err := p.store.UpdateKeyGrant(id, usd, exp, note); err != nil {
		return GrantView{}, err
	}
	g.USD, g.ExpiresMS, g.Note = usd, exp, note
	p.grants.put(g)
	return grantViews([]store.KeyGrant{g})[0], nil
}

// DeleteValueAdd removes a value add.
func (p *Proxy) DeleteValueAdd(id int64) error {
	if _, ok := p.grants.remove(id); !ok {
		return errors.New("no such value add")
	}
	return p.store.DeleteKeyGrant(id)
}

// CredentialView is one credential a key may be granted.
type CredentialView struct {
	Scope    string   `json:"scope" doc:"cred:<provider>/<name>, the value a grant names."`
	Provider string   `json:"provider"`
	Name     string   `json:"name"`
	Open     bool     `json:"open" doc:"No allowlist: every key may already use it."`
	Allow    []string `json:"allow,omitempty"`
	Models   []string `json:"models,omitempty" doc:"Served models that forward with this credential."`
}

// Credentials lists every provider credential, with the models using it.
func (p *Proxy) Credentials() []CredentialView {
	cfg := p.config()
	byScope := map[string]*CredentialView{}
	for _, ext := range cfg.Extensions {
		for pn, pv := range ext.Providers {
			for _, cr := range pv.Credentials {
				sk := cr.ScopeKey(pn)
				byScope[sk] = &CredentialView{Scope: sk, Provider: pn, Name: cr.Name, Open: len(cr.Allow) == 0, Allow: cr.Allow}
			}
		}
	}
	for name := range cfg.Models {
		cands, _ := cfg.ResolveServed(name)
		for _, cand := range cands {
			if cand.Credential == nil {
				continue
			}
			if v := byScope[cand.Credential.ScopeKey(cand.Model.ProviderName)]; v != nil && !contains(v.Models, name) {
				v.Models = append(v.Models, name)
			}
		}
	}
	out := make([]CredentialView, 0, len(byScope))
	for _, v := range byScope {
		sort.Strings(v.Models)
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Scope < out[j].Scope })
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// MintedKeyView is a minted key as the admin page shows it.
type MintedKeyView struct {
	Key       string            `json:"key"`
	Parent    string            `json:"parent,omitempty" doc:"Empty = issued by an admin."`
	Label     string            `json:"label"`
	Group     string            `json:"group,omitempty"`
	Models    []string          `json:"models" doc:"Selectors naming what this key may see and use. Empty = every model (a subkey: every model its parent may use)."`
	Budgets   config.KeyBudgets `json:"budgets" doc:"What this key may spend, by selector. Every matching budget applies."`
	Warnings  []string          `json:"warnings,omitempty" doc:"Where this key's subkeys' budgets add up to more than what they sit under."`
	Mint      bool              `json:"mint"`
	Created   string            `json:"created"`
	Expires   string            `json:"expires,omitempty"`
	Expired   bool              `json:"expired" doc:"Past its expiry. Still listed, and can be re-enabled."`
	Disabled  bool              `json:"disabled" doc:"Switched off by an admin. Kept, with its budgets, until switched back on."`
	ValueAdds []GrantView       `json:"valueAdds"`
	Windows   []KeyWindow       `json:"windows"`
}

// KeyWindow is one allocation window's live state, flattened for the page.
type KeyWindow struct {
	Scope    string  `json:"scope" doc:"The budget's selector (\"*\" for everything)."`
	Label    string  `json:"label"`
	Limit    float64 `json:"limit"`
	Used     float64 `json:"used"`
	Blocked  bool    `json:"blocked"`
	Hard     bool    `json:"hard"`
	ResetsAt string  `json:"resetsAt,omitempty"`
}

func (p *Proxy) keyWindows(key string) []KeyWindow {
	out := []KeyWindow{}
	for scope, ws := range p.budgetWindows(key) {
		for _, w := range ws {
			kw := KeyWindow{Scope: scope, Label: w.Label, Limit: w.Limit, Used: w.Used, Blocked: w.Blocked, Hard: w.Hard}
			if !w.ResetsAt.IsZero() {
				kw.ResetsAt = w.ResetsAt.UTC().Format(time.RFC3339)
			}
			out = append(out, kw)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		return out[i].Label < out[j].Label
	})
	return out
}

// MintedKeys lists every minted key, admin-issued first.
func (p *Proxy) MintedKeys() []MintedKeyView {
	now := time.Now()
	all := p.children.of("")
	sort.SliceStable(all, func(i, j int) bool { return all[i].Parent == "" && all[j].Parent != "" })
	out := make([]MintedKeyView, 0, len(all))
	for _, k := range all {
		v := MintedKeyView{Key: k.Key, Parent: k.Parent, Label: k.Label, Group: k.Group,
			Models: nonNilSelectors(k.models), Budgets: nonNilBudgets(k.budgets), Warnings: p.oversubscription(k.Key),
			Mint: k.CanMint, Created: time.UnixMilli(k.CreatedMS).UTC().Format(time.RFC3339),
			Expired: k.expired(now), Disabled: k.Disabled,
			ValueAdds: grantViews(p.grants.of(k.Key)), Windows: p.keyWindows(k.Key)}
		if k.ExpiresMS > 0 {
			v.Expires = time.UnixMilli(k.ExpiresMS).UTC().Format(time.RFC3339)
		}
		out = append(out, v)
	}
	return out
}

// KeyValueAdds lists the value adds of configured keys (minted keys carry
// theirs in MintedKeys).
func (p *Proxy) KeyValueAdds() []GrantView {
	var out []GrantView
	for key := range p.config().Keys {
		out = append(out, grantViews(p.grants.of(key))...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// UpdateKey applies an admin's edit to a key: a minted key's row, or a
// configured key's entry through edit. clearExpiry turns off a minted key's
// expiry when the edit names none.
func (p *Proxy) UpdateKey(key string, g AccessAsk, clearExpiry bool, edit ConfigEditor) error {
	_, minted := p.children.get(key)
	if _, configured := p.config().Keys[key]; !minted && !configured {
		return errors.New("no such key")
	}
	g.normalize(p.config())
	if err := p.checkAccess(g.Models, g.Budgets); err != nil {
		return err
	}
	if g.Group != nil && *g.Group != "" {
		if _, ok := p.config().PriorityGroups[*g.Group]; !ok {
			return fmt.Errorf("no priority group %q", *g.Group)
		}
	}
	if err := p.applyToKey(key, g, edit, "edit key "+key, time.Now()); err != nil {
		return err
	}
	if minted && clearExpiry && g.TTL == "" && g.Expires == "" {
		return p.ClearMintedKeyExpiry(key)
	}
	return nil
}

// MintedKey reports a minted key and whether it is one (as opposed to a
// configured key, whose entry lives in the config).
func (p *Proxy) MintedKey(key string) (MintedKeyView, bool) {
	for _, v := range p.MintedKeys() {
		if v.Key == key {
			return v, true
		}
	}
	return MintedKeyView{}, false
}

// KeyWindows is a key's allocation windows, live.
func (p *Proxy) KeyWindows(key string) []KeyWindow { return p.keyWindows(key) }

// ExpireMintedKey disables a minted key now (it stays listed).
func (p *Proxy) ExpireMintedKey(key string) error {
	k, ok := p.children.get(key)
	if !ok {
		return errors.New("no such minted key")
	}
	k.ExpiresMS, k.UpdatedMS = time.Now().UnixMilli(), time.Now().UnixMilli()
	return p.persistChildKey(k)
}

// ClearMintedKeyExpiry turns a minted key back on with no expiry.
func (p *Proxy) ClearMintedKeyExpiry(key string) error {
	k, ok := p.children.get(key)
	if !ok {
		return errors.New("no such minted key")
	}
	k.ExpiresMS, k.UpdatedMS = 0, time.Now().UnixMilli()
	return p.persistChildKey(k)
}

// DeleteMintedKey removes a minted key, its children and its value adds.
func (p *Proxy) DeleteMintedKey(key string) error {
	if _, ok := p.children.get(key); !ok {
		return errors.New("no such minted key")
	}
	for _, kid := range p.children.of(key) {
		if err := p.store.DeleteChildKey(kid.Key); err != nil {
			return err
		}
		p.children.remove(kid.Key)
		p.dropGrants(kid.Key)
	}
	if err := p.store.DeleteChildKey(key); err != nil {
		return err
	}
	p.children.remove(key)
	p.dropGrants(key)
	p.seedQuota(p.config())
	return nil
}
