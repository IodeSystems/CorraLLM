package api

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/freeroster"
	"github.com/iodesystems/corrallm/internal/store"
)

// Catalogue browsing: what a provider actually offers on one account.
//
// Discovery answers "what should this provider contribute automatically",
// which is a filter, and everything the filter rejects is dropped in memory
// with no record it ever existed. That leaves no way to reach a specific model
// the filter excluded except by loosening the filter and admitting a hundred
// others with it. This endpoint is the other half: the catalogue as the
// provider reports it, annotated with what corrallm has already decided about
// each row, so a model can be picked by hand.
//
// Fetched live rather than served from the discovery cache. The cache holds
// only the survivors, and the point here is the rows that did not survive.

// CatalogEntryView is one model as the provider reports it, plus what corrallm
// has decided about it.
type CatalogEntryView struct {
	ID            string  `json:"id" doc:"The provider's own model id — what actually goes on the wire."`
	ServedName    string  `json:"servedName" doc:"What it would be called here if enrolled."`
	Name          string  `json:"name" doc:"The provider's human label, when it reports one."`
	Description   string  `json:"description" doc:"The provider's blurb, when it reports one."`
	ContextLength int     `json:"contextLength" doc:"Advertised window, 0 when unreported."`
	Free          bool    `json:"free" doc:"Offered at no cost, by ':free' suffix or zero pricing."`
	PromptUSD     float64 `json:"promptUsd" doc:"Dollars per prompt token, 0 when unpriced."`
	CompletionUSD float64 `json:"completionUsd" doc:"Dollars per completion token, 0 when unpriced."`
	InputModality string  `json:"inputModality" doc:"e.g. text, text+image; empty when unreported."`
	OutputModality string `json:"outputModality" doc:"e.g. text; empty when unreported."`

	// State is the recorded decision, "" when none was ever made.
	State string `json:"state" doc:"approved | rejected | pending, or empty when undecided."`
	// Enrolled says the model is servable RIGHT NOW under this credential —
	// whether it got there by filter or by hand. It is the honest answer to
	// "can I call this", which state alone cannot give: a model can be approved
	// on an account whose credential does not gate anything.
	Enrolled bool `json:"enrolled" doc:"Currently servable on this credential."`
	// PassesFilter marks the rows discovery would take on its own, so the
	// browser distinguishes "you would get this anyway" from "this needs a
	// decision to ever serve".
	PassesFilter bool `json:"passesFilter" doc:"The provider's discover filter would admit this row."`
	// Declared marks a served name the operator wrote into config by hand.
	// Picking it would be a no-op — a declared model always wins — and saying
	// so beats a button that silently does nothing.
	Declared bool `json:"declared" doc:"Already declared in config under this served name."`
	// ConflictsWith is the upstream id ALREADY serving under this row's served
	// name, when it is a different id.
	//
	// Served names are lossy — ServedName drops ":free" and rewrites "/" and
	// ":" — so "vendor/m:free" and "vendor/m" collapse to one name. Picking the
	// paid row of a model whose free row is already enrolled would silently
	// repoint that name at the billable id. That may be exactly what someone
	// wants; it must not happen without them seeing it.
	ConflictsWith string `json:"conflictsWith" doc:"Upstream id currently serving under this served name, when it differs from this row's. Picking this row repoints that name."`
}

type BrowseCatalogInput struct {
	Provider   string `query:"provider" required:"true" doc:"Provider whose catalogue to enumerate."`
	Credential string `query:"credential" required:"false" doc:"Account to fetch it with; catalogues differ by key. Defaults to the provider's single/implicit credential."`
}

type BrowseCatalogOutput struct {
	Body struct {
		Provider   string             `json:"provider"`
		Credential string             `json:"credential"`
		URL        string             `json:"url" doc:"The models endpoint that was queried — the fastest way to see a wrong basePath."`
		Entries    []CatalogEntryView `json:"entries"`
		HasFilter  bool               `json:"hasFilter" doc:"Whether this provider has a discover block at all; without one, passesFilter is meaningless."`
		Error      string             `json:"error" doc:"Why the catalogue could not be read. Entries is empty when set."`
	}
}

// BrowseCatalog fetches one credential's /v1/models and annotates it.
//
// A fetch failure is reported in the body rather than as an HTTP error: the
// caller asked a legitimate question and the answer is "this endpoint would not
// tell me", which a form should render next to the fields that are probably
// wrong — not as a red toast with a status code.
func (h *Handlers) BrowseCatalog(ctx context.Context, in *BrowseCatalogInput) (*BrowseCatalogOutput, error) {
	out := &BrowseCatalogOutput{}
	out.Body.Entries = []CatalogEntryView{}
	out.Body.Provider, out.Body.Credential = in.Provider, in.Credential
	cfg := h.config()
	if cfg == nil {
		out.Body.Error = "no config loaded"
		return out, nil
	}
	cred := in.Credential
	if cred == "" {
		cred = config.DefaultCredentialName
	}
	_, tgt, crd, err := cfg.ProviderTarget(in.Provider, cred)
	if err != nil {
		out.Body.Error = err.Error()
		return out, nil
	}
	out.Body.Credential = cred
	modelsURL := strings.TrimRight(tgt.URL.String(), "/") + tgt.BasePath + "/v1/models"
	out.Body.URL = modelsURL

	// An authTokenCommand credential is not browsable. The token is minted per
	// request deep in the proxy, and the discovery loop does not use it either
	// — so rather than send an unauthenticated request and report the provider's
	// 401 as if the endpoint were wrong, say which of the two it is.
	if len(tgt.Headers) == 0 && crd.AuthTokenCommand != "" {
		out.Body.Error = "this credential mints its token per request (authTokenCommand); its catalogue cannot be browsed. It can still serve."
		return out, nil
	}

	hc := &http.Client{Timeout: 20 * time.Second}
	cat, err := freeroster.FetchCatalog(ctx, hc, modelsURL, tgt.Headers)
	if err != nil {
		out.Body.Error = err.Error()
		return out, nil
	}

	var filter config.DiscoverFilter
	if _, pv, ok := providerOf(cfg, in.Provider); ok && pv.Discover != nil {
		filter, out.Body.HasFilter = pv.Discover.Filter, true
	}

	// One lookup table for decisions rather than a query per row: a catalogue
	// is hundreds of entries and the decision set is tens.
	decided := map[string]store.ModelApproval{}
	if h.Store != nil {
		if rows, err := h.Store.LoadApprovals(); err == nil {
			for _, r := range rows {
				if r.Provider == in.Provider && r.Credential == cred {
					decided[r.Model] = r
				}
			}
		}
	}
	discovered := cfg.Discovered()
	declared := cfg.Models

	for _, e := range cat {
		served := config.ServedName(in.Provider, e.ID)
		dm, isDeclared := declared[served]
		disc, isDiscovered := discovered[served]
		// Whatever currently answers to this served name, if anything.
		holder := ""
		switch {
		case isDeclared:
			holder = dm.Upstream
		case isDiscovered:
			holder = disc.Upstream
		}
		v := CatalogEntryView{
			ID: e.ID, ServedName: served, Name: e.Name, Description: e.Description,
			ContextLength: e.ContextLength, Free: e.Free,
			PromptUSD: e.PromptUSD, CompletionUSD: e.CompletionUSD,
			InputModality: e.InputModality, OutputModality: e.OutputModality,
			PassesFilter: out.Body.HasFilter && filter.Admits(e.ID, e.Free, e.InputModality, e.OutputModality, e.ContextLength),
			Declared:     isDeclared,
			Enrolled:     isDeclared || (isDiscovered && cfg.DiscoveredServableBy(served, cred)),
		}
		if holder != "" && holder != e.ID {
			v.ConflictsWith = holder
		}
		if d, ok := decided[served]; ok {
			v.State = d.State
		}
		out.Body.Entries = append(out.Body.Entries, v)
	}
	// Context descending, matching the order discovery keeps under a limit, so
	// the rows a filter would have taken cluster at the top.
	sort.SliceStable(out.Body.Entries, func(i, j int) bool {
		a, b := out.Body.Entries[i], out.Body.Entries[j]
		if a.ContextLength != b.ContextLength {
			return a.ContextLength > b.ContextLength
		}
		return a.ID < b.ID
	})
	return out, nil
}

// providerOf finds a provider across extensions, mirroring the config-internal
// lookup the pick machinery uses.
func providerOf(cfg *config.Config, provider string) (string, config.Provider, bool) {
	names := make([]string, 0, len(cfg.Extensions))
	for n := range cfg.Extensions {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, en := range names {
		if pv, ok := cfg.Extensions[en].Providers[provider]; ok {
			return en, pv, true
		}
	}
	return "", config.Provider{}, false
}
