package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// A backend's own 400 keeps its message in the activity row: the context
// overflow was a bare 400 there and only the journal said why.
func TestBackend400MessageIsRecorded(t *testing.T) {
	const body = `{"error":{"code":400,"message":"request (89653 tokens) exceeds the available context size (86016 tokens), try increasing it","type":"exceed_context_size_error","n_prompt_tokens":89653,"n_ctx":86016}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, body)
	}))
	defer up.Close()

	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()
	cfg := &config.Config{
		Models:         map[string]config.Model{"local": modelTo(t, up.URL, "local")},
		PriorityGroups: map[string]config.PriorityGroup{"default": {Weight: 1}},
		Keys:           map[string]config.KeyPolicy{"k": {Group: "default"}},
	}
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"local","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer k")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || rec.Body.String() != body {
		t.Fatalf("the 400 must pass through unchanged: %d %s", rec.Code, rec.Body)
	}

	acts, _ := st.RecentActivity(store.Window{}, 10, "", "", "")
	if len(acts) != 1 {
		t.Fatalf("want 1 activity row, got %d", len(acts))
	}
	if want := "request (89653 tokens) exceeds the available context size (86016 tokens), try increasing it"; acts[0].Error != want {
		t.Errorf("error = %q, want %q", acts[0].Error, want)
	}
}

func TestUpstreamErrorText(t *testing.T) {
	for body, want := range map[string]string{
		`{"error":{"message":"m1","type":"x"}}`:        "m1",
		`{"error":"m2"}`:                               "m2",
		`{"object":"error","message":"m3","code":400}`: "m3",
		"plain failure\n":                              "plain failure",
		"":                                             "",
		strings.Repeat("z", 400):                       strings.Repeat("z", 300) + "…",
	} {
		if got := upstreamErrorText([]byte(body)); got != want {
			t.Errorf("upstreamErrorText(%.40q) = %q, want %q", body, got, want)
		}
	}
}
