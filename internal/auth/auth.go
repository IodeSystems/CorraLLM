// Package auth gates corrallm's management surface. A single admin token (read
// from, or generated into, <home>/admin.token) protects everything under /api/
// — the dashboard's GraphQL/REST ops and the load/unload control mutations. The
// OpenAI inference proxy (/v1/…) and the model web UIs (/upstream/…) are NOT
// gated here: those callers authenticate by their own API key (fairshare
// identity), and /health stays open for liveness probes.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// CookieName carries the admin token for browser clients — EventSource (SSE)
// can't set an Authorization header, so the dashboard also sends it as a cookie.
const CookieName = "corrallm_token"

// LoadOrCreateToken returns the admin token at path, generating and writing a
// fresh 256-bit hex token (0600) if the file is missing or empty. The returned
// bool reports whether it was newly created (so the caller can log it once).
func LoadOrCreateToken(path string) (token string, created bool, err error) {
	if b, rerr := os.ReadFile(path); rerr == nil {
		if t := strings.TrimSpace(string(b)); t != "" {
			return t, false, nil
		}
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", false, fmt.Errorf("generate admin token: %w", err)
	}
	t := hex.EncodeToString(raw)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", false, fmt.Errorf("create token dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(t+"\n"), 0o600); err != nil {
		return "", false, fmt.Errorf("write admin token: %w", err)
	}
	return t, true, nil
}

// Middleware gates /api/ requests on the admin token (Authorization: Bearer, or
// the CookieName cookie). All other paths pass through untouched.
func Middleware(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.URL.Path, "/api/") || authorized(r, token) {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="corrallm"`)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"type":"auth","message":"admin token required"}}`))
		})
	}
}

// authorized reports whether r presents the admin token (constant-time compared).
func authorized(r *http.Request, token string) bool {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		if tokenEqual(strings.TrimPrefix(h, "Bearer "), token) {
			return true
		}
	}
	if c, err := r.Cookie(CookieName); err == nil {
		return tokenEqual(c.Value, token)
	}
	return false
}

func tokenEqual(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
