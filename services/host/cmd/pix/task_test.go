// task_test.go — the cmd/pix `pix task` CLI-plumbing tests. The bulk of the
// SUBSTANTIVE task logic (naming, git-hygiene probing, the removal guard,
// clone vs worktree) is real-git tested directly in
// pix/host/workflow/task (the new L1 package); this file only exercises the
// argv parsing and the composition points specific to the CLI layer
// (`--task` shorthand rewriting, `ls`/`path` rendering).
//
// runTaskNew/runTaskRunVerb ultimately os.Exit on a launch failure (they
// delegate straight to runRun, the same `pix run` entry point, deliberately
// — see task_cmd.go's header comment for why that removes the old
// task-specific sandbox-lifecycle duplication). Driving that all the way
// through a real sandbox launch needs sbx + Docker on the host (same as
// `pix run` itself; see docs/design/worktree-tasks.md's host-verification
// section), so it is out of scope for this in-process suite.
package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"pix/host/workflow/task"
	"strings"
	"testing"
)

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Args = append([]string{"git", "-C", dir}, args...)
	}
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.test",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// newRepo creates a fresh repo with one commit and chdir's the test into it,
// returning its git-common-dir.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustGit(t, "", "init", "-q", "-b", "main", dir)
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o644)
	mustGit(t, dir, "add", ".")
	mustGit(t, dir, "commit", "-q", "-m", "init")
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
	root, err := task.ResolveMainroot(dir)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestParseTaskNewArgs(t *testing.T) {
	name, from, mech, pass, err := parseTaskNewArgs([]string{"fix-login", "--from", "main", "--", "-p", "hi"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if name != "fix-login" || from != "main" || mech != task.Clone {
		t.Errorf("got name=%q from=%q mech=%q", name, from, mech)
	}
	if strings.Join(pass, ",") != "-p,hi" {
		t.Errorf("passthrough = %v", pass)
	}
	if _, _, _, _, err := parseTaskNewArgs(nil); err == nil {
		t.Error("want an error when no name is given")
	}
	if _, _, _, _, err := parseTaskNewArgs([]string{"--bogus"}); err == nil {
		t.Error("want an error on an unknown flag")
	}
	_, _, mech2, _, err := parseTaskNewArgs([]string{"x", "--worktree"})
	if err != nil || mech2 != task.Worktree {
		t.Errorf("--worktree: mech=%q err=%v", mech2, err)
	}
}

func TestParseTaskRmArgs(t *testing.T) {
	name, force, err := parseTaskRmArgs([]string{"work", "--force"})
	if err != nil || name != "work" || !force {
		t.Errorf("name=%q force=%v err=%v", name, force, err)
	}
	if _, _, err := parseTaskRmArgs(nil); err == nil {
		t.Error("want an error when no name is given")
	}
	if _, _, err := parseTaskRmArgs([]string{"a", "b"}); err == nil {
		t.Error("want an error on a second positional argument")
	}
}

func TestResolveTaskRunArgv(t *testing.T) {
	mainroot := newRepo(t)
	dataDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dataDir)
	m, err := task.New(task.NewOptions{StateRoot: taskStateRootForTest(dataDir), Mainroot: mainroot, Name: "fix-login"})
	if err != nil {
		t.Fatal(err)
	}
	co, err := task.Path(taskStateRootForTest(dataDir), mainroot, "fix-login")
	if err != nil {
		t.Fatal(err)
	}
	argv, err := resolveTaskRunArgv("fix-login", []string{"--extra"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{co, "--name", m.Sandbox, "--extra"}
	if strings.Join(argv, "|") != strings.Join(want, "|") {
		t.Errorf("got %v, want %v", argv, want)
	}
}

func TestExpandTaskFlag_NoFlagPassesThrough(t *testing.T) {
	argv := []string{"run", "--name", "x"}
	out, matched, err := expandTaskFlag(argv)
	if matched || err != nil {
		t.Fatalf("matched=%v err=%v", matched, err)
	}
	if strings.Join(out, "|") != strings.Join(argv, "|") {
		t.Errorf("argv rewritten when it should not be: %v", out)
	}
}

func TestExpandTaskFlag_RewritesToCheckoutAndName(t *testing.T) {
	mainroot := newRepo(t)
	dataDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dataDir)
	m, err := task.New(task.NewOptions{StateRoot: taskStateRootForTest(dataDir), Mainroot: mainroot, Name: "work"})
	if err != nil {
		t.Fatal(err)
	}
	co, err := task.Path(taskStateRootForTest(dataDir), mainroot, "work")
	if err != nil {
		t.Fatal(err)
	}

	out, matched, err := expandTaskFlag([]string{"--task", "work", "--replace"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !matched {
		t.Fatal("want matched=true")
	}
	want := []string{co, "--name", m.Sandbox, "--replace"}
	if strings.Join(out, "|") != strings.Join(want, "|") {
		t.Errorf("got %v, want %v", out, want)
	}

	out2, matched2, err := expandTaskFlag([]string{"--task=work"})
	if err != nil || !matched2 {
		t.Fatalf("matched=%v err=%v", matched2, err)
	}
	if strings.Join(out2, "|") != strings.Join([]string{co, "--name", m.Sandbox}, "|") {
		t.Errorf("got %v", out2)
	}
}

func TestExpandTaskFlag_UnknownTaskErrors(t *testing.T) {
	newRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if _, matched, err := expandTaskFlag([]string{"--task", "nope"}); !matched || err == nil {
		t.Errorf("want matched=true, err!=nil for an unknown task; got matched=%v err=%v", matched, err)
	}
}

func TestRunTaskLs_HumanAndJSON(t *testing.T) {
	mainroot := newRepo(t)
	dataDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dataDir)
	if _, err := task.New(task.NewOptions{StateRoot: taskStateRootForTest(dataDir), Mainroot: mainroot, Name: "clean"}); err != nil {
		t.Fatal(err)
	}

	human := captureStdout(t, func() { runTaskLs(nil) })
	if !strings.Contains(human, "clean") {
		t.Errorf("human output missing task name:\n%s", human)
	}

	js := captureStdout(t, func() { runTaskLs([]string{"--json"}) })
	var rows []taskListRow
	if err := json.Unmarshal([]byte(js), &rows); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, js)
	}
	if len(rows) != 1 || rows[0].Name != "clean" {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].Path == "" {
		t.Error("want a non-empty path in the JSON row")
	}
}

func TestRunTaskPathVerb_PrintsCheckoutDir(t *testing.T) {
	mainroot := newRepo(t)
	dataDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dataDir)
	if _, err := task.New(task.NewOptions{StateRoot: taskStateRootForTest(dataDir), Mainroot: mainroot, Name: "fix-login"}); err != nil {
		t.Fatal(err)
	}
	co, err := task.Path(taskStateRootForTest(dataDir), mainroot, "fix-login")
	if err != nil {
		t.Fatal(err)
	}
	out := strings.TrimSpace(captureStdout(t, func() { runTaskPathVerb([]string{"fix-login"}) }))
	if out != co {
		t.Errorf("got %q, want %q", out, co)
	}
}

func TestRunTaskCmd_HelpAndUnknown(t *testing.T) {
	out := captureStdout(t, func() { runTaskCmd(nil) })
	if !strings.Contains(out, "usage: pix task") {
		t.Errorf("bare `task` should print usage, got %q", out)
	}
	out2 := captureStdout(t, func() { runTaskCmd([]string{"-h"}) })
	if !strings.Contains(out2, "usage: pix task") {
		t.Errorf("-h should print usage, got %q", out2)
	}
}

// --- small test-only seams: these mirror the production functions but avoid
// duplicating workspace.TaskStateRoot()'s XDG resolution rules (it always
// reads the live environment; the test sets XDG_STATE_HOME above and the
// production paths already call it directly, this just names the same
// intent for readability at call sites above).

func taskStateRootForTest(xdgStateHome string) string {
	return filepath.Join(xdgStateHome, "pix", "tasks")
}
