package api

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/iodesystems/corrallm/internal/auth"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proxy"
)

// The Requests page (P62): access requests waiting for a decision, every key
// minted at runtime with its spend, budgets, value adds and expiry, and the
// rules for who may file. Admin only — it shows keys.
//
// Wire types are flat (no pointers): the page always sends the full state it
// wants, prefilled from what is there now.

// KeyLimit is one budget on a caller key.
type KeyLimit struct {
	Req   float64 `json:"req,omitempty" required:"false" doc:"Requests."`
	USD   float64 `json:"usd,omitempty" required:"false" doc:"Dollars."`
	Per   string  `json:"per" doc:"Window: minute | hour | day | month | calendar-day | calendar-month | total (one-off, never resets)."`
	Hard  bool    `json:"hard,omitempty" required:"false" doc:"Refuse once spent."`
	Model string  `json:"model,omitempty" required:"false" doc:"Narrow to one model; empty = every model."`
}

func toKeyLimits(in config.LimitSet) []KeyLimit {
	out := make([]KeyLimit, 0, len(in))
	for _, l := range in {
		out = append(out, KeyLimit{Req: l.Req, USD: l.USD, Per: l.Per, Hard: l.Hard, Model: l.Model})
	}
	return out
}

func fromKeyLimits(in []KeyLimit) config.LimitSet {
	out := make(config.LimitSet, 0, len(in))
	for _, l := range in {
		out = append(out, config.Limit{Req: l.Req, USD: l.USD, Per: l.Per, Hard: l.Hard, Model: l.Model})
	}
	return out
}

// ValueAdd is a one-time amount on a key, spent once its own limits refuse.
type ValueAdd struct {
	ID      int64   `json:"id,omitempty" required:"false"`
	Key     string  `json:"key,omitempty" required:"false"`
	USD     float64 `json:"usd"`
	Spent   float64 `json:"spent,omitempty" required:"false"`
	Model   string  `json:"model,omitempty" required:"false" doc:"Empty = any model."`
	TTL     string  `json:"ttl,omitempty" required:"false" doc:"Expires this long after approval: 48h, 2d."`
	Expires string  `json:"expires,omitempty" required:"false" doc:"RFC 3339; empty with no ttl = until spent."`
	Created string  `json:"created,omitempty" required:"false"`
	Active  bool    `json:"active,omitempty" required:"false"`
	Note    string  `json:"note,omitempty" required:"false"`
}

func fromGrantViews(in []proxy.GrantView) []ValueAdd {
	out := make([]ValueAdd, 0, len(in))
	for _, g := range in {
		out = append(out, ValueAdd{ID: g.ID, Key: g.Key, USD: g.USD, Spent: g.Spent, Model: g.Model,
			Expires: g.Expires, Created: g.Created, Active: g.Active, Note: g.Note})
	}
	return out
}

// KeyGrant is what a request asks for, or an approval grants.
type KeyGrant struct {
	Label       string     `json:"label,omitempty" required:"false"`
	Purpose     string     `json:"purpose,omitempty" required:"false"`
	Models      []string   `json:"models,omitempty" required:"false" doc:"Models the requester said it wants."`
	Group       string     `json:"group,omitempty" required:"false" doc:"Priority group; empty = the fallback group."`
	Credentials []string   `json:"credentials,omitempty" required:"false" doc:"Credential scopes (cred:<provider>/<name>) the key may use."`
	Limits      []KeyLimit `json:"limits,omitempty" required:"false"`
	ValueAdds   []ValueAdd `json:"valueAdds,omitempty" required:"false"`
	TTL         string     `json:"ttl,omitempty" required:"false" doc:"Key expires this long after approval: 48h, 7d."`
	Expires     string     `json:"expires,omitempty" required:"false" doc:"Key expiry, RFC 3339."`
	Mint        bool       `json:"mint,omitempty" required:"false" doc:"May mint child keys."`
	// The asked side only: which fields the requester set at all.
	AskedLimits bool `json:"askedLimits,omitempty" required:"false"`
	AskedMint   bool `json:"askedMint,omitempty" required:"false"`
}

func fromAsk(a proxy.AccessAsk) KeyGrant {
	g := KeyGrant{Label: a.Label, Purpose: a.Purpose, Models: a.Models, TTL: a.TTL, Expires: a.Expires}
	if a.Group != nil {
		g.Group = *a.Group
	}
	if a.Credentials != nil {
		g.Credentials = *a.Credentials
	}
	if a.Limits != nil {
		g.Limits, g.AskedLimits = toKeyLimits(*a.Limits), true
	}
	if a.Mint != nil {
		g.Mint, g.AskedMint = *a.Mint, true
	}
	for _, v := range a.ValueAdds {
		g.ValueAdds = append(g.ValueAdds, ValueAdd{USD: v.USD, Model: v.Model, TTL: v.TTL, Expires: v.Expires, Note: v.Note})
	}
	return g
}

// toAsk is a full-state grant: every field set.
func (g KeyGrant) toAsk() proxy.AccessAsk {
	lim := fromKeyLimits(g.Limits)
	creds := append([]string{}, g.Credentials...)
	a := proxy.AccessAsk{Label: g.Label, Purpose: g.Purpose, Models: g.Models, Group: &g.Group,
		Credentials: &creds, Limits: &lim, TTL: g.TTL, Expires: g.Expires, Mint: &g.Mint}
	for _, v := range g.ValueAdds {
		a.ValueAdds = append(a.ValueAdds, proxy.ValueAddAsk{USD: v.USD, Model: v.Model, TTL: v.TTL, Expires: v.Expires, Note: v.Note})
	}
	return a
}

// AccessRequestRow is one request on the page.
type AccessRequestRow struct {
	ID           string   `json:"id"`
	Status       string   `json:"status" doc:"pending | approved | denied"`
	RequesterKey string   `json:"requesterKey,omitempty" required:"false" doc:"The key asking for more; empty = asking for a new key."`
	SourceIP     string   `json:"sourceIp"`
	Via          string   `json:"via" doc:"The filing rule that admitted it."`
	FiledBy      string   `json:"filedBy,omitempty" required:"false"`
	Asked        KeyGrant `json:"asked"`
	Granted      KeyGrant `json:"granted"`
	IssuedKey    string   `json:"issuedKey,omitempty" required:"false"`
	Delivered    bool     `json:"delivered" doc:"The requester has collected the issued key."`
	Reason       string   `json:"reason,omitempty" required:"false"`
	Created      string   `json:"created"`
	Decided      string   `json:"decided,omitempty" required:"false"`
	DecidedBy    string   `json:"decidedBy,omitempty" required:"false"`
}

// KeyWindowRow is one allocation window's live state.
type KeyWindowRow struct {
	Scope    string  `json:"scope" doc:"\"key\" = key-wide, else a model's share."`
	Label    string  `json:"label"`
	Limit    float64 `json:"limit"`
	Used     float64 `json:"used"`
	Blocked  bool    `json:"blocked"`
	Hard     bool    `json:"hard"`
	ResetsAt string  `json:"resetsAt,omitempty" required:"false"`
}

// ManagedKey is a key the page manages: minted at runtime, or configured.
type ManagedKey struct {
	Key         string         `json:"key"`
	Kind        string         `json:"kind" doc:"issued (by an admin) | child (minted by a key) | configured"`
	Parent      string         `json:"parent,omitempty" required:"false"`
	Label       string         `json:"label,omitempty" required:"false"`
	Group       string         `json:"group,omitempty" required:"false"`
	Credentials []string       `json:"credentials,omitempty" required:"false"`
	Mint        bool           `json:"mint"`
	Limits      []KeyLimit     `json:"limits"`
	Created     string         `json:"created,omitempty" required:"false"`
	Expires     string         `json:"expires,omitempty" required:"false"`
	Expired     bool           `json:"expired"`
	ValueAdds   []ValueAdd     `json:"valueAdds"`
	Windows     []KeyWindowRow `json:"windows"`
}

// RequestRule is one accessRequests rule.
type RequestRule struct {
	Name  string   `json:"name,omitempty" required:"false"`
	CIDRs []string `json:"cidrs,omitempty" required:"false" doc:"Direct connections from these subnets."`
	Token string   `json:"token,omitempty" required:"false" doc:"Shared token sent as X-Corrallm-Request-Token; ${NAME} reads the credential store."`
	Users []string `json:"users,omitempty" required:"false" doc:"Dashboard users (SSO emails)."`
}

type RequestsOutput struct {
	Body struct {
		Requests    []AccessRequestRow     `json:"requests"`
		Keys        []ManagedKey           `json:"keys"`
		Credentials []proxy.CredentialView `json:"credentials"`
		Groups      []string               `json:"groups"`
		Models      []string               `json:"models"`
		// Rules is nil-able in config; DefaultRules says the default applies.
		Rules        []RequestRule `json:"rules"`
		DefaultRules bool          `json:"defaultRules" doc:"No accessRequests block: direct connections from loopback and private networks may file."`
	}
}

func (h *Handlers) proxyOrErr() (*proxy.Proxy, error) {
	if h.Proxy == nil {
		return nil, huma.Error503ServiceUnavailable("the proxy is not attached")
	}
	return h.Proxy, nil
}

// ListRequests is the Requests page.
func (h *Handlers) ListRequests(ctx context.Context, _ *struct{}) (*RequestsOutput, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	p, err := h.proxyOrErr()
	if err != nil {
		return nil, err
	}
	out := &RequestsOutput{}
	b := &out.Body
	rs, err := p.AccessRequests()
	if err != nil {
		return nil, huma.Error500InternalServerError("requests", err)
	}
	b.Requests = make([]AccessRequestRow, 0, len(rs))
	for _, r := range rs {
		b.Requests = append(b.Requests, AccessRequestRow{ID: r.ID, Status: r.Status, RequesterKey: r.RequesterKey,
			SourceIP: r.SourceIP, Via: r.Via, FiledBy: r.FiledBy, Asked: fromAsk(r.Asked), Granted: fromAsk(r.Granted),
			IssuedKey: r.IssuedKey, Delivered: r.Delivered, Reason: r.Reason, Created: r.Created,
			Decided: r.Decided, DecidedBy: r.DecidedBy})
	}
	b.Keys = []ManagedKey{}
	for _, k := range p.MintedKeys() {
		kind := "issued"
		if k.Parent != "" {
			kind = "child"
		}
		b.Keys = append(b.Keys, ManagedKey{Key: k.Key, Kind: kind, Parent: k.Parent, Label: k.Label, Group: k.Group,
			Credentials: k.Credentials, Mint: k.Mint, Limits: toKeyLimits(k.Limits), Created: k.Created,
			Expires: k.Expires, Expired: k.Expired, ValueAdds: fromGrantViews(k.ValueAdds), Windows: windowRows(k.Windows)})
	}
	cfg := h.config()
	adds := map[string][]proxy.GrantView{}
	for _, g := range p.KeyValueAdds() {
		adds[g.Key] = append(adds[g.Key], g)
	}
	for _, key := range sortedNames(cfg.Keys) {
		kp := cfg.Keys[key]
		var creds []string
		for _, c := range p.Credentials() {
			if !c.Open && hasString(c.Allow, key) {
				creds = append(creds, c.Scope)
			}
		}
		b.Keys = append(b.Keys, ManagedKey{Key: key, Kind: "configured", Group: kp.Group, Credentials: creds,
			Mint: kp.Mint, Limits: toKeyLimits(kp.Limits), ValueAdds: fromGrantViews(adds[key]),
			Windows: windowRows(p.KeyWindows(key))})
	}
	b.Credentials = p.Credentials()
	b.Groups = sortedNames(cfg.PriorityGroups)
	b.Models = sortedNames(cfg.Models)
	b.Rules = []RequestRule{}
	if cfg.AccessRequests == nil {
		b.DefaultRules = true
	} else {
		for _, r := range cfg.AccessRequests.Rules {
			b.Rules = append(b.Rules, RequestRule{Name: r.Name, CIDRs: r.CIDRs, Token: r.Token, Users: r.Users})
		}
	}
	return out, nil
}

func windowRows(in []proxy.KeyWindow) []KeyWindowRow {
	out := make([]KeyWindowRow, 0, len(in))
	for _, w := range in {
		out = append(out, KeyWindowRow{Scope: w.Scope, Label: w.Label, Limit: w.Limit, Used: w.Used,
			Blocked: w.Blocked, Hard: w.Hard, ResetsAt: w.ResetsAt})
	}
	return out
}

func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func hasString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// actor names who made a decision.
func actor(ctx context.Context) string {
	if id, ok := auth.IdentityFrom(ctx); ok && id.Email != "" {
		return id.Email
	}
	return "admin token"
}

// configEditor adapts mutateConfig to proxy.ConfigEditor.
func (h *Handlers) configEditor(ctx context.Context) proxy.ConfigEditor {
	return func(note string, fn func(*config.Config) error) error {
		return h.mutateConfig(ctx, note, fn)
	}
}

// decisionErr maps a proxy refusal to a 4xx with its message.
func decisionErr(err error) error {
	if errors.Is(err, proxy.ErrNotPending) {
		return huma.Error409Conflict(err.Error())
	}
	var he huma.StatusError
	if errors.As(err, &he) {
		return err
	}
	return huma.Error400BadRequest(err.Error())
}

type ApproveRequestInput struct {
	Body struct {
		ID      string   `json:"id"`
		Granted KeyGrant `json:"granted"`
		Reason  string   `json:"reason,omitempty" required:"false"`
	}
}

type DecisionOutput struct {
	Body struct {
		OK  bool   `json:"ok"`
		Key string `json:"key,omitempty" required:"false" doc:"The issued key, for a request for a new one. The requester collects it by polling."`
	}
}

// ApproveRequest grants a pending request as edited.
func (h *Handlers) ApproveRequest(ctx context.Context, in *ApproveRequestInput) (*DecisionOutput, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	p, err := h.proxyOrErr()
	if err != nil {
		return nil, err
	}
	key, err := p.ApproveAccess(in.Body.ID, actor(ctx), strings.TrimSpace(in.Body.Reason), in.Body.Granted.toAsk(), h.configEditor(ctx))
	if err != nil {
		return nil, decisionErr(err)
	}
	out := &DecisionOutput{}
	out.Body.OK, out.Body.Key = true, key
	return out, nil
}

type DenyRequestInput struct {
	Body struct {
		ID     string `json:"id"`
		Reason string `json:"reason,omitempty" required:"false"`
	}
}

// DenyRequest refuses a pending request.
func (h *Handlers) DenyRequest(ctx context.Context, in *DenyRequestInput) (*DecisionOutput, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	p, err := h.proxyOrErr()
	if err != nil {
		return nil, err
	}
	if err := p.DenyAccess(in.Body.ID, actor(ctx), strings.TrimSpace(in.Body.Reason)); err != nil {
		return nil, decisionErr(err)
	}
	out := &DecisionOutput{}
	out.Body.OK = true
	return out, nil
}

type UpdateManagedKeyInput struct {
	Body struct {
		Key string `json:"key"`
		// Edit is the key's full desired state. Expiry: ttl or expires; both
		// empty with ClearExpiry clears it, both empty without leaves it.
		Edit        KeyGrant `json:"edit"`
		ClearExpiry bool     `json:"clearExpiry,omitempty" required:"false"`
	}
}

// UpdateManagedKey changes a key's label, group, credentials, mint
// permission, budgets and expiry. A configured key changes in the config
// (no expiry there).
func (h *Handlers) UpdateManagedKey(ctx context.Context, in *UpdateManagedKeyInput) (*DecisionOutput, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	p, err := h.proxyOrErr()
	if err != nil {
		return nil, err
	}
	g := in.Body.Edit.toAsk()
	g.ValueAdds = nil // edited on their own
	if err := p.UpdateKey(in.Body.Key, g, in.Body.ClearExpiry, h.configEditor(ctx)); err != nil {
		return nil, decisionErr(err)
	}
	out := &DecisionOutput{}
	out.Body.OK = true
	return out, nil
}

type ManagedKeyActionInput struct {
	Body struct {
		Key    string `json:"key"`
		Action string `json:"action" enum:"disable,enable,delete" doc:"disable = expire now (stays listed); enable = clear the expiry; delete = remove it, its children and value adds."`
	}
}

// ManagedKeyAction disables, re-enables or deletes a minted key.
func (h *Handlers) ManagedKeyAction(ctx context.Context, in *ManagedKeyActionInput) (*DecisionOutput, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	p, err := h.proxyOrErr()
	if err != nil {
		return nil, err
	}
	switch in.Body.Action {
	case "disable":
		err = p.ExpireMintedKey(in.Body.Key)
	case "enable":
		err = p.ClearMintedKeyExpiry(in.Body.Key)
	case "delete":
		err = p.DeleteMintedKey(in.Body.Key)
	default:
		err = errors.New("action: disable | enable | delete")
	}
	if err != nil {
		return nil, decisionErr(err)
	}
	out := &DecisionOutput{}
	out.Body.OK = true
	return out, nil
}

type ValueAddInput struct {
	Body struct {
		// Action add needs key; update and delete need id.
		Action   string   `json:"action" enum:"add,update,delete"`
		ValueAdd ValueAdd `json:"valueAdd"`
	}
}

// EditValueAdd adds, changes or removes a value add.
func (h *Handlers) EditValueAdd(ctx context.Context, in *ValueAddInput) (*DecisionOutput, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	p, err := h.proxyOrErr()
	if err != nil {
		return nil, err
	}
	v := in.Body.ValueAdd
	switch in.Body.Action {
	case "add":
		if !h.knownKey(p, v.Key) {
			return nil, huma.Error404NotFound("no such key")
		}
		_, err = p.AddValueAdd(v.Key, proxy.ValueAddAsk{USD: v.USD, Model: v.Model, TTL: v.TTL, Expires: v.Expires, Note: v.Note}, time.Now())
	case "update":
		_, err = p.UpdateValueAdd(v.ID, v.USD, v.TTL, v.Expires, v.Note)
	case "delete":
		err = p.DeleteValueAdd(v.ID)
	default:
		err = errors.New("action: add | update | delete")
	}
	if err != nil {
		return nil, decisionErr(err)
	}
	out := &DecisionOutput{}
	out.Body.OK = true
	return out, nil
}

func (h *Handlers) knownKey(p *proxy.Proxy, key string) bool {
	if _, ok := h.config().Keys[key]; ok {
		return true
	}
	for _, k := range p.MintedKeys() {
		if k.Key == key {
			return true
		}
	}
	return false
}

type RequestRulesInput struct {
	Body struct {
		// UseDefault removes the accessRequests block: direct connections from
		// loopback and private networks may file. Rules is then ignored.
		UseDefault bool          `json:"useDefault,omitempty" required:"false"`
		Rules      []RequestRule `json:"rules"`
	}
}

// SetRequestRules replaces who may file access requests.
func (h *Handlers) SetRequestRules(ctx context.Context, in *RequestRulesInput) (*ConfigMutationOutput, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	err := h.mutateConfig(ctx, "access request rules", func(c *config.Config) error {
		if in.Body.UseDefault {
			c.AccessRequests = nil
			return nil
		}
		a := &config.AccessRequests{Rules: []config.AccessRequestRule{}}
		for _, r := range in.Body.Rules {
			a.Rules = append(a.Rules, config.AccessRequestRule{Name: strings.TrimSpace(r.Name), CIDRs: trimAll(r.CIDRs),
				Token: strings.TrimSpace(r.Token), Users: trimAll(r.Users)})
		}
		c.AccessRequests = a
		return c.AccessRequests.Validate()
	})
	if err != nil {
		return nil, err
	}
	out := &ConfigMutationOutput{}
	out.Body.OK, out.Body.Message = true, "saved"
	return out, nil
}

func trimAll(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
