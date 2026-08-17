// memory_schema_version_test.go — the schema-version guard newMemStore enforces
// (PRAGMA user_version), split out of the former TTL-migration test file after
// migrateLegacyWatcherPerishableTTL (and its test) were deleted: that migration
// existed only to shorten legacy watcher/perishable rows to the current TTL, a
// behavior removed along with the watcher's event channel and perishable/TTL
// handling generally. This test covers an unrelated concern (schema versioning)
// that belongs regardless.
package main

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMemStoreSchemaVersionStamp covers the OTHER half of opening an existing
// db: a fresh store stamps user_version=1 (what a snapshot records and a
// restore gates on), and a db written by a NEWER pix (user_version=2) is
// refused rather than silently downgraded to the 1 marker, which would corrupt
// a forward-incompatible schema.
func TestMemStoreSchemaVersionStamp(t *testing.T) {
	st, err := newMemStore(filepath.Join(t.TempDir(), "fresh.db"), nil)
	if err != nil {
		t.Fatalf("newMemStore: %v", err)
	}
	var uv int
	if err := st.db.QueryRow("PRAGMA user_version").Scan(&uv); err != nil {
		t.Fatalf("PRAGMA user_version: %v", err)
	}
	st.db.Close()
	if uv != 1 {
		t.Errorf("fresh store user_version = %d, want 1", uv)
	}

	path := filepath.Join(t.TempDir(), "future.db")
	future, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := future.Exec("PRAGMA user_version = 2"); err != nil {
		t.Fatal(err)
	}
	if _, err := future.Exec("CREATE TABLE t(x)"); err != nil { // force the header to disk
		t.Fatal(err)
	}
	future.Close()
	if _, err := newMemStore(path, nil); err == nil {
		t.Error("newMemStore accepted a db with user_version=2; want a version error")
	}
}
