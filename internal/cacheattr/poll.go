package cacheattr

import (
	"context"
	"time"
)

// Target is one running backend the poller reads.
type Target struct {
	Served   string
	Instance string
	BaseURL  string
}

// Poller reads each instance's prompt-cache counters from /metrics every
// interval (never per request) and keeps an hour of them.
type Poller struct{}

// NewPoller makes a poller over the targets the function returns.
func NewPoller(targets func() []Target, every time.Duration) *Poller { return &Poller{} }

// Run polls until ctx ends.
func (p *Poller) Run(ctx context.Context) {}

// EvictionsBetween implements Evictions.
func (p *Poller) EvictionsBetween(instance string, fromMS, toMS int64) int { return -1 }

// Latest returns the newest counters of a served model's instances; ok false
// when none has any.
func (p *Poller) Latest(served string) (entries, bytes int64, evictionsPerHour float64, ok bool) {
	return 0, 0, 0, false
}
