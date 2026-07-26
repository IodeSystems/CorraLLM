// Package metrics is corrallm's Prometheus instrumentation: per-request token,
// cost, and count series (labeled by provider, model, and token class), plus
// per-model residency gauges for uptime. Handler is mounted at /metrics ahead of
// the web-UI SPA so Prometheus scrapes the exposition, not the app shell.
package metrics

import (
	"net/http"

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

	modelLoaded = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "corrallm_model_loaded",
		Help: "1 while a model is resident (ready); series absent when not loaded.",
	}, []string{"model"})

	modelLoadTS = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "corrallm_model_load_timestamp_seconds",
		Help: "Unix time a resident model became ready; uptime = time() - this.",
	}, []string{"model"})
)

func init() {
	reg.MustRegister(requests, tokens, cost, modelLoaded, modelLoadTS)
}

// Handler serves the Prometheus exposition for corrallm's registry.
func Handler() http.Handler { return promhttp.HandlerFor(reg, promhttp.HandlerOpts{}) }

// Request counts one served request.
func Request(provider, model, status string) {
	requests.WithLabelValues(provider, model, status).Inc()
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
