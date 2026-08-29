package env

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/hosttrust"
)

// tempDataDir isolates $XDG_DATA_HOME at a fresh temp path, so
// config.EnvsDir() (a zero-path add's scaffold target) never lands under a
// real user's ~/.local/share/pix.
func tempDataDir(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
}

// tier0Doc/tier1 fixtures reuse this file's own minimal Tier0 content
// (byte-identical to scaffoldSbxenv/env_cmd_test.go's registerTier0Env) and
// the shared hostexec Tier1 fixture (bom_test.go's copyFixture target).
func writeTier0Fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := writeFile(t, filepath.Join(root, sbxenvFilename), scaffoldSbxenv); err != nil {
		t.Fatal(err)
	}
	return root
}

func writeTier1Fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	copyFixture(t, "testdata/hostexec-fixture", root)
	return root
}

func fixedGetwd(dir string) func() (string, error) {
	return func() (string, error) { return dir, nil }
}

// ── register: Tier0 succeeds regardless of TTY/nonTTY/--yes ─────────────

func TestAdd_RegisterTier0_SucceedsInEveryReviewMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts AddOptions
	}{
		{"tty-no-answer-needed", AddOptions{TTY: true}},
		{"nontty-no-yes", AddOptions{TTY: false}},
		{"yes", AddOptions{Yes: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tempConfigAndState(t)
			cfg := loadConfig(t)
			root := writeTier0Fixture(t)

			var out bytes.Buffer
			opts := tc.opts
			opts.Out = &out
			opts.LookPath = noBareLookPath
			res, err := Add(cfg, "home", root, opts)
			if err != nil {
				t.Fatalf("Add: %v (stdout: %s)", err, out.String())
			}
			if res.Root != filepath.Clean(root) {
				t.Errorf("res.Root = %q, want %q", res.Root, filepath.Clean(root))
			}
			if cfg.Environments["home"] == "" {
				t.Fatalf("cfg.Environments[home] not set after Add: %v", cfg.Environments)
			}
			if res.Scaffolded {
				t.Error("registering an explicit path must never report Scaffolded")
			}
		})
	}
}

// ── register: Tier1 gates exactly like `pix env review` ─────────────────

func TestAdd_RegisterTier1_NonTTYWithoutYesFailsClosedTransactionally(t *testing.T) {
	tempConfigAndState(t)
	cfg := loadConfig(t)
	root := writeTier1Fixture(t)

	var out bytes.Buffer
	_, err := Add(cfg, "work", root, AddOptions{TTY: false, Out: &out, LookPath: noBareLookPath})
	if err == nil {
		t.Fatal("Add of a Tier1 environment, non-TTY, no --yes, must fail closed")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(err) = %d, want 2", got)
	}
	if len(cfg.Environments) != 0 {
		t.Errorf("cfg.Environments = %v, want untouched (empty) after a refused review", cfg.Environments)
	}
	// config.toml on disk must be untouched too — Save() was never called.
	if _, statErr := os.Stat(config.Path()); statErr == nil {
		t.Errorf("%s must not exist: a refused review must never reach cfg.Save()", config.Path())
	}
	// The environment trust store must hold no record for this root either.
	ts, err := loadEnvironmentTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if IsAccepted(&ts.AcceptanceStore, root) {
		t.Error("a refused review must never record acceptance")
	}
}

func TestAdd_RegisterTier1_YesAccepts(t *testing.T) {
	tempConfigAndState(t)
	cfg := loadConfig(t)
	root := writeTier1Fixture(t)

	var out bytes.Buffer
	res, err := Add(cfg, "work", root, AddOptions{Yes: true, Out: &out, LookPath: noBareLookPath})
	if err != nil {
		t.Fatalf("Add: %v (stdout: %s)", err, out.String())
	}
	if !res.Review.Accepted || res.Review.Fingerprint == "" {
		t.Errorf("Review result = %+v, want accepted with a fingerprint", res.Review)
	}
	if cfg.Environments["work"] != res.Root {
		t.Errorf("cfg.Environments[work] = %q, want %q", cfg.Environments["work"], res.Root)
	}
	if !strings.Contains(out.String(), "Accept this host-execution footprint?") {
		t.Errorf("stdout = %q, want the review bill", out.String())
	}
	if !strings.Contains(out.String(), "accepted via --yes") {
		t.Errorf("stdout = %q, want the --yes acceptance line", out.String())
	}
}

func TestAdd_RegisterTier1_TTYAnswerYesAccepts(t *testing.T) {
	tempConfigAndState(t)
	cfg := loadConfig(t)
	root := writeTier1Fixture(t)

	var out bytes.Buffer
	res, err := Add(cfg, "work", root, AddOptions{
		TTY: true, In: strings.NewReader("y\n"), Out: &out, LookPath: noBareLookPath,
	})
	if err != nil {
		t.Fatalf("Add: %v (stdout: %s)", err, out.String())
	}
	if !res.Review.Accepted {
		t.Errorf("Review result = %+v, want accepted", res.Review)
	}
}

// ── duplicate add is idempotent ──────────────────────────────────────────

func TestAdd_RegisterDuplicateIsIdempotent(t *testing.T) {
	tempConfigAndState(t)
	cfg := loadConfig(t)
	root := writeTier0Fixture(t)

	if _, err := Add(cfg, "home", root, AddOptions{LookPath: noBareLookPath}); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	first := cfg.Environments["home"]

	if _, err := Add(cfg, "home", root, AddOptions{LookPath: noBareLookPath}); err != nil {
		t.Fatalf("second (duplicate) Add: %v", err)
	}
	if len(cfg.Environments) != 1 || cfg.Environments["home"] != first {
		t.Errorf("cfg.Environments = %v, want the single unchanged entry %q", cfg.Environments, first)
	}
}

// ── repoint: config updates, but acceptance never transfers ─────────────

func TestAdd_RegisterRepointRequiresNewRootAcceptance(t *testing.T) {
	tempConfigAndState(t)
	cfg := loadConfig(t)
	rootA := writeTier0Fixture(t)
	rootB := writeTier1Fixture(t)

	if _, err := Add(cfg, "home", rootA, AddOptions{LookPath: noBareLookPath}); err != nil {
		t.Fatalf("Add rootA: %v", err)
	}
	if cfg.Environments["home"] != hosttrust.CanonicalRoot(rootA) {
		t.Fatalf("cfg.Environments[home] = %q, want rootA", cfg.Environments["home"])
	}

	// Repointing to a Tier1 root, non-TTY without --yes, must fail closed
	// AND must leave the EXISTING registration (rootA) untouched — a
	// refused repoint is not a partial repoint.
	if _, err := Add(cfg, "home", rootB, AddOptions{TTY: false, LookPath: noBareLookPath}); err == nil {
		t.Fatal("repointing to an unreviewed Tier1 root, non-TTY, no --yes, must fail closed")
	}
	if cfg.Environments["home"] != hosttrust.CanonicalRoot(rootA) {
		t.Errorf("cfg.Environments[home] = %q, want it still rootA after a refused repoint", cfg.Environments["home"])
	}

	// Accepting explicitly repoints it, and requires ITS OWN acceptance —
	// rootA's Subject was never accepted (Tier0 needs none) and rootB gets
	// a fresh gate regardless.
	res, err := Add(cfg, "home", rootB, AddOptions{Yes: true, LookPath: noBareLookPath})
	if err != nil {
		t.Fatalf("Add rootB --yes: %v", err)
	}
	if cfg.Environments["home"] != res.Root {
		t.Errorf("cfg.Environments[home] = %q, want the new root %q", cfg.Environments["home"], res.Root)
	}
	ts, err := loadEnvironmentTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if IsAccepted(&ts.AcceptanceStore, rootA) {
		t.Error("rootA must never be reported accepted; it was never reviewed")
	}
	if !IsAccepted(&ts.AcceptanceStore, rootB) {
		t.Error("rootB must be accepted after its own explicit --yes")
	}
}

// TestAdd_RepointNamesOldAndNewAbsoluteRootsOnSuccess is finding C11: a
// successful repoint's printed line, and its AddResult, must name BOTH the
// old and the new absolute root — not merely the plain "registered at"
// form a first-time add uses.
func TestAdd_RepointNamesOldAndNewAbsoluteRootsOnSuccess(t *testing.T) {
	tempConfigAndState(t)
	cfg := loadConfig(t)
	rootA := hosttrust.CanonicalRoot(writeTier0Fixture(t))
	rootB := hosttrust.CanonicalRoot(writeTier0Fixture(t))

	var out1 bytes.Buffer
	res1, err := Add(cfg, "home", rootA, AddOptions{Out: &out1, LookPath: noBareLookPath})
	if err != nil {
		t.Fatalf("Add rootA: %v (stdout: %s)", err, out1.String())
	}
	if res1.OldRoot != "" {
		t.Errorf("first-time Add: AddResult.OldRoot = %q, want empty", res1.OldRoot)
	}
	if strings.Contains(out1.String(), "repointed") {
		t.Errorf("first-time Add stdout = %q, must not mention a repoint", out1.String())
	}

	// An idempotent re-add of the SAME root is not a repoint either: nothing
	// actually moved.
	var outSame bytes.Buffer
	resSame, err := Add(cfg, "home", rootA, AddOptions{Out: &outSame, LookPath: noBareLookPath})
	if err != nil {
		t.Fatalf("Add rootA again: %v (stdout: %s)", err, outSame.String())
	}
	if resSame.OldRoot != "" {
		t.Errorf("idempotent re-add: AddResult.OldRoot = %q, want empty (root unchanged)", resSame.OldRoot)
	}
	if strings.Contains(outSame.String(), "repointed") {
		t.Errorf("idempotent re-add stdout = %q, must not mention a repoint", outSame.String())
	}

	var out2 bytes.Buffer
	res2, err := Add(cfg, "home", rootB, AddOptions{Out: &out2, LookPath: noBareLookPath})
	if err != nil {
		t.Fatalf("Add rootB (repoint): %v (stdout: %s)", err, out2.String())
	}
	if res2.OldRoot != rootA {
		t.Errorf("repoint: AddResult.OldRoot = %q, want the old root %q", res2.OldRoot, rootA)
	}
	if res2.Root != rootB {
		t.Errorf("repoint: AddResult.Root = %q, want the new root %q", res2.Root, rootB)
	}
	stdout := out2.String()
	if !strings.Contains(stdout, rootA) {
		t.Errorf("repoint stdout = %q, want it to name the old root %q", stdout, rootA)
	}
	if !strings.Contains(stdout, rootB) {
		t.Errorf("repoint stdout = %q, want it to name the new root %q", stdout, rootB)
	}
	if !strings.Contains(stdout, "pix env use home") {
		t.Errorf("repoint stdout = %q, want it to still name `pix env use home`", stdout)
	}
}

// ── zero-path: cwd ambiguity (D10) ───────────────────────────────────────

func TestAdd_ZeroPath_CwdHasSbxenvRefusesNamingBothIntents(t *testing.T) {
	tempConfigAndState(t)
	tempDataDir(t)
	cfg := loadConfig(t)
	cwd := t.TempDir()
	if err := writeFile(t, filepath.Join(cwd, sbxenvFilename), "schemaVersion: \"1\"\n"); err != nil {
		t.Fatal(err)
	}

	_, err := Add(cfg, "home", "", AddOptions{Getwd: fixedGetwd(cwd)})
	if err == nil {
		t.Fatal("zero-path add with an existing cwd .sbxenv.yaml must be refused")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(err) = %d, want 2", got)
	}
	msg := err.Error()
	if !strings.Contains(msg, "pix env add home "+cwd) {
		t.Errorf("error = %q, want it to name the register form `pix env add home %s`", msg, cwd)
	}
	if !strings.Contains(msg, "pix env add home") {
		t.Errorf("error = %q, want it to name the bare scaffold form `pix env add home`", msg)
	}
	if len(cfg.Environments) != 0 {
		t.Errorf("cfg.Environments = %v, want untouched", cfg.Environments)
	}
	if fileExists(filepath.Join(config.EnvsDir(), "home")) {
		t.Error("the ambiguous refusal must create nothing under config.EnvsDir()")
	}
}

func TestAdd_ZeroPath_CwdGetwdErrorDoesNotProceed(t *testing.T) {
	tempConfigAndState(t)
	tempDataDir(t)
	cfg := loadConfig(t)
	_, err := Add(cfg, "home", "", AddOptions{Getwd: func() (string, error) {
		return "", os.ErrPermission
	}})
	if err == nil {
		t.Fatal("a Getwd failure must abort the add, not silently proceed")
	}
	if fileExists(filepath.Join(config.EnvsDir(), "home")) {
		t.Error("nothing should be created when the cwd cannot even be resolved")
	}
}

// ── zero-path: scaffold collision, every shape, never overwrites ────────

func TestAdd_ZeroPath_ScaffoldCollisionNeverOverwrites(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, target string)
	}{
		{"existing-dir", func(t *testing.T, target string) {
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"existing-file", func(t *testing.T, target string) {
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, []byte("not an env"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"existing-symlink", func(t *testing.T, target string) {
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatal(err)
			}
			elsewhere := filepath.Join(filepath.Dir(target), "elsewhere")
			if err := os.MkdirAll(elsewhere, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, target); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tempConfigAndState(t)
			tempDataDir(t)
			cfg := loadConfig(t)
			target := filepath.Join(config.EnvsDir(), "home")
			tc.setup(t, target)

			before, _ := os.Lstat(target)

			cwd := t.TempDir() // no .sbxenv.yaml here: not the D10 case
			_, err := Add(cfg, "home", "", AddOptions{Getwd: fixedGetwd(cwd)})
			if err == nil {
				t.Fatal("scaffolding onto an existing entry must be refused")
			}
			if got := cli.ExitCode(err); got != 2 {
				t.Errorf("cli.ExitCode(err) = %d, want 2", got)
			}
			after, statErr := os.Lstat(target)
			if statErr != nil {
				t.Fatalf("target vanished: %v", statErr)
			}
			if before.Mode() != after.Mode() {
				t.Errorf("target mode changed from %v to %v; must never be touched", before.Mode(), after.Mode())
			}
			if len(cfg.Environments) != 0 {
				t.Errorf("cfg.Environments = %v, want untouched", cfg.Environments)
			}
			if fileExists(filepath.Join(target, sbxenvFilename)) && tc.name != "existing-dir" {
				t.Error("must never write into a colliding non-directory target")
			}
		})
	}
}

// ── zero-path: successful scaffold — golden bytes, first line, perms ────

func TestAdd_ZeroPath_ScaffoldSucceeds(t *testing.T) {
	tempConfigAndState(t)
	tempDataDir(t)
	cfg := loadConfig(t)
	cwd := t.TempDir()

	var out bytes.Buffer
	res, err := Add(cfg, "home", "", AddOptions{Getwd: fixedGetwd(cwd), Out: &out, LookPath: noBareLookPath})
	if err != nil {
		t.Fatalf("Add (scaffold): %v (stdout: %s)", err, out.String())
	}
	if !res.Scaffolded {
		t.Error("zero-path Add must report Scaffolded")
	}
	wantRoot := filepath.Join(config.EnvsDir(), "home")
	if res.Root != wantRoot {
		t.Errorf("res.Root = %q, want %q", res.Root, wantRoot)
	}

	// First output line is the created absolute root, nothing else on it.
	firstLine, _, _ := strings.Cut(out.String(), "\n")
	if firstLine != wantRoot {
		t.Errorf("first output line = %q, want the absolute created root %q", firstLine, wantRoot)
	}
	if !filepath.IsAbs(firstLine) {
		t.Errorf("first output line %q is not absolute", firstLine)
	}

	// Golden scaffold bytes: byte-exact, not merely "parses".
	got, err := os.ReadFile(filepath.Join(wantRoot, sbxenvFilename))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != scaffoldSbxenv {
		t.Errorf(".sbxenv.yaml = %q, want byte-exact %q", string(got), scaffoldSbxenv)
	}
	// No sidecar: nothing sbx cannot already express for pure defaults.
	if fileExists(filepath.Join(wantRoot, "pix.toml")) {
		t.Error("a bare scaffold must not write pix.toml")
	}

	// Restrictive, atomic-write permissions.
	dirInfo, err := os.Stat(wantRoot)
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("scaffold dir mode = %o, want 0700", perm)
	}
	fileInfo, err := os.Stat(filepath.Join(wantRoot, sbxenvFilename))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Errorf("scaffold file mode = %o, want 0600", perm)
	}

	// Registered and reviewed (Tier0: accepted with no prompt at all).
	if cfg.Environments["home"] != wantRoot {
		t.Errorf("cfg.Environments[home] = %q, want %q", cfg.Environments["home"], wantRoot)
	}
	if !res.Review.Accepted {
		t.Errorf("Review result = %+v, want accepted (Tier0, empty bill)", res.Review)
	}
	if strings.Contains(out.String(), "Accept this host-execution footprint?") {
		t.Errorf("stdout = %q, a Tier0 scaffold must never prompt", out.String())
	}

	// Success names the literal next command.
	if !strings.Contains(out.String(), "pix env use home") {
		t.Errorf("stdout = %q, want it to name `pix env use home` literally", out.String())
	}
	for _, banned := range []string{"configured", "enabled", "ready", "verified"} {
		if strings.Contains(strings.ToLower(out.String()), banned) {
			t.Errorf("stdout = %q, must not print earned-only success word %q", out.String(), banned)
		}
	}
}

// ── zero-path: no partial dirs survive a post-creation failure ──────────

func TestAdd_ZeroPath_NoPartialDirOnSaveFailure(t *testing.T) {
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked")
	t.Setenv("PIX_CONFIG", filepath.Join(blocked, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	// Load first, while `blocked` does not exist yet (a plain absent-file
	// load, same as every other test's tempConfig). Only AFTER loading do we
	// occupy `blocked` with a REGULAR FILE, so cfg.Save()'s own
	// os.MkdirAll(blocked) fails deterministically once Add reaches it —
	// AFTER the Tier0 scaffold + prompt-free review already succeeded.
	cfg := loadConfig(t)
	if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()

	target := filepath.Join(config.EnvsDir(), "home")
	_, err := Add(cfg, "home", "", AddOptions{Getwd: fixedGetwd(cwd), LookPath: noBareLookPath})
	if err == nil {
		t.Fatal("Add must fail when cfg.Save() cannot write config.toml")
	}
	if fileExists(target) {
		t.Errorf("%s must not survive a failed add: no partial scaffold directories", target)
	}
}

// ── config save is exact: nothing beyond the new registration ───────────

func TestAdd_ConfigExactSave(t *testing.T) {
	tempConfigAndState(t)
	cfg := loadConfig(t)
	root := writeTier0Fixture(t)

	res, err := Add(cfg, "home", root, AddOptions{LookPath: noBareLookPath})
	if err != nil {
		t.Fatal(err)
	}

	reloaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Environments) != 1 || reloaded.Environments["home"] != res.Root {
		t.Errorf("reloaded Environments = %v, want exactly {home: %q}", reloaded.Environments, res.Root)
	}
	if reloaded.Environment != "" {
		t.Errorf("reloaded Environment (machine default) = %q, want empty: Add never selects a default", reloaded.Environment)
	}
}
