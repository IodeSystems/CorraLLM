package api

import (
	"context"
	"net"
	"net/http"
)

// WHO CHANGED THIS, as far as one shared credential can say.
//
// A config revision recorded "edited through the dashboard" and nothing else.
// That is enough to see THAT something changed and useless for the question
// anybody actually opens the page with — "is this my doing?" — which two Lenny
// runs named, the second of them after a change made from this very session:
// the operator saw an unexplained edit timed the same afternoon his box felt
// slow, could not tell whether it was his, and said he would not guess in chat.
//
// There is one admin token, so corrallm CANNOT know a person and must not
// pretend to. What it can record is the door and the address: which endpoint
// made the change, and where the request came from. "changed local-Qwen3.8-27B
// from 127.0.0.1" is not a name, and it is enough for somebody to recognise
// their own machine — or to know it was not them.
type requestSource struct {
	Addr string // the caller's address, as near as the proxy chain allows
}

type sourceKey struct{}

// SourceMiddleware records where a request came from, for anything that later
// writes a config revision.
func SourceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// r.RemoteAddr, read AFTER chi's RealIP has resolved the forwarded
		// header — behind the front proxy every request otherwise appears to
		// come from the proxy. A client can set that header, so this is fine for
		// a note somebody reads and would not be fine for a trust decision;
		// captureConnAddr keeps the true connection address for those.
		addr := r.RemoteAddr
		if host, _, err := net.SplitHostPort(addr); err == nil {
			addr = host
		}
		ctx := context.WithValue(r.Context(), sourceKey{}, requestSource{Addr: addr})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// noteFor builds the sentence stored with a revision: what changed, and from
// where. Without an address it degrades to the description alone rather than
// inventing one.
func noteFor(ctx context.Context, what string) string {
	if what == "" {
		what = "configuration changed"
	}
	if src, ok := ctx.Value(sourceKey{}).(requestSource); ok && src.Addr != "" {
		return what + " — from " + src.Addr
	}
	return what
}
