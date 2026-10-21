package api

import (
	"context"
	"sort"
	"strings"
	"time"
)

// Provider billing reconciliation — read-only view of what each provider's own
// bill says next to what corrallm metered (internal/billing). The numbers come
// from the last hourly fetch, not a live call.

// BillingModelView is one upstream model's month and today, billed vs recorded.
type BillingModelView struct {
	Upstream         string  `json:"upstream" doc:"The provider's own model id, as on its bill."`
	Served           string  `json:"served" doc:"corrallm model(s) mapped to it; empty when none is configured."`
	BilledMonthUSD   float64 `json:"billedMonthUsd" doc:"Dollars the provider billed this UTC month."`
	RecordedMonthUSD float64 `json:"recordedMonthUsd" doc:"Dollars corrallm recorded for the mapped models this UTC month."`
	BilledTodayUSD   float64 `json:"billedTodayUsd" doc:"Dollars billed today (UTC)."`
	RecordedTodayUSD float64 `json:"recordedTodayUsd" doc:"Dollars corrallm recorded today (UTC)."`
}

// BillingDayView is one UTC day, billed vs recorded.
type BillingDayView struct {
	Day         string  `json:"day" doc:"UTC date, YYYY-MM-DD."`
	BilledUSD   float64 `json:"billedUsd"`
	RecordedUSD float64 `json:"recordedUsd"`
}

// ProviderBillingView is one credential's reconciliation.
type ProviderBillingView struct {
	Provider         string             `json:"provider"`
	Credential       string             `json:"credential"`
	FetchedMS        int64              `json:"fetchedMs" doc:"Unix millis of the last attempt to read the provider's bill."`
	LastOKMS         int64              `json:"lastOkMs" doc:"Unix millis of the last successful read; 0 = never."`
	Error            string             `json:"error" doc:"Why the last attempt failed; empty when it worked."`
	Settling         bool               `json:"settling" doc:"The provider says its latest hours may still be revised, so the newest billed figures can rise."`
	Attributable     bool               `json:"attributable" doc:"False when the provider has several credentials: recorded cost cannot be told apart by account, so recorded columns read 0."`
	BilledMonthUSD   float64            `json:"billedMonthUsd"`
	RecordedMonthUSD float64            `json:"recordedMonthUsd"`
	BilledTodayUSD   float64            `json:"billedTodayUsd"`
	RecordedTodayUSD float64            `json:"recordedTodayUsd"`
	Models           []BillingModelView `json:"models"`
	Days             []BillingDayView   `json:"days"`
}

// ProviderBillingOutput lists every reconciled credential.
type ProviderBillingOutput struct {
	Body struct {
		Providers []ProviderBillingView `json:"providers"`
	}
}

// ProviderBilling reports billed-vs-recorded spend for providers corrallm can
// read a bill from (Fireworks).
func (h *Handlers) ProviderBilling(_ context.Context, _ *struct{}) (*ProviderBillingOutput, error) {
	out := &ProviderBillingOutput{}
	out.Body.Providers = []ProviderBillingView{}
	if h.Store == nil {
		return out, nil
	}
	now := time.Now().UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	fetches, err := h.Store.BillingFetches()
	if err != nil {
		return nil, err
	}
	rows, err := h.Store.BillingRows(month.UnixMilli())
	if err != nil {
		return nil, err
	}
	// Whether recorded cost is attributable is a config fact: one credential.
	single := map[string]bool{}
	if cfg := h.config(); cfg != nil {
		for _, ext := range cfg.Extensions {
			for pn, pv := range ext.Providers {
				single[pn] = len(pv.CredentialList()) == 1
			}
		}
	}
	for _, f := range fetches {
		v := ProviderBillingView{Provider: f.Provider, Credential: f.Credential, FetchedMS: f.FetchedMS,
			LastOKMS: f.OKMS, Error: f.Error, Settling: f.Incomplete, Attributable: single[f.Provider],
			Models: []BillingModelView{}, Days: []BillingDayView{}}
		models := map[string]*BillingModelView{}
		days := map[string]*BillingDayView{}
		for _, r := range rows {
			if r.Provider != f.Provider || r.Credential != f.Credential {
				continue
			}
			isToday := r.HourMS >= today.UnixMilli()
			m := models[r.Upstream]
			if m == nil {
				m = &BillingModelView{Upstream: r.Upstream}
				models[r.Upstream] = m
			}
			if r.Served != "" {
				m.Served = r.Served
			}
			m.BilledMonthUSD += r.BilledUSD
			m.RecordedMonthUSD += r.RecordedUSD
			dk := time.UnixMilli(r.HourMS).UTC().Format("2006-01-02")
			d := days[dk]
			if d == nil {
				d = &BillingDayView{Day: dk}
				days[dk] = d
			}
			d.BilledUSD += r.BilledUSD
			d.RecordedUSD += r.RecordedUSD
			v.BilledMonthUSD += r.BilledUSD
			v.RecordedMonthUSD += r.RecordedUSD
			if isToday {
				m.BilledTodayUSD += r.BilledUSD
				m.RecordedTodayUSD += r.RecordedUSD
				v.BilledTodayUSD += r.BilledUSD
				v.RecordedTodayUSD += r.RecordedUSD
			}
		}
		for _, m := range models {
			v.Models = append(v.Models, *m)
		}
		sort.Slice(v.Models, func(i, j int) bool {
			if v.Models[i].BilledMonthUSD != v.Models[j].BilledMonthUSD {
				return v.Models[i].BilledMonthUSD > v.Models[j].BilledMonthUSD
			}
			return strings.Compare(v.Models[i].Upstream, v.Models[j].Upstream) < 0
		})
		for _, d := range days {
			v.Days = append(v.Days, *d)
		}
		sort.Slice(v.Days, func(i, j int) bool { return v.Days[i].Day > v.Days[j].Day })
		out.Body.Providers = append(out.Body.Providers, v)
	}
	return out, nil
}
