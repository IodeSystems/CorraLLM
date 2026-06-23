package auth

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestLoadOrCreateToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "admin.token")

	tok, created, err := LoadOrCreateToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if !created || len(tok) != 64 { // 32 bytes hex
		t.Fatalf("first load: created=%v len=%d", created, len(tok))
	}
	// Second load returns the same token, not created.
	tok2, created2, err := LoadOrCreateToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if created2 || tok2 != tok {
		t.Fatalf("second load: created=%v same=%v", created2, tok2 == tok)
	}
}

func TestMiddleware(t *testing.T) {
	const token = "secret-token"
	h := Middleware(token)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	do := func(path string, set func(*http.Request)) int {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if set != nil {
			set(r)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}

	// Ungated paths pass without a token.
	for _, p := range []string{"/v1/chat/completions", "/upstream/m/", "/health", "/"} {
		if code := do(p, nil); code != http.StatusOK {
			t.Errorf("%s without token = %d, want 200 (ungated)", p, code)
		}
	}

	// /api/ requires the token.
	if code := do("/api/v1/overview", nil); code != http.StatusUnauthorized {
		t.Errorf("/api without token = %d, want 401", code)
	}
	if code := do("/api/v1/overview", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }); code != http.StatusOK {
		t.Errorf("/api with bearer = %d, want 200", code)
	}
	if code := do("/api/v1/overview", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: CookieName, Value: token}) }); code != http.StatusOK {
		t.Errorf("/api with cookie = %d, want 200", code)
	}
	if code := do("/api/v1/overview", func(r *http.Request) { r.Header.Set("Authorization", "Bearer wrong") }); code != http.StatusUnauthorized {
		t.Errorf("/api with wrong token = %d, want 401", code)
	}
}
