package task

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// run executes git with argv (no implicit -C).
func run(argv ...string) (string, error) {
	cmd := exec.Command("git", argv...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

// runIn executes `git -C dir <argv...>`.
func runIn(dir string, argv ...string) (string, error) {
	return run(append([]string{"-C", dir}, argv...)...)
}

func parseCount(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

// ResolveMainroot returns the repository's git-common-dir as an absolute
// path (stable across a linked worktree, a normal repo, and a bare repo).
// This is what a task checkout is created FROM and the -C target for every
// git call against the main repo.
func ResolveMainroot(cwd string) (string, error) {
	out, err := runIn(cwd, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("%s", strings.TrimSpace(out))
	}
	gitdir := strings.TrimSpace(out)
	if gitdir == "" {
		return "", fmt.Errorf("empty git-common-dir")
	}
	return gitdir, nil
}

// ResolveCommit resolves ref (or HEAD when empty) to a concrete commit OID
// in mainroot. Resolving to an OID (not the ref string) matters for Clone:
// `git clone --local` does not copy the source's refs/remotes/* or
// refs/pull/*, so a source-only ref would fail a later checkout in the
// clone, while the OID always resolves (the object store is hardlinked).
func ResolveCommit(mainroot, ref string) (string, error) {
	if ref == "" {
		ref = "HEAD"
	}
	if strings.HasPrefix(ref, "-") {
		return "", fmt.Errorf("ref %q must not begin with '-'", ref)
	}
	out, err := runIn(mainroot, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("start point %q not found in %s", ref, mainroot)
	}
	oid := strings.TrimSpace(out)
	if oid == "" {
		return "", fmt.Errorf("start point %q did not resolve to a commit", ref)
	}
	return oid, nil
}

// OriginURL returns mainroot's `origin` remote URL, or "" when there is none.
func OriginURL(mainroot string) string {
	if out, err := runIn(mainroot, "remote", "get-url", "origin"); err == nil {
		return strings.TrimSpace(out)
	}
	return ""
}

// HasSubmodules reports whether mainroot declares any (v1: a task checkout
// does not auto-init them; the caller prints a note).
func HasSubmodules(mainroot string) bool {
	_, err := os.Stat(mainroot + "/.gitmodules")
	return err == nil
}

// ReserveCheckout atomically claims the checkout dir co: it creates co's
// parent tree, then os.Mkdir(co). Mkdir fails with fs.ErrExist when co
// already exists, which is the atomic "task exists" signal — no
// check-then-create race between two concurrent `task new` for the same
// name. owned=true only when THIS call created co, so a caller only ever
// rolls back a directory it is sure it alone created.
func ReserveCheckout(co string) (owned bool, err error) {
	if err := os.MkdirAll(dirOf(co), 0o700); err != nil {
		return false, err
	}
	if err := os.Mkdir(co, 0o700); err != nil {
		return false, err
	}
	return true, nil
}

func dirOf(p string) string {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return "."
	}
	return p[:i]
}

// createCheckout materializes branch (checked out at oid) into co, from
// mainroot, using mechanism.
func createCheckout(mechanism Mechanism, mainroot, co, oid, branch string) error {
	switch mechanism {
	case Worktree:
		if out, err := runIn(mainroot, "worktree", "add", "-q", "-b", branch, co, oid); err != nil {
			return fmt.Errorf("git worktree add: %w\n%s", err, out)
		}
		return nil
	default: // Clone
		if out, err := run("clone", "--quiet", "--local", mainroot, co); err != nil {
			return fmt.Errorf("git clone --local: %w\n%s", err, out)
		}
		if out, err := runIn(co, "checkout", "-q", "-b", branch, "--end-of-options", oid); err != nil {
			return fmt.Errorf("git checkout -b: %w\n%s", err, out)
		}
		return nil
	}
}

// PersistBranch ensures branch survives the checkout's removal. For
// Worktree this is a no-op: the branch already lives in mainroot's own
// refs/heads (a linked worktree shares the common object/ref store). For
// Clone it fetches the checkout's branch tip into mainroot under its OWN
// name — not a side "recovered" namespace, the branch persists as itself.
func PersistBranch(mechanism Mechanism, mainroot, co, branch string) error {
	if mechanism == Worktree {
		return nil
	}
	if out, err := runIn(mainroot, "fetch", "-q", co, "+"+branch+":"+branch); err != nil {
		return fmt.Errorf("could not persist %s into %s: %w\n%s", branch, mainroot, err, out)
	}
	return nil
}

// RemoveCheckout deletes the checkout directory itself. For Worktree this is
// `git worktree remove --force` (the --force there is git's OWN dirty/lock
// hygiene, not a sandbox override: RemoveGuard has already decided the
// removal is safe, or the caller's --force explicitly accepted the git
// hygiene risk). For Clone it is a plain recursive delete.
func RemoveCheckout(mechanism Mechanism, mainroot, co string) error {
	if mechanism == Worktree {
		if out, err := runIn(mainroot, "worktree", "remove", "--force", co); err != nil {
			return fmt.Errorf("git worktree remove: %w\n%s", err, out)
		}
		return nil
	}
	return os.RemoveAll(co)
}

// GatherGitState collects a checkout's git-hygiene facts. mechanism gates
// the expensive part: a Worktree checkout shares mainroot's object/ref
// store directly, so nothing committed there is EVER checkout-only —
// Unrecoverable is always 0 and no probe is needed. A Clone checkout is a
// fully independent repo, so Unrecoverable is measured by fetching its
// heads/HEAD/remotes into a throwaway namespace in mainroot and counting
// what mainroot cannot otherwise reach.
func GatherGitState(mechanism Mechanism, mainroot, co string) GitState {
	var st GitState
	if out, err := runIn(co, "status", "--porcelain"); err != nil {
		st.Unknown = true
	} else {
		for _, line := range strings.Split(out, "\n") {
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, "??") {
				st.Untracked = true
			} else {
				st.Dirty = true
			}
		}
	}
	if out, err := runIn(co, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"); err == nil && strings.TrimSpace(out) != "" {
		st.HasUpstream = true
		if c, err := runIn(co, "rev-list", "--count", "@{u}..HEAD"); err == nil {
			st.Ahead = parseCount(c)
		} else {
			st.Unknown = true
		}
	}
	if mechanism == Worktree {
		return st
	}
	ns := "refs/pix/_chk/" + SanitizeName(co)
	defer cleanupNamespace(mainroot, ns)
	if _, err := runIn(mainroot, "fetch", "-q", co,
		"+refs/heads/*:"+ns+"/heads/*",
		"+HEAD:"+ns+"/HEAD",
		"+refs/remotes/*:"+ns+"/remotes/*"); err != nil {
		st.Unknown = true
		return st
	}
	// The positive side is the checkout's own local branches + HEAD (ns/heads,
	// ns/HEAD). The negative (subtracted) side is mainroot's own normal refs
	// -- --branches/--tags/--remotes, NOT --all -- plus the checkout's own
	// remote-tracking view (ns/remotes, proof of an already-pushed commit).
	// --branches/--tags/--remotes can never match anything under the
	// refs/pix/* probe namespace ns lives in, so there is no --exclude
	// needed and thus no include/exclude ORDER for a future edit to get
	// wrong: an --all-based query needed --exclude=refs/pix/* to land on
	// the *exact* --all (or --glob) call it decorates (git's rev-list
	// scopes --exclude to only the next such flag) -- move --glob=ns/remotes
	// ahead of --all by mistake and --exclude silently binds to it instead,
	// self-cancelling the checkout's remote-tracking evidence and collapsing
	// Unrecoverable to 0 for every checkout, pushed or not. See
	// TestGatherGitState_BareOrigin_PushedAllowsRemoval_CloneOnlyBlocks.
	if out, err := runIn(mainroot, "rev-list", "--count",
		"--glob="+ns+"/heads", ns+"/HEAD",
		"--not", "--branches", "--tags", "--remotes", "--glob="+ns+"/remotes"); err == nil {
		st.Unrecoverable = parseCount(out)
	} else {
		st.Unknown = true
	}
	return st
}

func cleanupNamespace(mainroot, ns string) {
	if refs, err := runIn(mainroot, "for-each-ref", "--format=%(refname)", ns); err == nil {
		for _, r := range strings.Fields(refs) {
			_, _ = runIn(mainroot, "update-ref", "-d", r)
		}
	}
}
