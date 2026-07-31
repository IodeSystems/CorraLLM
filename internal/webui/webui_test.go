package webui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func diskRoot(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// The on-disk root wins when it has content: that is what lets a UI change ship
// without rebuilding the daemon.
func TestDiskRootBeatsEmbedded(t *testing.T) {
	dir := diskRoot(t, map[string]string{"index.html": "ON DISK"})
	embedded := fstest.MapFS{"index.html": {Data: []byte("EMBEDDED")}}

	rec := get(t, Handler(dir, embedded), "/")
	if rec.Code != http.StatusOK || rec.Body.String() != "ON DISK" {
		t.Fatalf("got %d %q, want 200 ON DISK", rec.Code, rec.Body.String())
	}
}

// The whole point of embedding: a binary started somewhere with no ui/dist must
// still serve a UI, because a fresh install is set up THROUGH that UI.
func TestFallsBackToEmbedded(t *testing.T) {
	embedded := fstest.MapFS{"index.html": {Data: []byte("EMBEDDED")}}

	rec := get(t, Handler("/nonexistent/ui/dist", embedded), "/")
	if rec.Code != http.StatusOK || rec.Body.String() != "EMBEDDED" {
		t.Fatalf("got %d %q, want 200 EMBEDDED", rec.Code, rec.Body.String())
	}
}

// An EXISTING but empty --web-root is the common case on a machine that merely
// contains the repo (ui/dist is gitignored and may never have been built).
// Treating that as "the UI is here" is what made every page load a 404.
func TestEmptyDiskRootFallsBackToEmbedded(t *testing.T) {
	dir := diskRoot(t, nil) // exists, no index.html
	embedded := fstest.MapFS{"index.html": {Data: []byte("EMBEDDED")}}

	rec := get(t, Handler(dir, embedded), "/")
	if rec.Code != http.StatusOK || rec.Body.String() != "EMBEDDED" {
		t.Fatalf("got %d %q, want 200 EMBEDDED", rec.Code, rec.Body.String())
	}
}

// A build that embedded only the .gitkeep placeholder has no UI at all. Say so:
// a bare 404 reads as a routing bug and sends people looking in the wrong place.
func TestNoUIAnywhereExplainsItself(t *testing.T) {
	placeholder := fstest.MapFS{".gitkeep": {Data: []byte("")}}

	rec := get(t, Handler("/nonexistent/ui/dist", placeholder), "/")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "make dist") || !strings.Contains(body, "--web-root") {
		t.Errorf("the not-built page must name how to fix it; got:\n%s", body)
	}
}

// Client-side routes are not files. They must resolve to the SPA shell rather
// than 404, or every deep link into the dashboard breaks on reload.
func TestUnknownPathServesSPAShell(t *testing.T) {
	dir := diskRoot(t, map[string]string{"index.html": "SHELL"})

	rec := get(t, Handler(dir, nil), "/config")
	if rec.Code != http.StatusOK || rec.Body.String() != "SHELL" {
		t.Fatalf("got %d %q, want 200 SHELL", rec.Code, rec.Body.String())
	}
}

// Hashed assets are immutable; the shell must always revalidate or a deploy is
// invisible until the browser cache expires.
func TestCacheHeaders(t *testing.T) {
	dir := diskRoot(t, map[string]string{
		"index.html":        "SHELL",
		"assets/app-abc.js": "console.log(1)",
	})
	h := Handler(dir, nil)

	if got := get(t, h, "/assets/app-abc.js").Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("asset Cache-Control = %q", got)
	}
	if got := get(t, h, "/").Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("index Cache-Control = %q", got)
	}
}
