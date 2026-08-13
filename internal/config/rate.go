package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Rate is a parsed limits budget: an amount of some dimension allowed per time
// window, e.g. "$20/hr" → {20, 1h}, "600s/min" → {600, 1m}, "100/min" → {100, 1m}.
type Rate struct {
	Amount float64
	Window time.Duration
}

// windowUnits maps a window suffix to its duration.
var windowUnits = map[string]time.Duration{
	"s": time.Second, "sec": time.Second, "second": time.Second,
	"m": time.Minute, "min": time.Minute, "minute": time.Minute,
	"h": time.Hour, "hr": time.Hour, "hour": time.Hour,
	"d": 24 * time.Hour, "day": 24 * time.Hour,
	// A month is 30 DAYS SLIDING, not a calendar month. The budgets underneath
	// are falloff counters with no reset boundary, so "month" names a rate
	// ($200 per 30 days, leaking continuously) rather than a billing cycle that
	// latches at zero on the 1st. Calendar semantics would need a different
	// mechanism; naming this "month" is the honest approximation, and the one
	// that avoids a month-end cliff followed by a thundering burst.
	"mo": 30 * 24 * time.Hour, "month": 30 * 24 * time.Hour,
}

// ParseRate parses a limits spec "<amount>/<window>" for the given dimension
// (cost | dwell | requests). The amount carries a dimension-specific frame: cost
// is "$N", dwell is "Ns" (seconds), requests is a bare count.
func ParseRate(dim, s string) (Rate, error) {
	amountStr, windowStr, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok {
		return Rate{}, fmt.Errorf("rate %q: want <amount>/<window>", s)
	}
	window, ok := windowUnits[strings.ToLower(strings.TrimSpace(windowStr))]
	if !ok {
		return Rate{}, fmt.Errorf("rate %q: unknown window %q", s, windowStr)
	}
	amountStr = strings.TrimSpace(amountStr)
	switch dim {
	case "cost":
		amountStr = strings.TrimPrefix(amountStr, "$")
	case "dwell":
		amountStr = strings.TrimSuffix(amountStr, "s")
	case "requests":
		// bare count
	default:
		return Rate{}, fmt.Errorf("rate %q: unknown dimension %q", s, dim)
	}
	amount, err := strconv.ParseFloat(strings.TrimSpace(amountStr), 64)
	if err != nil {
		return Rate{}, fmt.Errorf("rate %q: %w", s, err)
	}
	return Rate{Amount: amount, Window: window}, nil
}
