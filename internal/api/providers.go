package api

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/iodesystems/corrallm/internal/config"
)

// CredentialSpec is one account of a provider, in the shape a form edits.
//
// SecretRef, not the secret. The value lives in ~/.corrallm/credentials and is
// referenced as ${NAME}; putting it here would place it in the document
// /api/v1/config/* serves. Set the value through SetSecret instead — write
// only, never read back.
type CredentialSpec struct {
	Name       string   `json:"name" doc:"Account name; also the budget key, stable across restarts."`
	SecretRef  string   `json:"secretRef" required:"false" doc:"Name of the credential-store entry holding this account's token. Referenced as ${NAME}; the value never enters config."`
	HeaderName string   `json:"headerName" required:"false" doc:"Header to carry it in (default authorization, as 'Bearer <value>')."`
	Allow      []string `json:"allow" required:"false" doc:"corrallm keys permitted to use this account. Empty = all."`
	Limits     []Limit  `json:"limits" required:"false" doc:"Budgets for this account alone."`
	// Output-only: reported so a form can show which refs resolve, ignored on
	// input. Marked optional or every write would have to echo it back.
	HasSecret bool `json:"hasSecret" required:"false" doc:"Whether the referenced entry exists in the store. The value itself is never returned."`
}

// Limit mirrors config.Limit in the wire shape.
type Limit struct {
	Req float64 `json:"req,omitempty" doc:"Requests."`
	USD float64 `json:"usd,omitempty" doc:"Dollars."`
	Sec float64 `json:"sec,omitempty" doc:"Seconds of backend dwell."`
	Per string  `json:"per" doc:"Window: minute | hour | day | month."`
}

// ProviderSpec is one upstream endpoint and the accounts held against it.
type ProviderSpec struct {
	Extension   string           `json:"extension" doc:"Extension that groups this provider."`
	Name        string           `json:"name" doc:"Provider name; served models are <provider>-<id>."`
	Host        string           `json:"host" doc:"Upstream host."`
	Port        int              `json:"port" required:"false" doc:"Upstream port (default 443)."`
	BasePath    string           `json:"basePath" required:"false" doc:"Path prefix, e.g. /api."`
	Limits      []Limit          `json:"limits" required:"false" doc:"Budget across ALL this provider's accounts together."`
	Credentials []CredentialSpec `json:"credentials" required:"false" doc:"Accounts held against this endpoint."`

	// Directory is the DEFAULT FILTER for browsing this provider's catalogue.
	// It enrols nothing — see config.Provider.Directory for why that changed.
	Directory *DiscoverSpec `json:"directory,omitempty" required:"false" doc:"Default filter applied when browsing this provider's directory. Enrols nothing."`

	// Manual says models are chosen off the directory. The normal case now that
	// a filter contributes nothing.
	Manual bool `json:"manual" required:"false" doc:"Models are chosen by hand from this provider's directory."`

	// Provides counts the models declared for this provider in YAML. Output
	// only: a form cannot edit a hand-written model list, but it must be able
	// to SAY there is one — otherwise a provider that declares its models looks
	// identical to one that contributes nothing.
	Provides int `json:"provides" required:"false" doc:"How many models this provider declares in config by hand. Read-only here; edit them in the extension YAML."`
}

// DiscoverSpec is the filter a provider's catalogue is admitted through.
type DiscoverSpec struct {
	FreeOnly       bool     `json:"freeOnly" required:"false" doc:"Keep only rows the provider offers at no cost."`
	InputModality  string   `json:"inputModality" required:"false" doc:"Exact match, e.g. text. Keeps a music model out of a chat lane."`
	OutputModality string   `json:"outputModality" required:"false" doc:"Exact match, e.g. text."`
	MinContext     int      `json:"minContext" required:"false" doc:"Drop rows with a smaller advertised window."`
	Exclude        []string `json:"exclude" required:"false" doc:"Drop ids containing any of these substrings."`
	Limit          int      `json:"limit" required:"false" doc:"Cap how many models this provider contributes (largest context first)."`
	Quality        float64  `json:"quality" required:"false" doc:"Rank applied to every discovered model until one is approved with its own."`
	Type           string   `json:"type" required:"false" doc:"Cost class for discovered models (default chat)."`
}

// VirtualProviderView is a VIRTUAL extension: one that satisfies the provider contract by
// pooling its members' catalogues rather than holding an endpoint of its own.
//
// Reported alongside providers, not as one of them, because the difference is
// the thing worth seeing: a pool has no host and no key, its membership changes
// under it as providers add and withdraw models, and its whole value is that it
// spans several endpoints at once.
type VirtualProviderView struct {
	Extension  string        `json:"extension" doc:"The extension acting as a provider."`
	Sources    []string      `json:"sources" doc:"Member providers whose catalogues it draws on."`
	FreeOnly   bool          `json:"freeOnly" doc:"Filter: keep only rows offered at no cost."`
	MinContext int           `json:"minContext" doc:"Filter: minimum advertised window."`
	Limit      int           `json:"limit" doc:"Cap on the POOL as a whole, largest window first."`
	Lanes      []LaneRefView `json:"lanes" doc:"Lanes every member of the pool joins, and at what priority."`
	Models     int           `json:"models" doc:"How many models it is contributing right now. 0 before the first refresh."`
}

// LocalProviderView is a top-level provider whose models own their process.
//
// Reported separately from the remote providers because almost nothing about
// them is the same: there is no host, no key and no catalogue to browse — its
// models are declared, not discovered — and its served names are the only ones
// on the box that also answer to their bare spelling.
type LocalProviderView struct {
	Name string `json:"name"`
	// BarePrecedence is how strongly it claims an unprefixed name; 0 = off.
	BarePrecedence int                      `json:"barePrecedence" doc:"Strength of this provider's claim on unprefixed model names. 0 means only the prefixed name resolves."`
	Notes          string                   `json:"notes"`
	Models         []LocalProviderModelView `json:"models"`
}

// LocalProviderModelView is one declared model of a local provider.
type LocalProviderModelView struct {
	ID     string `json:"id" doc:"As written under the provider."`
	Served string `json:"served" doc:"What callers ask for: <provider>-<id>."`
	Bare   bool   `json:"bare" doc:"Whether the unprefixed id also resolves here."`
	Type   string `json:"type"`
	Server string `json:"server" doc:"Which box runs its process."`
	HasCmd bool   `json:"hasCmd" doc:"Whether it spawns a process (vs proxying something already listening)."`
}

type ProvidersOutput struct {
	Body struct {
		Providers []ProviderSpec        `json:"providers"`
		Local     []LocalProviderView   `json:"local" doc:"Top-level providers whose models own their own process."`
		Pools     []VirtualProviderView `json:"pools" doc:"Virtual extensions — those that pool their members' catalogues."`
		Secrets   []string              `json:"secrets" doc:"Names present in the credential store. Names only — values are never served."`
	}
}

// ListProviders returns every configured provider with its accounts, plus the
// names the credential store holds so a form can show which refs resolve.
func (h *Handlers) ListProviders(_ context.Context, _ *struct{}) (*ProvidersOutput, error) {
	out := &ProvidersOutput{}
	out.Body.Providers = []ProviderSpec{}
	out.Body.Pools = []VirtualProviderView{}
	out.Body.Local = []LocalProviderView{}
	out.Body.Secrets = config.SecretNames()
	sort.Strings(out.Body.Secrets)
	have := map[string]bool{}
	for _, n := range out.Body.Secrets {
		have[n] = true
	}
	cfg := h.Cfg
	if cfg == nil {
		return out, nil
	}
	for en, ext := range cfg.Extensions {
		for pn, pv := range ext.Providers {
			ps := ProviderSpec{Extension: en, Name: pn, Credentials: []CredentialSpec{}}
			var po struct {
				Host     string `yaml:"host"`
				Port     int    `yaml:"port"`
				BasePath string `yaml:"basePath"`
			}
			_ = pv.Proxy.Decode(&po)
			ps.Host, ps.Port, ps.BasePath = po.Host, po.Port, po.BasePath
			ps.Limits = toWireLimits(pv.Limits)
			ps.Manual = pv.Manual
			ps.Provides = len(pv.Provides)
			// Reported so an edit form can ROUND-TRIP the filter. Without it a
			// form could only ever send a filter it had invented, and the only
			// safe thing to do with the field was omit it — which made "edit
			// this provider" and "keep its browse defaults" mutually exclusive.
			if d := pv.Directory; d != nil {
				f := d.Filter
				ps.Directory = &DiscoverSpec{
					FreeOnly: f.Free, InputModality: f.InputModality,
					OutputModality: f.OutputModality, MinContext: f.MinContext,
					Exclude: f.Exclude, Limit: d.Limit,
					Quality: d.Template.Quality, Type: d.Template.Type,
				}
			}
			for _, cr := range pv.Credentials {
				ref, header := secretRefOf(cr)
				ps.Credentials = append(ps.Credentials, CredentialSpec{
					Name: cr.Name, SecretRef: ref, HeaderName: header,
					Allow: cr.Allow, Limits: toWireLimits(cr.Limits),
					HasSecret: ref != "" && have[ref],
				})
			}
			out.Body.Providers = append(out.Body.Providers, ps)
		}
	}
	// Virtual extensions: reported with their live membership, because "how big
	// is the free pool right now" is the question a pool exists to answer and it
	// is not derivable from config alone.
	contributing := map[string]int{}
	for _, m := range cfg.Discovered() {
		contributing[m.Extension]++
	}
	for en, ext := range cfg.Extensions {
		if ext.Virtual == nil {
			continue
		}
		v := VirtualProviderView{Extension: en, Sources: []string{}, Lanes: []LaneRefView{}}
		for pn, pv := range ext.Providers {
			if !pv.Proxy.IsZero() {
				v.Sources = append(v.Sources, pn)
			}
		}
		sort.Strings(v.Sources)
		v.FreeOnly = ext.Virtual.Filter.Free
		v.MinContext = ext.Virtual.Filter.MinContext
		v.Limit = ext.Virtual.Limit
		for _, lp := range ext.Virtual.Lanes {
			v.Lanes = append(v.Lanes, LaneRefView{Lane: lp.Lane, Order: lp.Order})
		}
		v.Models = contributing[en]
		out.Body.Pools = append(out.Body.Pools, v)
	}
	for pn, lp := range cfg.Providers {
		v := LocalProviderView{Name: pn, Notes: lp.Notes, Models: []LocalProviderModelView{}}
		v.BarePrecedence = config.BarePrecedenceOf(lp)
		for id, m := range lp.Models {
			v.Models = append(v.Models, LocalProviderModelView{
				ID: id, Served: config.ServedName(pn, id), Bare: v.BarePrecedence > 0,
				Type: m.Type, Server: m.Server, HasCmd: m.Cmd != "",
			})
		}
		sort.Slice(v.Models, func(i, j int) bool { return v.Models[i].ID < v.Models[j].ID })
		out.Body.Local = append(out.Body.Local, v)
	}
	sort.Slice(out.Body.Local, func(i, j int) bool { return out.Body.Local[i].Name < out.Body.Local[j].Name })
	sort.Slice(out.Body.Pools, func(i, j int) bool {
		return out.Body.Pools[i].Extension < out.Body.Pools[j].Extension
	})
	sort.Slice(out.Body.Providers, func(i, j int) bool {
		a, b := out.Body.Providers[i], out.Body.Providers[j]
		if a.Extension != b.Extension {
			return a.Extension < b.Extension
		}
		return a.Name < b.Name
	})
	return out, nil
}

// secretRefOf recovers which store entry a credential's header points at, so a
// form can round-trip what it wrote without ever reading the value.
func secretRefOf(cr config.Credential) (ref, header string) {
	for k, v := range cr.Headers {
		if s := strings.Index(v, "${"); s >= 0 {
			if e := strings.Index(v[s:], "}"); e > 0 {
				return v[s+2 : s+e], k
			}
		}
	}
	return "", ""
}

func toWireLimits(in config.LimitSet) []Limit {
	out := make([]Limit, 0, len(in))
	for _, l := range in {
		out = append(out, Limit{Req: l.Req, USD: l.USD, Sec: l.Sec, Per: l.Per})
	}
	return out
}

func toConfigLimits(in []Limit) config.LimitSet {
	out := make(config.LimitSet, 0, len(in))
	for _, l := range in {
		out = append(out, config.Limit{Req: l.Req, USD: l.USD, Sec: l.Sec, Per: l.Per})
	}
	return out
}

type UpsertProviderInput struct {
	Body ProviderSpec
}

// UpsertProvider creates or replaces a provider and its accounts.
//
// Goes through mutateConfig, the same path the YAML editor uses, so validation,
// persistence and the live reload are shared rather than reimplemented — a
// second write path is how two shapes of one config drift apart.
func (h *Handlers) UpsertProvider(_ context.Context, in *UpsertProviderInput) (*ConfigMutationOutput, error) {
	out := &ConfigMutationOutput{}
	b := in.Body
	if strings.TrimSpace(b.Extension) == "" || strings.TrimSpace(b.Name) == "" {
		out.Body.Message = "extension and name are required"
		return out, nil
	}
	if strings.TrimSpace(b.Host) == "" {
		out.Body.Message = "host is required: a provider is an endpoint"
		return out, nil
	}

	port := b.Port
	if port == 0 {
		port = 443
	}
	err := h.mutateConfig(func(c *config.Config) error {
		ext, ok := c.Extensions[b.Extension]
		if !ok {
			return fmt.Errorf("unknown extension %q", b.Extension)
		}
		// Zero value when new; when it exists this KEEPS provides/discover, so
		// editing credentials from a form does not silently drop the model list
		// or the discovery block someone wrote by hand.
		pv := ext.Providers[b.Name]

		var proxy yaml.Node
		if err := proxy.Encode(map[string]any{
			"host": b.Host, "port": port, "basePath": b.BasePath,
		}); err != nil {
			return err
		}
		pv.Proxy = proxy
		pv.Limits = toConfigLimits(b.Limits)
		// A provider added through a form is always chosen-by-hand: a filter no
		// longer enrols anything, so there is nothing else it could be.
		pv.Manual = true
		pv.Discover = nil // the retired key never survives a rewrite
		if b.Directory != nil {
			d := b.Directory
			typ := d.Type
			if typ == "" {
				typ = "chat"
			}
			pv.Directory = &config.Discover{
				Filter: config.DiscoverFilter{
					Free: d.FreeOnly, InputModality: d.InputModality,
					OutputModality: d.OutputModality, MinContext: d.MinContext,
					Exclude: d.Exclude,
				},
				Template: config.Model{Type: typ, Quality: d.Quality},
				Limit:    d.Limit,
			}
		}

		creds := make([]config.Credential, 0, len(b.Credentials))
		for _, cs := range b.Credentials {
			cc := config.Credential{
				Name: cs.Name, Allow: cs.Allow, Limits: toConfigLimits(cs.Limits),
			}
			if cs.SecretRef != "" {
				header := cs.HeaderName
				if header == "" {
					header = "authorization"
				}
				// A REFERENCE, never the value — this lands in the document the
				// config API serves.
				cc.Headers = map[string]string{header: "Bearer ${" + cs.SecretRef + "}"}
			}
			creds = append(creds, cc)
		}
		pv.Credentials = creds

		if ext.Providers == nil {
			ext.Providers = map[string]config.Provider{}
		}
		ext.Providers[b.Name] = pv
		c.Extensions[b.Extension] = ext
		return nil
	})
	if err != nil {
		return nil, err
	}
	out.Body.OK = true
	out.Body.Message = "saved provider " + b.Name
	return out, nil
}

type SetSecretInput struct {
	Body struct {
		Name  string `json:"name" doc:"Store entry name; referenced from config as ${NAME}."`
		Value string `json:"value" doc:"The secret. Empty clears it. NEVER returned by any endpoint."`
	}
}

// SetSecret writes one entry into the credential store.
//
// Write-only on purpose. The property that makes a separate store worth having
// is that /api/v1/config/* — and every backup, and every config pasted into a
// chat window — carries references rather than secrets. A UI can set a value
// and see that it exists; never read what it is.
func (h *Handlers) SetSecret(_ context.Context, in *SetSecretInput) (*ConfigMutationOutput, error) {
	out := &ConfigMutationOutput{}
	if err := config.SetSecret(in.Body.Name, in.Body.Value); err != nil {
		out.Body.Message = err.Error()
		return out, nil
	}
	out.Body.OK = true
	if in.Body.Value == "" {
		out.Body.Message = "cleared " + in.Body.Name
	} else {
		out.Body.Message = "stored " + in.Body.Name
	}
	return out, nil
}
