// Package handlers holds the HTTP handlers. Handlers call store repositories;
// they never issue raw SQL and never spawn background work (codex principles 2
// and 3).
package handlers

import (
	"net/http"

	"codexapp/store"
)

// Handlers wires the HTTP surface to the store repositories.
type Handlers struct {
	Users  store.Users
	Orders store.Orders
}

// GetUser returns a single user by id — synchronous, via the store.
func (h *Handlers) GetUser(w http.ResponseWriter, r *http.Request) {
	// decode id from r, call h.Users.ByID, encode to w. Synchronous; returns.
	_ = w
	_ = r
}
