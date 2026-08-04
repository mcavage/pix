package task

// Real-git integration tests: every case below drives an actual `git`
// binary against a temp repo (+ a temp bare "origin" for the ahead/no-upstream
// cases), never a fake. This is the "one unverified assumption" the design
// doc calls out for the sandbox-mount slice — but the git plumbing itself is
// fully exercised here.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func mustRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Args = append([]string{"git", "-C", dir}, args...)
	}
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.test",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.test",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// newMainroot creates a fresh repo with one commit and returns its
// git-common-dir (what ResolveMainroot would report).
func newMainroot(t *testing.T) (worktree, mainroot string) {
	t.Helper()
	dir := t.TempDir()
	mustRun(t, "", "init", "-q", "-b", "main", dir)
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o644)
	mustRun(t, dir, "add", ".")
	mustRun(t, dir, "commit", "-q", "-m", "init")
	root, err := ResolveMainroot(dir)
	if err != nil {
		t.Fatalf("ResolveMainroot: %v", err)
	}
	return dir, root
}

// withBareOrigin adds a bare "origin" remote to mainroot and pushes main to
// it, returning the bare repo's path.
func withBareOrigin(t *testing.T, mainroot string) string {
	t.Helper()
	bare := t.TempDir()
	mustRun(t, "", "init", "-q", "--bare", bare)
	mustRun(t, mainroot, "remote", "add", "origin", bare)
	mustRun(t, mainroot, "push", "-q", "origin", "main")
	return bare
}

func newTask(t *testing.T, mainroot, name string, mech Mechanism) (co string, m Meta) {
	t.Helper()
	state := t.TempDir()
	m, err := New(NewOptions{StateRoot: state, Mainroot: mainroot, Name: name, Mechanism: mech})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	co, err = Path(state, mainroot, name)
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	return co, m
}

func TestNew_CloneAndWorktree(t *testing.T) {
	for _, mech := range []Mechanism{Clone, Worktree} {
		t.Run(string(mech), func(t *testing.T) {
			_, mainroot := newMainroot(t)
			co, m := newTask(t, mainroot, "fix-login", mech)
			if _, err := os.Stat(filepath.Join(co, "f.txt")); err != nil {
				t.Fatalf("checkout missing the mainroot's file: %v", err)
			}
			if m.Branch != "pix/fix-login" {
				t.Errorf("Branch = %q", m.Branch)
			}
			out := mustRun(t, co, "rev-parse", "--abbrev-ref", "HEAD")
			if strings.TrimSpace(out) != "pix/fix-login" {
				t.Errorf("checked-out branch = %q", strings.TrimSpace(out))
			}
			// A commit made in the checkout must be visible without any
			// extra fetch for Worktree (shared store); for Clone it stays
			// local until PersistBranch runs.
			os.WriteFile(filepath.Join(co, "g.txt"), []byte("y\n"), 0o644)
			mustRun(t, co, "add", ".")
			mustRun(t, co, "commit", "-q", "-m", "work")
		})
	}
}

func TestGatherGitState_Dirty(t *testing.T) {
	_, mainroot := newMainroot(t)
	co, _ := newTask(t, mainroot, "work", Clone)
	os.WriteFile(filepath.Join(co, "f.txt"), []byte("changed\n"), 0o644)
	st := GatherGitState(Clone, mainroot, co)
	if !st.Dirty {
		t.Error("want Dirty=true")
	}
	if st.Untracked {
		t.Error("want Untracked=false")
	}
}

func TestGatherGitState_Untracked(t *testing.T) {
	_, mainroot := newMainroot(t)
	co, _ := newTask(t, mainroot, "work", Clone)
	os.WriteFile(filepath.Join(co, "new.txt"), []byte("y\n"), 0o644)
	st := GatherGitState(Clone, mainroot, co)
	if !st.Untracked {
		t.Error("want Untracked=true")
	}
	if st.Dirty {
		t.Error("want Dirty=false (untracked, not modified)")
	}
}

func TestGatherGitState_NoUpstream_UnrecoverableWhenUnpushed(t *testing.T) {
	_, mainroot := newMainroot(t)
	co, _ := newTask(t, mainroot, "work", Clone)
	mustRun(t, co, "commit", "-q", "--allow-empty", "-m", "clone-only work")
	st := GatherGitState(Clone, mainroot, co)
	if st.HasUpstream {
		t.Error("want HasUpstream=false (no origin push, no upstream configured)")
	}
	if st.Unrecoverable == 0 {
		t.Error("want Unrecoverable > 0: this commit lives only in the clone")
	}
}

func TestGatherGitState_Ahead_WithUpstream(t *testing.T) {
	_, mainroot := newMainroot(t)
	withBareOrigin(t, mainroot)
	co, m := newTask(t, mainroot, "work", Clone)
	mustRun(t, co, "push", "-q", "--set-upstream", "origin", m.Branch)
	mustRun(t, co, "commit", "-q", "--allow-empty", "-m", "ahead work")
	st := GatherGitState(Clone, mainroot, co)
	if !st.HasUpstream {
		t.Error("want HasUpstream=true")
	}
	if st.Ahead != 1 {
		t.Errorf("Ahead = %d, want 1", st.Ahead)
	}
	// This commit is genuinely UNPUSHED (made after the last push), so it is
	// correctly unrecoverable: ahead-of-upstream and would-be-lost are not the
	// same thing, but an ahead commit that was never pushed anywhere is both.
	if st.Unrecoverable != 1 {
		t.Errorf("Unrecoverable = %d, want 1 (never pushed anywhere)", st.Unrecoverable)
	}
}

func TestGatherGitState_PushedCommit_NotUnrecoverable(t *testing.T) {
	_, mainroot := newMainroot(t)
	withBareOrigin(t, mainroot)
	co, m := newTask(t, mainroot, "work", Clone)
	mustRun(t, co, "commit", "-q", "--allow-empty", "-m", "pushed work")
	mustRun(t, co, "push", "-q", "--set-upstream", "origin", m.Branch)
	st := GatherGitState(Clone, mainroot, co)
	if st.Ahead != 0 {
		t.Errorf("Ahead = %d, want 0 (in sync with upstream)", st.Ahead)
	}
	// mainroot has not fetched this commit at all, but the clone's OWN
	// remote-tracking ref proves it already lives on a remote -- not
	// checkout-only, so it must not count as unrecoverable.
	if st.Unrecoverable != 0 {
		t.Errorf("Unrecoverable = %d, want 0 (already pushed to origin)", st.Unrecoverable)
	}
}

func TestGatherGitState_Worktree_NeverUnrecoverable(t *testing.T) {
	_, mainroot := newMainroot(t)
	co, _ := newTask(t, mainroot, "work", Worktree)
	mustRun(t, co, "commit", "-q", "--allow-empty", "-m", "scratch work")
	st := GatherGitState(Worktree, mainroot, co)
	if st.Unrecoverable != 0 {
		t.Errorf("Unrecoverable = %d, want 0: a worktree shares mainroot's object store", st.Unrecoverable)
	}
}

func TestRemoveGuard_BlocksThenAllowsAfterPersist(t *testing.T) {
	_, mainroot := newMainroot(t)
	co, m := newTask(t, mainroot, "work", Clone)
	mustRun(t, co, "commit", "-q", "--allow-empty", "-m", "clone-only")
	st := GatherGitState(Clone, mainroot, co)
	if _, ok := RemoveGuard(st, SandboxAbsent, false); ok {
		t.Fatal("want the guard to refuse: unrecoverable commit")
	}
	if err := PersistBranch(Clone, mainroot, co, m.Branch); err != nil {
		t.Fatalf("PersistBranch: %v", err)
	}
	st2 := GatherGitState(Clone, mainroot, co)
	if st2.Unrecoverable != 0 {
		t.Errorf("Unrecoverable after persist = %d, want 0", st2.Unrecoverable)
	}
	if _, ok := RemoveGuard(st2, SandboxAbsent, false); !ok {
		t.Fatal("want the guard to allow removal once the branch is persisted")
	}
	// Checkout/branch persist: mainroot itself now carries the branch.
	out := mustRun(t, mainroot, "rev-parse", "--verify", m.Branch)
	if strings.TrimSpace(out) == "" {
		t.Fatal("want the branch to exist in mainroot after PersistBranch")
	}
}

func TestRemoveCheckout_CloneAndWorktree(t *testing.T) {
	for _, mech := range []Mechanism{Clone, Worktree} {
		t.Run(string(mech), func(t *testing.T) {
			_, mainroot := newMainroot(t)
			co, _ := newTask(t, mainroot, "work", mech)
			if err := RemoveCheckout(mech, mainroot, co); err != nil {
				t.Fatalf("RemoveCheckout: %v", err)
			}
			if _, err := os.Stat(co); !os.IsNotExist(err) {
				t.Fatalf("checkout still present: err=%v", err)
			}
		})
	}
}

func TestList_PredictsRemoval(t *testing.T) {
	_, mainroot := newMainroot(t)
	state := t.TempDir()
	if _, err := New(NewOptions{StateRoot: state, Mainroot: mainroot, Name: "clean"}); err != nil {
		t.Fatal(err)
	}
	co, _ := Path(state, mainroot, "dirty")
	_ = co
	if _, err := New(NewOptions{StateRoot: state, Mainroot: mainroot, Name: "dirty"}); err != nil {
		t.Fatal(err)
	}
	dirtyCo, _ := Path(state, mainroot, "dirty")
	os.WriteFile(filepath.Join(dirtyCo, "f.txt"), []byte("changed\n"), 0o644)

	names, err := SandboxNames(state, mainroot)
	if err != nil {
		t.Fatal(err)
	}
	dispositions := make(map[string]SandboxDisposition, len(names))
	for _, n := range names {
		dispositions[n] = SandboxAbsent
	}
	entries, err := List(state, mainroot, dispositions)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}
	byName := map[string]Entry{}
	for _, e := range entries {
		byName[e.Meta.Name] = e
	}
	if byName["clean"].WouldRefuse {
		t.Errorf("clean task predicted refuse: %v", byName["clean"].Reasons)
	}
	if !byName["dirty"].WouldRefuse {
		t.Error("dirty task should predict refuse")
	}
}

func TestList_NoProbeMeansUnknownPredictsRefuse(t *testing.T) {
	_, mainroot := newMainroot(t)
	state := t.TempDir()
	if _, err := New(NewOptions{StateRoot: state, Mainroot: mainroot, Name: "clean"}); err != nil {
		t.Fatal(err)
	}
	entries, err := List(state, mainroot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !entries[0].WouldRefuse {
		t.Error("a nil dispositions map must fail safe: unknown sandbox disposition always predicts refuse")
	}
}

// TestGatherGitState_BareOrigin_PushedAllowsRemoval_CloneOnlyBlocks is the
// U06 review-defect regression: it drives a REAL bare "origin" (not
// mainroot's own local clone path) so a pushed commit's only proof of
// survival is the checkout's OWN remote-tracking ref, fetched into the temp
// probe namespace -- exactly the ref set the include/exclude ordering in
// GatherGitState's rev-list must actually subtract. One task pushes then
// asks to be removed (must be ALLOWED); a second stays clone-only (must be
// REFUSED). A mis-ordered rev-list (--glob=<probe>/remotes sharing the
// --exclude that --all consumes, instead of --all on its own) silently
// collapses Unrecoverable to 0 for BOTH tasks -- this test catches that.
func TestGatherGitState_BareOrigin_PushedAllowsRemoval_CloneOnlyBlocks(t *testing.T) {
	_, mainroot := newMainroot(t)
	withBareOrigin(t, mainroot)

	// Task "pushed": the work lands on the bare origin before rm is
	// attempted. mainroot itself never fetches it back -- the checkout's own
	// remote-tracking ref (captured into the temp probe namespace) is the
	// ONLY evidence it survives elsewhere.
	coPushed, mPushed := newTask(t, mainroot, "pushed", Clone)
	mustRun(t, coPushed, "commit", "-q", "--allow-empty", "-m", "pushed work")
	mustRun(t, coPushed, "push", "-q", "--set-upstream", "origin", mPushed.Branch)
	stPushed := GatherGitState(Clone, mainroot, coPushed)
	if stPushed.Unrecoverable != 0 {
		t.Errorf("pushed task: Unrecoverable = %d, want 0 (already on the bare origin)", stPushed.Unrecoverable)
	}
	if _, ok := RemoveGuard(stPushed, SandboxAbsent, false); !ok {
		t.Error("pushed task: RemoveGuard should ALLOW removal; the work survives on origin")
	}

	// Task "clone-only": never pushed anywhere, even though mainroot HAS a
	// real bare origin configured (so this isn't the simpler no-upstream-at-
	// all case already covered elsewhere).
	coCloneOnly, _ := newTask(t, mainroot, "clone-only", Clone)
	mustRun(t, coCloneOnly, "commit", "-q", "--allow-empty", "-m", "clone-only work")
	stCloneOnly := GatherGitState(Clone, mainroot, coCloneOnly)
	if stCloneOnly.Unrecoverable == 0 {
		t.Fatal("clone-only task: want Unrecoverable > 0; this commit lives only in the clone")
	}
	if _, ok := RemoveGuard(stCloneOnly, SandboxAbsent, false); ok {
		t.Error("clone-only task: RemoveGuard should REFUSE removal; the work would be lost")
	}
}
