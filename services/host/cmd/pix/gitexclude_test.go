package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return dir
}

func TestEnsurePixGitExcludePreservesKnowledgePointer(t *testing.T) {
	dir := initGitRepo(t)
	exclude := filepath.Join(dir, ".git", "info", "exclude")
	if err := os.WriteFile(exclude, []byte("*.local\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := ensurePixGitExclude(dir)
	if err != nil || !changed {
		t.Fatalf("ensurePixGitExclude = (%v, %v), want (true, nil)", changed, err)
	}
	got, err := os.ReadFile(exclude)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), "*.local\n") {
		t.Fatalf("existing excludes were not preserved:\n%s", got)
	}
	if !strings.Contains(string(got), pixGitExcludeBlock) {
		t.Fatalf("Pix exclude block missing:\n%s", got)
	}

	for _, tc := range []struct {
		path    string
		ignored bool
	}{
		{path: ".pix/profile", ignored: true},
		{path: ".pix/sandbox.pack", ignored: true},
		{path: ".pix/knowledge", ignored: false},
	} {
		cmd := exec.Command("git", "-C", dir, "check-ignore", "--no-index", "-q", tc.path)
		err := cmd.Run()
		ignored := err == nil
		if ignored != tc.ignored {
			t.Errorf("%s ignored=%v, want %v (err=%v)", tc.path, ignored, tc.ignored, err)
		}
	}
}

func TestEnsurePixGitExcludeIsIdempotent(t *testing.T) {
	dir := initGitRepo(t)
	if _, err := ensurePixGitExclude(dir); err != nil {
		t.Fatal(err)
	}
	changed, err := ensurePixGitExclude(dir)
	if err != nil || changed {
		t.Fatalf("second ensure = (%v, %v), want (false, nil)", changed, err)
	}
	exclude := filepath.Join(dir, ".git", "info", "exclude")
	got, err := os.ReadFile(exclude)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(got), pixGitExcludeBlock) != 1 {
		t.Fatalf("Pix block count != 1:\n%s", got)
	}
}

func TestEnsurePixGitExcludeConcurrentRunsPreserveExistingRules(t *testing.T) {
	dir := initGitRepo(t)
	exclude := filepath.Join(dir, ".git", "info", "exclude")
	const existing = "keep-this-rule.local\n"
	if err := os.WriteFile(exclude, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < cap(errs); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := ensurePixGitExclude(dir)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	got, err := os.ReadFile(exclude)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), existing) {
		t.Fatalf("concurrent updates lost existing rule:\n%s", got)
	}
	if !strings.Contains(string(got), pixGitExcludeBlock) {
		t.Fatalf("Pix block missing after concurrent updates:\n%s", got)
	}
}

func TestEnsurePixGitExcludeRefusesSymlink(t *testing.T) {
	dir := initGitRepo(t)
	exclude := filepath.Join(dir, ".git", "info", "exclude")
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("do not change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(exclude); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, exclude); err != nil {
		t.Fatal(err)
	}

	if changed, err := ensurePixGitExclude(dir); err == nil || changed {
		t.Fatalf("symlink ensure = (%v, %v), want (false, error)", changed, err)
	}
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "do not change\n" {
		t.Fatalf("symlink target changed: %q", got)
	}
}

func TestEnsurePixGitExcludeSkipsNonGitDirectory(t *testing.T) {
	changed, err := ensurePixGitExclude(t.TempDir())
	if err != nil || changed {
		t.Fatalf("non-git ensure = (%v, %v), want (false, nil)", changed, err)
	}
}
