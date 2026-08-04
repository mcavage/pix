package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestBuildStoresReturnErrorNotFatal is the F3 regression guard: the built-in
// store constructors used inside runServe must RETURN an error on a DB/schema
// failure, not log.Fatalf. A bare os.Exit inside runServe (which runs AFTER a
// plugin subprocess may already be launched) would skip sup.shutdown() /
// goplugin.CleanupClients() and orphan that subprocess (which may hold the
// bearer). By returning the error, runServe routes it through its cleanup-aware
// `fatalf`. This test would panic the whole run if either constructor still
// fatal-exited (the process would die instead of returning).
func TestBuildStoresReturnErrorNotFatal(t *testing.T) {
	// Point each DB at a path that cannot be opened: a regular file used as a
	// directory component, so opening "<file>/db.sqlite" fails at PRAGMA/schema.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(blocker, "store.db")


	t.Run("memory", func(t *testing.T) {
		t.Setenv("MEMORY_DB", bad)
		store, _, err := buildMemStore()
		if err == nil {
			t.Fatalf("buildMemStore(%q) = nil error, want a returned error (must not fatal)", bad)
		}
		if store != nil {
			t.Errorf("expected nil store on error, got %v", store)
		}
	})
}
