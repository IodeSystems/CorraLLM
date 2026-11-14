// Package db opens the service's SQLite state file.
package db

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // driver "sqlite"
)

// Open opens (creating) the SQLite file at path with WAL and a busy timeout,
// and runs every schema statement given.
func Open(path string, schema ...string) (*sql.DB, error) {
	d, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	// One writer: SQLite serializes anyway, and one connection keeps
	// read-modify-write sequences in a transaction simple.
	d.SetMaxOpenConns(1)
	for _, s := range schema {
		if _, err := d.Exec(s); err != nil {
			d.Close()
			return nil, fmt.Errorf("schema: %w", err)
		}
	}
	return d, nil
}
