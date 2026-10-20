package proxy

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

// Speed sources, stored in activity.speed_source. "" is the backend's own
// timings object (llama-server).
const (
	speedProvider = "provider" // the upstream's timing headers
	speedProxy    = "proxy"    // corrallm's clock: includes network and the provider's queue
)

// upstreamSpeeds fills prefill and generation speed for a response that
// carried no llama.cpp timings — a remote API. It prefers the provider's own
// timing headers and falls back to corrallm's clock, and says which it used.
//
// Prefill speed is uncached prompt tokens over time to first token. That time
// includes the provider's queue (no provider reports queue separately), so it
// is a lower bound on the provider's prefill rate.
//
// Generation speed is completion tokens over the time after the first token.
// A non-streamed reply arrives in one write, so without provider headers its
// split is unknown and both speeds stay 0.
func upstreamSpeeds(u usage, h http.Header, streaming bool, upStart, first, last time.Time) (usage, string) {
	uncached := max(u.PromptTokens-u.CachedTokens, 0)
	source := ""
	ttft := headerSeconds(h, "Fireworks-Server-Time-To-First-Token")
	total := headerSeconds(h, "Fireworks-Server-Processing-Time")
	if ttft > 0 {
		source = speedProvider
		if uncached > 0 {
			u.PromptPerSec = float64(uncached) / ttft
		}
		if total > ttft && u.CompletionTokens > 0 {
			u.PredictedPerSec = float64(u.CompletionTokens) / (total - ttft)
		}
	}
	if !streaming || first.IsZero() || upStart.IsZero() {
		return u, source
	}
	if u.PromptPerSec == 0 && uncached > 0 {
		if d := first.Sub(upStart).Seconds(); d > 0 {
			u.PromptPerSec = float64(uncached) / d
			source = speedProxy
		}
	}
	// The first streamed event carries one token; the rest arrive after it.
	if u.PredictedPerSec == 0 && u.CompletionTokens > 1 {
		if d := last.Sub(first).Seconds(); d > 0 {
			u.PredictedPerSec = float64(u.CompletionTokens-1) / d
			if source == "" {
				source = speedProxy
			}
		}
	}
	return u, source
}

func headerSeconds(h http.Header, name string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(h.Get(name)), 64)
	if err != nil || v <= 0 {
		return 0
	}
	return v
}

// keptHeaderPrefixes are the upstream response headers stored with a payload:
// provider timing and token accounting, rate-limit state, and request ids to
// quote to a provider's support. Nothing else — a response header can carry a
// cookie or a signed URL.
var keptHeaderPrefixes = []string{
	"fireworks-",
	"x-ratelimit-",
	"anthropic-ratelimit-",
	"openai-processing-ms",
	"x-request-id",
	"request-id",
	"retry-after",
}

// keptHeaders renders the allowlisted headers as sorted "Name: value" lines.
func keptHeaders(h http.Header) string {
	var lines []string
	for name, vals := range h {
		lower := strings.ToLower(name)
		for _, p := range keptHeaderPrefixes {
			if strings.HasPrefix(lower, p) {
				for _, v := range vals {
					lines = append(lines, lower+": "+v)
				}
				break
			}
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// isRemote is config.Model.Remote, a variable so a test can treat a loopback
// fake as a remote API.
var isRemote = func(m config.Model) bool { return m.Remote() }

// retryAfterSecs renders a wait as a Retry-After value: whole seconds,
// rounded up, at least 1.
func retryAfterSecs(d time.Duration) int {
	secs := int((d + time.Second - 1) / time.Second)
	return max(secs, 1)
}
