package env

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/hosttrust"
)

func writeFile(t *testing.T, path, content string) error {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ── AC-10: exact-name resolution is identical from any cwd ──────────────

// TestResolveIdenticalAcrossThreeOrMoreCwds is AC-10's own table test: the
// SAME registered name, looked up while the process stands in >= 3 distinct
// working directories, must return the byte-identical canonical root every
// time. Resolve never consults the working directory at all (see doc.go),
// so this proves that by exercising it, not merely by reading the source.
func TestResolveIdenticalAcrossThreeOrMoreCwds(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)
	envRoot := t.TempDir()
	want, err := Register(cfg, "home", envRoot)
	if err != nil {
		t.Fatal(err)
	}

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })

	cwds := []string{t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()}
	if len(cwds) < 3 {
		t.Fatalf("test setup error: need >= 3 cwds, got %d", len(cwds))
	}
	for _, cwd := range cwds {
		t.Run(cwd, func(t *testing.T) {
			if err := os.Chdir(cwd); err != nil {
				t.Fatal(err)
			}
			got, err := Resolve(cfg, "home")
			if err != nil {
				t.Fatalf("Resolve(home) from cwd %s: %v", cwd, err)
			}
			if got != want {
				t.Errorf("Resolve(home) from cwd %s = %q, want %q (identical regardless of cwd)", cwd, got, want)
			}
		})
	}
}

// ── unknown exact name: typed error, known list, no fuzzy fallback ───────

func TestResolveUnknownNameIsTypedWithKnownList(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)
	if _, err := Register(cfg, "work", "/abs/work"); err != nil {
		t.Fatal(err)
	}
	if _, err := Register(cfg, "home", "/abs/home"); err != nil {
		t.Fatal(err)
	}

	_, err := Resolve(cfg, "hoem")
	if err == nil {
		t.Fatal("Resolve(hoem) must fail: no such name is registered")
	}
	var unknown *config.UnknownEnvironmentError
	if !errors.As(err, &unknown) {
		t.Fatalf("Resolve error = %#v, want *config.UnknownEnvironmentError", err)
	}
	if unknown.Name != "hoem" {
		t.Errorf("Name = %q, want %q", unknown.Name, "hoem")
	}
	if want := []string{"home", "work"}; !slices.Equal(unknown.Known, want) {
		t.Errorf("Known = %v, want %v", unknown.Known, want)
	}
}

// TestResolveNeverFuzzyMatches: a name that is a near-miss of a registered
// one (a prefix, a case fold) must still be reported unknown, never silently
// resolved to the closest registration.
func TestResolveNeverFuzzyMatches(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)
	if _, err := Register(cfg, "home", "/abs/home"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"hom", "HOME", "home-2"} {
		if _, err := Resolve(cfg, name); err == nil {
			t.Errorf("Resolve(%q) must fail; only the exact name %q is registered", name, "home")
		}
	}
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

func containsAll(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// ── AC-12: a symlinked root or referenced executable is refused ─────────

// TestRefuseSymlinkedRootAndReference is the red_first_tests case verbatim:
// "symlinked root and symlinked referenced executable: two refusals, exit
// 2." One test, two independently-checked surfaces.
func TestRefuseSymlinkedRootAndReference(t *testing.T) {
	dir := t.TempDir()
	realRoot := filepath.Join(dir, "real-root")
	if err := os.MkdirAll(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	linkedRoot := filepath.Join(dir, "linked-root")
	if err := os.Symlink(realRoot, linkedRoot); err != nil {
		t.Fatal(err)
	}

	realExec := filepath.Join(dir, "warehouse-proxy")
	if err := writeFile(t, realExec, "#!/bin/sh\n"); err != nil {
		t.Fatal(err)
	}
	linkedExec := filepath.Join(dir, "warehouse-proxy-link")
	if err := os.Symlink(realExec, linkedExec); err != nil {
		t.Fatal(err)
	}

	// Refusal 1: the symlinked root.
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

	// Refusal 2: the symlinked referenced executable.
	err = RefuseSymlinkedReference("local MCP command", linkedExec)
	if err == nil {
		t.Fatal("RefuseSymlinkedReference must refuse a symlinked referenced executable")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(reference refusal) = %d, want 2", got)
	}
	var refSym *SymlinkError
	if !errors.As(err, &refSym) || refSym.Path != linkedExec {
		t.Fatalf("reference refusal = %#v, want *SymlinkError{Path: %q}", err, linkedExec)
	}
	if err := RefuseSymlinkedReference("local MCP command", realExec); err != nil {
		t.Errorf("RefuseSymlinkedReference on a real file = %v, want nil", err)
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

// ── registered root canonicalized/validated before ANY Join/stat/read ────

// TestResolveEnvironmentFailsClosedOnHandEditedRelativeRoot proves
// ResolveEnvironment validates the raw value Resolve returns BEFORE doing
// anything else with it: a *config.Config built directly (bypassing
// Register/config.AddEnvironment entirely, exactly what a hand-edited
// config.toml would look like in memory once TOML-decoded) with a relative
// registered root must refuse, never silently resolve that value against
// the calling process's current directory. Standing in a cwd that genuinely
// has a matching subdirectory proves this the hard way: if
// ResolveEnvironment ever fell through to filepath.Abs/os.Stat on the raw
// value, this exact setup would succeed instead of refusing.
func TestResolveEnvironmentFailsClosedOnHandEditedRelativeRoot(t *testing.T) {
	cfg := &config.Config{Environments: map[string]string{"home": "relative-env"}}

	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, "relative-env"), 0o755); err != nil {
		t.Fatal(err)
	}
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}

	_, err = ResolveEnvironment(cfg, "home", nil)
	if err == nil {
		t.Fatal("ResolveEnvironment must refuse a noncanonical (relative) hand-edited root, not resolve it against cwd")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(err) = %d, want 2", got)
	}
	var nonCanon *NoncanonicalRootError
	if !errors.As(err, &nonCanon) {
		t.Fatalf("error = %#v, want *NoncanonicalRootError", err)
	}
	if nonCanon.Name != "home" || nonCanon.Root != "relative-env" {
		t.Errorf("NoncanonicalRootError = %+v, want {Name: home, Root: relative-env}", nonCanon)
	}
}

// TestResolveEnvironmentFailsClosedOnHandEditedTildeRoot: same refusal for a
// leading-`~` value — the other shape config.AddEnvironment always expands
// before persisting, so it too can only reach cfg.Environments by hand or by
// a caller building *config.Config directly.
func TestResolveEnvironmentFailsClosedOnHandEditedTildeRoot(t *testing.T) {
	cfg := &config.Config{Environments: map[string]string{"home": "~/envs/home"}}

	_, err := ResolveEnvironment(cfg, "home", nil)
	if err == nil {
		t.Fatal("ResolveEnvironment must refuse a noncanonical (tilde) hand-edited root")
	}
	var nonCanon *NoncanonicalRootError
	if !errors.As(err, &nonCanon) {
		t.Fatalf("error = %#v, want *NoncanonicalRootError", err)
	}
}

// TestResolveEnvironmentAcceptsAlreadyCanonicalRoot is the negative control:
// a root that IS already canonical (exactly what Register/AddEnvironment
// always produce) passes the new check untouched.
func TestResolveEnvironmentAcceptsAlreadyCanonicalRoot(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)
	root := t.TempDir()
	canon, err := Register(cfg, "home", root)
	if err != nil {
		t.Fatal(err)
	}

	got, err := ResolveEnvironment(cfg, "home", nil)
	if err != nil {
		t.Fatalf("ResolveEnvironment on an already-canonical root = %v, want nil", err)
	}
	if got != canon {
		t.Errorf("ResolveEnvironment = %q, want %q", got, canon)
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
	tempConfig(t)
	cfg := loadConfig(t)
	oldPath := t.TempDir()
	newPath := t.TempDir()

	oldRoot, err := Register(cfg, "home", oldPath)
	if err != nil {
		t.Fatal(err)
	}

	store := &hosttrust.AcceptanceStore{}
	store.Put(Subject(oldRoot), hosttrust.Record{Fingerprint: "accepted-old-fp"})

	if !IsAccepted(store, oldRoot) {
		t.Fatal("setup: old root must read accepted before the repoint")
	}

	// Repoint "home" to a different canonical root.
	newRoot, err := Register(cfg, "home", newPath)
	if err != nil {
		t.Fatal(err)
	}
	if newRoot == oldRoot {
		t.Fatalf("test setup error: repoint produced the same root %q", newRoot)
	}
	got, _ := Resolve(cfg, "home")
	if got != newRoot {
		t.Fatalf("Resolve(home) after repoint = %q, want the new root %q", got, newRoot)
	}

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
