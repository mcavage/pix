package env

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/hosttrust"
)

func writeFile(t *testing.T, path, content string) error {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// ── AC-11: containment refusal names both absolute paths ────────────────

func TestRefuseContainmentNestedRootNamesBothPaths(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, "sub", "env")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	err := RefuseContainment(root, []string{workspace})
	if err == nil {
		t.Fatal("RefuseContainment must refuse a root nested inside a declared workspace")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(err) = %d, want 2", got)
	}
	var containment *ContainmentError
	if !errors.As(err, &containment) {
		t.Fatalf("error = %#v, want *ContainmentError", err)
	}
	if containment.Root != root {
		t.Errorf("Root = %q, want %q", containment.Root, root)
	}
	if containment.Workspace != workspace {
		t.Errorf("Workspace = %q, want %q", containment.Workspace, workspace)
	}
	msg := err.Error()
	if !containsAll(msg, root, workspace) {
		t.Errorf("refusal text %q must name both absolute paths %q and %q", msg, root, workspace)
	}
}

// TestRefuseContainmentRootEqualsWorkspace: an environment root that IS the
// declared workspace (not merely nested under it) is refused the same way —
// "resolves inside" includes equality, not only a strict subdirectory.
func TestRefuseContainmentRootEqualsWorkspace(t *testing.T) {
	shared := t.TempDir()
	err := RefuseContainment(shared, []string{shared})
	if err == nil {
		t.Fatal("RefuseContainment must refuse a root equal to a declared workspace")
	}
	var containment *ContainmentError
	if !errors.As(err, &containment) {
		t.Fatalf("error = %#v, want *ContainmentError", err)
	}
}

// TestRefuseContainmentAllowsDisjointRoot is the negative control: a root
// that shares no ancestry with any declared workspace passes clean.
func TestRefuseContainmentAllowsDisjointRoot(t *testing.T) {
	root := t.TempDir()
	workspace := t.TempDir()
	if err := RefuseContainment(root, []string{workspace}); err != nil {
		t.Fatalf("RefuseContainment on disjoint paths = %v, want nil", err)
	}
}

// TestRefuseContainmentChecksEveryDeclaredWorkspace: the root need only sit
// inside ONE of several declared workspaces to be refused.
func TestRefuseContainmentChecksEveryDeclaredWorkspace(t *testing.T) {
	other1 := t.TempDir()
	other2 := t.TempDir()
	workspace := t.TempDir()
	root := filepath.Join(workspace, "env")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	err := RefuseContainment(root, []string{other1, other2, workspace})
	if err == nil {
		t.Fatal("RefuseContainment must refuse when ANY declared workspace contains the root")
	}
}

// TestRefuseContainmentWorkspaceSymlinkAliasBypass is finding A1's planted
// bypass: the WORKSPACE is registered by a symlinked alias path, while the
// environment root is a real subdirectory of the alias's physical target.
// Lexically the two paths share no prefix at all (disjoint text), so the
// pre-fix string-only comparison let this straight through; the physical
// (EvalSymlinks) forms are nested, and must be refused.
func TestRefuseContainmentWorkspaceSymlinkAliasBypass(t *testing.T) {
	realWorkspace := t.TempDir()
	root := filepath.Join(realWorkspace, "env")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	aliasParent := t.TempDir()
	workspaceAlias := filepath.Join(aliasParent, "workspace-alias")
	if err := os.Symlink(realWorkspace, workspaceAlias); err != nil {
		t.Fatal(err)
	}

	err := RefuseContainment(root, []string{workspaceAlias})
	if err == nil {
		t.Fatal("RefuseContainment must refuse: root is physically nested inside the workspace's symlinked alias, even though the authored strings are lexically disjoint")
	}
	var containment *ContainmentError
	if !errors.As(err, &containment) {
		t.Fatalf("error = %#v, want *ContainmentError", err)
	}
	// The error still names the AUTHORED (alias) workspace path, not the
	// resolved physical target a user never registered.
	if containment.Workspace != workspaceAlias {
		t.Errorf("Workspace = %q, want the authored alias path %q", containment.Workspace, workspaceAlias)
	}
}

// TestRefuseContainmentRootParentSymlinkAliasBypass is finding A1's other
// planted bypass: an ANCESTOR of root (not root itself, so
// RefuseSymlinkedRoot's own Lstat-of-root-only check would never catch it)
// is a symlink into the workspace. The lexical root path shares no prefix
// with the workspace, but its physical location is nested underneath it.
func TestRefuseContainmentRootParentSymlinkAliasBypass(t *testing.T) {
	workspace := t.TempDir()
	hidden := filepath.Join(workspace, "hidden")
	actualEnv := filepath.Join(hidden, "actualenv")
	if err := os.MkdirAll(actualEnv, 0o755); err != nil {
		t.Fatal(err)
	}

	elsewhere := t.TempDir()
	aliasedAncestor := filepath.Join(elsewhere, "alias")
	if err := os.Symlink(hidden, aliasedAncestor); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(aliasedAncestor, "actualenv") // root itself is a real dir, not a symlink

	if hosttrust.IsSymlink(root) {
		t.Fatal("test setup error: root itself must not be a symlink (that is RefuseSymlinkedRoot's own, separate case)")
	}

	err := RefuseContainment(root, []string{workspace})
	if err == nil {
		t.Fatal("RefuseContainment must refuse: root's own ancestor is a symlink whose target is nested inside the workspace")
	}
	var containment *ContainmentError
	if !errors.As(err, &containment) {
		t.Fatalf("error = %#v, want *ContainmentError", err)
	}
	if containment.Root != root {
		t.Errorf("Root = %q, want the authored (unresolved) root %q", containment.Root, root)
	}
}

// TestRefuseContainmentNonexistentPathsStillUsePureArithmetic is the negative
// control the doc comment promises: paths that do not exist on disk at all
// (the shape every hypothetical/fabricated-path test in this file and
// errors_test.go already relies on) have nothing to EvalSymlinks, so they
// still resolve via lexical canonicalization alone — unaffected by this
// finding's fix.
func TestRefuseContainmentNonexistentPathsStillUsePureArithmetic(t *testing.T) {
	if err := RefuseContainment("/does/not/exist/env", []string{"/does/not/exist"}); err == nil {
		t.Fatal("a nonexistent root nested (lexically) inside a nonexistent workspace must still refuse via lexical arithmetic")
	}
	if err := RefuseContainment("/does/not/exist/other", []string{"/does/not/exist/ws"}); err != nil {
		t.Fatalf("disjoint nonexistent paths = %v, want nil", err)
	}
}

func containsAll(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// ── AC-12: a symlinked root or referenced executable is refused ─────────

// TestRefuseSymlinkedRoot is AC-12's root half of the original
// red_first_tests case: a symlinked environment root is refused, a real
// one is not. The reference half moved to the ResolveSymlinkedReference
// tests below: a referenced executable's symlink is now RESOLVED to its
// physical target rather than blindly refused (the gog/Homebrew fix).
func TestRefuseSymlinkedRoot(t *testing.T) {
	dir := t.TempDir()
	realRoot := filepath.Join(dir, "real-root")
	if err := os.MkdirAll(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	linkedRoot := filepath.Join(dir, "linked-root")
	if err := os.Symlink(realRoot, linkedRoot); err != nil {
		t.Fatal(err)
	}

	err := RefuseSymlinkedRoot(linkedRoot)
	if err == nil {
		t.Fatal("RefuseSymlinkedRoot must refuse a symlinked root")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(root refusal) = %d, want 2", got)
	}
	var rootSym *SymlinkError
	if !errors.As(err, &rootSym) || rootSym.Path != linkedRoot {
		t.Fatalf("root refusal = %#v, want *SymlinkError{Path: %q}", err, linkedRoot)
	}
	if err := RefuseSymlinkedRoot(realRoot); err != nil {
		t.Errorf("RefuseSymlinkedRoot on a real directory = %v, want nil", err)
	}
}

// ── ResolveSymlinkedReference: the gog/Homebrew fix ──────────────────────
//
// `/opt/homebrew/bin/gog` (or any Homebrew-installed command) is an
// ORDINARY symlink into the package's own Cellar keg. The prior
// RefuseSymlinkedReference refused ANY symlink outright, so an
// environment naming a Homebrew tool as an MCP server command made
// `pix env show`/`--effective` refuse to load at all. These tests pin the
// fixed contract: a symlink chain that resolves to a real, executable
// (or, for a non-command reference, directory) target is now TRUSTED AT
// ITS RESOLVED TARGET; only a broken, escaping, or wrong-shaped result is
// still refused.

// TestResolveSymlinkedReferenceNotASymlinkPassesThrough: the common case
// (a real file, never a symlink at all) returns path unchanged.
func TestResolveSymlinkedReferenceNotASymlinkPassesThrough(t *testing.T) {
	dir := t.TempDir()
	realExec := filepath.Join(dir, "warehouse-proxy")
	if err := writeFile(t, realExec, "#!/bin/sh\n"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(realExec, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveSymlinkedReference("local MCP command", realExec, true, nil)
	if err != nil {
		t.Fatalf("ResolveSymlinkedReference on a real file = %v, want nil", err)
	}
	if got != realExec {
		t.Errorf("got %q, want unchanged %q", got, realExec)
	}
}

// TestResolveSymlinkedReferenceResolvesHomebrewStyleSymlink is the literal
// reported blocker: a symlink (gog) pointing at a real executable (its own
// Cellar keg) resolves to that executable's PHYSICAL path rather than
// being refused.
func TestResolveSymlinkedReferenceResolvesHomebrewStyleSymlink(t *testing.T) {
	dir := t.TempDir()
	realExec := filepath.Join(dir, "Cellar", "gog", "1.0", "bin", "gog")
	if err := writeFile(t, realExec, "#!/bin/sh\n"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(realExec, 0o755); err != nil {
		t.Fatal(err)
	}
	linkedExec := filepath.Join(dir, "bin", "gog")
	if err := os.MkdirAll(filepath.Dir(linkedExec), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realExec, linkedExec); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveSymlinkedReference("local MCP command", linkedExec, true, nil)
	if err != nil {
		t.Fatalf("ResolveSymlinkedReference on a valid symlink chain = %v, want nil", err)
	}
	want, err := filepath.EvalSymlinks(realExec)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("got %q, want the resolved physical target %q", got, want)
	}
}

// TestResolveSymlinkedReferenceRefusesBrokenSymlink: a symlink whose target
// does not exist is refused with ReferenceBroken, never silently passed
// through as "nothing to check" the way a directly-missing path is.
func TestResolveSymlinkedReferenceRefusesBrokenSymlink(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken-link")
	if err := os.Symlink(filepath.Join(dir, "does-not-exist"), broken); err != nil {
		t.Fatal(err)
	}
	_, err := ResolveSymlinkedReference("local MCP command", broken, true, nil)
	if err == nil {
		t.Fatal("ResolveSymlinkedReference must refuse a broken symlink")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(broken) = %d, want 2", got)
	}
	var refErr *ReferenceTargetError
	if !errors.As(err, &refErr) || refErr.Reason != ReferenceBroken {
		t.Fatalf("broken refusal = %#v, want ReferenceTargetError{Reason: broken}", err)
	}
}

// TestResolveSymlinkedReferenceRefusesNonExecutableTargetWhenRequired: a
// symlink to a real, REGULAR but non-executable file is refused for a
// command reference (requireExecutable=true) with ReferenceNotExecutable.
func TestResolveSymlinkedReferenceRefusesNonExecutableTargetWhenRequired(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "data.txt")
	if err := writeFile(t, target, "not a program\n"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	_, err := ResolveSymlinkedReference("MCP server command foo", link, true, nil)
	if err == nil {
		t.Fatal("ResolveSymlinkedReference must refuse a non-executable target when requireExecutable is true")
	}
	var refErr *ReferenceTargetError
	if !errors.As(err, &refErr) || refErr.Reason != ReferenceNotExecutable {
		t.Fatalf("non-executable refusal = %#v, want ReferenceTargetError{Reason: not-executable}", err)
	}

	// The identical non-executable target is FINE when the caller does not
	// require one (a local kit path resolving through a symlink, for
	// example) — requireExecutable is the caller's own declared need, not a
	// universal rule.
	if _, err := ResolveSymlinkedReference("kit path kits[0]", link, false, nil); err != nil {
		t.Errorf("ResolveSymlinkedReference(requireExecutable=false) on a non-executable target = %v, want nil", err)
	}
}

// TestResolveSymlinkedReferenceAllowsDirectoryTargetForKits: a local kit
// path resolving, through a symlink, to a real DIRECTORY is accepted when
// requireExecutable is false, but refused as ReferenceNotRegular when a
// command reference (requireExecutable=true) resolves to one — a command
// must be a file, never a directory.
func TestResolveSymlinkedReferenceAllowsDirectoryTargetForKits(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "kit-real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "kit-link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveSymlinkedReference("kit path kits[0]", link, false, nil)
	if err != nil {
		t.Fatalf("ResolveSymlinkedReference(requireExecutable=false) on a directory target = %v, want nil", err)
	}
	want, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	if _, err := ResolveSymlinkedReference("MCP server command foo", link, true, nil); err == nil {
		t.Fatal("ResolveSymlinkedReference(requireExecutable=true) must refuse a directory target")
	} else {
		var refErr *ReferenceTargetError
		if !errors.As(err, &refErr) || refErr.Reason != ReferenceNotRegular {
			t.Fatalf("directory-as-command refusal = %#v, want ReferenceTargetError{Reason: not-regular}", err)
		}
	}
}

// TestResolveSymlinkedReferenceRefusesNonRegularTarget: a symlink resolving
// to neither a regular file nor a directory (a device node) is refused as
// ReferenceNotRegular.
func TestResolveSymlinkedReferenceRefusesNonRegularTarget(t *testing.T) {
	if _, err := os.Stat("/dev/null"); err != nil {
		t.Skip("/dev/null unavailable on this platform")
	}
	dir := t.TempDir()
	link := filepath.Join(dir, "link-to-device")
	if err := os.Symlink("/dev/null", link); err != nil {
		t.Fatal(err)
	}
	_, err := ResolveSymlinkedReference("local MCP command", link, false, nil)
	if err == nil {
		t.Fatal("ResolveSymlinkedReference must refuse a symlink resolving to a non-regular, non-directory target")
	}
	var refErr *ReferenceTargetError
	if !errors.As(err, &refErr) || refErr.Reason != ReferenceNotRegular {
		t.Fatalf("device refusal = %#v, want ReferenceTargetError{Reason: not-regular}", err)
	}
}

// TestResolveSymlinkedReferenceRefusesEscapingResolver: an injected
// resolver that returns a non-absolute or non-clean path (something the
// real filepath.EvalSymlinks never does) is refused as ReferenceEscape
// rather than trusted — defense in depth proven with the SAME fakeable-seam
// discipline ResolveLocalCommand's own tests already use for lookPath.
func TestResolveSymlinkedReferenceRefusesEscapingResolver(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "whatever"), link); err != nil {
		t.Fatal(err)
	}
	escaping := func(string) (string, error) { return "../etc/passwd", nil }
	_, err := ResolveSymlinkedReference("local MCP command", link, true, escaping)
	if err == nil {
		t.Fatal("ResolveSymlinkedReference must refuse a resolver answer that is not an absolute, clean path")
	}
	var refErr *ReferenceTargetError
	if !errors.As(err, &refErr) || refErr.Reason != ReferenceEscape {
		t.Fatalf("escape refusal = %#v, want ReferenceTargetError{Reason: escape}", err)
	}
}

// TestRequiresSymlinkCheckFailsClosedOnAmbiguousShapes: only an unambiguous
// URL-shaped remote reference is exempt. Everything else — a plain local
// path, an scp-style shorthand git accepts as remote but this classifier
// cannot positively identify as such, an empty string — still requires the
// check ("unknown local-vs-remote fails closed").
func TestRequiresSymlinkCheckFailsClosedOnAmbiguousShapes(t *testing.T) {
	local := []string{
		"./kit",
		"/abs/kit",
		"../kit",
		"git@host:path",
		"host:path",
		"",
		"warehouse-proxy",
	}
	for _, raw := range local {
		if !RequiresSymlinkCheck(raw) {
			t.Errorf("RequiresSymlinkCheck(%q) = false, want true (fail closed on anything not an unambiguous remote URL)", raw)
		}
	}

	remote := []string{
		"https://example.com/kit.git",
		"git+ssh://git@example.com/kit.git",
	}
	for _, raw := range remote {
		if RequiresSymlinkCheck(raw) {
			t.Errorf("RequiresSymlinkCheck(%q) = true, want false (unambiguous remote reference)", raw)
		}
	}
}

// ── local executable references resolve deterministically, never via cwd ─

// TestResolveLocalCommandBarePathSymlinkRegularMissing is the bare-command
// PATH matrix: symlinked target refused-worthy (the resolved path IS a
// symlink), a regular executable (resolved, not a symlink), and a missing
// one (lookPath fails, nothing to check — the same silence the prior
// os.Lstat-based check gave a nonexistent file).
func TestResolveLocalCommandBarePathSymlinkRegularMissing(t *testing.T) {
	dir := t.TempDir()
	realExec := filepath.Join(dir, "warehouse-proxy-real")
	if err := writeFile(t, realExec, "#!/bin/sh\n"); err != nil {
		t.Fatal(err)
	}
	linkedExec := filepath.Join(dir, "warehouse-proxy-link")
	if err := os.Symlink(realExec, linkedExec); err != nil {
		t.Fatal(err)
	}

	fake := func(target string, err error) func(string) (string, error) {
		return func(name string) (string, error) { return target, err }
	}

	resolved, ok := ResolveLocalCommand(dir, "warehouse-proxy", fake(linkedExec, nil))
	if !ok || resolved != linkedExec {
		t.Fatalf("ResolveLocalCommand(bare, symlinked) = (%q, %v), want (%q, true)", resolved, ok, linkedExec)
	}
	if !hosttrust.IsSymlink(resolved) {
		t.Errorf("resolved path %q must be the symlink lookPath found, not something Lstat'd relative to cwd", resolved)
	}

	resolved, ok = ResolveLocalCommand(dir, "warehouse-proxy", fake(realExec, nil))
	if !ok || resolved != realExec {
		t.Fatalf("ResolveLocalCommand(bare, regular) = (%q, %v), want (%q, true)", resolved, ok, realExec)
	}
	if hosttrust.IsSymlink(resolved) {
		t.Errorf("resolved path %q must not read as a symlink", resolved)
	}

	_, ok = ResolveLocalCommand(dir, "warehouse-proxy", fake("", errNotFound))
	if ok {
		t.Error("ResolveLocalCommand(bare, missing) must report ok=false: nothing local was found to check")
	}
}

// TestResolveLocalCommandRelativePathResolvesAgainstRootNotCwd: a raw value
// containing a path separator ("./bin/proxy") is NOT a bare PATH name — it
// must resolve against the environment's own root, never the calling
// process's cwd, exercised from >= 3 distinct cwd values the same way
// AC-10's TestResolveIdenticalAcrossThreeOrMoreCwds proves Resolve itself is
// cwd-independent.
func TestResolveLocalCommandRelativePathResolvesAgainstRootNotCwd(t *testing.T) {
	root := t.TempDir()
	want := filepath.Join(root, "bin", "proxy")

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })

	for _, cwd := range []string{t.TempDir(), t.TempDir(), t.TempDir()} {
		t.Run(cwd, func(t *testing.T) {
			if err := os.Chdir(cwd); err != nil {
				t.Fatal(err)
			}
			got, ok := ResolveLocalCommand(root, "./bin/proxy", nil)
			if !ok || got != want {
				t.Errorf("ResolveLocalCommand from cwd %s = (%q, %v), want (%q, true)", cwd, got, ok, want)
			}
		})
	}
}

// TestResolveLocalCommandAbsolutePathStaysAbsolute: an absolute raw value is
// returned unchanged (cleaned), never re-joined against root.
func TestResolveLocalCommandAbsolutePathStaysAbsolute(t *testing.T) {
	root := t.TempDir()
	abs := filepath.Join(t.TempDir(), "proxy")
	got, ok := ResolveLocalCommand(root, abs, nil)
	if !ok || got != abs {
		t.Errorf("ResolveLocalCommand(absolute) = (%q, %v), want (%q, true)", got, ok, abs)
	}
}

// errNotFound stands in for exec.ErrNotFound in a fake lookPath: only the
// non-nil-ness matters to ResolveLocalCommand.
var errNotFound = errors.New("executable file not found in $PATH")

// ── AC-16: repointing a name never inherits acceptance ───────────────────

// TestRepointNeverInheritsAcceptance is the exact red_first_tests case:
// "repoint test: acceptance reads unaccepted after the path changes." It
// additionally proves the stronger claim the task calls for: the OLD root's
// acceptance record is untouched and remains readable — repointing does not
// even affect it, let alone transfer it.
func TestRepointNeverInheritsAcceptance(t *testing.T) {
	oldRoot := t.TempDir()
	newRoot := t.TempDir()
	if newRoot == oldRoot {
		t.Fatalf("test setup error: t.TempDir() produced the same root twice %q", newRoot)
	}

	store := &hosttrust.AcceptanceStore{}
	store.Put(Subject(oldRoot), hosttrust.Record{Fingerprint: "accepted-old-fp"})

	if !IsAccepted(store, oldRoot) {
		t.Fatal("setup: old root must read accepted before the repoint")
	}

	// A name ("home", conceptually) repointed from oldRoot to newRoot: the
	// v2 selection model has no registry to repoint in this package (an
	// environment IS a directory; "repointing" it is a filesystem symlink
	// change ResolveIn resolves fresh on every call), but the acceptance
	// property under test is entirely about Subject/IsAccepted being keyed
	// by canonical ROOT, never by name — which two distinct roots alone
	// already prove.
	if IsAccepted(store, newRoot) {
		t.Error("the new root must NOT inherit the old root's acceptance (AC-16)")
	}
	if !IsAccepted(store, oldRoot) {
		t.Error("the old root's acceptance must remain scoped only to the old root, unaffected by the repoint")
	}

	// The name "home" itself carries no trust: looking a record up by the
	// CURRENT resolution of the name (newRoot) must not find the OLD
	// record merely because the name used to point there.
	if rec, ok := store.Get(Subject(newRoot)); ok {
		t.Errorf("Subject(newRoot) unexpectedly found a record: %+v", rec)
	}
}

func TestIsAcceptedFalseOnNilOrEmptyStore(t *testing.T) {
	root := t.TempDir()
	var nilStore *hosttrust.AcceptanceStore
	if IsAccepted(nilStore, root) {
		t.Error("a nil store must report unaccepted")
	}
	if IsAccepted(&hosttrust.AcceptanceStore{}, root) {
		t.Error("an empty store must report unaccepted")
	}
}
