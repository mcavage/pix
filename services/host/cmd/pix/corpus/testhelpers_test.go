package corpus

import (
	"os"
	"path/filepath"
	"testing"
)

// realShardsDir/realRetirementPath/rootGoPath point at the actual corpus data
// this package ships (not a scratch fixture), so schema and coverage tests
// exercise the real baseline the harness is meant to guard.
func realShardsDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(".", "shards")
}

func realRetirementPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(".", "retirement.jsonl")
}

func rootGoPath(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "root.go")
}

func repoRoot(t *testing.T) string {
	t.Helper()
	// services/host/cmd/pix/corpus -> repo root is four levels up.
	root, err := filepath.Abs(filepath.Join("..", "..", "..", "..", ".."))
	if err != nil {
		t.Fatalf("repoRoot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatalf("repoRoot guess %q does not contain AGENTS.md (layout moved?): %v", root, err)
	}
	return root
}

func statPath(p string) (os.FileInfo, error) { return os.Stat(p) }
