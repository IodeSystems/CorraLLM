package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A revision has to say what changed and where it came from. It used to say
// "edited through the dashboard" for every edit ever made — which tells an
// operator that something changed and not whether it was theirs, the question
// they open the page with (Lenny runs 4 and 6).
func TestNoteCarriesWhatChangedAndFromWhere(t *testing.T) {
	var got string
	h := SourceMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = noteFor(r.Context(), "model local-Qwen3.8-27B saved")
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/graphql", nil)
	req.RemoteAddr = "192.168.1.76:54321"
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !strings.Contains(got, "model local-Qwen3.8-27B saved") {
		t.Errorf("note lost what changed: %q", got)
	}
	// The port is noise — it is different on every request from the same person.
	if !strings.Contains(got, "192.168.1.76") || strings.Contains(got, "54321") {
		t.Errorf("note should carry the host and not the port: %q", got)
	}
}

// Not every write has a request behind it: an agent adopting its own endpoints
// and a probe recording what it measured both edit the config. Those must not
// borrow an address they do not have — a made-up origin is worse than none,
// because it reads as a person.
func TestNoteWithoutARequestSaysOnlyWhatChanged(t *testing.T) {
	got := noteFor(context.Background(), "capabilities recorded for local-Qwen3.8-27B (by corrallm, from a probe)")
	if strings.Contains(got, "from ") && strings.Contains(got, ".") {
		// "from a probe" is part of the description; an ADDRESS would follow the
		// em dash this function adds.
		if strings.Contains(got, "—") {
			t.Errorf("no request means no address: %q", got)
		}
	}
	if got == "" {
		t.Error("a note is never empty")
	}
}

// An empty description still produces something readable rather than an address
// on its own, which would say where without saying what.
func TestNoteFallsBackToSomethingReadable(t *testing.T) {
	if got := noteFor(context.Background(), ""); got != "configuration changed" {
		t.Errorf("empty description = %q", got)
	}
}
