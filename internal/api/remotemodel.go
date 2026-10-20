package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/freeroster"
)

// RemoteModelSpec is everything needed to serve one model from somebody else's
// OpenAI-compatible endpoint, in the terms a person has in hand: the base URL
// from the provider's docs, its model id, and a key.
//
// It exists because the alternative was three forms — provider, secret, model —
// filled in the right order, with the model form offering process fields that a
// remote model is then refused for having.
type RemoteModelSpec struct {
	Provider  string `json:"provider" doc:"Provider name; the model is served as <provider>-<name>. Lowercase letters, digits and dashes."`
	Extension string `json:"extension" required:"false" doc:"Extension grouping the provider. Default: the provider name."`
	BaseURL   string `json:"baseUrl" doc:"OpenAI-compatible base URL, e.g. https://api.fireworks.ai/inference/v1. A trailing /v1 is optional."`

	ModelID string `json:"modelId" doc:"The id the provider calls the model, sent upstream unchanged."`
	Name    string `json:"name" required:"false" doc:"Served-name suffix. Default: the model id's last path segment."`

	SecretRef   string `json:"secretRef" required:"false" doc:"Credential-store entry holding the API key. Empty = no key."`
	SecretValue string `json:"secretValue" required:"false" doc:"When set, stored under secretRef first. Write-only; never returned."`
	HeaderName  string `json:"headerName" required:"false" doc:"Header carrying the key. Default authorization (as Bearer)."`
	API         string `json:"api" required:"false" doc:"Wire format upstream: openai (default) or anthropic (Messages API with prompt caching)."`

	Type          string  `json:"type" required:"false" doc:"Cost class. Default: <provider>-<name> when pricing is given, else chat."`
	Quality       float64 `json:"quality" required:"false" doc:"Rank among models serving the same name. Higher wins."`
	MaxTokens     int     `json:"maxTokens" required:"false" doc:"Output-token cap injected when a caller sets none. 0 = no cap."`
	MaxConcurrent int     `json:"maxConcurrent" required:"false" doc:"Concurrent requests corrallm admits. 0 = no limit."`

	Allow  []string `json:"allow" required:"false" doc:"corrallm keys allowed to use this key. Empty = every caller."`
	Limits []Limit  `json:"limits" required:"false" doc:"Budgets on this key. Use hard: true for spend caps."`

	Pricing *RemotePricing `json:"pricing,omitempty" required:"false" doc:"Per-1M-token prices, so cost, budgets and the paid comparison are right."`
}

// RemotePricing is a provider's list price, per million tokens.
type RemotePricing struct {
	InputPerM  float64 `json:"inputPerM" doc:"$ per 1M uncached input tokens."`
	CachedPerM float64 `json:"cachedPerM" required:"false" doc:"$ per 1M cached input tokens. 0 = same as input."`
	OutputPerM float64 `json:"outputPerM" doc:"$ per 1M output tokens."`
}

type AddRemoteModelInput struct {
	Body RemoteModelSpec
}

var providerName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// AddRemoteModel writes the extension, provider, credential, model and price
// for one remote model in ONE config edit, so a half-configured provider never
// exists. Re-running it with the same provider updates that provider in place
// and adds or replaces the one model; other models of the provider are kept.
func (h *Handlers) AddRemoteModel(ctx context.Context, in *AddRemoteModelInput) (*ConfigMutationOutput, error) {
	out := &ConfigMutationOutput{}
	b := in.Body
	scheme, host, port, basePath, err := splitBaseURL(b.BaseURL)
	if err != nil {
		out.Body.Message = err.Error()
		return out, nil
	}
	if !providerName.MatchString(b.Provider) {
		out.Body.Message = "provider must be lowercase letters, digits and dashes (it prefixes the served name)"
		return out, nil
	}
	modelID := strings.TrimSpace(b.ModelID)
	if modelID == "" {
		out.Body.Message = "model id is required"
		return out, nil
	}
	name := strings.TrimSpace(b.Name)
	if name == "" {
		name = servedSuffix(modelID)
	}
	ext := strings.TrimSpace(b.Extension)
	if ext == "" {
		ext = b.Provider
	}
	typ := strings.TrimSpace(b.Type)
	if typ == "" {
		typ = "chat"
		if b.Pricing != nil {
			typ = b.Provider + "-" + name
		}
	}
	// The secret goes first, as in the provider dialog: config must never
	// reference an entry that does not resolve yet.
	if b.SecretRef != "" && b.SecretValue != "" {
		if err := config.SetSecret(b.SecretRef, b.SecretValue); err != nil {
			out.Body.Message = err.Error()
			return out, nil
		}
	}

	served := b.Provider + "-" + name
	err = h.mutateConfig(ctx, "remote model "+served+" saved", func(c *config.Config) error {
		if c.Extensions == nil {
			c.Extensions = map[string]config.Extension{}
		}
		e := c.Extensions[ext]
		if !e.Proxy.IsZero() || len(e.Provides) > 0 {
			return fmt.Errorf("extension %q uses the single-endpoint shorthand; pick another provider name or edit it in YAML", ext)
		}
		if e.Providers == nil {
			e.Providers = map[string]config.Provider{}
		}
		pv := e.Providers[b.Provider]

		var proxy yaml.Node
		target := map[string]any{"scheme": scheme, "host": host, "port": port, "basePath": basePath}
		if b.API != "" && b.API != config.PresetAPIOpenAI {
			target["api"] = b.API
		}
		if err := proxy.Encode(target); err != nil {
			return err
		}
		pv.Proxy = proxy
		pv.Manual = true

		cred, _ := pv.CredentialNamed(config.DefaultCredentialName)
		cred.Name = config.DefaultCredentialName
		cred.Allow = b.Allow
		cred.Limits = toConfigLimits(b.Limits)
		cred.Headers = nil
		if b.SecretRef != "" {
			header := b.HeaderName
			if header == "" {
				header = "authorization"
			}
			cred.Headers = map[string]string{header: authValue(header, b.SecretRef)}
		}
		pv.Credentials = []config.Credential{cred}

		if pv.Provides == nil {
			pv.Provides = map[string]config.Model{}
		}
		m := pv.Provides[name]
		m.Upstream = modelID
		m.Type = typ
		m.Quality = b.Quality
		m.MaxTokens = b.MaxTokens
		m.MaxConcurrent = b.MaxConcurrent
		pv.Provides[name] = m

		e.Providers[b.Provider] = pv
		c.Extensions[ext] = e

		if b.Pricing != nil {
			if c.CommandCosts == nil {
				c.CommandCosts = map[string]map[string]any{}
			}
			cached := b.Pricing.CachedPerM
			if cached == 0 {
				cached = b.Pricing.InputPerM
			}
			c.CommandCosts[typ] = map[string]any{"extract": map[string]any{
				"processCostFactor":  b.Pricing.InputPerM / 1e6,
				"cachedCostFactor":   cached / 1e6,
				"generateCostFactor": b.Pricing.OutputPerM / 1e6,
			}}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out.Body.OK = true
	out.Body.Message = "callers can ask for " + served
	return out, nil
}

// splitBaseURL turns an OpenAI-style base URL into the host/port/basePath the
// proxy target is written in. corrallm appends /v1/... itself, so a trailing
// /v1 — the form every provider's docs give — is dropped rather than doubled.
func splitBaseURL(raw string) (scheme, host string, port int, basePath string, err error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", "", 0, "", fmt.Errorf("base URL %q is not a URL", raw)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", "", 0, "", fmt.Errorf("base URL must be http(s), got %q", u.Scheme)
	}
	port = 443
	if u.Scheme == "http" {
		port = 80
	}
	if p := u.Port(); p != "" {
		if port, err = strconv.Atoi(p); err != nil {
			return "", "", 0, "", fmt.Errorf("base URL port %q: %w", p, err)
		}
	}
	basePath = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/v1")
	return u.Scheme, u.Hostname(), port, basePath, nil
}

// servedSuffix derives a served-name suffix from a provider's model id:
// `accounts/fireworks/models/deepseek-v4p1-flash` → `deepseek-v4p1-flash`.
func servedSuffix(id string) string {
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
			return r
		}
		return '-'
	}, id)
}

// RemoteModelsPreviewInput asks an endpoint that is not configured yet for its
// model list, so the add-model form can offer ids instead of a blank field.
type RemoteModelsPreviewInput struct {
	Body struct {
		BaseURL     string `json:"baseUrl" doc:"OpenAI-compatible base URL, as for addRemoteModel."`
		API         string `json:"api" required:"false" doc:"openai (default) or anthropic."`
		SecretRef   string `json:"secretRef" required:"false" doc:"Stored key to authenticate with."`
		SecretValue string `json:"secretValue" required:"false" doc:"A key not stored yet. Used for this request only; never stored or returned."`
		HeaderName  string `json:"headerName" required:"false" doc:"Header carrying the key. Default authorization (as Bearer)."`
	}
}

// RemoteModelView is one row of a provider's /v1/models, with the vendor
// extensions that matter when choosing a model.
type RemoteModelView struct {
	ID            string  `json:"id"`
	Name          string  `json:"name" doc:"The provider's label, when it reports one."`
	ContextLength int     `json:"contextLength" doc:"Advertised context window in tokens, 0 when unreported."`
	InputPerM     float64 `json:"inputPerM" doc:"$ per 1M input tokens, 0 when unreported."`
	OutputPerM    float64 `json:"outputPerM" doc:"$ per 1M output tokens, 0 when unreported."`
}

type RemoteModelsPreviewOutput struct {
	Body struct {
		Models []RemoteModelView `json:"models"`
		Error  string            `json:"error" doc:"Why the list could not be read; empty on success."`
	}
}

// PreviewRemoteModels GETs <base>/v1/models with the given key. An admin
// action: it makes the daemon call an arbitrary URL with a credential.
func (h *Handlers) PreviewRemoteModels(ctx context.Context, in *RemoteModelsPreviewInput) (*RemoteModelsPreviewOutput, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	out := &RemoteModelsPreviewOutput{}
	out.Body.Models = []RemoteModelView{}
	b := in.Body
	scheme, host, port, basePath, err := splitBaseURL(b.BaseURL)
	if err != nil {
		out.Body.Error = err.Error()
		return out, nil
	}
	key := b.SecretValue
	if key == "" && b.SecretRef != "" {
		key = config.ExpandSecrets("${" + b.SecretRef + "}")
	}
	headers := map[string]string{}
	if key != "" {
		header := b.HeaderName
		if header == "" {
			header = "authorization"
		}
		v := key
		if strings.EqualFold(header, "authorization") {
			v = "Bearer " + key
		}
		headers[header] = v
	}
	if b.API == config.APIAnthropic {
		headers["anthropic-version"] = "2023-06-01"
	}
	u := fmt.Sprintf("%s://%s:%d%s/v1/models", scheme, host, port, basePath)
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	rows, err := freeroster.FetchCatalog(cctx, &http.Client{Timeout: 15 * time.Second}, u, headers)
	if err != nil {
		out.Body.Error = err.Error()
		return out, nil
	}
	for _, r := range rows {
		out.Body.Models = append(out.Body.Models, RemoteModelView{
			ID: r.ID, Name: r.Name, ContextLength: r.ContextLength,
			InputPerM: r.PromptUSD * 1e6, OutputPerM: r.CompletionUSD * 1e6,
		})
	}
	sort.Slice(out.Body.Models, func(i, j int) bool { return out.Body.Models[i].ID < out.Body.Models[j].ID })
	return out, nil
}
