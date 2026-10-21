package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// BenchmarkRequestPath measures one request through the whole proxy against a
// fake upstream, with the activity store in memory and on disk. It exists to
// price the in-flight marker (one extra insert and delete per request, on the
// store's single connection): compare ns/op with and without it.
//
//	go test ./internal/proxy -run '^$' -bench RequestPath -benchtime 2000x
func BenchmarkRequestPath(b *testing.B) {
	prev := isRemote
	isRemote = func(config.Model) bool { return true }
	b.Cleanup(func() { isRemote = prev })
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	b.Cleanup(up.Close)

	for _, db := range []string{"memory", "file"} {
		b.Run(db, func(b *testing.B) {
			path := ":memory:"
			if db == "file" {
				path = filepath.Join(b.TempDir(), "bench.db")
			}
			st, err := store.Open(context.Background(), path)
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = st.Close() })
			mgr := proc.NewManager(&config.Config{})
			b.Cleanup(mgr.Shutdown)
			m := modelTo(b, up.URL, "chat")
			cfg := &config.Config{
				Models:         map[string]config.Model{"m": m},
				PriorityGroups: map[string]config.PriorityGroup{"default": {Weight: 1}},
				Keys:           map[string]config.KeyPolicy{"k": {Group: "default"}},
			}
			r := chi.NewRouter()
			New(cfg, mgr, sched.New(), st).Mount(r)
			body := `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("hello ", 200) + `"}]}`
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
				req.Header.Set("X-Corrallm-Key", "k")
				rec := httptest.NewRecorder()
				r.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					b.Fatalf("status %d", rec.Code)
				}
			}
		})
	}
}
