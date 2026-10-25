package main

import (
	"context"
	"net"
	"net/http"

	"github.com/iodesystems/corrallm/internal/auth"
	"github.com/iodesystems/corrallm/internal/config"
)

// connAddrKey carries the address the TCP connection actually came from.
type connAddrKey struct{}

// captureConnAddr stashes r.RemoteAddr before middleware.RealIP replaces it.
//
// RealIP rewrites RemoteAddr from X-Forwarded-For / X-Real-IP / True-Client-IP
// whether or not the deployment sets those headers, so downstream RemoteAddr is
// caller-controlled. That is fine for logging, where a spoofed value costs
// nothing, and unusable for a trust decision — a remote client could claim to be
// 127.0.0.1 simply by sending the header. Anything gating on locality must read
// what was observed before the rewrite.
func captureConnAddr(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), connAddrKey{}, r.RemoteAddr)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// localCaller reports whether the request arrived over loopback or a Unix
// socket — that is, from the machine corrallm is running on.
//
// Deliberately loopback ONLY, not "private ranges": the daemon binds 0.0.0.0 and
// serves a LAN, and every other host on that LAN is a client, not an operator.
func localCaller(r *http.Request) bool {
	addr, _ := r.Context().Value(connAddrKey{}).(string)
	if addr == "" {
		// No captured address means the middleware did not run — a test, or a
		// future refactor that drops it. Withhold rather than guess.
		return false
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// A Unix socket's RemoteAddr has no port; it is by definition local.
		host = addr
	}
	if host == "" || host == "@" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// forwardingHeaders mark a request that came through a proxy. The front proxy
// always adds one, and its own address is on the LAN, so the TCP peer alone
// cannot tell public traffic from a LAN client.
var forwardingHeaders = []string{"X-Forwarded-For", "X-Real-Ip", "True-Client-Ip", "Forwarded", "Cf-Connecting-Ip", "X-Forwarded-Host"}

// requestFiler describes a request filing an access request to the config's
// accessRequests rules. The dashboard user counts only for a same-origin
// request: a cross-site page can make a signed-in browser POST a plain-text
// body without a preflight, and must not file in that user's name.
func requestFiler(sso *auth.SSO) func(r *http.Request) config.Filer {
	return func(r *http.Request) config.Filer {
		var f config.Filer
		for _, h := range forwardingHeaders {
			if r.Header.Get(h) != "" {
				f.Forwarded = true
			}
		}
		addr, _ := r.Context().Value(connAddrKey{}).(string)
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		if host == "" || host == "@" {
			f.Peer = net.IPv6loopback // a Unix socket is this machine
		} else {
			f.Peer = net.ParseIP(host)
		}
		if sso != nil && r.Header.Get("Sec-Fetch-Site") == "same-origin" {
			if id, ok := sso.Session(r); ok {
				f.User = id.Email
			}
		}
		return f
	}
}
