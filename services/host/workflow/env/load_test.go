package env

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"pix/host/cli"
	"pix/host/envinfo"
	"pix/host/hosttrust"
)

// writeEnvFile writes content at dir/name, creating parent directories as
// needed, and fails the test on any error — the same shape writeFile
// (resolve_test.go) already uses for a symlink target, generalized to a
// literal string body.
func writeEnvFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := writeFile(t, path, content); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimalSbxenv = `schemaVersion: "1"
agent: pix
`

func kitSbxenv(kitRelPath string) string {
	return "schemaVersion: \"1\"\nagent: pix\n\nkits:\n  - " + kitRelPath + "\n"
}

func minimalSidecarWithSkills(skillsAbsPath string) string {
	return "schema = 1\n\n[pi]\nskills = [\"" + skillsAbsPath + "\"]\n"
}

// ── both files parsed ────────────────────────────────────────────────────

// TestLoad_BothFilesParsed proves Load reads AND typed-parses both the
// required native document and the optional sidecar in one composed call,
// returning both on the *Environment it hands back.
func TestLoad_BothFilesParsed(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)

	workspace := t.TempDir()
	skillsDir := filepath.Join(workspace, "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	writeEnvFile(t, root, ".sbxenv.yaml", minimalSbxenv)
	writeEnvFile(t, root, "pix.toml", minimalSidecarWithSkills(skillsDir))

	if _, err := Register(cfg, "home", root); err != nil {
		t.Fatal(err)
	}

	got, err := Load(cfg, &hosttrust.AcceptanceStore{}, "home", EffectiveMounts{{Path: workspace}}, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Document == nil {
		t.Fatal("Environment.Document is nil, want the parsed native document")
	}
	if got.Document.SchemaVersion != "1" || got.Document.Agent != "pix" {
		t.Errorf("Document = %+v, want SchemaVersion 1, Agent pix", got.Document)
	}
	if got.Sidecar == nil {
		t.Fatal("Environment.Sidecar is nil, want the parsed sidecar")
	}
	if len(got.Sidecar.Pi.Skills) != 1 || got.Sidecar.Pi.Skills[0] != skillsDir {
		t.Errorf("Sidecar.Pi.Skills = %v, want [%q]", got.Sidecar.Pi.Skills, skillsDir)
	}
	if got.Tree == nil {
		t.Fatal("Environment.Tree is nil, want the pre-composition tree")
	}
	if got.Root != hosttrust.CanonicalRoot(root) {
		t.Errorf("Root = %q, want %q", got.Root, hosttrust.CanonicalRoot(root))
	}
	if got.SidecarPath == "" {
		t.Error("SidecarPath must be set when a sidecar was found")
	}
	if got.Subject != Subject(root) {
		t.Errorf("Subject = %+v, want %+v", got.Subject, Subject(root))
	}
}

// ── invalid optional sidecar refuses ─────────────────────────────────────

// TestLoad_InvalidOptionalSidecarRefuses: a pix.toml that exists but fails
// its own strict validation (an unknown key) must refuse the whole Load, as
// a usage error (exit 2) — never silently ignored because the sidecar is
// "merely optional".
func TestLoad_InvalidOptionalSidecarRefuses(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)

	root := t.TempDir()
	writeEnvFile(t, root, ".sbxenv.yaml", minimalSbxenv)
	writeEnvFile(t, root, "pix.toml", "schema = 1\nbogusTopLevelKey = true\n")

	if _, err := Register(cfg, "home", root); err != nil {
		t.Fatal(err)
	}

	_, err := Load(cfg, nil, "home", nil, nil)
	if err == nil {
		t.Fatal("Load must refuse an invalid pix.toml, got nil error")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(err) = %d, want 2", got)
	}
	var sidecarErr *envinfo.Error
	if !errors.As(err, &sidecarErr) {
		t.Fatalf("error = %#v, want *envinfo.Error", err)
	}
}

// ── missing optional sidecar accepted ────────────────────────────────────

// TestLoad_MissingOptionalSidecarAccepted: no pix.toml at all is not an
// error — Load succeeds with a nil Sidecar and an empty SidecarPath.
func TestLoad_MissingOptionalSidecarAccepted(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)

	root := t.TempDir()
	writeEnvFile(t, root, ".sbxenv.yaml", minimalSbxenv)

	if _, err := Register(cfg, "home", root); err != nil {
		t.Fatal(err)
	}

	got, err := Load(cfg, nil, "home", nil, nil)
	if err != nil {
		t.Fatalf("Load with no pix.toml must succeed, got: %v", err)
	}
	if got.Sidecar != nil {
		t.Errorf("Sidecar = %+v, want nil (no pix.toml present)", got.Sidecar)
	}
	if got.SidecarPath != "" {
		t.Errorf("SidecarPath = %q, want empty", got.SidecarPath)
	}
	if got.Document == nil {
		t.Fatal("Document must still be parsed when only the required file is present")
	}
}

// TestLoad_MissingRequiredNativeFileRefuses: an environment root with no
// `.sbxenv.yaml` at all is refused — it is required, unlike pix.toml.
func TestLoad_MissingRequiredNativeFileRefuses(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)
	root := t.TempDir()
	if _, err := Register(cfg, "home", root); err != nil {
		t.Fatal(err)
	}

	_, err := Load(cfg, nil, "home", nil, nil)
	if err == nil {
		t.Fatal("Load must refuse a root with no .sbxenv.yaml")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(err) = %d, want 2", got)
	}
	var missing *MissingRequiredFileError
	if !errors.As(err, &missing) {
		t.Fatalf("error = %#v, want *MissingRequiredFileError", err)
	}
	if missing.File != ".sbxenv.yaml" {
		t.Errorf("File = %q, want .sbxenv.yaml", missing.File)
	}
}

// ── unknown native field refuses ─────────────────────────────────────────

// TestLoad_UnknownNativeFieldRefuses: envinfo.Parse's own strict decode
// (KnownFields(true)) must surface through Load as a usage refusal, exit 2
// — a typo in the required document is not silently ignored just because
// Load composes several steps around the parse.
func TestLoad_UnknownNativeFieldRefuses(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)
	root := t.TempDir()
	writeEnvFile(t, root, ".sbxenv.yaml", "schemaVersion: \"1\"\nbogusField: true\n")
	if _, err := Register(cfg, "home", root); err != nil {
		t.Fatal(err)
	}

	_, err := Load(cfg, nil, "home", nil, nil)
	if err == nil {
		t.Fatal("Load must refuse an unknown top-level .sbxenv.yaml field")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(err) = %d, want 2", got)
	}
}

// ── containment is actually called from composition ──────────────────────

// TestLoad_ContainmentCalledFromComposition proves RefuseContainment is
// wired into Load's own composition, not merely available as a standalone
// primitive a caller might forget to invoke: registering a root that
// resolves inside a workspace Load is told about must refuse through Load
// itself with a *ContainmentError naming both paths.
func TestLoad_ContainmentCalledFromComposition(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)

	workspace := t.TempDir()
	root := filepath.Join(workspace, "env")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeEnvFile(t, root, ".sbxenv.yaml", minimalSbxenv)

	if _, err := Register(cfg, "home", root); err != nil {
		t.Fatal(err)
	}

	_, err := Load(cfg, nil, "home", EffectiveMounts{{Path: workspace}}, nil)
	if err == nil {
		t.Fatal("Load must refuse a root that resolves inside a declared workspace")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(err) = %d, want 2", got)
	}
	var containment *ContainmentError
	if !errors.As(err, &containment) {
		t.Fatalf("error = %#v, want *ContainmentError (proves RefuseContainment ran inside Load)", err)
	}
	if !containsAll(err.Error(), hosttrust.CanonicalRoot(root), hosttrust.CanonicalRoot(workspace)) {
		t.Errorf("refusal text %q must name both absolute paths", err.Error())
	}
}

// ── root/reference symlinks reached end-to-end ────────────────────────────

// TestLoad_RootSymlinkReachedEndToEnd proves a symlinked registered root is
// refused when reached the ONLY way a real caller reaches it: through
// Load's full composition, not by calling RefuseSymlinkedRoot directly.
func TestLoad_RootSymlinkReachedEndToEnd(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)

	dir := t.TempDir()
	realRoot := filepath.Join(dir, "real-root")
	if err := os.MkdirAll(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeEnvFile(t, realRoot, ".sbxenv.yaml", minimalSbxenv)
	linkedRoot := filepath.Join(dir, "linked-root")
	if err := os.Symlink(realRoot, linkedRoot); err != nil {
		t.Fatal(err)
	}

	if _, err := Register(cfg, "linked", linkedRoot); err != nil {
		t.Fatal(err)
	}

	_, err := Load(cfg, nil, "linked", nil, nil)
	if err == nil {
		t.Fatal("Load must refuse a symlinked registered root")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(err) = %d, want 2", got)
	}
	var symErr *SymlinkError
	if !errors.As(err, &symErr) || symErr.Kind != "environment root" {
		t.Fatalf("error = %#v, want *SymlinkError{Kind: \"environment root\"}", err)
	}
}

// TestLoad_ReferencedKitSymlinkReachedEndToEnd proves a symlinked LOCAL kit
// reference — resolved by envinfo.Parse against the source file's own
// directory, never checked by envinfo itself — is refused only when Load's
// own composition runs the check end-to-end.
func TestLoad_ReferencedKitSymlinkReachedEndToEnd(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)

	root := t.TempDir()
	realKit := filepath.Join(root, "real-kit")
	if err := os.MkdirAll(realKit, 0o755); err != nil {
		t.Fatal(err)
	}
	linkedKit := filepath.Join(root, "kit")
	if err := os.Symlink(realKit, linkedKit); err != nil {
		t.Fatal(err)
	}
	writeEnvFile(t, root, ".sbxenv.yaml", kitSbxenv("./kit"))

	if _, err := Register(cfg, "home", root); err != nil {
		t.Fatal(err)
	}

	_, err := Load(cfg, nil, "home", nil, nil)
	if err == nil {
		t.Fatal("Load must refuse a symlinked local kit reference")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(err) = %d, want 2", got)
	}
	var symErr *SymlinkError
	if !errors.As(err, &symErr) {
		t.Fatalf("error = %#v, want *SymlinkError", err)
	}
	if symErr.Path != linkedKit {
		t.Errorf("SymlinkError.Path = %q, want %q", symErr.Path, linkedKit)
	}
}

// TestLoad_ReferencedHostServiceCommandSymlinkReachedEndToEnd proves the
// SAME end-to-end refusal for a pix.toml [[host.services]] command: the
// concrete "referenced executable" example docs/design/environments.md
// §5.2 itself uses (warehouse-proxy).
func TestLoad_ReferencedHostServiceCommandSymlinkReachedEndToEnd(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)

	root := t.TempDir()
	realExec := filepath.Join(root, "warehouse-proxy")
	if err := os.WriteFile(realExec, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	linkedExec := filepath.Join(root, "warehouse-proxy-link")
	if err := os.Symlink(realExec, linkedExec); err != nil {
		t.Fatal(err)
	}
	writeEnvFile(t, root, ".sbxenv.yaml", minimalSbxenv)
	writeEnvFile(t, root, "pix.toml", "schema = 1\n\n[[host.services]]\nname = \"warehouse-proxy\"\ncommand = \""+linkedExec+"\"\n")

	if _, err := Register(cfg, "home", root); err != nil {
		t.Fatal(err)
	}

	_, err := Load(cfg, nil, "home", nil, nil)
	if err == nil {
		t.Fatal("Load must refuse a symlinked host.services command")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(err) = %d, want 2", got)
	}
	var symErr *SymlinkError
	if !errors.As(err, &symErr) || symErr.Path != linkedExec {
		t.Fatalf("error = %#v, want *SymlinkError{Path: %q}", err, linkedExec)
	}
}

// ── bare local-command references resolve via PATH, never Lstat(cwd) ────

// TestLoad_BareMCPServerCommandSymlinkReachedEndToEnd proves a NATIVE
// `mcp.servers[].command` naming a BARE PATH command (no separator) is
// resolved through the injected lookPath — the exec.LookPath production
// seam — and the RESOLVED executable is what gets symlink-checked, never an
// os.Lstat of the bare name relative to the calling process's cwd. Standing
// in a cwd that happens to have an unrelated file with the SAME bare name
// proves the fix: the old Lstat-relative-to-cwd behavior would have checked
// that unrelated cwd file instead of the real one lookPath resolves to.
func TestLoad_BareMCPServerCommandSymlinkReachedEndToEnd(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)

	root := t.TempDir()
	realExec := filepath.Join(root, "warehouse-proxy-real")
	if err := writeFile(t, realExec, "#!/bin/sh\n"); err != nil {
		t.Fatal(err)
	}
	linkedExec := filepath.Join(root, "warehouse-proxy-link")
	if err := os.Symlink(realExec, linkedExec); err != nil {
		t.Fatal(err)
	}
	writeEnvFile(t, root, ".sbxenv.yaml", "schemaVersion: \"1\"\nmcp:\n  servers:\n    - name: warehouse\n      command: warehouse-proxy\n")

	if _, err := Register(cfg, "home", root); err != nil {
		t.Fatal(err)
	}

	// A cwd with a DIFFERENT, non-symlinked file that happens to share the
	// bare command's name — the exact trap an os.Lstat(cwd-relative) check
	// would fall into.
	cwd := t.TempDir()
	if err := writeFile(t, filepath.Join(cwd, "warehouse-proxy"), "#!/bin/sh\n"); err != nil {
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

	fakeLookPath := func(name string) (string, error) {
		if name == "warehouse-proxy" {
			return linkedExec, nil
		}
		return "", errNotFound
	}

	_, err = Load(cfg, nil, "home", nil, fakeLookPath)
	if err == nil {
		t.Fatal("Load must refuse a bare MCP command that resolves (via PATH) to a symlink")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(err) = %d, want 2", got)
	}
	var symErr *SymlinkError
	if !errors.As(err, &symErr) || symErr.Path != linkedExec {
		t.Fatalf("error = %#v, want *SymlinkError{Path: %q} (the PATH-resolved executable, not a cwd-relative Lstat)", err, linkedExec)
	}
}

// TestLoad_BareMCPServerCommandRegularPassesAndMissingSkips: the other two
// legs of the bare-command matrix reached through Load — a regular
// (non-symlink) PATH resolution passes clean, and a command lookPath cannot
// find at all is not itself a refusal (that is a `pix doctor`-shaped
// concern, not a symlink one).
func TestLoad_BareMCPServerCommandRegularPassesAndMissingSkips(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)

	root := t.TempDir()
	realExec := filepath.Join(root, "warehouse-proxy-real")
	if err := writeFile(t, realExec, "#!/bin/sh\n"); err != nil {
		t.Fatal(err)
	}
	writeEnvFile(t, root, ".sbxenv.yaml", "schemaVersion: \"1\"\nmcp:\n  servers:\n    - name: warehouse\n      command: warehouse-proxy\n")
	if _, err := Register(cfg, "home", root); err != nil {
		t.Fatal(err)
	}

	regular := func(string) (string, error) { return realExec, nil }
	if _, err := Load(cfg, nil, "home", nil, regular); err != nil {
		t.Fatalf("Load with a regular (non-symlink) PATH-resolved command = %v, want nil", err)
	}

	missing := func(string) (string, error) { return "", errNotFound }
	if _, err := Load(cfg, nil, "home", nil, missing); err != nil {
		t.Fatalf("Load with a bare command missing from PATH = %v, want nil (not a symlink refusal)", err)
	}
}

// ── acceptance state changes on repoint ──────────────────────────────────

// TestLoad_AcceptanceStateChangesOnRepoint is AC-16 proven through the
// composed Load entry point rather than the bare IsAccepted primitive: the
// *Environment.Accepted field must read true for the originally-accepted
// root and false the moment the same registered name is repointed to a new
// root Load has never seen accepted.
func TestLoad_AcceptanceStateChangesOnRepoint(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)

	oldRoot := t.TempDir()
	writeEnvFile(t, oldRoot, ".sbxenv.yaml", minimalSbxenv)
	newRoot := t.TempDir()
	writeEnvFile(t, newRoot, ".sbxenv.yaml", minimalSbxenv)

	canonOld, err := Register(cfg, "home", oldRoot)
	if err != nil {
		t.Fatal(err)
	}

	store := &hosttrust.AcceptanceStore{}
	store.Put(Subject(canonOld), hosttrust.Record{Fingerprint: "accepted-old-fp"})

	before, err := Load(cfg, store, "home", nil, nil)
	if err != nil {
		t.Fatalf("Load (before repoint): %v", err)
	}
	if !before.Accepted {
		t.Error("Environment.Accepted = false before repoint, want true (store already holds a record for this root)")
	}

	if _, err := Register(cfg, "home", newRoot); err != nil {
		t.Fatal(err)
	}

	after, err := Load(cfg, store, "home", nil, nil)
	if err != nil {
		t.Fatalf("Load (after repoint): %v", err)
	}
	if after.Accepted {
		t.Error("Environment.Accepted = true after repoint, want false (the new root has no acceptance record)")
	}
	if after.Root == before.Root {
		t.Fatalf("test setup error: repoint produced the same root %q", after.Root)
	}
}

// ── operational vs usage classification ──────────────────────────────────

// TestLoad_UnknownNameIsUsageError proves an unregistered name is reported
// as a usage error, the same classification every other Load refusal
// carries, so a single cli.ExitCode(err) call is enough for a future
// caller to decide exit 2 vs exit 1.
func TestLoad_UnknownNameIsUsageError(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)
	_, err := Load(cfg, nil, "nope", nil, nil)
	if err == nil {
		t.Fatal("Load must fail for an unregistered name")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(err) = %d, want 2", got)
	}
}
