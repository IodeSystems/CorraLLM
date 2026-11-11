package api

import (
	"context"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/iodesystems/corrallm/internal/cacheattr"
)

// CacheReportInput is one model and a window, as the model page's time
// control sends it.
type CacheReportInput struct {
	Served  string `query:"served" required:"true" doc:"The served model."`
	Minutes int    `query:"minutes" default:"1440" minimum:"1" doc:"Relative window, minutes back from now. Ignored when from/to is set."`
	FromMS  int64  `query:"from" doc:"Absolute window start, epoch ms (inclusive). Overrides minutes."`
	ToMS    int64  `query:"to" doc:"Absolute window end, epoch ms (exclusive). Overrides minutes."`
}

// CacheReportOutput is the prompt-cache attribution of the window (P75).
type CacheReportOutput struct {
	Body cacheattr.Report
}

// CacheReport says why the model's prompt cache missed, and what to change.
func (h *Handlers) CacheReport(_ context.Context, in *CacheReportInput) (*CacheReportOutput, error) {
	if h.Proxy == nil || h.Store == nil {
		return nil, huma.Error503ServiceUnavailable("proxy unavailable")
	}
	w := windowOf(in.FromMS, in.ToMS, in.Minutes)
	to := w.ToMS
	if to == 0 {
		to = time.Now().UnixMilli()
	}
	rep := cacheattr.BuildReport(in.Served, w.FromMS, to, nil, nil, cacheattr.Settings{}, "")
	return &CacheReportOutput{Body: rep}, nil
}
