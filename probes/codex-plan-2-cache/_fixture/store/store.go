// Package store holds the repository interfaces. Per the codex, ALL persistence
// goes through here — handlers never issue raw SQL.
package store

// User is a persisted user.
type User struct {
	ID   int64
	Name string
}

// Order is a persisted order.
type Order struct {
	ID     int64
	UserID int64
	Total  int64
}

// Users is the user repository.
type Users interface {
	ByID(id int64) (User, error)
}

// Orders is the order repository.
type Orders interface {
	RecentByUser(userID int64, limit int) ([]Order, error)
}
