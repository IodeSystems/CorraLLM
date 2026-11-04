// Package metrics is corrallm's Prometheus instrumentation: per-request token,
// cost, and count series (labeled by provider, model, and token class), plus
// per-model residency gauges for uptime. Handler is mounted at /metrics ahead of
// the web-UI SPA so Prometheus scrapes the exposition, not the app shell.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// reg is a private registry so /metrics carries only corrallm's own series
// (no default Go/process collectors) — keeps the scrape focused on cost/usage.
var reg = prometheus.NewRegistry()

var (
	requests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "corrallm_requests_total",
		Help: "Requests served, by provider, model, and HTTP status.",
	}, []string{"provider", "model", "status"})

	tokens = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "corrallm_tokens_total",
		Help: "Tokens metered, by provider, model, and class (cached|processed|generated).",
	}, []string{"provider", "model", "class"})

	cost = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "corrallm_cost_usd_total",
		Help: "Request cost in USD, by provider, model, and class. Phase 1 emits class=\"total\".",
	}, []string{"provider", "model", "class"})

	loopWatched = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "corrallm_loop_watched_total",
		Help: "Streamed chat replies watched for loops, by model.",
	}, []string{"model"})

	loopDetected = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "corrallm_loop_detected_total",
		Help: "Replies found in a loop, by model and kind (exact|cycle|phrase|tool).",
	}, []string{"model", "kind"})

	loopCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "corrallm_loop_count_total",
		Help: "Times round the loop, summed over the replies found in one, by model and kind.",
	}, []string{"model", "kind"})

	loopInterrupted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "corrallm_loop_interrupted_total",
		Help: "Streams cut because the loop count reached the tolerance, by model and kind.",
	}, []string{"model", "kind"})

	modelLoaded = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "corrallm_model_loaded",
		Help: "1 while a model is resident (ready); series absent when not loaded.",
	}, []string{"model"})

	modelLoadTS = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "corrallm_model_load_timestamp_seconds",
		Help: "Unix time a resident model became ready; uptime = time() - this.",
	}, []string{"model"})

	// Spend gauges mirror GET /v1/spend. A figure corrallm does not have is an
	// absent series, never 0, as in the JSON.
	spendEstimated = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "corrallm_spend_estimated_usd",
		Help: "Spend this period in USD: the last reconciled figure plus the ledger after it.",
	}, []string{"provider"})

	spendLimit = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "corrallm_spend_limit_usd",
		Help: "Calendar-month USD limit; absent when there is none.",
	}, []string{"provider"})

	spendRemaining = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "corrallm_spend_remaining_usd",
		Help: "USD left this period, timed budgets included; absent when there is no limit.",
	}, []string{"provider"})

	spendResetsAt = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "corrallm_spend_resets_at_seconds",
		Help: "Unix time the period renews; absent when there is no limit.",
	}, []string{"provider"})

	spendPeriodStart = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "corrallm_spend_period_start_seconds",
		Help: "Unix time the period started.",
	}, []string{"provider"})

	spendReconciledAt = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "corrallm_spend_reconciled_at_seconds",
		Help: "Unix time of the last real figure this period; absent when the figure is an estimate only.",
	}, []string{"provider"})

	spendReconciled = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "corrallm_spend_reconciled",
		Help: "1 when this period has a real figure (source=reconciled), 0 when the ledger alone (source=estimate).",
	}, []string{"provider"})
)

var spendGauges = []*prometheus.GaugeVec{spendEstimated, spendLimit, spendRemaining, spendResetsAt, spendPeriodStart, spendReconciledAt, spendReconciled}

func init() {
	reg.MustRegister(requests, tokens, cost, loopWatched, loopDetected, loopCount, loopInterrupted, modelLoaded, modelLoadTS)
	for _, g := range spendGauges {
		reg.MustRegister(g)
	}
}

// Handler serves the Prometheus exposition for corrallm's registry.
func Handler() http.Handler { return promhttp.HandlerFor(reg, promhttp.HandlerOpts{}) }

// Request counts one served request.
func Request(provider, model, status string) {
	requests.WithLabelValues(provider, model, status).Inc()
}

// Loop counts one watched reply: kind is "" when it was not in a loop.
func Loop(model, kind string, loops int, interrupted bool) {
	loopWatched.WithLabelValues(model).Inc()
	if kind == "" {
		return
	}
	loopDetected.WithLabelValues(model, kind).Inc()
	loopCount.WithLabelValues(model, kind).Add(float64(loops))
	if interrupted {
		loopInterrupted.WithLabelValues(model, kind).Inc()
	}
}

// Tokens adds n tokens of the given class (cached|processed|generated).
func Tokens(provider, model, class string, n int) {
	if n > 0 {
		tokens.WithLabelValues(provider, model, class).Add(float64(n))
	}
}

// Cost adds a dollar cost of the given class (cached|processed|generated|load|audio).
func Cost(provider, model, class string, usd float64) {
	if usd != 0 {
		cost.WithLabelValues(provider, model, class).Add(usd)
	}
}

// Resident is one loaded model, for the residency gauges.
type Resident struct {
	Model      string
	LoadedUnix int64 // seconds; 0 if unknown
}

// Spend is one provider's period, as GET /v1/spend reports it. A nil field
// leaves its series absent.
type Spend struct {
	Provider     string
	EstimatedUSD float64
	LimitUSD     *float64
	RemainingUSD *float64
	ResetsAt     *time.Time
	PeriodStart  time.Time
	ReconciledAt *time.Time // nil when the figure is an estimate only
}

// SetSpend replaces the spend gauges with the current report.
func SetSpend(providers []Spend) {
	for _, g := range spendGauges {
		g.Reset()
	}
	for _, s := range providers {
		p := s.Provider
		spendEstimated.WithLabelValues(p).Set(s.EstimatedUSD)
		spendPeriodStart.WithLabelValues(p).Set(float64(s.PeriodStart.Unix()))
		if s.LimitUSD != nil {
			spendLimit.WithLabelValues(p).Set(*s.LimitUSD)
		}
		if s.RemainingUSD != nil {
			spendRemaining.WithLabelValues(p).Set(*s.RemainingUSD)
		}
		if s.ResetsAt != nil {
			spendResetsAt.WithLabelValues(p).Set(float64(s.ResetsAt.Unix()))
		}
		if s.ReconciledAt != nil {
			spendReconciledAt.WithLabelValues(p).Set(float64(s.ReconciledAt.Unix()))
			spendReconciled.WithLabelValues(p).Set(1)
		} else {
			spendReconciled.WithLabelValues(p).Set(0)
		}
	}
}

// SetResidency replaces the residency gauges with the current resident set.
// Called periodically from a sampler; Reset clears models no longer loaded.
func SetResidency(models []Resident) {
	modelLoaded.Reset()
	modelLoadTS.Reset()
	for _, m := range models {
		modelLoaded.WithLabelValues(m.Model).Set(1)
		if m.LoadedUnix > 0 {
			modelLoadTS.WithLabelValues(m.Model).Set(float64(m.LoadedUnix))
		}
	}
}
