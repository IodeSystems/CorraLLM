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

// The provider directory: what a provider actually offers on one account.
//
// This is the PRIMARY way models get into corrallm. A discover filter is the
// bulk shortcut — right for a small catalogue or a churning free roster — but
// against a provider listing four hundred models a filter is a guess, and
// everything it rejects is dropped in memory with no record it existed. The
// directory is the catalogue as the provider reports it, annotated with what
// has already been chosen, so an operator can pick the models they actually
// want and say where each one goes.
//
// Fetched live rather than served from the discovery cache. The cache holds
// only what a filter admitted, and the point here is everything else.

// CatalogEntryView is one model as the provider reports it, plus what corrallm
// already knows about it.
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

	// Assigned says the operator chose this row: there is a selection for it.
	Assigned bool `json:"assigned" doc:"Selected off this directory by the operator."`
	// Lanes/Quality echo that selection's placement, so the directory can show
	// where an assigned model went without a second round trip.
	Lanes   []LaneRefView `json:"lanes" doc:"Lanes this assignment placed it in."`
	Quality float64       `json:"quality" doc:"Rank recorded with the assignment, 0 when none."`
	// Enrolled says the model is servable RIGHT NOW under this credential —
	// whether it got there by filter or by assignment. It is the honest answer
	// to "can I call this", which Assigned alone cannot give: a filter
	// contributes models nobody assigned.
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

	// One lookup table for selections rather than a query per row: a directory
	// is hundreds of entries and the selection set is tens.
	chosen := map[string]store.ModelSelection{}
	if h.Store != nil {
		if rows, err := h.Store.LoadSelections(); err == nil {
			for _, r := range rows {
				if r.Provider == in.Provider && r.Credential == cred {
					chosen[r.Model] = r
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
			Lanes:        []LaneRefView{},
			PassesFilter: out.Body.HasFilter && filter.Admits(e.ID, e.Free, e.InputModality, e.OutputModality, e.ContextLength),
			Declared:     isDeclared,
			Enrolled:     isDeclared || (isDiscovered && cfg.DiscoveredServableBy(served, cred)),
		}
		if holder != "" && holder != e.ID {
			v.ConflictsWith = holder
		}
		if sel, ok := chosen[served]; ok {
			v.Assigned = true
			v.Quality = sel.Quality
			for _, l := range sel.Lanes {
				v.Lanes = append(v.Lanes, LaneRefView{Lane: l.Lane, Order: l.Order})
			}
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
