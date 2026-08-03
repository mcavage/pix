package main

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"pix/host/hostenv"
	"pix/host/sys/systest"
	"pix/host/workflow/doctor"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- pure helpers -----------------------------------------------------------

func TestTaskRepoKey_StableHexPrefix(t *testing.T) {
	dir := t.TempDir()
	k1 := taskRepoKey(dir)
	k2 := taskRepoKey(dir)
	if k1 != k2 {
		t.Errorf("taskRepoKey not stable: %q vs %q", k1, k2)
	}
	if len(k1) != 8 {
		t.Errorf("taskRepoKey len = %d, want 8 (%q)", len(k1), k1)
	}
	if _, err := hex.DecodeString(k1); err != nil {
		t.Errorf("taskRepoKey %q is not hex: %v", k1, err)
	}
	// Distinct paths yield distinct keys.
	if other := taskRepoKey(filepath.Join(dir, "sub")); other == k1 {
		t.Errorf("distinct paths collided on key %q", k1)
	}
}

func TestTaskPaths_UnderStateHome(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)
	co, meta := taskPaths("abcd1234", "fix-login")
	wantCo := filepath.Join(tmp, "pix", "tasks", "abcd1234", "co", "fix-login")
	wantMeta := filepath.Join(tmp, "pix", "tasks", "abcd1234", "meta", "fix-login.json")
	if co != wantCo {
		t.Errorf("co = %q, want %q", co, wantCo)
	}
	if meta != wantMeta {
		t.Errorf("meta = %q, want %q", meta, wantMeta)
	}
}

func TestSanitizeTaskName(t *testing.T) {
	cases := map[string]string{
		"fix-login":   "fix-login",
		"feat/thing":  "feat-thing",
		"a b c":       "a-b-c",
		"weird:*name": "weird--name",
		"":            "task",
	}
	for in, want := range cases {
		if got := sanitizeTaskName(in); got != want {
			t.Errorf("sanitizeTaskName(%q) = %q, want %q", in, got, want)
		}
	}
	// Overflow is capped and stays within the bound.
	long := strings.Repeat("x", 80)
	got := sanitizeTaskName(long)
	if len(got) > maxTaskNameLen {
		t.Errorf("sanitizeTaskName(long) len = %d, want <= %d", len(got), maxTaskNameLen)
	}
	// Only safe characters survive.
	for _, r := range got {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			t.Fatalf("sanitizeTaskName produced unsafe rune %q in %q", r, got)
		}
	}
}

func TestTaskSandboxName(t *testing.T) {
	if got := taskSandboxName("myrepo", "abcd1234", "fix-login", "default"); got != "pix-t-myrepo-abcd1234-fix-login" {
		t.Errorf("default profile: got %q", got)
	}
	if got := taskSandboxName("myrepo", "abcd1234", "fix-login", ""); got != "pix-t-myrepo-abcd1234-fix-login" {
		t.Errorf("empty profile: got %q", got)
	}
	// Profiles were removed: the profile arg is ignored, never a name suffix.
	if got := taskSandboxName("myrepo", "abcd1234", "fix login", "work"); got != "pix-t-myrepo-abcd1234-fix-login" {
		t.Errorf("profile arg must be ignored: got %q", got)
	}
}

// U-W3.06 (AC-P0-406): pin the PRE-LABEL legacy formula exactly, alongside
// the current one above, before the rename changes the "pix-t-"
// prefix both derive from (see sandboxname_test.go's header for why exact
// pins, not just "it still composes something", matter here). This formula
// is deleted outright in U-W3.09 (not renamed -- legacyTaskSandboxName
// reconstructs a namespace that never had legacy tasks under the new name),
// so this pin is also the record of exactly what gets deleted.
func TestLegacyTaskSandboxName_ExactComposition(t *testing.T) {
	if got := legacyTaskSandboxName("abcd1234", "fix-login", "default"); got != "pix-t-abcd1234-fix-login" {
		t.Errorf("got %q, want %q", got, "pix-t-abcd1234-fix-login")
	}
	// No label segment at all (the pre-label formula never had one) -- this is
	// the exact structural difference from taskSandboxName's output for the
	// SAME (repokey, name): one fewer "-<label>" segment.
	newer := taskSandboxName("myrepo", "abcd1234", "fix-login", "")
	legacy := legacyTaskSandboxName("abcd1234", "fix-login", "")
	if newer == legacy {
		t.Errorf("new-layout and legacy formulas produced the SAME name (%q); they must differ by the label segment", newer)
	}
	if legacy != "pix-t-abcd1234-fix-login" {
		t.Errorf("legacy formula got %q, want %q", legacy, "pix-t-abcd1234-fix-login")
	}
	// Profile arg is ignored here too (retained for call-site stability only).
	if got := legacyTaskSandboxName("abcd1234", "fix-login", "work"); got != legacy {
		t.Errorf("profile arg must be ignored: got %q, want %q", got, legacy)
	}
}

// U-W3.06: pin the per-repo state-DIR segment ("<label>-<repokey>", browsed
// under $STATE/pix/tasks/) separately from the sandbox-name formula --
// they share the label+repokey but compose them in the OPPOSITE order and
// with no "pix-t-" prefix at all, which is easy to conflate.
func TestTaskRepoDir_ExactComposition(t *testing.T) {
	dir := t.TempDir()
	got := taskRepoDir(dir)
	label := taskRepoLabel(dir)
	key := taskRepoKey(dir)
	want := label + "-" + key
	if got != want {
		t.Errorf("taskRepoDir(%q) = %q, want %q (label-then-repokey, no prefix)", dir, got, want)
	}
	if strings.HasPrefix(got, "pix") {
		t.Errorf("taskRepoDir must never carry the sandbox-name prefix (it names a DIRECTORY, not a sandbox): got %q", got)
	}
}

// --- guard table ------------------------------------------------------------

func TestTaskRemoveGuard(t *testing.T) {
	cases := []struct {
		name      string
		st        taskState
		force     bool
		wantOK    bool
		wantInMsg string
	}{
		{"clean", taskState{sandbox: sbxAbsent}, false, true, ""},
		{"running blocks", taskState{sandbox: sbxRunning}, false, false, "still running"},
		{"unknown sandbox blocks", taskState{sandbox: sbxUnknown}, false, false, "cannot determine sandbox state"},
		{"stopped sandbox is safe", taskState{sandbox: sbxStopped}, false, true, ""},
		{"dirty blocks", taskState{sandbox: sbxAbsent, dirty: true}, false, false, "uncommitted"},
		{"pushed/reachable work (unrec 0) is safe", taskState{sandbox: sbxAbsent, unrec: 0, unpushed: 2}, false, true, ""},
		{"any clone-only commit blocks", taskState{sandbox: sbxAbsent, unrec: 2, unpushed: 2}, false, false, "only in this clone"},
		{"clone-only commit blocks even when unpushed reads 0", taskState{sandbox: sbxAbsent, unrec: 2, unpushed: 0}, false, false, "only in this clone"},
		{"force overrides running", taskState{sandbox: sbxRunning}, true, true, ""},
		{"force overrides unknown sandbox", taskState{sandbox: sbxUnknown}, true, true, ""},
		{"force overrides dirty", taskState{sandbox: sbxAbsent, dirty: true}, true, true, ""},
		{"force overrides would-lose-work", taskState{sandbox: sbxAbsent, unrec: 2, unpushed: 2}, true, true, ""},
		{"multiple reasons joined", taskState{sandbox: sbxRunning, dirty: true}, false, false, ";"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg, ok := taskRemoveGuard(c.st, c.force)
			if ok != c.wantOK {
				t.Errorf("ok = %v, want %v (msg %q)", ok, c.wantOK, msg)
			}
			if c.wantOK && msg != "" {
				t.Errorf("allow should carry no message, got %q", msg)
			}
			if !c.wantOK && !strings.Contains(msg, c.wantInMsg) {
				t.Errorf("msg %q missing %q", msg, c.wantInMsg)
			}
		})
	}
}

// --- dispatcher -------------------------------------------------------------

func TestRunTaskPath_PrintsCheckoutDir(t *testing.T) {
	main := newMainRepo(t)
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	t.Setenv("PIX_PROFILE", "")

	env := gitEnv(t, "", nil)
	mainroot, err := resolveMainroot(env, main)
	if err != nil {
		t.Fatal(err)
	}
	repoDir := taskRepoDir(mainroot)
	co, metaPath := taskPaths(repoDir, "fix-login")
	makeTaskClone(t, main, co, "pix/fix-login", "HEAD")
	if err := writeTaskMeta(metaPath, taskMeta{
		Name: "fix-login", Mode: "localclone", Mainroot: mainroot,
		Branch: "pix/fix-login", Profile: "default",
	}); err != nil {
		t.Fatal(err)
	}

	t.Chdir(main)
	// Both grammars: `task path fix-login` and `task fix-login path`.
	for _, argv := range [][]string{{"path", "fix-login"}, {"fix-login", "path"}} {
		out := strings.TrimSpace(captureStdout(t, func() { runTask(argv) }))
		if out != co {
			t.Errorf("runTask(%v) = %q, want %q", argv, out, co)
		}
	}
}

func TestRunTask_BareAndHelpPrintUsage(t *testing.T) {
	for _, argv := range [][]string{nil, {"-h"}, {"--help"}} {
		out := captureStdout(t, func() { runTask(argv) })
		if !strings.Contains(out, "usage: pix task") {
			t.Errorf("runTask(%v) = %q, want usage", argv, out)
		}
	}
}

func TestTask_KnownAndRoutable(t *testing.T) {
	if !knownVerbs["task"] {
		t.Error("task missing from knownVerbs")
	}
	if u, ok := verbUsage("task"); !ok || u == "" {
		t.Error("verbUsage(task) empty")
	}
}

// --- real-git integration ---------------------------------------------------

// gitEnv is the hostenv.Env seam used by the integration tests: REAL git, but sbx
// is faked (records argv, returns canned output so the sandbox always reads as
// absent / not running).
func gitEnv(t *testing.T, sbxLs string, recorded *[][]string) hostenv.Env {
	t.Helper()
	return hostenv.Env{System: &systest.Fake{RunFn: func(name string, args ...string) (string, error) {
		if name == "sbx" {
			if recorded != nil {
				*recorded = append(*recorded, append([]string{name}, args...))
			}
			if len(args) > 0 && args[0] == "ls" {
				return sbxLs, nil
			}
			return "", nil
		}
		out, err := exec.Command(name, args...).CombinedOutput()
		return string(out), err
	}}}
}

// tgit runs a real git command in dir, failing the test on error.
func tgit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// newMainRepo creates a git repo with one commit on the default branch, and
// returns its path.
func newMainRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	tgit(t, root, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "f"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tgit(t, root, "add", ".")
	tgit(t, root, "commit", "-q", "-m", "init")
	return root
}

// makeTaskClone mimics `task new`'s clone step: local clone + branch off ref.
func makeTaskClone(t *testing.T, mainroot, co, branch, ref string) {
	t.Helper()
	if out, err := exec.Command("git", "clone", "--local", "-q", mainroot, co).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	tgit(t, co, "checkout", "-q", "-b", branch, ref)
}

func TestGatherTaskState_Clean(t *testing.T) {
	main := newMainRepo(t)
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pix/clean", "HEAD")
	env := gitEnv(t, "", nil)
	m := taskMeta{Name: "clean", Sandbox: "pix-t-x-clean", Mainroot: main, Branch: "pix/clean"}
	st := gatherTaskState(env, m, co)
	if st.sandbox != sbxAbsent || st.dirty || st.unrec != 0 || st.unpushed != 0 {
		t.Errorf("clean case: %+v, want sandbox=absent and all zero/false", st)
	}
	if msg, ok := taskRemoveGuard(st, false); !ok {
		t.Errorf("clean should allow, got refuse %q", msg)
	}
}

func TestGatherTaskState_Dirty(t *testing.T) {
	main := newMainRepo(t)
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pix/dirty", "HEAD")
	if err := os.WriteFile(filepath.Join(co, "f"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := gitEnv(t, "", nil)
	m := taskMeta{Name: "dirty", Sandbox: "pix-t-x-dirty", Mainroot: main, Branch: "pix/dirty"}
	st := gatherTaskState(env, m, co)
	if !st.dirty {
		t.Errorf("expected dirty, got %+v", st)
	}
	if _, ok := taskRemoveGuard(st, false); ok {
		t.Error("dirty should refuse without --force")
	}
}

func TestGatherTaskState_CommittedUnpushedNoRemote(t *testing.T) {
	main := newMainRepo(t)
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pix/work", "HEAD")
	// A new commit on the task branch, not present anywhere in the main repo.
	if err := os.WriteFile(filepath.Join(co, "g"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tgit(t, co, "add", ".")
	tgit(t, co, "commit", "-q", "-m", "task work")
	// No upstream, and the local clone's `origin` is the main repo, but there is
	// no push relationship: unpushed falls back to unrec.
	env := gitEnv(t, "", nil)
	m := taskMeta{Name: "work", Sandbox: "pix-t-x-work", Mainroot: main, Branch: "pix/work"}
	st := gatherTaskState(env, m, co)
	if st.unrec != 1 {
		t.Errorf("unrec = %d, want 1", st.unrec)
	}
	if st.unpushed != 1 {
		t.Errorf("unpushed = %d, want 1 (fallback to unrec)", st.unpushed)
	}
	if _, ok := taskRemoveGuard(st, false); ok {
		t.Error("committed-unpushed should refuse without --force")
	}
	if _, ok := taskRemoveGuard(st, true); !ok {
		t.Error("--force should override")
	}
}

func TestGatherTaskState_CommittedButInMain(t *testing.T) {
	main := newMainRepo(t)
	// Add a second commit to main, then branch the task at that commit so the
	// task branch's tip is reachable from a ref in the main repo (unrec == 0).
	if err := os.WriteFile(filepath.Join(main, "h"), []byte("m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tgit(t, main, "add", ".")
	tgit(t, main, "commit", "-q", "-m", "second")
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pix/inmain", "HEAD")
	env := gitEnv(t, "", nil)
	m := taskMeta{Name: "inmain", Sandbox: "pix-t-x-inmain", Mainroot: main, Branch: "pix/inmain"}
	st := gatherTaskState(env, m, co)
	if st.unrec != 0 {
		t.Errorf("unrec = %d, want 0 (tip is reachable from main)", st.unrec)
	}
	if _, ok := taskRemoveGuard(st, false); !ok {
		t.Error("committed-but-in-main should allow (nothing to lose)")
	}
}

// TestGatherTaskState_ScratchBranchCommit is the R8-1 core case: the user made a
// commit on a NEW branch inside the clone, leaving the task branch untouched. The
// old code only inspected the task branch and reported unrec=0, so a non-force
// `task rm` would silently drop the scratch commit. unrec must now see it (all
// clone heads are enumerated) and the guard must refuse without --force.
func TestGatherTaskState_ScratchBranchCommit(t *testing.T) {
	main := newMainRepo(t)
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pix/work", "HEAD")
	// The task branch stays at init; the work lands on a fresh 'scratch' branch.
	tgit(t, co, "checkout", "-q", "-b", "scratch")
	if err := os.WriteFile(filepath.Join(co, "scratch.txt"), []byte("scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tgit(t, co, "add", ".")
	tgit(t, co, "commit", "-q", "-m", "scratch work")
	// The task branch tip must be unchanged (proves the old task-branch-only probe
	// would have read unrec=0 here).
	taskTip := strings.TrimSpace(tgit(t, co, "rev-parse", "pix/work"))
	initTip := strings.TrimSpace(tgit(t, main, "rev-parse", "HEAD"))
	if taskTip != initTip {
		t.Fatalf("precondition: task branch moved (%q != %q)", taskTip, initTip)
	}

	env := gitEnv(t, "", nil)
	m := taskMeta{Name: "work", Sandbox: "x", Mainroot: main, Branch: "pix/work"}
	st := gatherTaskState(env, m, co)
	if st.unrec != 1 {
		t.Errorf("unrec = %d, want 1 (scratch-branch commit is clone-only)", st.unrec)
	}
	if _, ok := taskRemoveGuard(st, false); ok {
		t.Error("a clone-only scratch-branch commit must refuse teardown without --force")
	}
	if _, ok := taskRemoveGuard(st, true); !ok {
		t.Error("--force should override")
	}
}

// TestGatherTaskState_DetachedHeadCommit proves R8-1 for a detached HEAD: a commit
// made with no branch pointing at it is reachable only from HEAD. Enumerating
// +HEAD in the probe catches it; the old task-branch-only probe reported unrec=0.
func TestGatherTaskState_DetachedHeadCommit(t *testing.T) {
	main := newMainRepo(t)
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pix/work", "HEAD")
	// Detach HEAD, then commit: no branch will reference the new commit.
	tgit(t, co, "checkout", "-q", "--detach", "HEAD")
	if err := os.WriteFile(filepath.Join(co, "detached.txt"), []byte("detached\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tgit(t, co, "add", ".")
	tgit(t, co, "commit", "-q", "-m", "detached work")
	// Sanity: no local branch reaches this commit (only HEAD does).
	detachedTip := strings.TrimSpace(tgit(t, co, "rev-parse", "HEAD"))
	branches := tgit(t, co, "for-each-ref", "--format=%(objectname)", "refs/heads")
	if strings.Contains(branches, detachedTip) {
		t.Fatalf("precondition: a branch unexpectedly points at the detached commit")
	}

	env := gitEnv(t, "", nil)
	m := taskMeta{Name: "work", Sandbox: "x", Mainroot: main, Branch: "pix/work"}
	st := gatherTaskState(env, m, co)
	if st.unrec != 1 {
		t.Errorf("unrec = %d, want 1 (detached-HEAD commit is clone-only)", st.unrec)
	}
	if _, ok := taskRemoveGuard(st, false); ok {
		t.Error("a clone-only detached-HEAD commit must refuse teardown without --force")
	}
}

// TestGatherTaskState_PushedWorkNotCounted proves the false-positive guard: work
// that is already on a remote (reachable from the clone's refs/remotes/*) is NOT
// would-lose-work, so unrec stays 0 and teardown is allowed. A real bare remote
// is pushed to and fetched back so refs/remotes/<remote>/* is populated exactly
// as it would be in production.
func TestGatherTaskState_PushedWorkNotCounted(t *testing.T) {
	main := newMainRepo(t)
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pix/work", "HEAD")
	// A commit on the task branch...
	if err := os.WriteFile(filepath.Join(co, "g"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tgit(t, co, "add", ".")
	tgit(t, co, "commit", "-q", "-m", "task work")
	// ...then simulate it having been pushed: a bare remote + push + fetch, which
	// populates refs/remotes/rem/* in the clone.
	rem := filepath.Join(t.TempDir(), "rem.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", rem).CombinedOutput(); err != nil {
		t.Fatalf("init bare remote: %v\n%s", err, out)
	}
	tgit(t, co, "remote", "add", "rem", rem)
	tgit(t, co, "push", "-q", "rem", "HEAD:refs/heads/pushed")
	tgit(t, co, "fetch", "-q", "rem")

	env := gitEnv(t, "", nil)
	m := taskMeta{Name: "work", Sandbox: "x", Mainroot: main, Branch: "pix/work"}
	st := gatherTaskState(env, m, co)
	if st.unrec != 0 {
		t.Errorf("unrec = %d, want 0 (work is reachable from the clone's remote-tracking refs)", st.unrec)
	}
	if _, ok := taskRemoveGuard(st, false); !ok {
		t.Error("pushed work must not block teardown (nothing would be lost)")
	}
}

// TestExecuteTaskTeardown_SnapshotCapturesScratchCommit proves R8-1 point 3: even
// a --force teardown snapshots ALL of the clone's heads + HEAD, so a scratch-branch
// commit that never touched the task branch survives in the recovered namespace.
func TestExecuteTaskTeardown_SnapshotCapturesScratchCommit(t *testing.T) {
	main := newMainRepo(t)
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pix/work", "HEAD")
	// A commit on a scratch branch (task branch untouched).
	tgit(t, co, "checkout", "-q", "-b", "scratch")
	if err := os.WriteFile(filepath.Join(co, "scratch.txt"), []byte("scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tgit(t, co, "add", ".")
	tgit(t, co, "commit", "-q", "-m", "scratch work")
	scratchOID := strings.TrimSpace(tgit(t, co, "rev-parse", "HEAD"))

	sbxname := "pix-t-x-work"
	env := gitEnv(t, "", nil) // sbx reads absent; `sbx rm -f` succeeds.
	m := taskMeta{Name: "work", Sandbox: sbxname, Mainroot: main, Branch: "pix/work"}
	st := gatherTaskState(env, m, co)

	var out bytes.Buffer
	recovered := "refs/pix/recovered/work"
	rc := executeTaskTeardown(env, &out, m, co, "work", recovered, true, st)
	if rc != 0 {
		t.Fatalf("rc = %d, want 0 (--force teardown proceeds), out:\n%s", rc, out.String())
	}
	// The scratch branch must be captured under the recovered namespace...
	gotBranch := strings.TrimSpace(tgit(t, main, "rev-parse", recovered+"/heads/scratch"))
	if gotBranch != scratchOID {
		t.Errorf("recovered scratch head = %q, want %q", gotBranch, scratchOID)
	}
	// ...and HEAD (which pointed at the scratch commit) too.
	gotHead := strings.TrimSpace(tgit(t, main, "rev-parse", recovered+"/HEAD"))
	if gotHead != scratchOID {
		t.Errorf("recovered HEAD = %q, want %q", gotHead, scratchOID)
	}
	// The scratch commit is now reachable from a durable ref in the main repo.
	if out, err := exec.Command("git", "-C", main, "merge-base", "--is-ancestor", scratchOID, recovered+"/heads/scratch").CombinedOutput(); err != nil {
		t.Errorf("scratch commit not recoverable from the snapshot: %v\n%s", err, out)
	}
}

func TestResolveMainroot_RealRepo(t *testing.T) {
	main := newMainRepo(t)
	env := gitEnv(t, "", nil)
	got, err := resolveMainroot(env, main)
	if err != nil {
		t.Fatalf("resolveMainroot: %v", err)
	}
	// resolveMainroot now returns the git-common-dir (the repo's `.git` dir for a
	// normal repo), not the worktree root.
	want := filepath.Join(main, ".git")
	wantResolved, _ := filepath.EvalSymlinks(want)
	gotResolved, _ := filepath.EvalSymlinks(got)
	if gotResolved != wantResolved {
		t.Errorf("resolveMainroot = %q, want %q", got, want)
	}
}

// TestResolveMainroot_BareRepo asserts a bare repo resolves to its own dir as
// the git-common-dir, and that a local clone from that path works. Submodules
// remain a v2 concern (see the .gitmodules note in `task new`).
func TestResolveMainroot_BareRepo(t *testing.T) {
	main := newMainRepo(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	if out, err := exec.Command("git", "clone", "--local", "-q", "--bare", main, bare).CombinedOutput(); err != nil {
		t.Fatalf("clone --bare: %v\n%s", err, out)
	}
	env := gitEnv(t, "", nil)
	got, err := resolveMainroot(env, bare)
	if err != nil {
		t.Fatalf("resolveMainroot(bare): %v", err)
	}
	wantResolved, _ := filepath.EvalSymlinks(bare)
	gotResolved, _ := filepath.EvalSymlinks(got)
	if gotResolved != wantResolved {
		t.Errorf("resolveMainroot(bare) = %q, want %q", got, bare)
	}
	// The resolved git-common-dir must be directly cloneable (what `task new` does).
	clone := filepath.Join(t.TempDir(), "clone")
	if out, err := exec.Command("git", "clone", "--local", "-q", got, clone).CombinedOutput(); err != nil {
		t.Fatalf("clone from git-common-dir: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(clone, "f")); err != nil {
		t.Errorf("clone missing committed file: %v", err)
	}
}

// TestResolveMainroot_LinkedWorktree asserts a linked worktree reports the SAME
// git-common-dir as its main worktree, so tasks started from either share one
// repo key and one object/ref store.
func TestResolveMainroot_LinkedWorktree(t *testing.T) {
	main := newMainRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	tgit(t, main, "worktree", "add", "-q", wt, "-b", "wtbranch")
	env := gitEnv(t, "", nil)
	fromMain, err := resolveMainroot(env, main)
	if err != nil {
		t.Fatalf("resolveMainroot(main): %v", err)
	}
	fromWt, err := resolveMainroot(env, wt)
	if err != nil {
		t.Fatalf("resolveMainroot(worktree): %v", err)
	}
	if fromMain != fromWt {
		t.Errorf("worktree common-dir %q != main common-dir %q", fromWt, fromMain)
	}
}

// --- M1: --from arg-injection guard -----------------------------------------

func TestParseTaskNewArgs_RejectsDashFrom(t *testing.T) {
	// A --from value beginning with '-' would be read by git as an option.
	for _, argv := range [][]string{
		{"work", "--from", "-x"},
		{"work", "--from=-rf"},
	} {
		if _, _, _, err := parseTaskNewArgs(argv); err == nil {
			t.Errorf("parseTaskNewArgs(%v) = nil error, want rejection", argv)
		} else if !strings.Contains(err.Error(), "must not begin with '-'") {
			t.Errorf("parseTaskNewArgs(%v) error = %q, want dash rejection", argv, err)
		}
	}
	// A normal ref is still accepted.
	if _, from, _, err := parseTaskNewArgs([]string{"work", "--from", "main"}); err != nil || from != "main" {
		t.Errorf("parseTaskNewArgs good --from: from=%q err=%v", from, err)
	}
}

// --- M2: tampered metadata never targets a main-repo ref --------------------

func TestHardenTaskMeta_RejectsMismatchedName(t *testing.T) {
	// The stored name must sanitize back to the file base, else the file was
	// renamed or hand-edited.
	if _, err := hardenTaskMeta(taskMeta{Name: "evil", Profile: "default"}, "/main", "abcd1234", false, "work"); err == nil {
		t.Error("mismatched name should be rejected")
	}
	m, err := hardenTaskMeta(taskMeta{Name: "work", Profile: "default", Branch: "pix/../../heads/main", Sandbox: "sneaky"}, "/main", "abcd1234", false, "work")
	if err != nil {
		t.Fatalf("valid name should pass: %v", err)
	}
	if m.Branch != "pix/work" {
		t.Errorf("branch = %q, want re-derived pix/work", m.Branch)
	}
	if m.Sandbox != taskSandboxName("main", "abcd1234", "work", "default") {
		t.Errorf("sandbox = %q, want re-derived", m.Sandbox)
	}
	if m.Mainroot != "/main" {
		t.Errorf("mainroot = %q, want re-derived /main", m.Mainroot)
	}
}

func TestTaskMeta_TamperedBranchNeverTargetsMainRef(t *testing.T) {
	main := newMainRepo(t)
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pix/work", "HEAD")
	// A commit that lives only in the clone.
	if err := os.WriteFile(filepath.Join(co, "g"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tgit(t, co, "add", ".")
	tgit(t, co, "commit", "-q", "-m", "task work")

	// Record every git fetch invocation while delegating to real git.
	var fetches [][]string
	env := hostenv.Env{System: &systest.Fake{RunFn: func(name string, args ...string) (string, error) {
		if name == "sbx" {
			return "", nil
		}
		if name == "git" && len(args) >= 3 && args[2] == "fetch" {
			fetches = append(fetches, append([]string{name}, args...))
		}
		out, err := exec.Command(name, args...).CombinedOutput()
		return string(out), err
	}}}

	// A tampered branch that, un-hardened, would resolve a fetch onto refs/heads/main.
	tampered := taskMeta{Name: "work", Profile: "default", Branch: "pix/../../heads/main", Sandbox: "x", Mainroot: main}
	before := strings.TrimSpace(tgit(t, main, "rev-parse", "refs/heads/main"))

	hardened, err := hardenTaskMeta(tampered, main, taskRepoKey(main), false, "work")
	if err != nil {
		t.Fatalf("harden: %v", err)
	}
	st := gatherTaskState(env, hardened, co)
	if st.unrec != 1 {
		t.Errorf("unrec = %d, want 1", st.unrec)
	}

	after := strings.TrimSpace(tgit(t, main, "rev-parse", "refs/heads/main"))
	if before != after {
		t.Fatalf("refs/heads/main moved: %q -> %q", before, after)
	}
	// Every fetch destination refspec must live under refs/pix/{_chk,recovered}/work.
	for _, f := range fetches {
		spec := f[len(f)-1]
		dst := spec
		if i := strings.LastIndexByte(spec, ':'); i >= 0 {
			dst = spec[i+1:]
		}
		ok := strings.HasPrefix(dst, "refs/pix/_chk/work") || strings.HasPrefix(dst, "refs/pix/recovered/work")
		if !ok {
			t.Errorf("fetch targeted a ref outside refs/pix/{_chk,recovered}/work: %q", dst)
		}
	}
}

// --- M3: origin token leak + file mode --------------------------------------

func TestStripURLUserinfo(t *testing.T) {
	cases := map[string]string{
		"https://user:token@host/org/repo.git": "https://host/org/repo.git",
		"https://host/org/repo.git":            "https://host/org/repo.git",
		"git@github.com:org/repo.git":          "git@github.com:org/repo.git",
		"":                                     "",
	}
	for in, want := range cases {
		if got := stripURLUserinfo(in); got != want {
			t.Errorf("stripURLUserinfo(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWriteTaskMeta_StripsUserinfoAndMode(t *testing.T) {
	dir := t.TempDir()
	metaPath := filepath.Join(dir, "meta", "work.json")
	m := taskMeta{Name: "work", Origin: "https://user:token@host/org/repo.git"}
	if err := writeTaskMeta(metaPath, m); err != nil {
		t.Fatalf("writeTaskMeta: %v", err)
	}
	fi, err := os.Stat(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("meta file mode = %o, want 600", fi.Mode().Perm())
	}
	di, err := os.Stat(filepath.Dir(metaPath))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("meta dir mode = %o, want 700", di.Mode().Perm())
	}
	b, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "user:token") || strings.Contains(string(b), "token@") {
		t.Errorf("stored origin leaked userinfo:\n%s", b)
	}
	got, err := readTaskMeta(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.Origin != "https://host/org/repo.git" {
		t.Errorf("stored origin = %q, want stripped", got.Origin)
	}
}

// --- L1: --force must abort when snapshot fails and work is unrecoverable ----

func TestTaskForceSnapshotAbort(t *testing.T) {
	cases := []struct {
		name           string
		force          bool
		unrec          int
		unknown        bool
		snapshotFailed bool
		want           bool
	}{
		{"force + unrec + snapshot failed aborts", true, 2, false, true, true},
		{"force + unknown + snapshot failed aborts", true, 0, true, true, true},
		{"clean force (snapshot ok) proceeds", true, 2, false, false, false},
		{"clean force (snapshot ok) proceeds even if unknown", true, 0, true, false, false},
		{"force but nothing unrecoverable + known proceeds", true, 0, false, true, false},
		{"no force never aborts here (guard already ran)", false, 2, false, true, false},
		{"no force never aborts here even if unknown", false, 0, true, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := taskForceSnapshotAbort(c.force, c.unrec, c.unknown, c.snapshotFailed); got != c.want {
				t.Errorf("taskForceSnapshotAbort(%v,%d,%v,%v) = %v, want %v", c.force, c.unrec, c.unknown, c.snapshotFailed, got, c.want)
			}
		})
	}
}

// --- L2: a failed probe is UNKNOWN (fail-safe refuse), not unrec=0 -----------

func TestGatherTaskState_ProbeFailIsUnknown(t *testing.T) {
	main := newMainRepo(t)
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pix/work", "HEAD")
	// Point the probe at a directory that is NOT a git repo so the _chk fetch
	// fails; unrec must read as UNKNOWN, and the guard must refuse without --force.
	badmain := t.TempDir()
	env := gitEnv(t, "", nil)
	m := taskMeta{Name: "work", Sandbox: "x", Mainroot: badmain, Branch: "pix/work"}
	st := gatherTaskState(env, m, co)
	if !st.unknown {
		t.Errorf("expected unknown=true on failed probe, got %+v", st)
	}
	if msg, ok := taskRemoveGuard(st, false); ok {
		t.Error("unknown unrec should refuse teardown without --force")
	} else if !strings.Contains(msg, "could not determine") {
		t.Errorf("guard msg = %q, want unknown-probe reason", msg)
	}
	if _, ok := taskRemoveGuard(st, true); !ok {
		t.Error("--force should still override an unknown probe")
	}
}

// --- R1-1: tri-state sandbox probe (errored != absent) ----------------------

func TestProbeTaskSandbox_TriState(t *testing.T) {
	name := "pix-t-x-work"
	// running: present with a running status column.
	env := gitEnv(t, name+"  img  running\n", nil)
	if got := probeTaskSandbox(env, name); got != sbxRunning {
		t.Errorf("running: got %v, want sbxRunning", got)
	}
	// stopped: present with any other status.
	env = gitEnv(t, name+"  img  stopped\n", nil)
	if got := probeTaskSandbox(env, name); got != sbxStopped {
		t.Errorf("stopped: got %v, want sbxStopped", got)
	}
	// absent: sbx responded but the name is not listed.
	env = gitEnv(t, "some-other  img  running\n", nil)
	if got := probeTaskSandbox(env, name); got != sbxAbsent {
		t.Errorf("absent: got %v, want sbxAbsent", got)
	}
	// unknown: sbx invocation errors. Must NOT read as absent.
	errEnv := hostenv.Env{System: &systest.Fake{RunFn: func(n string, a ...string) (string, error) {
		return "boom", fmt.Errorf("sbx exploded")
	}}}
	if got := probeTaskSandbox(errEnv, name); got != sbxUnknown {
		t.Errorf("errored sbx: got %v, want sbxUnknown", got)
	}
	// unknown: no runner wired.
	if got := probeTaskSandbox(hostenv.Env{System: &systest.Fake{}}, name); got != sbxUnknown {
		t.Errorf("nil runner: got %v, want sbxUnknown", got)
	}
}

// --- R1-3: a failed `git status` probe is UNKNOWN, not clean -----------------

func TestGatherTaskState_StatusFailIsUnknown(t *testing.T) {
	main := newMainRepo(t)
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pix/work", "HEAD")
	// Fail ONLY `git status --porcelain`; delegate every other command to real
	// git so unrec resolves cleanly (0). The dirty state is therefore UNKNOWN.
	env := hostenv.Env{System: &systest.Fake{RunFn: func(name string, args ...string) (string, error) {
		if name == "sbx" {
			return "", nil
		}
		if name == "git" && len(args) >= 4 && args[2] == "status" && args[3] == "--porcelain" {
			return "fatal: simulated status failure", fmt.Errorf("status blew up")
		}
		out, err := exec.Command(name, args...).CombinedOutput()
		return string(out), err
	}}}
	m := taskMeta{Name: "work", Sandbox: "x", Mainroot: main, Branch: "pix/work"}
	st := gatherTaskState(env, m, co)
	if !st.unknown {
		t.Errorf("expected unknown=true on failed status probe, got %+v", st)
	}
	if st.dirty {
		t.Error("a failed status probe must not report dirty")
	}
	if _, ok := taskRemoveGuard(st, false); ok {
		t.Error("unknown status should refuse teardown without --force")
	}
	if _, ok := taskRemoveGuard(st, true); !ok {
		t.Error("--force should still override an unknown status")
	}
}

// --- R1-4 / R2-1: the CREATION profile drives the sandbox name; a meta with no
// stored profile is INVALID (no current-profile fallback) ---------------------

func TestHardenTaskMeta_UsesStoredProfileNotCurrent(t *testing.T) {
	// A task created under an old, named profile "work" must still harden cleanly
	// (an OLDER meta may carry a real profile name); hardenTaskMeta never reads it
	// back off the current environment.
	m := taskMeta{Name: "work", Profile: "work"}
	hardened, err := hardenTaskMeta(m, "/main", "abcd1234", false, "work")
	if err != nil {
		t.Fatalf("harden: %v", err)
	}
	want := taskSandboxName("main", "abcd1234", "work", "work")
	if hardened.Sandbox != want {
		t.Errorf("sandbox = %q, want %q (stored profile must win)", hardened.Sandbox, want)
	}
	if !strings.HasSuffix(hardened.Sandbox, "-work") {
		t.Errorf("sandbox %q lost its -work profile suffix", hardened.Sandbox)
	}
	// AC-P0-008: a meta with NO stored profile (what the current writer always
	// produces, since profiles were removed) must harden cleanly too, not be
	// rejected as invalid — that rejection was the actual bug, hiding every task
	// the real writer creates.
	current := taskMeta{Name: "work"}
	hardened, err = hardenTaskMeta(current, "/main", "abcd1234", false, "work")
	if err != nil {
		t.Fatalf("harden of an empty-profile meta must succeed, got: %v", err)
	}
	if hardened.Sandbox == "" {
		t.Error("empty-profile meta hardened to an empty sandbox name")
	}
}

// --- R2-3: `task ls` surfaces an unknown git state, not clean/0 --------------

func TestRunTaskLs_UnknownStateSurfaced(t *testing.T) {
	main := newMainRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	t.Setenv("PIX_PROFILE", "")
	t.Chdir(main)

	// Delegate every command to real git EXCEPT `git status --porcelain`, which
	// fails; that makes the clean/dirty state UNKNOWN in gatherTaskState.
	env := hostenv.Env{System: &systest.Fake{RunFn: func(name string, args ...string) (string, error) {
		if name == "sbx" {
			return "", nil
		}
		if name == "git" && len(args) >= 4 && args[2] == "status" && args[3] == "--porcelain" {
			return "fatal: simulated status failure", fmt.Errorf("status blew up")
		}
		out, err := exec.Command(name, args...).CombinedOutput()
		return string(out), err
	}}}

	// Lay down a clone + meta the way `task new` would, keyed the way ls resolves.
	mainroot, err := resolveMainroot(env, main)
	if err != nil {
		t.Fatalf("resolveMainroot: %v", err)
	}
	repokey := taskRepoKey(mainroot)
	co, metaPath := taskPaths(repokey, "work")
	makeTaskClone(t, main, co, "pix/work", "HEAD")
	if err := writeTaskMeta(metaPath, taskMeta{
		Name: "work", Mode: "localclone", Profile: "default", Branch: "pix/work",
	}); err != nil {
		t.Fatalf("writeTaskMeta: %v", err)
	}

	human := captureStdout(t, func() { runTaskLs(env, nil) })
	if !strings.Contains(human, "unknown") {
		t.Errorf("human ls did not surface the unknown state:\n%s", human)
	}
	if strings.Contains(human, "clean") {
		t.Errorf("human ls reported an unknown task as clean:\n%s", human)
	}

	js := captureStdout(t, func() { runTaskLs(env, []string{"--json"}) })
	if !strings.Contains(js, "\"unknown\": true") {
		t.Errorf("json ls did not serialize unknown:true:\n%s", js)
	}
}

// captureStderr runs fn with os.Stderr swapped for a pipe and returns whatever
// fn wrote, mirroring captureStdout (state_test.go).
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	rp, wp, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = wp
	fn()
	_ = wp.Close()
	os.Stderr = old
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(rp)
	return buf.String()
}

// --- AC-P0-008/009: `task ls` must never hide a row because the real writer's
// own metadata disagrees with what the harden guard expects. Profiles were
// removed (workspace.LoadResolvedConfig always returns "" now, see profile.go), so every
// task the CURRENT `task new` writes stores an empty profile; hardenTaskMeta
// used to reject exactly that, which meant `task ls` hid every task on a fresh
// checkout. This test drives the real writer end to end — a hand-built
// taskMeta fixture would not have caught the bug, because the fixture and the
// writer could quietly drift apart (which is exactly what happened). ---------

func TestTaskNewLs_RoundTrip_EmptyProfileMetadataListed(t *testing.T) {
	main := newMainRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	t.Setenv("PIX_PROFILE", "")
	t.Chdir(main)

	orig := taskLaunch
	taskLaunch = func(o runOpts) error { return nil }
	t.Cleanup(func() { taskLaunch = orig })

	env := gitEnv(t, "", nil)
	runTaskNew(env, []string{"roundtrip"})

	// Confirm this reproduces the actual bug: the real writer stores an empty
	// profile today, on disk, not a value this test invented.
	mainroot, err := resolveMainroot(env, main)
	if err != nil {
		t.Fatalf("resolveMainroot: %v", err)
	}
	_, metaPath := taskPaths(taskRepoDir(mainroot), "roundtrip")
	metaBytes, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("reading the written meta.json: %v", err)
	}
	if !strings.Contains(string(metaBytes), `"profile": ""`) {
		t.Fatalf("expected the real writer to store an empty profile (reproducing the bug), got:\n%s", metaBytes)
	}
	// Evidence for uat/w0-task-roundtrip.log: the exact on-disk empty-profile
	// JSON the real writer produced (printed under -v so the artifact can quote
	// it verbatim, not a hand-typed stand-in).
	t.Logf("on-disk meta.json (%s):\n%s", metaPath, metaBytes)

	var stderr string
	stdout := captureStdout(t, func() {
		stderr = captureStderr(t, func() { runTaskLs(env, nil) })
	})

	if !strings.Contains(stdout, "roundtrip") {
		t.Errorf("task ls did not list the task it just created:\n%s", stdout)
	}
	if strings.Contains(stderr, "skipping") {
		t.Errorf("task ls emitted a skipping warning for the writer's own metadata:\n%s", stderr)
	}
	if strings.Contains(stdout, taskMetaUnreadableStatus) {
		t.Errorf("a valid empty-profile task must not render as unreadable:\n%s", stdout)
	}

	// --json must not hide it either.
	jsonOut := captureStdout(t, func() { runTaskLs(env, []string{"--json"}) })
	if !strings.Contains(jsonOut, `"name": "roundtrip"`) {
		t.Errorf("json ls did not list the task:\n%s", jsonOut)
	}
}

func TestRunTaskLs_UnreadableMetadataStillAppearsAsADegradedRow(t *testing.T) {
	main := newMainRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	t.Setenv("PIX_PROFILE", "")
	t.Chdir(main)

	env := gitEnv(t, "", nil)
	mainroot, err := resolveMainroot(env, main)
	if err != nil {
		t.Fatalf("resolveMainroot: %v", err)
	}
	_, metaPath := taskPaths(taskRepoDir(mainroot), "broken")
	if err := os.MkdirAll(filepath.Dir(metaPath), 0o700); err != nil {
		t.Fatal(err)
	}
	// Genuinely corrupt on-disk bytes — not a hand-built taskMeta value. This is
	// what "unreadable" means: a torn write, a hand edit, or disk corruption, none
	// of which json.Unmarshal can parse.
	if err := os.WriteFile(metaPath, []byte("{ not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}

	human := captureStdout(t, func() { runTaskLs(env, nil) })
	if !strings.Contains(human, "broken") {
		t.Errorf("unreadable metadata omitted the task entirely from human output:\n%s", human)
	}
	if !strings.Contains(human, taskMetaUnreadableStatus) {
		t.Errorf("unreadable metadata did not render the degraded marker:\n%s", human)
	}

	js := captureStdout(t, func() { runTaskLs(env, []string{"--json"}) })
	if !strings.Contains(js, `"unreadable": true`) {
		t.Errorf("json ls did not mark the row unreadable:\n%s", js)
	}
	if !strings.Contains(js, `"name": "broken"`) {
		t.Errorf("json ls omitted the unreadable task:\n%s", js)
	}
}

// --- R2-4: a failed removal must not print a false "Removed" claim ------------

func TestRemoveTaskArtifacts_FailureNamesLeftovers(t *testing.T) {
	co := "/state/co/work"
	metaPath := "/state/meta/work.json"

	// Both removals succeed: no error.
	if err := removeTaskArtifacts(co, metaPath,
		func(string) error { return nil },
		func(string) error { return nil }); err != nil {
		t.Errorf("clean removal returned error: %v", err)
	}

	// An already-gone path (IsNotExist) is treated as success.
	if err := removeTaskArtifacts(co, metaPath,
		func(string) error { return os.ErrNotExist },
		func(string) error { return os.ErrNotExist }); err != nil {
		t.Errorf("already-gone paths should be success, got %v", err)
	}

	// A real clone-removal failure must surface, naming the clone that remains.
	err := removeTaskArtifacts(co, metaPath,
		func(string) error { return fmt.Errorf("device busy") },
		func(string) error { return nil })
	if err == nil {
		t.Fatal("a failed clone removal must return an error")
	}
	if !strings.Contains(err.Error(), co) {
		t.Errorf("error %q does not name the leftover clone %q", err, co)
	}

	// A meta-removal failure must surface too, naming the meta file.
	err = removeTaskArtifacts(co, metaPath,
		func(string) error { return nil },
		func(string) error { return fmt.Errorf("read-only fs") })
	if err == nil {
		t.Fatal("a failed metadata removal must return an error")
	}
	if !strings.Contains(err.Error(), metaPath) {
		t.Errorf("error %q does not name the leftover metadata %q", err, metaPath)
	}
}

// --- R1-6: a source-only ref checks out by resolved OID ----------------------

func TestTaskNew_SourceOnlyRefChecksOutByOID(t *testing.T) {
	main := newMainRepo(t)
	// Create a commit reachable ONLY from a remote-tracking-style ref, then drop
	// the local branch. `git clone --local` does not copy refs/remotes/*, so the
	// ref won't exist in the clone; only its OID can select the commit there.
	tgit(t, main, "checkout", "-q", "-b", "feat")
	if err := os.WriteFile(filepath.Join(main, "feat.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tgit(t, main, "add", ".")
	tgit(t, main, "commit", "-q", "-m", "feature work")
	oid := strings.TrimSpace(tgit(t, main, "rev-parse", "HEAD"))
	tgit(t, main, "update-ref", "refs/remotes/origin/feat", oid)
	tgit(t, main, "checkout", "-q", "main")
	tgit(t, main, "branch", "-q", "-D", "feat")

	// Sanity: the ref is NOT a local branch (checkout <ref> in a fresh clone
	// would fail); only the OID resolves.
	tmpState := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmpState)
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	t.Setenv("PIX_PROFILE", "")
	t.Chdir(main)

	orig := taskLaunch
	var launched runOpts
	launched.Name = ""
	taskLaunch = func(o runOpts) error { launched = o; return nil }
	t.Cleanup(func() { taskLaunch = orig })

	env := gitEnv(t, "", nil)
	runTaskNew(env, []string{"remote-oid", "--from", "refs/remotes/origin/feat"})

	if launched.Workspace == "" {
		t.Fatal("taskLaunch was not invoked")
	}
	co := launched.Workspace
	// The clone must be on the task branch, at the source-only commit's OID.
	head := strings.TrimSpace(tgit(t, co, "rev-parse", "HEAD"))
	if head != oid {
		t.Errorf("clone HEAD = %q, want source-only OID %q", head, oid)
	}
	branch := strings.TrimSpace(tgit(t, co, "rev-parse", "--abbrev-ref", "HEAD"))
	if branch != "pix/remote-oid" {
		t.Errorf("clone branch = %q, want pix/remote-oid", branch)
	}
	if _, err := os.Stat(filepath.Join(co, "feat.txt")); err != nil {
		t.Errorf("clone missing the source-only commit's file: %v", err)
	}
}

// --- R3-1: teardown-race fail-safe (single source of truth + fresh refusal) --

// TestTaskTeardownAbort is the pure fresh-probe decision: after the guard passed,
// a final sandbox probe reading running or unknown must still refuse to reach
// `sbx rm -f` unless --force is set.
func TestTaskTeardownAbort(t *testing.T) {
	cases := []struct {
		name  string
		final doctor.SbxState
		force bool
		want  bool
	}{
		{"running without force aborts", sbxRunning, false, true},
		{"unknown without force aborts", sbxUnknown, false, true},
		{"stopped without force proceeds", sbxStopped, false, false},
		{"absent without force proceeds", sbxAbsent, false, false},
		{"force overrides running", sbxRunning, true, false},
		{"force overrides unknown", sbxUnknown, true, false},
		{"force stopped proceeds", sbxStopped, true, false},
		{"force absent proceeds", sbxAbsent, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := taskTeardownAbort(c.final, c.force); got != c.want {
				t.Errorf("taskTeardownAbort(%v,%v) = %v, want %v", c.final, c.force, got, c.want)
			}
		})
	}
}

// countingSbxEnv is a hostenv.Env whose `sbx ls` returns a DIFFERENT status per call
// (so the gather-time probe and the teardown's fresh probe can disagree), records
// every sbx invocation, and delegates all other commands to real git.
func countingSbxEnv(name string, lsByCall []string, recorded *[][]string) hostenv.Env {
	var calls int
	return hostenv.Env{System: &systest.Fake{RunFn: func(cmd string, args ...string) (string, error) {
		if cmd == "sbx" {
			if recorded != nil {
				*recorded = append(*recorded, append([]string{cmd}, args...))
			}
			if len(args) > 0 && args[0] == "ls" {
				out := ""
				if calls < len(lsByCall) {
					out = lsByCall[calls]
				} else if len(lsByCall) > 0 {
					out = lsByCall[len(lsByCall)-1]
				}
				calls++
				if out == "ERR" {
					return "", fmt.Errorf("sbx ls exploded")
				}
				return out, nil
			}
			return "", nil
		}
		out, err := exec.Command(cmd, args...).CombinedOutput()
		return string(out), err
	}}}
}

// TestExecuteTaskTeardown_FreshRunningRefuses proves the R3-1 fix: the gather-time
// probe said STOPPED (guard passed), but by the final probe the sandbox is RUNNING.
// The executor must abort (rc 1) and NEVER call `sbx rm -f`.
func TestExecuteTaskTeardown_FreshRunningRefuses(t *testing.T) {
	main := newMainRepo(t)
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pix/work", "HEAD")
	sbxname := "pix-t-x-work"

	var recorded [][]string
	// Call 1 (gather) => stopped, call 2 (final probe) => running.
	env := countingSbxEnv(sbxname, []string{sbxname + "  img  stopped\n", sbxname + "  img  running\n"}, &recorded)

	m := taskMeta{Name: "work", Sandbox: sbxname, Mainroot: main, Branch: "pix/work"}
	st := gatherTaskState(env, m, co)
	if st.sandbox != sbxStopped {
		t.Fatalf("gather sandbox = %v, want sbxStopped", st.sandbox)
	}
	if _, ok := taskRemoveGuard(st, false); !ok {
		t.Fatal("guard should pass on the gather-time (stopped) state")
	}

	var out bytes.Buffer
	rc := executeTaskTeardown(env, &out, m, co, "work", "refs/pix/recovered/work", false, st)
	if rc != 1 {
		t.Fatalf("rc = %d, want 1 (fresh running must refuse)", rc)
	}
	for _, c := range recorded {
		if len(c) >= 2 && c[0] == "sbx" && c[1] == "rm" {
			t.Errorf("`sbx rm` was called despite a fresh running probe: %v", c)
		}
	}
	if !strings.Contains(out.String(), "now running") {
		t.Errorf("abort message missing the running explanation:\n%s", out.String())
	}
}

// TestExecuteTaskTeardown_FreshUnknownRefuses proves the same fail-safe when the
// final probe cannot determine the state (sbx ls errors).
func TestExecuteTaskTeardown_FreshUnknownRefuses(t *testing.T) {
	main := newMainRepo(t)
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pix/work", "HEAD")
	sbxname := "pix-t-x-work"

	var recorded [][]string
	// Call 1 (gather) => absent, call 2 (final probe) => sbx ls errors (unknown).
	env := countingSbxEnv(sbxname, []string{"", "ERR"}, &recorded)

	m := taskMeta{Name: "work", Sandbox: sbxname, Mainroot: main, Branch: "pix/work"}
	st := gatherTaskState(env, m, co)
	if st.sandbox != sbxAbsent {
		t.Fatalf("gather sandbox = %v, want sbxAbsent", st.sandbox)
	}

	var out bytes.Buffer
	rc := executeTaskTeardown(env, &out, m, co, "work", "refs/pix/recovered/work", false, st)
	if rc != 1 {
		t.Fatalf("rc = %d, want 1 (fresh unknown must refuse)", rc)
	}
	for _, c := range recorded {
		if len(c) >= 2 && c[0] == "sbx" && c[1] == "rm" {
			t.Errorf("`sbx rm` was called despite a fresh unknown probe: %v", c)
		}
	}
	if !strings.Contains(out.String(), "cannot determine") {
		t.Errorf("abort message missing the unknown explanation:\n%s", out.String())
	}
}

// TestExecuteTaskTeardown_ForceOverridesRunning proves --force lets a running
// sandbox proceed to `sbx rm -f` (the user has accepted stopping/removing).
func TestExecuteTaskTeardown_ForceOverridesRunning(t *testing.T) {
	main := newMainRepo(t)
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pix/work", "HEAD")
	sbxname := "pix-t-x-work"

	var recorded [][]string
	env := countingSbxEnv(sbxname, []string{sbxname + "  img  running\n"}, &recorded)

	m := taskMeta{Name: "work", Sandbox: sbxname, Mainroot: main, Branch: "pix/work"}
	st := gatherTaskState(env, m, co)

	var out bytes.Buffer
	rc := executeTaskTeardown(env, &out, m, co, "work", "refs/pix/recovered/work", true, st)
	if rc != 0 {
		t.Fatalf("rc = %d, want 0 (--force must proceed), out:\n%s", rc, out.String())
	}
	var rm []string
	for _, c := range recorded {
		if len(c) >= 2 && c[0] == "sbx" && c[1] == "rm" {
			rm = c
		}
	}
	want := []string{"sbx", "rm", "-f", sbxname}
	if !reflect.DeepEqual(rm, want) {
		t.Errorf("--force teardown argv = %v, want %v", rm, want)
	}
}

// TestExecuteTaskTeardown_NonForceIssuesRmWithoutF proves the R4-1 fix: a
// non-force teardown issues `sbx rm <name>` with NO -f flag. sbx itself refuses a
// running sandbox there, so the CLI call is the atomic guard rather than the racy
// probe-then-force-kill.
func TestExecuteTaskTeardown_NonForceIssuesRmWithoutF(t *testing.T) {
	main := newMainRepo(t)
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pix/work", "HEAD")
	sbxname := "pix-t-x-work"

	var recorded [][]string
	// Both probes read stopped: guard passes and the fresh probe stays friendly.
	env := countingSbxEnv(sbxname, []string{sbxname + "  img  stopped\n"}, &recorded)

	m := taskMeta{Name: "work", Sandbox: sbxname, Mainroot: main, Branch: "pix/work"}
	st := gatherTaskState(env, m, co)

	var out bytes.Buffer
	rc := executeTaskTeardown(env, &out, m, co, "work", "refs/pix/recovered/work", false, st)
	if rc != 0 {
		t.Fatalf("rc = %d, want 0 (non-force stopped proceeds), out:\n%s", rc, out.String())
	}
	var rm []string
	for _, c := range recorded {
		if len(c) >= 2 && c[0] == "sbx" && c[1] == "rm" {
			rm = c
		}
	}
	want := []string{"sbx", "rm", sbxname}
	if !reflect.DeepEqual(rm, want) {
		t.Errorf("non-force teardown argv = %v, want %v (no -f)", rm, want)
	}
}

// TestExecuteTaskTeardown_NonForceRmFailureAborts proves that when `sbx rm` (no
// -f) FAILS — which is how sbx signals "that sandbox is running, refusing" — the
// executor aborts (rc 1), does NOT fall back to `sbx rm -f`, and leaves the clone
// intact (runTaskRm skips removeTaskArtifacts on any non-zero return).
func TestExecuteTaskTeardown_NonForceRmFailureAborts(t *testing.T) {
	main := newMainRepo(t)
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pix/work", "HEAD")
	sbxname := "pix-t-x-work"

	var recorded [][]string
	// sbx ls => stopped (guard + fresh probe both pass); `sbx rm` (no -f) fails,
	// mimicking sbx refusing a sandbox that came up after the probe.
	env := hostenv.Env{System: &systest.Fake{RunFn: func(cmd string, args ...string) (string, error) {
		if cmd == "sbx" {
			recorded = append(recorded, append([]string{cmd}, args...))
			if len(args) > 0 && args[0] == "ls" {
				return sbxname + "  img  stopped\n", nil
			}
			if len(args) > 0 && args[0] == "rm" {
				return "Error: sandbox is running; use -f", fmt.Errorf("exit status 1")
			}
			return "", nil
		}
		out, err := exec.Command(cmd, args...).CombinedOutput()
		return string(out), err
	}}}

	m := taskMeta{Name: "work", Sandbox: sbxname, Mainroot: main, Branch: "pix/work"}
	st := gatherTaskState(env, m, co)

	var out bytes.Buffer
	rc := executeTaskTeardown(env, &out, m, co, "work", "refs/pix/recovered/work", false, st)
	if rc != 1 {
		t.Fatalf("rc = %d, want 1 (`sbx rm` failure must abort)", rc)
	}
	// Never fall back to -f on the non-force path.
	for _, c := range recorded {
		if len(c) >= 3 && c[0] == "sbx" && c[1] == "rm" && c[2] == "-f" {
			t.Errorf("non-force path fell back to `sbx rm -f`: %v", c)
		}
	}
	// The clone must still be on disk — executeTaskTeardown never removes it, and its
	// rc 1 makes runTaskRm skip removeTaskArtifacts entirely.
	if _, err := os.Stat(co); err != nil {
		t.Errorf("clone should be intact after abort, but stat(%s) failed: %v", co, err)
	}
	if !strings.Contains(out.String(), "still running") || !strings.Contains(out.String(), "--force") {
		t.Errorf("abort message missing the teachable running/--force hint:\n%s", out.String())
	}
}

// --- R5-1: the task-launch recreate path must not force-kill without --force --

// TestPrepareTaskLaunchSandbox_AbsentCreatesCleanly proves an absent sandbox is
// nothing in the way: prepare returns nil and never issues `sbx rm`.
func TestPrepareTaskLaunchSandbox_AbsentCreatesCleanly(t *testing.T) {
	name := "pix-t-x-work"
	var recorded [][]string
	env := gitEnv(t, "", &recorded)
	if err := prepareTaskLaunchSandbox(env, name); err != nil {
		t.Fatalf("absent should proceed, got %v", err)
	}
	for _, c := range recorded {
		if len(c) >= 2 && c[0] == "sbx" && c[1] == "rm" {
			t.Errorf("`sbx rm` issued for an absent sandbox: %v", c)
		}
	}
}

// TestPrepareTaskLaunchSandbox_StoppedIssuesRmWithoutF proves the R5-1 fix: a
// stopped pre-existing sandbox is recreated via `sbx rm <name>` with NO -f. sbx
// atomically refuses a running sandbox there, so a sandbox that came up after the
// probe is left alone rather than force-killed.
func TestPrepareTaskLaunchSandbox_StoppedIssuesRmWithoutF(t *testing.T) {
	name := "pix-t-x-work"
	var recorded [][]string
	env := gitEnv(t, name+"  img  stopped\n", &recorded)
	if err := prepareTaskLaunchSandbox(env, name); err != nil {
		t.Fatalf("stopped should recreate, got %v", err)
	}
	var rm []string
	for _, c := range recorded {
		if len(c) >= 2 && c[0] == "sbx" && c[1] == "rm" {
			rm = c
		}
	}
	want := []string{"sbx", "rm", name}
	if !reflect.DeepEqual(rm, want) {
		t.Errorf("stopped-recreate argv = %v, want %v (no -f)", rm, want)
	}
}

// TestPrepareTaskLaunchSandbox_RunningRefuses proves a RUNNING pre-existing
// sandbox is refused (a live session owns the name) and never force-removed.
func TestPrepareTaskLaunchSandbox_RunningRefuses(t *testing.T) {
	name := "pix-t-x-work"
	var recorded [][]string
	env := gitEnv(t, name+"  img  running\n", &recorded)
	err := prepareTaskLaunchSandbox(env, name)
	if err == nil {
		t.Fatal("running sandbox must be refused")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("error %q missing the running explanation", err)
	}
	for _, c := range recorded {
		if len(c) >= 2 && c[0] == "sbx" && c[1] == "rm" {
			t.Errorf("`sbx rm` issued for a running sandbox: %v", c)
		}
	}
}

// TestPrepareTaskLaunchSandbox_UnknownAborts proves an indeterminate probe (sbx
// ls errored) aborts the launch and never force-removes anything.
func TestPrepareTaskLaunchSandbox_UnknownAborts(t *testing.T) {
	name := "pix-t-x-work"
	var recorded [][]string
	env := hostenv.Env{System: &systest.Fake{RunFn: func(cmd string, args ...string) (string, error) {
		if cmd == "sbx" {
			recorded = append(recorded, append([]string{cmd}, args...))
			return "", fmt.Errorf("sbx ls exploded")
		}
		return "", nil
	}}}
	err := prepareTaskLaunchSandbox(env, name)
	if err == nil {
		t.Fatal("unknown probe must abort the launch")
	}
	if !strings.Contains(err.Error(), "cannot determine") {
		t.Errorf("error %q missing the unknown-state explanation", err)
	}
	for _, c := range recorded {
		if len(c) >= 2 && c[0] == "sbx" && c[1] == "rm" {
			t.Errorf("`sbx rm` issued on an unknown probe: %v", c)
		}
	}
}

// TestPrepareTaskLaunchSandbox_StoppedRmFailureAborts proves that when the
// non-force `sbx rm` FAILS (how sbx signals "that sandbox is running, refusing"),
// prepare aborts with a teachable message and NEVER falls back to `sbx rm -f`.
func TestPrepareTaskLaunchSandbox_StoppedRmFailureAborts(t *testing.T) {
	name := "pix-t-x-work"
	var recorded [][]string
	env := hostenv.Env{System: &systest.Fake{RunFn: func(cmd string, args ...string) (string, error) {
		if cmd == "sbx" {
			recorded = append(recorded, append([]string{cmd}, args...))
			if len(args) > 0 && args[0] == "ls" {
				return name + "  img  stopped\n", nil
			}
			if len(args) > 0 && args[0] == "rm" {
				return "Error: sandbox is running; use -f", fmt.Errorf("exit status 1")
			}
		}
		return "", nil
	}}}
	err := prepareTaskLaunchSandbox(env, name)
	if err == nil {
		t.Fatal("a failed non-force `sbx rm` must abort the launch")
	}
	if !strings.Contains(err.Error(), "may be running") || !strings.Contains(err.Error(), "sbx stop") {
		t.Errorf("error %q missing the teachable running/stop hint", err)
	}
	for _, c := range recorded {
		if len(c) >= 3 && c[0] == "sbx" && c[1] == "rm" && c[2] == "-f" {
			t.Errorf("prepare fell back to `sbx rm -f`: %v", c)
		}
	}
}

// countingSbxEnvRmErr behaves like countingSbxEnv but makes EVERY `sbx rm` (with
// or without -f) fail, so a test can exercise the post-failure re-probe
// classification. `sbx ls` still returns a DIFFERENT status per call from
// lsByCall (gather, fresh probe, then the re-probe after the rm fails).
func countingSbxEnvRmErr(name string, lsByCall []string, recorded *[][]string) hostenv.Env {
	var calls int
	return hostenv.Env{System: &systest.Fake{RunFn: func(cmd string, args ...string) (string, error) {
		if cmd == "sbx" {
			if recorded != nil {
				*recorded = append(*recorded, append([]string{cmd}, args...))
			}
			if len(args) > 0 && args[0] == "ls" {
				out := ""
				if calls < len(lsByCall) {
					out = lsByCall[calls]
				} else if len(lsByCall) > 0 {
					out = lsByCall[len(lsByCall)-1]
				}
				calls++
				if out == "ERR" {
					return "", fmt.Errorf("sbx ls exploded")
				}
				return out, nil
			}
			if len(args) > 0 && args[0] == "rm" {
				return "Error: sbx rm failed", fmt.Errorf("exit status 1")
			}
			return "", nil
		}
		out, err := exec.Command(cmd, args...).CombinedOutput()
		return string(out), err
	}}}
}

// rmArgv returns the last recorded `sbx rm ...` invocation, or nil.
func rmArgv(recorded [][]string) []string {
	var rm []string
	for _, c := range recorded {
		if len(c) >= 2 && c[0] == "sbx" && c[1] == "rm" {
			rm = c
		}
	}
	return rm
}

// TestExecuteTaskTeardown_AbsentStillIssuesRm proves the R6-1 fix: even when the
// fresh probe reads ABSENT, the non-force teardown ALWAYS routes through the
// atomic `sbx rm` (no -f). A probe reading absent can no longer green-light
// deleting the clone — sbx's exit status is the authority. Here the rm succeeds,
// so teardown proceeds (rc 0) to artifact removal.
func TestExecuteTaskTeardown_AbsentStillIssuesRm(t *testing.T) {
	main := newMainRepo(t)
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pix/work", "HEAD")
	sbxname := "pix-t-x-work"

	var recorded [][]string
	// Both probes read absent; `sbx rm` (delegated to "" nil by countingSbxEnv)
	// succeeds.
	env := countingSbxEnv(sbxname, []string{""}, &recorded)

	m := taskMeta{Name: "work", Sandbox: sbxname, Mainroot: main, Branch: "pix/work"}
	st := gatherTaskState(env, m, co)
	if st.sandbox != sbxAbsent {
		t.Fatalf("gather sandbox = %v, want sbxAbsent", st.sandbox)
	}

	var out bytes.Buffer
	rc := executeTaskTeardown(env, &out, m, co, "work", "refs/pix/recovered/work", false, st)
	if rc != 0 {
		t.Fatalf("rc = %d, want 0 (rm succeeded, proceed), out:\n%s", rc, out.String())
	}
	want := []string{"sbx", "rm", sbxname}
	if rm := rmArgv(recorded); !reflect.DeepEqual(rm, want) {
		t.Errorf("absent-path teardown argv = %v, want %v (rm always attempted, no -f)", rm, want)
	}
}

// TestExecuteTaskTeardown_NonForceRmFailsReprobeAbsentProceeds proves the
// was-not-present branch: `sbx rm` (no -f) fails, but a re-probe shows the
// sandbox is now absent — the failure was "no such sandbox", so teardown proceeds
// (rc 0) to artifact removal, still never falling back to -f.
func TestExecuteTaskTeardown_NonForceRmFailsReprobeAbsentProceeds(t *testing.T) {
	main := newMainRepo(t)
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pix/work", "HEAD")
	sbxname := "pix-t-x-work"

	var recorded [][]string
	// gather => absent, fresh probe => absent, re-probe after rm-failure => absent.
	env := countingSbxEnvRmErr(sbxname, []string{"", "", ""}, &recorded)

	m := taskMeta{Name: "work", Sandbox: sbxname, Mainroot: main, Branch: "pix/work"}
	st := gatherTaskState(env, m, co)

	var out bytes.Buffer
	rc := executeTaskTeardown(env, &out, m, co, "work", "refs/pix/recovered/work", false, st)
	if rc != 0 {
		t.Fatalf("rc = %d, want 0 (rm failed but sandbox gone, proceed), out:\n%s", rc, out.String())
	}
	want := []string{"sbx", "rm", sbxname}
	if rm := rmArgv(recorded); !reflect.DeepEqual(rm, want) {
		t.Errorf("teardown argv = %v, want %v (no -f)", rm, want)
	}
}

// TestExecuteTaskTeardown_NonForceRmFailsReprobeRunningAborts proves the TOCTOU
// close: `sbx rm` (no -f) fails and the re-probe shows the sandbox came up
// RUNNING. The executor aborts (rc 1), leaves the clone intact, and never falls
// back to -f.
func TestExecuteTaskTeardown_NonForceRmFailsReprobeRunningAborts(t *testing.T) {
	main := newMainRepo(t)
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pix/work", "HEAD")
	sbxname := "pix-t-x-work"

	var recorded [][]string
	// gather => stopped (guard passes), fresh probe => stopped (proceed), then
	// `sbx rm` fails and the re-probe shows the sandbox is now running.
	env := countingSbxEnvRmErr(sbxname, []string{sbxname + "  img  stopped\n", sbxname + "  img  stopped\n", sbxname + "  img  running\n"}, &recorded)

	m := taskMeta{Name: "work", Sandbox: sbxname, Mainroot: main, Branch: "pix/work"}
	st := gatherTaskState(env, m, co)

	var out bytes.Buffer
	rc := executeTaskTeardown(env, &out, m, co, "work", "refs/pix/recovered/work", false, st)
	if rc != 1 {
		t.Fatalf("rc = %d, want 1 (re-probe running must abort)", rc)
	}
	want := []string{"sbx", "rm", sbxname}
	if rm := rmArgv(recorded); !reflect.DeepEqual(rm, want) {
		t.Errorf("teardown argv = %v, want %v (no -f)", rm, want)
	}
	if _, err := os.Stat(co); err != nil {
		t.Errorf("clone should be intact after abort, but stat(%s) failed: %v", co, err)
	}
	if !strings.Contains(out.String(), "still running") || !strings.Contains(out.String(), "--force") {
		t.Errorf("abort message missing the teachable running/--force hint:\n%s", out.String())
	}
}

// TestExecuteTaskTeardown_NonForceRmFailsReprobeUnknownAborts proves the
// fail-safe: `sbx rm` (no -f) fails and the re-probe cannot determine the state
// (sbx ls errors). The executor aborts (rc 1) and leaves the clone intact.
func TestExecuteTaskTeardown_NonForceRmFailsReprobeUnknownAborts(t *testing.T) {
	main := newMainRepo(t)
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pix/work", "HEAD")
	sbxname := "pix-t-x-work"

	var recorded [][]string
	// gather => stopped, fresh probe => stopped, then `sbx rm` fails and the
	// re-probe errors (unknown).
	env := countingSbxEnvRmErr(sbxname, []string{sbxname + "  img  stopped\n", sbxname + "  img  stopped\n", "ERR"}, &recorded)

	m := taskMeta{Name: "work", Sandbox: sbxname, Mainroot: main, Branch: "pix/work"}
	st := gatherTaskState(env, m, co)

	var out bytes.Buffer
	rc := executeTaskTeardown(env, &out, m, co, "work", "refs/pix/recovered/work", false, st)
	if rc != 1 {
		t.Fatalf("rc = %d, want 1 (re-probe unknown must abort)", rc)
	}
	if _, err := os.Stat(co); err != nil {
		t.Errorf("clone should be intact after abort, but stat(%s) failed: %v", co, err)
	}
	if !strings.Contains(out.String(), "can no longer be determined") {
		t.Errorf("abort message missing the unknown-state explanation:\n%s", out.String())
	}
}

// --- R7-1: concurrent `task new` reservation is atomic; rollback is ownership-gated ---

// TestReserveTaskCheckout_AtomicOwnership proves the reservation primitive that
// closes the TOCTOU: the first reserve of a path OWNS it (owned=true), and any
// later reserve of the same path fails with fs.ErrExist WITHOUT disturbing the
// existing directory. That EEXIST is the atomic "task exists" signal, and the
// owned flag is what gates whether a caller may ever delete the dir.
func TestReserveTaskCheckout_AtomicOwnership(t *testing.T) {
	co := filepath.Join(t.TempDir(), "sub", "co")

	owned, err := reserveTaskCheckout(co)
	if err != nil {
		t.Fatalf("first reserve failed: %v", err)
	}
	if !owned {
		t.Fatal("first reserve must report ownership (owned=true)")
	}
	if fi, err := os.Stat(co); err != nil || !fi.IsDir() {
		t.Fatalf("reserve did not create the checkout dir: %v", err)
	}

	// Fabricate a sentinel inside the reserved dir; a losing reserve must never
	// touch it (the losing concurrent `task new` must not delete the winner's work).
	sentinel := filepath.Join(co, "live-work.txt")
	if err := os.WriteFile(sentinel, []byte("do not delete\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	owned2, err := reserveTaskCheckout(co)
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second reserve error = %v, want fs.ErrExist", err)
	}
	if owned2 {
		t.Fatal("a losing reserve must NOT report ownership (owned=false)")
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("losing reserve disturbed the existing checkout: %v", err)
	}
}

// TestTaskNew_ExistingCheckoutNotDeleted drives the full runTaskNew against a
// checkout dir that already exists (the concurrent-loser case): it must report
// `task ... already exists`, exit 1, and NEVER delete the pre-existing dir (we
// did not create it). runTaskNew calls os.Exit, so it runs in a subprocess.
func TestTaskNew_ExistingCheckoutNotDeleted(t *testing.T) {
	// Subprocess branch first: it inherits MAIN + XDG_STATE_HOME from the parent
	// (do NOT create fresh temp dirs here, or co would resolve to a different path).
	if os.Getenv("PIX_TASKNEW_EXIST") == "1" {
		if err := os.Chdir(os.Getenv("PIX_TASKNEW_MAIN")); err != nil {
			t.Fatal(err)
		}
		runTaskNew(gitEnv(t, "", nil), []string{"busy"})
		return // unreachable: runTaskNew exits
	}

	main := newMainRepo(t)
	state := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("XDG_STATE_HOME", state)
	mainroot, err := resolveMainroot(gitEnv(t, "", nil), main)
	if err != nil {
		t.Fatal(err)
	}
	co, _ := taskPaths(taskRepoDir(mainroot), sanitizeTaskName("busy"))

	// Fabricate a pre-existing checkout with a sentinel standing in for a
	// sibling invocation's live work.
	if err := os.MkdirAll(co, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(co, "sibling-work.txt")
	if err := os.WriteFile(sentinel, []byte("live\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run", "TestTaskNew_ExistingCheckoutNotDeleted")
	cmd.Env = append(os.Environ(),
		"PIX_TASKNEW_EXIST=1",
		"PIX_TASKNEW_MAIN="+main,
		"XDG_STATE_HOME="+state,
		"PIX_CONFIG="+cfg,
		"PIX_PROFILE=")
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected an ExitError, got %v\n%s", err, out)
	}
	if ee.ExitCode() != 1 {
		t.Errorf("exit code = %d, want 1\n%s", ee.ExitCode(), out)
	}
	if !strings.Contains(string(out), "already exists") {
		t.Errorf("output missing the teachable 'already exists' message:\n%s", out)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("pre-existing checkout was deleted (sentinel gone): %v", err)
	}
}

// TestTaskNew_OwnedRollbackOnLaunchFailure proves the other half of the
// ownership gate: a FRESH reservation that this invocation created (owned) and
// that then fails at launch with a CERTAIN-absent sandbox rolls the clone back.
// runTaskNew calls os.Exit, so it runs in a subprocess.
func TestTaskNew_OwnedRollbackOnLaunchFailure(t *testing.T) {
	// Subprocess branch first: inherit MAIN + XDG_STATE_HOME from the parent.
	if os.Getenv("PIX_TASKNEW_BOOM") == "1" {
		if err := os.Chdir(os.Getenv("PIX_TASKNEW_MAIN")); err != nil {
			t.Fatal(err)
		}
		// Stub the launch to fail; `sbx ls` returns empty => sbxAbsent, the only
		// state under which runTaskNew rolls back the clone it created.
		taskLaunch = func(runOpts) error { return fmt.Errorf("launch exploded") }
		runTaskNew(gitEnv(t, "", nil), []string{"boom"})
		return // unreachable: runTaskNew exits
	}

	main := newMainRepo(t)
	state := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("XDG_STATE_HOME", state)
	mainroot, err := resolveMainroot(gitEnv(t, "", nil), main)
	if err != nil {
		t.Fatal(err)
	}
	co, metaPath := taskPaths(taskRepoKey(mainroot), sanitizeTaskName("boom"))

	cmd := exec.Command(os.Args[0], "-test.run", "TestTaskNew_OwnedRollbackOnLaunchFailure")
	cmd.Env = append(os.Environ(),
		"PIX_TASKNEW_BOOM=1",
		"PIX_TASKNEW_MAIN="+main,
		"XDG_STATE_HOME="+state,
		"PIX_CONFIG="+cfg,
		"PIX_PROFILE=")
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected an ExitError, got %v\n%s", err, out)
	}
	if ee.ExitCode() != 1 {
		t.Errorf("exit code = %d, want 1\n%s", ee.ExitCode(), out)
	}
	if !strings.Contains(string(out), "rolled back clone") {
		t.Errorf("output missing the rollback message:\n%s", out)
	}
	// Owned + certain-absent => the clone AND its meta are cleaned up.
	if _, err := os.Stat(co); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("owned clone was not rolled back: stat(%s) err = %v", co, err)
	}
	if _, err := os.Stat(metaPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("meta was not removed: stat(%s) err = %v", metaPath, err)
	}
}

// --- per-task lock: serialize the same-name concurrency class ----------------

// TestWithTaskLock_MutualExclusion proves two concurrent callers of the SAME
// (repokey, name) are serialized: only one runs the locked fn at a time, so the
// observed peak concurrency is exactly 1.
func TestWithTaskLock_MutualExclusion(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const repokey, name = "abcd1234", "work"

	var mu sync.Mutex
	inside, maxConcurrent := 0, 0
	work := func() error {
		mu.Lock()
		inside++
		if inside > maxConcurrent {
			maxConcurrent = inside
		}
		mu.Unlock()
		// Hold the lock long enough that a second holder would overlap if the
		// lock did not serialize them.
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		inside--
		mu.Unlock()
		return nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := withTaskLock(repokey, name, work); err != nil {
				t.Errorf("withTaskLock: %v", err)
			}
		}()
	}
	wg.Wait()
	if maxConcurrent != 1 {
		t.Errorf("peak concurrency = %d, want 1 (same-name ops must be serialized)", maxConcurrent)
	}
	// The lock file was created and left behind (it is not a task artifact).
	if _, err := os.Stat(taskLockPath(repokey, name)); err != nil {
		t.Errorf("lock file missing after withTaskLock: %v", err)
	}
}

// TestWithTaskLock_DistinctTasksConcurrent proves DIFFERENT names and DIFFERENT
// repokeys do NOT block each other: three distinct (repokey, name) locks can all
// be held at once. If they blocked, fewer than three would enter and the reads
// would time out.
func TestWithTaskLock_DistinctTasksConcurrent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	pairs := []struct{ repokey, name string }{
		{"aaaa1111", "alpha"},
		{"aaaa1111", "beta"},  // same repokey, different name
		{"bbbb2222", "alpha"}, // different repokey, same name
	}

	entered := make(chan struct{}, len(pairs))
	release := make(chan struct{})
	var wg sync.WaitGroup
	for _, p := range pairs {
		wg.Add(1)
		go func(repokey, name string) {
			defer wg.Done()
			_ = withTaskLock(repokey, name, func() error {
				entered <- struct{}{}
				<-release
				return nil
			})
		}(p.repokey, p.name)
	}
	// LIFO: close(release) first so every goroutine unblocks even on t.Fatalf
	// (Goexit runs defers), then wait for them.
	defer wg.Wait()
	defer close(release)
	for i := 0; i < len(pairs); i++ {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d of %d distinct-task locks entered concurrently; they blocked each other", i, len(pairs))
		}
	}
}

// TestTaskRm_AcquiresLock is the smoke test that `task rm` runs its critical
// section under the per-task lock: even a `no such task` rm creates (and
// releases) the lock file for the name before it looks for metadata. runTaskRm
// calls os.Exit, so it runs in a subprocess.
func TestTaskRm_AcquiresLock(t *testing.T) {
	if os.Getenv("PIX_TASKRM_LOCK") == "1" {
		if err := os.Chdir(os.Getenv("PIX_TASKRM_MAIN")); err != nil {
			t.Fatal(err)
		}
		runTaskRm(gitEnv(t, "", nil), []string{"ghost"})
		return // unreachable: runTaskRm exits
	}

	main := newMainRepo(t)
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	mainroot, err := resolveMainroot(gitEnv(t, "", nil), main)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := taskLockPath(taskRepoKey(mainroot), sanitizeTaskName("ghost"))

	cmd := exec.Command(os.Args[0], "-test.run", "TestTaskRm_AcquiresLock")
	cmd.Env = append(os.Environ(),
		"PIX_TASKRM_LOCK=1",
		"PIX_TASKRM_MAIN="+main,
		"XDG_STATE_HOME="+state,
		"PIX_CONFIG="+filepath.Join(t.TempDir(), "config.toml"),
		"PIX_PROFILE=")
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected an ExitError, got %v\n%s", err, out)
	}
	if ee.ExitCode() != 1 {
		t.Errorf("exit code = %d, want 1 (no such task)\n%s", ee.ExitCode(), out)
	}
	if !strings.Contains(string(out), "no such task") {
		t.Errorf("output missing 'no such task':\n%s", out)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Errorf("expected the per-task lock file at %s to exist after `task rm`: %v", lockPath, err)
	}
}
