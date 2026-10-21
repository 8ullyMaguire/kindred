package tagfamily

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// openTestDB opens an in-memory SQLite with the repo's driver.
//
// The driver is "sqlite" (modernc.org/sqlite, the repo's choice), NOT
// "sqlite3" (mattn/go-sqlite3): the driver name is what sql.Open
// resolves, and a wrong name fails at OPEN time, not at compile time.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// mustExec runs a statement and fails the test if it does not work.
func mustExec(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}
