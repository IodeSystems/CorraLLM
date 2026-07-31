package auth

import (
	"net/http"
	"time"
)

// cookieMaxAge is how long the mirrored SSE cookie lives.
//
// Long, because the admin token it mirrors does not expire and the dashboard
// keeps it in localStorage indefinitely. A SHORTER life is what caused the bug
// this exists to fix: the cookie was written with no Max-Age at all, so it died
// with the browser session while localStorage survived, and the dashboard came
// back signed in with a live stream that could only 401.
const cookieMaxAge = 365 * 24 * time.Hour

// refreshCookie mirrors the admin token into the SSE cookie when the request
// authenticated some other way (a Bearer header) and the cookie is missing or
// stale.
//
// Not HttpOnly: the dashboard clears this cookie from JS on sign-out, and a
// cookie it cannot delete would leave the next user of a shared browser with a
// working event stream after the UI says they are signed out. The token is
// already in localStorage, which JS can read anyway, so HttpOnly would protect
// nothing that is not already exposed.
func refreshCookie(w http.ResponseWriter, r *http.Request, token string) {
	if c, err := r.Cookie(CookieName); err == nil && tokenEqual(c.Value, token) {
		return // already in step
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(cookieMaxAge.Seconds()),
		SameSite: http.SameSiteStrictMode,
		// Secure only over TLS: a Secure cookie is DROPPED on plain http, which
		// would silently break every local `http://box:8111` dashboard. The
		// forwarded header covers the reverse-proxied deployment, where the
		// connection corrallm sees is plaintext but the browser's is not.
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
}
