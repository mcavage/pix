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
	"fmt"
	"path/filepath"
	"testing"
)

// TestMemStoreSchemaVersionStamp covers the OTHER half of opening an existing
// db: a fresh store stamps user_version=memSchemaVersion (what a snapshot
// records and a restore gates on), and a db written by a NEWER pix
// (user_version > memSchemaVersion) is refused rather than silently
// downgraded to this binary's marker, which would corrupt a
// forward-incompatible schema.
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
	if uv != memSchemaVersion {
		t.Errorf("fresh store user_version = %d, want %d", uv, memSchemaVersion)
	}

	path := filepath.Join(t.TempDir(), "future.db")
	future, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := future.Exec(fmt.Sprintf("PRAGMA user_version = %d", memSchemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	if _, err := future.Exec("CREATE TABLE t(x)"); err != nil { // force the header to disk
		t.Fatal(err)
	}
	future.Close()
	if _, err := newMemStore(path, nil); err == nil {
		t.Errorf("newMemStore accepted a db with user_version=%d; want a version error", memSchemaVersion+1)
	}
}
