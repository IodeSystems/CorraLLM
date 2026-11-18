package proxy

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"syscall"
)

// localTransport carries requests to LOCAL backends. It is the default
// transport plus one retry for the stale keep-alive race.
//
// THE RACE. llama-server closes the socket right after a streamed reply and
// does not say so (no `Connection: close`; measured 11-18 on Swift: the
// non-streamed reply keeps the connection, the streamed one is closed within
// 0 ms). net/http has already put the connection back in its idle pool when the
// last chunk arrived; a request sent in the next few milliseconds takes it,
// writes, and reads EOF. 11-15..11-17 that was 204 requests answered 503
// backend_restarting (502 before 215be68), nearly every one within 5 ms of
// another reply from the same backend; life-raglit, which sends its next
// request the moment a reply lands, had most of them.
//
// net/http retries this itself only for idempotent methods, and these are
// POSTs. Sending the request again is safe here because all of these hold:
//   - the connection was REUSED (a fresh one that closes is a backend going
//     down — that stays a 503, see backendRestarting);
//   - the error is EOF or reset, which arrives before any byte of a reply: the
//     transport returns no response, so nothing reached the caller;
//   - the server closed the socket before the request reached it, so nothing
//     reached the model;
//   - the body can be replayed (GetBody, set where the body is buffered).
//
// The retry dials a NEW connection, so it cannot land on another stale one.
// Remote APIs do not use this: a paid upstream that read the request before it
// closed could bill it twice.
var localTransport http.RoundTripper = staleConnRetry{
	base:  http.DefaultTransport,
	fresh: freshConnTransport(),
}

func freshConnTransport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DisableKeepAlives = true
	return t
}

type staleConnRetry struct {
	base, fresh http.RoundTripper
}

func (t staleConnRetry) RoundTrip(req *http.Request) (*http.Response, error) {
	reused := false
	trace := &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) { reused = i.Reused }}
	resp, err := t.base.RoundTrip(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	if err == nil || !reused || !staleConnErr(err) || req.Context().Err() != nil {
		return resp, err
	}
	retry := req.Clone(req.Context())
	if req.Body != nil && req.Body != http.NoBody {
		if req.GetBody == nil {
			return resp, err
		}
		body, gerr := req.GetBody()
		if gerr != nil {
			return resp, err
		}
		retry.Body = body
	}
	return t.fresh.RoundTrip(retry)
}

// staleConnErr is the transport error of a connection the server closed
// before it sent anything back.
func staleConnErr(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET)
}
