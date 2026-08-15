package api

import (
	"context"

	"github.com/iodesystems/corrallm/internal/config"
)

// ProviderPresetView is one known endpoint a form can fill itself from.
//
// Served from the daemon rather than baked into the dashboard so the table has
// exactly one home: `validate`, the CLI and the UI all get the same answer, and
// a corrected basePath ships with the binary instead of needing a UI rebuild to
// match.
type ProviderPresetView struct {
	ID          string  `json:"id"`
	Label       string  `json:"label"`
	Group       string  `json:"group" doc:"aggregator | lab | local — buckets the typeahead."`
	Host        string  `json:"host"`
	Port        int     `json:"port"`
	BasePath    string  `json:"basePath" doc:"Path prefix in front of /v1. Empty for endpoints mounted at root."`
	API         string  `json:"api" doc:"Wire format. Only 'openai' is handled today."`
	AuthHeader  string  `json:"authHeader"`
	AuthScheme  string  `json:"authScheme"`
	SecretRef   string  `json:"secretRef" doc:"Suggested credential-store entry name."`
	NeedsSecret bool    `json:"needsSecret" doc:"False for local endpoints, which take no key."`
	Catalog     string  `json:"catalog" doc:"public = /v1/models answers without a key, so it can be browsed before one is stored; authed = it cannot."`
	Docs        string  `json:"docs"`
	Notes       string  `json:"notes"`
	FreeOnly    bool    `json:"freeOnly" doc:"Suggested discovery filter: keep only free rows."`
	MinContext  int     `json:"minContext" doc:"Suggested discovery filter: minimum advertised window."`
	Limit       int     `json:"limit" doc:"Suggested cap on how many models this provider contributes."`
	Quality     float64 `json:"quality" doc:"Rank applied to discovered models until one is approved with its own."`
}

type ProviderPresetsOutput struct {
	Body struct {
		Presets []ProviderPresetView `json:"presets"`
	}
}

// ListProviderPresets returns the known-endpoint table.
func (h *Handlers) ListProviderPresets(_ context.Context, _ *struct{}) (*ProviderPresetsOutput, error) {
	out := &ProviderPresetsOutput{}
	out.Body.Presets = []ProviderPresetView{}
	for _, p := range config.ProviderPresets() {
		out.Body.Presets = append(out.Body.Presets, ProviderPresetView{
			ID: p.ID, Label: p.Label, Group: p.Group, Host: p.Host, Port: p.Port,
			BasePath: p.BasePath, API: p.API, AuthHeader: p.AuthHeader,
			AuthScheme: p.AuthScheme, SecretRef: p.SecretRef, NeedsSecret: p.NeedsSecret,
			Catalog: p.Catalog, Docs: p.Docs, Notes: p.Notes,
			FreeOnly: p.Filter.Free, MinContext: p.Filter.MinContext, Limit: p.Filter.Limit,
			// Discovery's uniform guess. Not a preset field: it is a statement
			// about this deployment's other models, not about the vendor.
			Quality: 3,
		})
	}
	return out, nil
}
