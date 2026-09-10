package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/cli"
)

func upgradeGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "commit.gpgsign=false", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func upgradeFixture(t *testing.T, linked bool) (string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	source := t.TempDir()
	upgradeGit(t, source, "init", "-b", "main")
	writeValidSbxenvForAddCmd(t, source)
	upgradeGit(t, source, "add", ".")
	upgradeGit(t, source, "commit", "-m", "initial")
	root := filepath.Join(home, "envs", "team")
	if err := os.MkdirAll(filepath.Dir(root), 0700); err != nil {
		t.Fatal(err)
	}
	if linked {
		root = filepath.Join(t.TempDir(), "checkout")
		if err := os.Symlink(root, filepath.Join(home, "envs", "team")); err != nil {
			t.Fatal(err)
		}
	}
	upgradeGit(t, source, "clone", source, root)
	if err := os.WriteFile(filepath.Join(source, "new.txt"), []byte("update"), 0600); err != nil {
		t.Fatal(err)
	}
	upgradeGit(t, source, "add", ".")
	upgradeGit(t, source, "commit", "-m", "update")
	return source, root
}

func TestEnvUpgradePullThenSetup(t *testing.T) {
	for _, linked := range []bool{false, true} {
		t.Run(map[bool]string{false: "clone", true: "linked"}[linked], func(t *testing.T) {
			source, root := upgradeFixture(t, linked)
			var out, errb bytes.Buffer
			called := false
			err := (&envUpgradeCmd{Name: "team", Verbose: true}).run(&cli.Deps{Out: &out, Err: &errb}, func(c *setupCmd, d *cli.Deps) error {
				called = true
				if c.Env != "team" || !c.Verbose {
					t.Fatalf("setup options: %+v", c)
				}
				if upgradeGit(t, root, "rev-parse", "HEAD") != upgradeGit(t, source, "rev-parse", "HEAD") {
					t.Fatal("setup ran before pull")
				}
				return nil
			})
			if err != nil || !called || !strings.Contains(out.String(), "Restart your session") {
				t.Fatalf("err=%v setup=%v out=%s", err, called, &out)
			}
		})
	}
}

func TestEnvUpgradeRefusals(t *testing.T) {
	cases := []struct {
		name, want string
		change     func(*testing.T, string, string)
	}{
		{"untracked", "local changes", func(t *testing.T, s, r string) { os.WriteFile(filepath.Join(r, "local"), []byte("keep"), 0600) }},
		{"staged", "local changes", func(t *testing.T, s, r string) {
			os.WriteFile(filepath.Join(r, "local"), []byte("keep"), 0600)
			upgradeGit(t, r, "add", ".")
		}},
		{"modified", "local changes", func(t *testing.T, s, r string) {
			os.WriteFile(filepath.Join(r, ".sbxenv.yaml"), []byte("# keep\n"), 0600)
		}},
		{"detached", "detached HEAD", func(t *testing.T, s, r string) { upgradeGit(t, r, "checkout", "--detach") }},
		{"no upstream", "no upstream", func(t *testing.T, s, r string) { upgradeGit(t, r, "branch", "--unset-upstream") }},
		{"diverged", "could not fast-forward", func(t *testing.T, s, r string) {
			os.WriteFile(filepath.Join(r, "local"), []byte("keep"), 0600)
			upgradeGit(t, r, "add", ".")
			upgradeGit(t, r, "commit", "-m", "local")
		}},
		{"remote missing", "could not fast-forward", func(t *testing.T, s, r string) {
			upgradeGit(t, r, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source, root := upgradeFixture(t, false)
			tc.change(t, source, root)
			head := upgradeGit(t, root, "rev-parse", "HEAD")
			var out, errb bytes.Buffer
			// Exercise parsing and dispatch as well as the real Git caller. Any
			// accidental setup invocation would replace the expected refusal.
			code := dispatch([]string{"env", "upgrade", "team"}, &cli.Deps{Out: &out, Err: &errb})
			if code == 0 || !strings.Contains(errb.String(), tc.want) {
				t.Fatalf("code=%d stderr=%s", code, &errb)
			}
			if upgradeGit(t, root, "rev-parse", "HEAD") != head {
				t.Fatal("refusal changed HEAD")
			}
		})
	}
}

func TestEnvUpgradeSetupFailureKeepsUpdatedCheckout(t *testing.T) {
	source, root := upgradeFixture(t, false)
	var out, errb bytes.Buffer
	sentinel := errors.New("setup canceled")
	err := (&envUpgradeCmd{Name: "team"}).run(&cli.Deps{Out: &out, Err: &errb}, func(*setupCmd, *cli.Deps) error { return sentinel })
	if !errors.Is(err, sentinel) || !strings.Contains(errb.String(), "Retry with pix setup --env team") || strings.Contains(out.String(), "Restart your session") {
		t.Fatalf("err=%v out=%s stderr=%s", err, &out, &errb)
	}
	if upgradeGit(t, root, "rev-parse", "HEAD") != upgradeGit(t, source, "rev-parse", "HEAD") {
		t.Fatal("updated checkout was not retained")
	}
}

func TestEnvUpgradeNonGitAndNestedRefused(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(map[bool]string{false: "non-git", true: "nested"}[nested], func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("PIX_HOME", home)
			root := filepath.Join(home, "envs", "team")
			writeValidSbxenvForAddCmd(t, root)
			if nested {
				upgradeGit(t, home, "init", "-b", "main")
			}
			var out, errb bytes.Buffer
			if code := dispatch([]string{"env", "upgrade", "team"}, &cli.Deps{Out: &out, Err: &errb}); code == 0 {
				t.Fatal("upgrade accepted non-root checkout")
			}
			if !strings.Contains(errb.String(), "manually") {
				t.Fatalf("stderr=%s", &errb)
			}
		})
	}
}

// The real dispatch reaches production setup after the checkout changes.
// A test binary has no installed release bundle, so setup refuses before
// any Docker or Gateway operation.
func TestEnvUpgradeDispatchReachesProductionSetup(t *testing.T) {
	source, root := upgradeFixture(t, false)
	var out, errb bytes.Buffer
	code := dispatch([]string{"env", "upgrade", "team"}, &cli.Deps{Out: &out, Err: &errb})
	if code == 0 || !strings.Contains(errb.String(), "pix setup:") || !strings.Contains(errb.String(), "Retry with pix setup --env team") {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, &out, &errb)
	}
	if upgradeGit(t, root, "rev-parse", "HEAD") != upgradeGit(t, source, "rev-parse", "HEAD") {
		t.Fatal("production setup reached without updating checkout")
	}
}

func TestEnvUpgradeSuppressesGitHooks(t *testing.T) {
	_, root := upgradeFixture(t, false)
	hooks := t.TempDir()
	marker := filepath.Join(t.TempDir(), "hook-ran")
	// Use a custom hook directory to prove repository config cannot opt back in.
	upgradeGit(t, root, "config", "core.hooksPath", hooks)
	if err := os.WriteFile(filepath.Join(hooks, "post-merge"), []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	err := (&envUpgradeCmd{Name: "team"}).run(&cli.Deps{Out: &out, Err: &errb}, func(*setupCmd, *cli.Deps) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("Git hook executed")
	}
}
