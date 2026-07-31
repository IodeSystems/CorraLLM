package api

import (
	"context"

	"github.com/danielgtaylor/huma/v2"
)

// RequestOverheadInput scopes the sum to one caller, one model, one window.
type RequestOverheadInput struct {
	Key    string `query:"key" required:"true" doc:"Caller key whose requests to sum (the bench's own CORRALLM_BENCH_KEY)."`
	Served string `query:"served" required:"true" doc:"Served model name."`
	From   int64  `query:"from" required:"true" doc:"Window start, unix millis, inclusive."`
	To     int64  `query:"to" required:"true" doc:"Window end, unix millis, inclusive."`
}

// RequestOverheadOutput reports what corrallm added to those requests.
type RequestOverheadOutput struct {
	Body struct {
		Requests int   `json:"requests" doc:"Rows summed. 0 means the window matched nothing, which is not the same as matching requests that never waited."`
		QueuedMS int64 `json:"queuedMs" doc:"Total time blocked in admission control."`
		LoadMS   int64 `json:"loadMs" doc:"Total time waiting for a backend to become resident."`
		TotalMS  int64 `json:"totalMs" doc:"queuedMs + loadMs — what to subtract from a wall clock to get execution time."`
	}
}

// RequestOverhead answers "how much of that window was corrallm, not the model".
//
// A benchmark measures wall time, and on a shared box most of it can be queueing
// and cold spawns. The response headers carry the same numbers per request, but
// agentkit's client does not surface headers to its caller and a probe stage is
// an agent loop of many calls — so the useful shape is a sum over a window,
// which is this.
func (h *Handlers) RequestOverhead(ctx context.Context, in *RequestOverheadInput) (*RequestOverheadOutput, error) {
	out := &RequestOverheadOutput{}
	if h.Store == nil {
		return nil, huma.Error503ServiceUnavailable("no store")
	}
	if in.To < in.From {
		return nil, huma.Error400BadRequest("to must not precede from")
	}
	o, err := h.Store.OverheadFor(ctx, in.Key, in.Served, in.From, in.To)
	if err != nil {
		return nil, huma.Error500InternalServerError("sum overhead", err)
	}
	out.Body.Requests = o.Requests
	out.Body.QueuedMS = o.QueuedMS
	out.Body.LoadMS = o.LoadMS
	out.Body.TotalMS = o.QueuedMS + o.LoadMS
	return out, nil
}
