package proxy

import (
	"net/http"
	"strconv"
)

// Timing headers corrallm adds to every proxied response.
//
// They exist so a caller can subtract corrallm's own overhead from its wall
// clock and be left with the model's execution time. That subtraction is the
// only way to benchmark on a box that is also serving real traffic: without it
// a probe's "wall time" moves with the queue depth of whatever else is running,
// and the resulting numbers describe the neighbours rather than the model.
//
// Deliberately NOT a total-execution header. They are written from
// ModifyResponse, which is the last moment a streaming response can still
// accept headers — the body has not been streamed yet, so its duration is
// unknown. A client computes execution as (its own total) − queued − load,
// which works identically for streamed and buffered responses.
const (
	// HeaderQueuedMS is time blocked in admission control, summed across every
	// backend the request queued on before one admitted it.
	HeaderQueuedMS = "X-Corrallm-Queued-Ms"
	// HeaderLoadMS is time spent waiting for a backend to become resident —
	// a cold spawn and its health wait. Zero when the model was already up.
	HeaderLoadMS = "X-Corrallm-Load-Ms"
	// HeaderTTFBMS is time from dispatching upstream to its response headers.
	// Unlike the other two this IS execution: it is the model's prompt-eval
	// latency, offered so a caller can separate that from generation.
	HeaderTTFBMS = "X-Corrallm-Upstream-Ttfb-Ms"
)

// setTimingHeaders stamps the breakdown onto a response.
//
// Always set, including the zeros: a caller that has to distinguish "no queue"
// from "this corrallm is too old to report" needs the header's presence to mean
// something, and an omitted-when-zero header makes those two cases identical.
func setTimingHeaders(h http.Header, queuedMS, loadMS, ttfbMS int64) {
	h.Set(HeaderQueuedMS, strconv.FormatInt(queuedMS, 10))
	h.Set(HeaderLoadMS, strconv.FormatInt(loadMS, 10))
	h.Set(HeaderTTFBMS, strconv.FormatInt(ttfbMS, 10))
}
