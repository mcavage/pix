// env_add_cmd_test.go — `pix env add <git-url|local-directory> [name]`
// proven at the REAL command boundary: `dispatch(["env", "add", ...], ...)`,
// the exact entry point a user hits, never workflow/env.Add called directly.
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/cli"
	nativeenv "pix/host/workflow/env"
)

func writeValidSbxenvForAddCmd(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".sbxenv.yaml"), []byte("schemaVersion: \"1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestEnvAddCmd_LocalDirectory_SymlinksAndPrintsNextSteps(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	src := filepath.Join(t.TempDir(), "myproj")
	writeValidSbxenvForAddCmd(t, src)

	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb}
	code := dispatch([]string{"env", "add", src}, d)
	if code != 0 {
		t.Fatalf("dispatch exit = %d, stderr=%s", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, `"myproj"`) {
		t.Fatalf("output missing derived name: %q", got)
	}
	if !strings.Contains(got, "pix env show myproj") ||
		!strings.Contains(got, "pix env trust myproj") ||
		!strings.Contains(got, "pix env default myproj") {
		t.Fatalf("output missing the exact next-step commands: %q", got)
	}

	target := filepath.Join(home, "envs", "myproj")
	fi, err := os.Lstat(target)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected a symlink at %s", target)
	}

	// `pix env show myproj` must now resolve it.
	var showOut, showErr bytes.Buffer
	if code := dispatch([]string{"env", "show", "myproj", "--path"}, &cli.Deps{Out: &showOut, Err: &showErr}); code != 0 {
		t.Fatalf("env show --path exit = %d, stderr=%s", code, showErr.String())
	}
	canonicalSrc, _ := filepath.EvalSymlinks(src)
	if strings.TrimSpace(showOut.String()) != canonicalSrc {
		t.Fatalf("env show --path = %q, want %q", showOut.String(), canonicalSrc)
	}
}

func TestEnvAddCmd_ExplicitName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	src := filepath.Join(t.TempDir(), "src")
	writeValidSbxenvForAddCmd(t, src)

	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb}
	if code := dispatch([]string{"env", "add", src, "picked"}, d); code != 0 {
		t.Fatalf("dispatch exit = %d, stderr=%s", code, errb.String())
	}
	if _, err := os.Lstat(filepath.Join(home, "envs", "picked")); err != nil {
		t.Fatalf("expected envs/picked to exist: %v", err)
	}
}

func TestEnvAddCmd_RefusesToOverwriteExistingEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	existing := filepath.Join(home, "envs", "taken")
	writeValidSbxenvForAddCmd(t, existing)
	src := filepath.Join(t.TempDir(), "other")
	writeValidSbxenvForAddCmd(t, src)

	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb}
	code := dispatch([]string{"env", "add", src, "taken"}, d)
	if code == 0 {
		t.Fatalf("dispatch exit = 0, want a nonzero refusal; stdout=%s", out.String())
	}
	if !strings.Contains(errb.String(), "already exists") {
		t.Fatalf("stderr = %q, want an 'already exists' refusal", errb.String())
	}
	fi, err := os.Lstat(existing)
	if err != nil || fi.IsDir() == false {
		t.Fatalf("the pre-existing plain directory must survive unchanged: err=%v mode=%v", err, fi)
	}
}

func TestEnvAddCmd_MissingSbxenv_RemovesTheSymlinkItCreated(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	src := t.TempDir() // no .sbxenv.yaml

	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb}
	code := dispatch([]string{"env", "add", src, "badenv"}, d)
	if code == 0 {
		t.Fatalf("dispatch exit = 0, want nonzero; stdout=%s", out.String())
	}
	if !strings.Contains(errb.String(), "did not pass validation") {
		t.Fatalf("stderr = %q", errb.String())
	}
	if _, statErr := os.Lstat(filepath.Join(home, "envs", "badenv")); !os.IsNotExist(statErr) {
		t.Fatal("the symlink this invocation created must be removed")
	}
	if _, statErr := os.Stat(src); statErr != nil {
		t.Fatalf("the source directory itself must never be touched: %v", statErr)
	}
}

func TestEnvAddCmd_GitURL_ClonesViaGitClone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)

	orig := nativeenv.GitClone
	defer func() { nativeenv.GitClone = orig }()
	nativeenv.GitClone = func(url, dest string) (string, error) {
		writeValidSbxenvForAddCmd(t, dest)
		return "", nil
	}

	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb}
	code := dispatch([]string{"env", "add", "https://example.com/org/somerepo.git"}, d)
	if code != 0 {
		t.Fatalf("dispatch exit = %d, stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "cloned") {
		t.Fatalf("output = %q, want 'cloned'", out.String())
	}
	if _, err := os.Stat(filepath.Join(home, "envs", "somerepo", ".sbxenv.yaml")); err != nil {
		t.Fatalf("expected the clone to land under envs/somerepo: %v", err)
	}
}

func TestEnvAddCmd_NoTTYRequired(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	src := filepath.Join(t.TempDir(), "quiet")
	writeValidSbxenvForAddCmd(t, src)

	var out, errb bytes.Buffer
	// Deps.Interactive left at its zero value (false, i.e. non-interactive):
	// `pix env add` must succeed with no TTY and no prompt, unlike `pix env
	// trust` which refuses without --yes on a non-interactive terminal.
	d := &cli.Deps{Out: &out, Err: &errb}
	code := dispatch([]string{"env", "add", src}, d)
	if code != 0 {
		t.Fatalf("dispatch exit = %d, stderr=%s (env add must not require a TTY)", code, errb.String())
	}
}

func TestEnvAddCmd_NeverSetsDefaultOrTrust(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	src := filepath.Join(t.TempDir(), "untouched")
	writeValidSbxenvForAddCmd(t, src)

	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb}
	if code := dispatch([]string{"env", "add", src}, d); code != 0 {
		t.Fatalf("dispatch exit = %d, stderr=%s", code, errb.String())
	}

	var defOut, defErr bytes.Buffer
	if code := dispatch([]string{"env", "default"}, &cli.Deps{Out: &defOut, Err: &defErr}); code != 0 {
		t.Fatalf("env default exit = %d, stderr=%s", code, defErr.String())
	}
	if strings.Contains(defOut.String(), "untouched") {
		t.Fatalf("env add must never set the machine default, got %q", defOut.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".state", "trust", "environments", "untouched.json")); !os.IsNotExist(err) {
		t.Fatal("env add must never write a trust acceptance record")
	}
}
