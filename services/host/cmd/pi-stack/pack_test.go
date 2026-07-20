package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGitEnv records git invocations and pretends they succeed.
func fakeGitEnv(calls *[]string) shellEnv {
	return shellEnv{run: func(name string, args ...string) (string, error) {
		if calls != nil {
			*calls = append(*calls, name+" "+strings.Join(args, " "))
		}
		return "", nil
	}}
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
	cases := []struct{ raw, url, ref, name string }{
		{"https://github.com/me/dev-pack.git", "https://github.com/me/dev-pack.git", "", "dev-pack"},
		{"git+https://github.com/me/work-pack#ref=v2", "https://github.com/me/work-pack", "v2", "work-pack"},
		{"git@github.com:me/x.git#main", "git@github.com:me/x.git", "main", "x"},
	}
	for _, c := range cases {
		if !isPackGitURL(c.raw) {
			t.Errorf("%q should be a git URL", c.raw)
		}
		u, r := parsePackURL(c.raw)
		if u != c.url || r != c.ref {
			t.Errorf("parsePackURL(%q) = (%q,%q), want (%q,%q)", c.raw, u, r, c.url, c.ref)
		}
		if got := packNameFromURL(u); got != c.name {
			t.Errorf("packNameFromURL(%q) = %q, want %q", u, got, c.name)
		}
	}
	if isPackGitURL("/local/path/pack") || isPackGitURL("./rel") {
		t.Error("local paths must not be git URLs")
	}
}
