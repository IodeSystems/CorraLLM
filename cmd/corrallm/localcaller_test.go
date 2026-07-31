package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The token path is disclosed on the strength of localCaller, so a spoofable
// answer here is an information leak on an internet-fronted daemon. The
// captured address must be the one the connection arrived on, NOT the one
// middleware.RealIP derives from caller-supplied headers.
func TestLocalCallerIgnoresForwardedHeaders(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		headers    map[string]string
		want       bool
	}{
		{"loopback", "127.0.0.1:51234", nil, true},
		{"loopback v6", "[::1]:51234", nil, true},
		{"lan client", "192.168.1.42:51234", nil, false},
		{"public client", "203.0.113.7:443", nil, false},
		{
			// The attack: a remote caller claiming to be local.
			name:       "remote spoofing X-Forwarded-For",
			remoteAddr: "203.0.113.7:443",
			headers:    map[string]string{"X-Forwarded-For": "127.0.0.1"},
			want:       false,
		},
		{
			name:       "remote spoofing X-Real-IP",
			remoteAddr: "203.0.113.7:443",
			headers:    map[string]string{"X-Real-IP": "127.0.0.1"},
			want:       false,
		},
		{
			// A LAN host behind the reverse proxy is still not the server.
			name:       "proxied lan client",
			remoteAddr: "192.168.1.42:51234",
			headers:    map[string]string{"X-Forwarded-For": "127.0.0.1"},
			want:       false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got bool
			h := captureConnAddr(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got = localCaller(r)
			}))
			r := httptest.NewRequest(http.MethodGet, "/health", nil)
			r.RemoteAddr = tt.remoteAddr
			for k, v := range tt.headers {
				r.Header.Set(k, v)
			}
			h.ServeHTTP(httptest.NewRecorder(), r)
			if got != tt.want {
				t.Errorf("localCaller = %v, want %v", got, tt.want)
			}
		})
	}
}

// Without the middleware there is no observed address, and guessing would mean
// guessing in the permissive direction.
func TestLocalCallerWithoutMiddlewareWithholds(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/health", nil)
	r.RemoteAddr = "127.0.0.1:51234"
	if localCaller(r) {
		t.Error("localCaller = true with no captured address; must withhold")
	}
}
