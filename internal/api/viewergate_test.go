package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/iodesystems/corrallm/internal/auth"
	"github.com/iodesystems/corrallm/internal/config"
)

// The admin-only reads are refused to a viewer through the real GraphQL path,
// where the identity has to survive gat's dispatch to reach the handler.
func TestViewerRefusedSensitiveReadsOverGraphQL(t *testing.T) {
	cfg, err := config.Load(managedConfig(t, remoteProviderYAML))
	if err != nil {
		t.Fatal(err)
	}
	h := storeBackedHandlers(t, cfg)
	gql := func(role auth.Role, query string) string {
		r := chi.NewRouter()
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(auth.WithIdentity(req.Context(), auth.Identity{Email: "v@x.com", Role: role})))
			})
		})
		if _, err := BuildGateway(r, h); err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/graphql", strings.NewReader(`{"query":`+jsonQuote(query)+`}`))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(rec, req)
		return rec.Body.String()
	}
	q := `{ corrallm { configExport { yaml } } }`
	if body := gql(auth.RoleViewer, q); !strings.Contains(body, "admin only") {
		t.Errorf("viewer read the config export: %s", body)
	}
	if body := gql(auth.RoleAdmin, q); strings.Contains(body, "admin only") || !strings.Contains(body, "yaml") {
		t.Errorf("admin refused: %s", body)
	}
}

func jsonQuote(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}
