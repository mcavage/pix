package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureTree_FreshSandboxCreatesOneTree(t *testing.T) {
	base := t.TempDir()
	sandboxDir := filepath.Join(base, "sandboxes", "pix-proj-1234abcd")
	storeRoot := filepath.Join(base, "sessions")
	store := Store{Root: storeRoot}

	id, err := EnsureTree(store, sandboxDir, "work", "/w")
	if err != nil {
		t.Fatalf("EnsureTree: %v", err)
	}
	if id == "" {
		t.Fatal("EnsureTree returned an empty tree id")
	}
	tr, err := store.ReadTree(id)
	if err != nil {
		t.Fatalf("ReadTree(%s): %v", id, err)
	}
	if tr.Environment != "work" || tr.Workspace != "/w" {
		t.Fatalf("tree = %+v, want environment=work workspace=/w", tr)
	}
}

func TestEnsureTree_ResumesTheSamePointerOnASecondCall(t *testing.T) {
	base := t.TempDir()
	sandboxDir := filepath.Join(base, "sandboxes", "pix-proj-1234abcd")
	store := Store{Root: filepath.Join(base, "sessions")}

	first, err := EnsureTree(store, sandboxDir, "work", "/w")
	if err != nil {
		t.Fatalf("first EnsureTree: %v", err)
	}
	second, err := EnsureTree(store, sandboxDir, "work", "/w")
	if err != nil {
		t.Fatalf("second EnsureTree: %v", err)
	}
	if first != second {
		t.Fatalf("EnsureTree did not resume: first=%s second=%s", first, second)
	}

	// Exactly one tree directory exists — a resume must never fork a second
	// tree just because EnsureTree was called again.
	entries, err := store.ListTrees()
	if err != nil {
		t.Fatalf("ListTrees: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ListTrees = %v, want exactly one tree", entries)
	}
}

func TestEnsureTree_StaleOrCorruptPointerStartsAFreshTreeInsteadOfFailing(t *testing.T) {
	base := t.TempDir()
	sandboxDir := filepath.Join(base, "sandboxes", "pix-proj-1234abcd")
	store := Store{Root: filepath.Join(base, "sessions")}

	if err := os.MkdirAll(sandboxDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sandboxDir, treePointerFileName), []byte("does-not-exist\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	id, err := EnsureTree(store, sandboxDir, "work", "/w")
	if err != nil {
		t.Fatalf("EnsureTree with a dangling pointer must recover, not fail: %v", err)
	}
	if id == "" || id == "does-not-exist" {
		t.Fatalf("EnsureTree resumed a tree it cannot read: %q", id)
	}
	if _, rerr := store.ReadTree(id); rerr != nil {
		t.Fatalf("the fresh tree EnsureTree started must itself be readable: %v", rerr)
	}
}

func TestEnsureTree_EmptyPointerFileAlsoStartsFresh(t *testing.T) {
	base := t.TempDir()
	sandboxDir := filepath.Join(base, "sandboxes", "pix-proj-1234abcd")
	store := Store{Root: filepath.Join(base, "sessions")}
	if err := os.MkdirAll(sandboxDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sandboxDir, treePointerFileName), []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := EnsureTree(store, sandboxDir, "work", "/w")
	if err != nil {
		t.Fatalf("EnsureTree: %v", err)
	}
	if id == "" {
		t.Fatal("EnsureTree returned an empty tree id for an empty pointer file")
	}
}
