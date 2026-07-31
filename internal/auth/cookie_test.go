package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

const tok = "sekrit-admin-token"

func serve(t *testing.T, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	Middleware(tok)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, r)
	return rec
}

func cookieFrom(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == CookieName {
			return c
		}
	}
	return nil
}

// The dashboard authenticates with a Bearer header but EventSource cannot send
// one, so the SSE stream is cookie-only. When the cookie is missing the server
// must re-issue it, or a browser that dropped it stays signed in with a live
// stream that can only 401 — with nothing on screen to explain why.
func TestBearerRequestReissuesMissingCookie(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/overview", nil)
	r.Header.Set("Authorization", "Bearer "+tok)

	rec := serve(t, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	c := cookieFrom(rec)
	if c == nil {
		t.Fatal("no cookie re-issued for a Bearer request that had none")
	}
	if c.Value != tok {
		t.Errorf("cookie value = %q, want the admin token", c.Value)
	}
	// The whole bug was a session cookie outliving nothing while localStorage
	// outlived everything.
	if c.MaxAge <= 0 {
		t.Errorf("MaxAge = %d — a session cookie is exactly the bug this fixes", c.MaxAge)
	}
}

// Re-issuing on every request would be pointless Set-Cookie noise on a client
// that is already in step.
func TestNoCookieReissuedWhenAlreadyInStep(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/overview", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: tok})

	if c := cookieFrom(serve(t, r)); c != nil {
		t.Errorf("re-issued a cookie that already matched: %+v", c)
	}
}

// A stale cookie must be replaced, not left to keep failing.
func TestStaleCookieIsReplaced(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/overview", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: "an-old-token"})

	c := cookieFrom(serve(t, r))
	if c == nil || c.Value != tok {
		t.Fatalf("stale cookie not replaced: %+v", c)
	}
}

// Secure is conditional: a Secure cookie is DROPPED over plain http, which
// would silently break every local http://box:8111 dashboard.
func TestSecureOnlyWhenTheBrowsersConnectionIsTLS(t *testing.T) {
	for _, tc := range []struct {
		name       string
		forwarded  string
		wantSecure bool
	}{
		{"plain http", "", false},
		{"behind a TLS reverse proxy", "https", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/overview", nil)
			r.Header.Set("Authorization", "Bearer "+tok)
			if tc.forwarded != "" {
				r.Header.Set("X-Forwarded-Proto", tc.forwarded)
			}
			c := cookieFrom(serve(t, r))
			if c == nil {
				t.Fatal("no cookie issued")
			}
			if c.Secure != tc.wantSecure {
				t.Errorf("Secure = %v, want %v", c.Secure, tc.wantSecure)
			}
		})
	}
}

// An unauthenticated request must never be handed a valid cookie.
func TestUnauthorizedRequestGetsNoCookie(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/overview", nil)
	rec := serve(t, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if c := cookieFrom(rec); c != nil {
		t.Fatalf("handed a cookie to an unauthenticated caller: %+v", c)
	}
}
