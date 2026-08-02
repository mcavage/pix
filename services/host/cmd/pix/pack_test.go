package main

import (
	"bytes"
	"os"
	"path/filepath"
	"pix/host/hostenv"
	"pix/host/sys/systest"
	"strings"
	"testing"
)

// fakeGitEnv records git invocations and pretends they succeed.
func fakeGitEnv(calls *[]string) hostenv.Env {
	return hostenv.Env{System: &systest.Fake{RunFn: func(name string, args ...string) (string, error) {
		if calls != nil {
			*calls = append(*calls, name+" "+strings.Join(args, " "))
		}
		return "", nil
	}}}
}

func TestPackNew_InitsAndAddSkill(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "mypack")
	var git []string
	env := fakeGitEnv(&git)

	var out bytes.Buffer
	runPackNew(env, &out, []string{root})
	if _, err := os.Stat(filepath.Join(root, "pack.toml")); err != nil {
		t.Fatalf("pack.toml not created: %v", err)
	}
	if !strings.Contains(strings.Join(git, "\n"), "git -C "+root+" init") {
		t.Errorf("expected git init, got: %v", git)
	}

	// loadPack reads it back.
	p, err := loadPack(root)
	if err != nil {
		t.Fatalf("loadPack: %v", err)
	}
	if p.Manifest.Name != "mypack" || p.Manifest.Schema != 1 {
		t.Errorf("manifest = %+v", p.Manifest)
	}
	if p.SkillsDir != "" {
		t.Error("no skills yet, SkillsDir should be empty")
	}

	// add a skill -> SkillsDir now populated.
	out.Reset()
	runPackAdd(env, &out, []string{"skill", "deploy", root})
	sk := filepath.Join(root, "skills", "deploy", "SKILL.md")
	if _, err := os.Stat(sk); err != nil {
		t.Fatalf("skill not written: %v", err)
	}
	p2, _ := loadPack(root)
	if p2.SkillsDir == "" {
		t.Error("SkillsDir should be set after adding a skill")
	}
}

func TestPackNew_AdoptsExistingRepo(t *testing.T) {
	dir := t.TempDir()
	// Pre-existing repo: a .git dir already present.
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	var git []string
	env := fakeGitEnv(&git)
	var out bytes.Buffer
	runPackNew(env, &out, []string{dir})
	// Must NOT re-init an existing repo.
	if strings.Contains(strings.Join(git, "\n"), "init") {
		t.Errorf("must not git-init an existing repo, calls: %v", git)
	}
	if !strings.Contains(out.String(), "adopted existing repo") {
		t.Errorf("want adopt message, got %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "pack.toml")); err != nil {
		t.Errorf("pack.toml should be added to the adopted repo: %v", err)
	}
}

func TestLoadPack_NotAPack(t *testing.T) {
	if _, err := loadPack(t.TempDir()); err == nil {
		t.Error("a dir with no pack.toml is not a pack")
	}
}

func TestActivePackRoot_OverrideWins(t *testing.T) {
	if got := activePackRoot("/cfg/pack", "/flag/pack"); got != "/flag/pack" {
		t.Errorf("--pack override should win, got %q", got)
	}
	if got := activePackRoot("/cfg/pack", ""); got != "/cfg/pack" {
		t.Errorf("config pack when no override, got %q", got)
	}
	if got := activePackRoot("", ""); got != "" {
		t.Errorf("no pack -> empty, got %q", got)
	}
}

func TestPackURLParsing(t *testing.T) {
	cases := []struct{ raw, url, ref, namePrefix string }{
		{"https://github.com/me/dev-pack.git", "https://github.com/me/dev-pack.git", "", "dev-pack-"},
		{"git+https://github.com/me/work-pack#ref=v2", "https://github.com/me/work-pack", "v2", "work-pack-"},
		{"git@github.com:me/x.git#main", "git@github.com:me/x.git", "main", "x-"},
	}
	for _, c := range cases {
		if !isPackGitURL(c.raw) {
			t.Errorf("%q should be a git URL", c.raw)
		}
		u, r := parsePackURL(c.raw)
		if u != c.url || r != c.ref {
			t.Errorf("parsePackURL(%q) = (%q,%q), want (%q,%q)", c.raw, u, r, c.url, c.ref)
		}
		// Name is <sanitized-basename>-<8 hex> (hash of the full url).
		if got := packNameFromURL(u); !strings.HasPrefix(got, c.namePrefix) || len(got) != len(c.namePrefix)+16 {
			t.Errorf("packNameFromURL(%q) = %q, want prefix %q + 16 hex", u, got, c.namePrefix)
		}
	}
	if isPackGitURL("/local/path/pack") || isPackGitURL("./rel") {
		t.Error("local paths must not be git URLs")
	}
}

func TestPackNameFromURL_NoCollisionOrTraversal(t *testing.T) {
	// Same basename, different orgs -> different dest names (no collision).
	a := packNameFromURL("https://github.com/org-a/tools")
	b := packNameFromURL("https://github.com/org-b/tools")
	if a == b {
		t.Errorf("basename collision: both resolved to %q", a)
	}
	// A traversal-y basename is neutralized (no `/`, `\`, `..`).
	for _, u := range []string{"https://x/../../etc", "https://x/..", "https://x/a/b/..%2f.."} {
		n := packNameFromURL(u)
		if strings.ContainsAny(n, "/\\") || strings.Contains(n, "..") {
			t.Errorf("packNameFromURL(%q) = %q still has traversal chars", u, n)
		}
	}
}

func TestSafeGitURL(t *testing.T) {
	ok := []string{"https://github.com/me/p.git", "ssh://git@h/p", "git@github.com:me/p.git"}
	for _, u := range ok {
		if !safeGitURL(u) {
			t.Errorf("safeGitURL(%q) = false, want true", u)
		}
	}
	bad := []string{"ext::sh -c touch/pwn", "file:///etc/passwd", "http://h/p", "git://h/p", "fd::0", "-oProxyCommand=evil", "", "/local/path", "./rel"}
	for _, u := range bad {
		if safeGitURL(u) {
			t.Errorf("safeGitURL(%q) = true, want false (unsafe transport)", u)
		}
	}
}

func TestSafeArtifactName(t *testing.T) {
	for _, n := range []string{"deploy", "fix-login", "a_b.c"} {
		if !safeArtifactName(n) {
			t.Errorf("safeArtifactName(%q) = false, want true", n)
		}
	}
	for _, n := range []string{"", ".", "..", "../x", "a/b", "a\\b", "a b"} {
		if safeArtifactName(n) {
			t.Errorf("safeArtifactName(%q) = true, want false", n)
		}
	}
}

func TestLoadPack_RejectsEscapingSymlink(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "pack")
	if err := os.MkdirAll(filepath.Join(root, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pack.toml"), []byte("name=\"p\"\nschema=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A symlink in skills/ pointing outside the pack (at /etc) must be refused.
	if err := os.Symlink("/etc", filepath.Join(root, "skills", "escape")); err != nil {
		t.Skip("symlink unsupported: " + err.Error())
	}
	if _, err := loadPack(root); err == nil {
		t.Error("loadPack must reject a pack whose skills/ escapes via symlink")
	}
}
