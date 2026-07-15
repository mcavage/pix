package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// bundleWithRemote creates a bundle git repo whose origin is a local bare repo,
// with one initial commit. Returns the bundle dir and the bare origin dir.
func bundleWithRemote(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	bundle := filepath.Join(root, "bundle")
	if err := exec.Command("git", "init", "--bare", "-q", origin).Run(); err != nil {
		t.Fatalf("init bare: %v", err)
	}
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, bundle, "init", "-q")
	git(t, bundle, "config", "user.email", "t@t")
	git(t, bundle, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(bundle, "index.md"), []byte("# kb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, bundle, "add", "-A")
	git(t, bundle, "commit", "-q", "-m", "init")
	git(t, bundle, "remote", "add", "origin", origin)
	git(t, bundle, "push", "-q", "-u", "origin", "HEAD")
	return bundle, origin
}

func TestKnowledgeSyncBranchPush(t *testing.T) {
	bundle, origin := bundleWithRemote(t)
	// Make a local change so there is something to commit.
	if err := os.WriteFile(filepath.Join(bundle, "concept.md"), []byte("---\ntype: reference\n---\n# c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := knowledgeSync(bundle, "", false, &out); err != nil {
		t.Fatalf("knowledgeSync: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "pushed branch knowledge/sync-") {
		t.Errorf("output missing branch push: %q", out.String())
	}
	// The origin must now have a knowledge/sync-* branch.
	branches := git(t, origin, "branch", "--list", "knowledge/sync-*")
	if strings.TrimSpace(branches) == "" {
		t.Errorf("origin has no knowledge/sync branch; branches:\n%s", git(t, origin, "branch"))
	}
}

func TestKnowledgeSyncDoesNotAdvanceMain(t *testing.T) {
	bundle, _ := bundleWithRemote(t)
	mainTip := strings.TrimSpace(git(t, bundle, "rev-parse", "HEAD"))
	branchBefore := strings.TrimSpace(git(t, bundle, "rev-parse", "--abbrev-ref", "HEAD"))
	if err := os.WriteFile(filepath.Join(bundle, "concept.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := knowledgeSync(bundle, "", false, &bytes.Buffer{}); err != nil {
		t.Fatalf("knowledgeSync: %v", err)
	}
	// The default (safe) sync must NOT advance the original branch tip: the commit
	// belongs on the new knowledge/sync-* branch, not on main.
	gotTip := strings.TrimSpace(git(t, bundle, "rev-parse", branchBefore))
	if gotTip != mainTip {
		t.Errorf("safe sync advanced %s: was %s now %s", branchBefore, mainTip, gotTip)
	}
}

func TestKnowledgeSyncAllowMain(t *testing.T) {
	bundle, origin := bundleWithRemote(t)
	if err := os.WriteFile(filepath.Join(bundle, "concept.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := knowledgeSync(bundle, "sync it", true, &out); err != nil {
		t.Fatalf("knowledgeSync allow-main: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "pushed") {
		t.Errorf("output = %q", out.String())
	}
	// origin's default branch should have advanced (2 commits).
	log := git(t, origin, "log", "--oneline")
	if !strings.Contains(log, "sync it") {
		t.Errorf("origin default branch missing sync commit; log:\n%s", log)
	}
}

func TestKnowledgeSyncNotGitRepo(t *testing.T) {
	dir := t.TempDir()
	err := knowledgeSync(dir, "", false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "not a git repo") {
		t.Errorf("err = %v, want 'not a git repo'", err)
	}
}

func TestKnowledgeSyncNoRemote(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	err := knowledgeSync(dir, "", false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no origin remote") {
		t.Errorf("err = %v, want 'no origin remote'", err)
	}
}

func TestRedactURL(t *testing.T) {
	cases := map[string]string{
		"https://user:tok@github.com/a/b.git": "https://***@github.com/a/b.git",
		"https://github.com/a/b.git":          "https://github.com/a/b.git",
		"git@github.com:a/b.git":              "git@github.com:a/b.git",
	}
	for in, want := range cases {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKnowledgeSyncDetachedHeadAllowMain(t *testing.T) {
	bundle, _ := bundleWithRemote(t)
	// Detach HEAD.
	head := strings.TrimSpace(git(t, bundle, "rev-parse", "HEAD"))
	git(t, bundle, "checkout", "-q", head)
	err := knowledgeSync(bundle, "", true, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "detached HEAD") {
		t.Errorf("err = %v, want 'detached HEAD'", err)
	}
}

func TestResolveSyncBundleFlag(t *testing.T) {
	got, err := resolveSyncBundle("/tmp/somewhere")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/somewhere" {
		t.Errorf("got %q, want /tmp/somewhere", got)
	}
}

func TestResolveSyncBundleSingleConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("knowledge_bundles = [\"/kb/only\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PI_STACK_CONFIG", cfgPath)
	got, err := resolveSyncBundle("")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/kb/only" {
		t.Errorf("got %q, want /kb/only", got)
	}
}

func TestResolveSyncBundleNoneAndMany(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	t.Setenv("PI_STACK_CONFIG", cfgPath)

	if err := os.WriteFile(cfgPath, []byte("knowledge_bundles = []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveSyncBundle(""); err == nil || !strings.Contains(err.Error(), "no knowledge bundle") {
		t.Errorf("none: err = %v, want 'no knowledge bundle'", err)
	}

	if err := os.WriteFile(cfgPath, []byte("knowledge_bundles = [\"/a\",\"/b\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveSyncBundle(""); err == nil || !strings.Contains(err.Error(), "multiple bundles") {
		t.Errorf("many: err = %v, want 'multiple bundles'", err)
	}
}
