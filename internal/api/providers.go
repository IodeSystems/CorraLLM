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
	Name             string   `json:"name" doc:"Account name; also the budget key, stable across restarts."`
	SecretRef        string   `json:"secretRef" required:"false" doc:"Name of the credential-store entry holding this account's token. Referenced as ${NAME}; the value never enters config."`
	HeaderName       string   `json:"headerName" required:"false" doc:"Header to carry it in (default authorization, as 'Bearer <value>')."`
	ApprovalRequired bool     `json:"approvalRequired" required:"false" doc:"Gate DISCOVERED models behind an explicit decision before they serve."`
	Allow            []string `json:"allow" required:"false" doc:"corrallm keys permitted to use this account. Empty = all."`
	Limits           []Limit  `json:"limits" required:"false" doc:"Budgets for this account alone."`
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

	// Discover is how a provider contributes models without hand-listing them.
	// A provider must contribute SOMETHING — config validation refuses one that
	// declares neither provides nor discover, because it would be an endpoint
	// nothing can reach — and a form has no sane way to hand-list a remote
	// catalogue, so this is the shape "add a provider" actually takes.
	Discover *DiscoverSpec `json:"discover,omitempty" required:"false" doc:"Enumerate the provider's own catalogue instead of listing models by hand."`
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

type ProvidersOutput struct {
	Body struct {
		Providers []ProviderSpec `json:"providers"`
		Secrets   []string       `json:"secrets" doc:"Names present in the credential store. Names only — values are never served."`
	}
}

// ListProviders returns every configured provider with its accounts, plus the
// names the credential store holds so a form can show which refs resolve.
func (h *Handlers) ListProviders(_ context.Context, _ *struct{}) (*ProvidersOutput, error) {
	out := &ProvidersOutput{}
	out.Body.Providers = []ProviderSpec{}
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
			for _, cr := range pv.Credentials {
				ref, header := secretRefOf(cr)
				ps.Credentials = append(ps.Credentials, CredentialSpec{
					Name: cr.Name, SecretRef: ref, HeaderName: header,
					ApprovalRequired: cr.ApprovalRequired, Allow: cr.Allow,
					Limits: toWireLimits(cr.Limits), HasSecret: ref != "" && have[ref],
				})
			}
			out.Body.Providers = append(out.Body.Providers, ps)
		}
	}
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
	if b.Discover == nil {
		// Caught here rather than by config validation so the message names the
		// fix instead of the rule: a provider must contribute models, and a
		// form's way of doing that is discovery.
		out.Body.Message = "discover is required when adding a provider: an endpoint that contributes no models cannot be reached. Set discover (e.g. {freeOnly: true, inputModality: text}) or add provides by editing the extension YAML."
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
		if b.Discover != nil {
			d := b.Discover
			typ := d.Type
			if typ == "" {
				typ = "chat"
			}
			pv.Discover = &config.Discover{
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
				Name: cs.Name, ApprovalRequired: cs.ApprovalRequired,
				Allow: cs.Allow, Limits: toConfigLimits(cs.Limits),
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
