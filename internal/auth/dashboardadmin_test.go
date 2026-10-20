package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDashboardAdminByTokenCookie(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if DashboardAdmin(r, "tok", nil) {
		t.Fatal("no cookie: want false")
	}
	r.Header.Set("Authorization", "Bearer tok")
	if DashboardAdmin(r, "tok", nil) {
		t.Fatal("Bearer is the caller key slot, not a dashboard credential: want false")
	}
	r.AddCookie(&http.Cookie{Name: CookieName, Value: "tok"})
	if !DashboardAdmin(r, "tok", nil) {
		t.Fatal("admin cookie: want true")
	}
	disabled := &SSO{TokenEnabled: func() bool { return false }}
	if DashboardAdmin(r, "tok", disabled) {
		t.Fatal("admin token disabled: want false")
	}
}
