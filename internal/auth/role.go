package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/IodeSystems/graphql-go/v2/language/ast"
	"github.com/IodeSystems/graphql-go/v2/language/parser"
)

// Role is what a dashboard user may do. The admin token is always RoleAdmin.
type Role string

const (
	RoleAdmin  Role = "admin"
	RoleViewer Role = "viewer"
)

// Identity is who made an /api request, as the middleware resolved it. Email
// is empty for the admin token.
type Identity struct {
	Email string
	Role  Role
}

type identityKey struct{}

// WithIdentity attaches id to ctx.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// IdentityFrom returns the identity the middleware attached. ok is false when
// none was: --insecure, which has no gate, or a self-authenticating route.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// IsViewer reports whether ctx carries a read-only identity.
func IsViewer(ctx context.Context) bool {
	id, ok := IdentityFrom(ctx)
	return ok && id.Role != RoleAdmin
}

// graphQLPaths are the routes that execute a GraphQL document. Every other
// non-GET route is a write; these are the only ones a viewer can POST to, and
// only with a document that does nothing but read.
var graphQLPaths = map[string]bool{
	"/api/graphql": true,
	// gat answers introspection POSTs here with the same executor.
	"/api/schema/graphql": true,
}

// maxGraphQLBody bounds what is buffered to inspect a viewer's document. A
// dashboard query is a few KB; anything this big is refused, not parsed.
const maxGraphQLBody = 1 << 20

// readOnly reports whether r cannot change anything. It is an allowlist: GET
// and HEAD anywhere except a GraphQL route, and a GraphQL request whose
// document holds only query operations. Everything else is a write. It may
// replace r.Body with a buffered copy so the handler still reads it.
func readOnly(r *http.Request) bool {
	if graphQLPaths[r.URL.Path] {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			q := r.URL.Query().Get("query")
			return q == "" || queriesOnly(q)
		case http.MethodPost:
			body, err := io.ReadAll(io.LimitReader(r.Body, maxGraphQLBody+1))
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(body))
			if err != nil || len(body) > maxGraphQLBody {
				return false
			}
			// Decoded exactly as gat's handler decodes it (same struct, same
			// library), so both read the same query string. operationName is
			// ignored on purpose: every operation in the document must be a
			// query, whichever one is picked to run.
			var req struct {
				Query string `json:"query"`
			}
			if err := json.NewDecoder(bytes.NewReader(body)).Decode(&req); err != nil {
				return false
			}
			return queriesOnly(req.Query)
		}
		return false
	}
	return r.Method == http.MethodGet || r.Method == http.MethodHead
}

// queriesOnly parses doc with the parser the executor uses and reports whether
// it defines at least one operation and every operation is a query. Fragments
// are allowed; anything that does not parse, or any other definition, is not.
func queriesOnly(doc string) bool {
	d, err := parser.Parse(parser.ParseParams{Source: doc})
	if err != nil || d == nil {
		return false
	}
	ops := 0
	for _, def := range d.Definitions {
		switch n := def.(type) {
		case *ast.OperationDefinition:
			if n.Operation != ast.OperationTypeQuery {
				return false
			}
			ops++
		case *ast.FragmentDefinition:
		default:
			return false
		}
	}
	return ops > 0
}

// denyReadOnly is the viewer's answer to a write.
func denyReadOnly(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"error":"read-only access"}`))
}
