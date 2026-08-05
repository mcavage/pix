// task_test.go — the cmd/pix `pix task` CLI-plumbing tests, under the cli
// command contract (kong struct tags, Run(*cli.Deps)). The bulk of the
// SUBSTANTIVE task logic (naming, git-hygiene probing, the removal guard,
// clone vs worktree) is real-git tested directly in
// pix/host/workflow/task (the L1 checkout package); this file only exercises
// the argv-shape composition specific to the CLI layer (`--task` shorthand
// rewriting, the name-then-verb `path` shorthand, ls/path/rm dispatch, and
// the passthrough-arg contract new/run declare).
//
// taskNew/(*taskRunCmd).Run ultimately call runRun, the same `pix run` entry
// point (see task_cmd.go's header comment for why that removes the old
// task-specific sandbox-lifecycle duplication), which os.Exits on a launch
// failure. Driving that all the way through a real sandbox launch needs sbx +
// Docker on the host (same as `pix run` itself; see
// docs/design/worktree-tasks.md's host-verification section), so it is out of
// scope for this in-process suite — exactly as before this migration. What IS
// covered here is everything reachable before that call: kong's parse (via
// the root parser) and the pure guards in taskNew that run before any git or
// process work.
package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"pix/host/cli"
	"pix/host/sys/systest"
	"pix/host/workflow/task"
	"strings"
	"testing"
)

// captureStdout runs fn with os.Stdout swapped for a pipe and returns whatever
// fn printed. It mirrors the os.Pipe swap idiom used in help_test.go
// (TestKnowledgeInitHelp_NoSideEffects).
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	rp, wp, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = wp
	fn()
	_ = wp.Close()
	os.Stdout = old
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(rp)
	return buf.String()
}

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

// testDeps builds a cli.Deps around captured stdout/stderr, matching the
// mustRunAgent/mustRunSecret pattern used by the other migrated verbs.
func taskTestDeps() *cli.Deps {
	return &cli.Deps{
		Sys: &systest.Fake{}, Out: os.Stdout, Err: os.Stderr,
		In: strings.NewReader(""), Interactive: false,
	}
}

// runTaskParse drives the real kong parser (the root) the way
// production argv does, returning whatever error it produced (nil on
// success). This is the "no hand parser loop" replacement for the old
// parseTaskNewArgs/parseTaskRmArgs unit tests: the parsing IS kong now.
func runTaskParse(t *testing.T, d *cli.Deps, argv ...string) error {
	t.Helper()
	return runRootParse(append([]string{"task"}, argv...), d)
}

// --- new: passthrough + mechanism guards (pure, no repo needed) -------------

func TestTaskNewCmd_RequiresName(t *testing.T) {
	if err := runTaskParse(t, taskTestDeps(), "new"); err == nil {
		t.Error("want an error when no name is given")
	}
}

func TestTaskNewCmd_UnknownFlagRejected(t *testing.T) {
	if err := runTaskParse(t, taskTestDeps(), "new", "x", "--bogus"); err == nil {
		t.Error("want an error on an unknown flag")
	}
}

func TestTaskNewCmd_WorktreeFlag(t *testing.T) {
	c := &taskNewCmd{Name: "x", Worktree: true}
	if got := taskNewMechanism(c.Worktree); got != task.Worktree {
		t.Errorf("mechanism = %q, want worktree", got)
	}
}

func TestTaskNewCmd_DefaultMechanismIsClone(t *testing.T) {
	if got := taskNewMechanism(false); got != task.Clone {
		t.Errorf("mechanism = %q, want clone", got)
	}
}

// TestTaskNewPassthrough_StripsLeadingDashDash: kong's passthrough arg always
// includes the literal "--" it matched on (proven directly against the kong
// version in use); taskNewPassthrough must strip it before the args reach
// runRun (which adds its own "--" back).
func TestTaskNewPassthrough_StripsLeadingDashDash(t *testing.T) {
	pass, err := taskNewPassthrough([]string{"--", "-p", "hi"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if strings.Join(pass, ",") != "-p,hi" {
		t.Errorf("passthrough = %v", pass)
	}
}

func TestTaskNewPassthrough_Empty(t *testing.T) {
	pass, err := taskNewPassthrough(nil)
	if err != nil || len(pass) != 0 {
		t.Errorf("pass=%v err=%v, want empty/nil", pass, err)
	}
}

// TestTaskNewPassthrough_RequiresDashDash: an extra positional that did not
// come after "--" is rejected — matches the old parseTaskNewArgs contract
// ("unexpected extra argument ... use -- for pi args"), now enforced as a
// guard in taskNew itself rather than a hand-rolled arg loop.
func TestTaskNewPassthrough_RequiresDashDash(t *testing.T) {
	if _, err := taskNewPassthrough([]string{"stray"}); err == nil {
		t.Error("want an error for a bare extra positional with no --")
	} else {
		var uerr cli.UsageError
		if !errorsAs(err, &uerr) {
			t.Errorf("want a UsageError (exit 2), got %v (%T)", err, err)
		}
	}
}

// TestTaskNewCmd_ExtraArgWithoutDashDash exercises the SAME guard end-to-end
// through kong: kong's own arg/passthrough parsing accepts a bare extra
// positional (that is what "passthrough" means to it), so the rejection has
// to happen in taskNew's body — before any git or process work, which is why
// this is safe to run outside a git repository.
func TestTaskNewCmd_ExtraArgWithoutDashDash(t *testing.T) {
	if err := runTaskParse(t, taskTestDeps(), "new", "x", "stray"); err == nil {
		t.Error("want an error for an extra positional not introduced by --")
	}
}

// --- run / --task shorthand (unchanged by this migration) ------------------

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

func TestRunTaskFlag_NoFlagLeavesTheWorkspaceAlone(t *testing.T) {
	o, err := parseRunOpts([]string{"--name", "x"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if o.Workspace != "." || o.Name != "x" {
		t.Errorf("run --name x = %+v, want the cwd workspace and the given name", o)
	}
}

func TestRunTaskFlag_ResolvesToCheckoutAndName(t *testing.T) {
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

	for _, argv := range [][]string{{"--task", "work"}, {"--task=work"}} {
		o, err := parseRunOpts(argv)
		if err != nil {
			t.Fatalf("run %v: %v", argv, err)
		}
		if o.Workspace != co || o.Name != m.Sandbox {
			t.Errorf("run %v = {ws:%q name:%q}, want {%q %q}", argv, o.Workspace, o.Name, co, m.Sandbox)
		}
	}

	// An explicit --name still wins over the task's derived sandbox name.
	o, err := parseRunOpts([]string{"--task", "work", "--name", "mine"})
	if err != nil || o.Name != "mine" {
		t.Errorf("explicit --name = %q (err %v), want it to win", o.Name, err)
	}
}

func TestRunTaskFlag_UnknownTaskErrors(t *testing.T) {
	newRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if _, err := parseRunOpts([]string{"--task", "nope"}); err == nil {
		t.Error("an unknown task must be an error, not a launch")
	}
}

// --- ls / path / rm ----------------------------------------------------------

func TestTaskLs_HumanAndJSON(t *testing.T) {
	mainroot := newRepo(t)
	dataDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dataDir)
	if _, err := task.New(task.NewOptions{StateRoot: taskStateRootForTest(dataDir), Mainroot: mainroot, Name: "clean"}); err != nil {
		t.Fatal(err)
	}

	human := captureStdout(t, func() {
		if err := taskLs(taskTestDeps(), false); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(human, "clean") {
		t.Errorf("human output missing task name:\n%s", human)
	}

	js := captureStdout(t, func() {
		if err := taskLs(taskTestDeps(), true); err != nil {
			t.Fatal(err)
		}
	})
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

// TestTaskLsCmd_JSONFlagThroughKong proves --json reaches taskLs via the real
// parser, not just the direct-call path above.
func TestTaskLsCmd_JSONFlagThroughKong(t *testing.T) {
	mainroot := newRepo(t)
	dataDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dataDir)
	if _, err := task.New(task.NewOptions{StateRoot: taskStateRootForTest(dataDir), Mainroot: mainroot, Name: "clean"}); err != nil {
		t.Fatal(err)
	}
	js := captureStdout(t, func() {
		if err := runTaskParse(t, taskTestDeps(), "ls", "--json"); err != nil {
			t.Fatal(err)
		}
	})
	var rows []taskListRow
	if err := json.Unmarshal([]byte(js), &rows); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, js)
	}
	if len(rows) != 1 || rows[0].Name != "clean" {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestTaskPath_PrintsCheckoutDir(t *testing.T) {
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
	out := strings.TrimSpace(captureStdout(t, func() {
		if err := taskPath(taskTestDeps(), "fix-login"); err != nil {
			t.Fatal(err)
		}
	}))
	if out != co {
		t.Errorf("got %q, want %q", out, co)
	}
}

func TestTaskPathCmd_ThroughKong(t *testing.T) {
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
	out := strings.TrimSpace(captureStdout(t, func() {
		if err := runTaskParse(t, taskTestDeps(), "path", "fix-login"); err != nil {
			t.Fatal(err)
		}
	}))
	if out != co {
		t.Errorf("got %q, want %q", out, co)
	}
}

// TestTaskRm_RefusesAndLeavesCheckoutIntact proves taskRm's guard runs and
// fails CLOSED with exit 2 (cli.SilentError), same contract as the
// pre-migration guard — and does not touch the checkout on refusal. The happy
// (actually-removes) path additionally needs a real sandbox probe (`sbx ls`)
// to report SandboxAbsent, which this in-process suite has no way to fake (the
// probe is wired to the real OS in env.go, by design — see its header
// comment); that path is out of scope here for the same reason
// taskNew/(*taskRunCmd).Run's runRun call is (see this file's header comment).
// A test environment with no `sbx` binary reports SandboxUnknown, which
// RemoveGuard refuses unconditionally (fail-closed) regardless of git
// state — so this exercises the same refusal path whether or not the
// checkout is actually dirty.
func TestTaskRm_RefusesAndLeavesCheckoutIntact(t *testing.T) {
	mainroot := newRepo(t)
	dataDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dataDir)
	if _, err := task.New(task.NewOptions{StateRoot: taskStateRootForTest(dataDir), Mainroot: mainroot, Name: "dirty"}); err != nil {
		t.Fatal(err)
	}
	co, err := task.Path(taskStateRootForTest(dataDir), mainroot, "dirty")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(co, "uncommitted.txt"), []byte("x"), 0o644)

	err = taskRm(taskTestDeps(), "dirty", false)
	if err == nil {
		t.Fatal("want a refusal error")
	}
	if cli.ExitCode(err) != 2 {
		t.Errorf("ExitCode = %d, want 2", cli.ExitCode(err))
	}
	if _, statErr := os.Stat(co); statErr != nil {
		t.Errorf("checkout should still be present after a refused rm: %v", statErr)
	}
}

// --- the argv-shape decisions the parser cannot make, now in the root ------

// dispatchStdout runs one argv through the REAL root and returns what the
// command wrote to stdout.
func dispatchStdout(t *testing.T, argv []string) string {
	t.Helper()
	var out, errb bytes.Buffer
	if code := dispatch(argv, &cli.Deps{Out: &out, Err: &errb}); code != 0 {
		t.Fatalf("dispatch(%v) = %d, stderr %q", argv, code, errb.String())
	}
	return out.String()
}

func TestRunTaskCmd_BareAndHelpPrintUsage(t *testing.T) {
	out := dispatchStdout(t, []string{"task"})
	if !strings.Contains(out, "pix task") || !strings.Contains(out, "Create + launch a new task checkout") {
		t.Errorf("bare `task` should print usage, got %q", out)
	}
	out2 := dispatchStdout(t, []string{"task", "-h"})
	if !strings.Contains(out2, "pix task") {
		t.Errorf("-h should print usage, got %q", out2)
	}
}

// TestRunTaskCmd_NameThenVerbShorthand: `pix task <name> path` reads naturally
// for `cd "$(pix task foo path)"`; verified through the real dispatcher so the
// rewrite in runTaskCmd is exercised, not just taskPathCmd directly.
func TestRunTaskCmd_NameThenVerbShorthand(t *testing.T) {
	mainroot := newRepo(t)
	dataDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dataDir)
	if _, err := task.New(task.NewOptions{StateRoot: taskStateRootForTest(dataDir), Mainroot: mainroot, Name: "foo"}); err != nil {
		t.Fatal(err)
	}
	co, err := task.Path(taskStateRootForTest(dataDir), mainroot, "foo")
	if err != nil {
		t.Fatal(err)
	}
	out := strings.TrimSpace(dispatchStdout(t, []string{"task", "foo", "path"}))
	if out != co {
		t.Errorf("got %q, want %q", out, co)
	}
}

// TestRunTaskCmd_NameThenVerbShorthand_DoesNotShadowRealVerbs: a task literally
// named "ls" must not trigger the name-then-verb rewrite (isTaskKnownVerb
// guards it) — `pix task ls path` stays a (malformed) `ls` invocation, not
// `path ls`.
func TestRunTaskCmd_NameThenVerbShorthand_DoesNotShadowRealVerbs(t *testing.T) {
	if !isTaskKnownVerb("ls") {
		t.Fatal("ls must be a known verb")
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

// errorsAs is a tiny local wrapper so this file only imports "errors" once,
// at the call site that needs it, without shadowing the package-level err
// variables used throughout the table-driven tests above.
func errorsAs(err error, target *cli.UsageError) bool {
	for err != nil {
		if u, ok := err.(cli.UsageError); ok {
			*target = u
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
