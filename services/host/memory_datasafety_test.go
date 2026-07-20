package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBackupRefusesExistingOut is the B1 gate: an --out pointed at a file that
// already exists is refused, and that existing archive is left byte-for-byte
// intact (os.Link's no-clobber commit + the explicit pre-check must both hold).
func TestBackupRefusesExistingOut(t *testing.T) {
	st, dbPath := seedMemDB(t, 2)
	defer st.db.Close()

	outDir := t.TempDir()
	outPath := filepath.Join(outDir, "pi-stack-backup-20260715-120000.tar.gz")
	// A pre-existing archive (its bytes must survive a refused backup).
	sentinel := []byte("a precious previous backup that must not be destroyed")
	if err := os.WriteFile(outPath, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := memoryBackup(backupParams{
		DBPath: dbPath, OutPath: outPath, Keep: 7, Now: time.Now(),
	})
	if err == nil {
		t.Fatal("backup accepted an --out that already exists; want refusal")
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("existing archive missing after refused backup: %v", err)
	}
	if string(got) != string(sentinel) {
		t.Errorf("existing archive was overwritten by a refused backup: %q", string(got))
	}
}

// TestBackupDefaultNameHasRandomSuffix is the B1 gate for the collision-proof
// default name: with no --out, the derived path is pi-stack-backup-<ts>-<rand>.tar.gz
// so two backups in the same second cannot collide.
func TestBackupDefaultNameHasRandomSuffix(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	a := resolveBackupParams("", 7, now).OutPath
	b := resolveBackupParams("", 7, now).OutPath
	base := filepath.Base(a)
	if !backupNameRe.MatchString(base) {
		t.Errorf("default name %q does not match the backup name pattern", base)
	}
	prefix := "pi-stack-backup-20260715-120000-"
	if !strings.HasPrefix(base, prefix) || !strings.HasSuffix(base, ".tar.gz") {
		t.Errorf("default name %q lacks the <ts>-<rand> shape", base)
	}
	if a == b {
		t.Errorf("two default names in the same second collided: %q", a)
	}
}

// TestResolveBackupParamsUsesCanonicalPaths is the B2 gate: with PI_STACK_CONFIG
// set, both backup and restore derive config.toml + op-refs.env from the config
// DIR (XDG sibling), never a CWD-relative config/op-refs.env.
func TestResolveBackupParamsUsesCanonicalPaths(t *testing.T) {
	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "config.toml")
	t.Setenv("PI_STACK_CONFIG", cfgPath)
	t.Setenv("HOME", t.TempDir())

	bp := resolveBackupParams("", 7, time.Now())
	if bp.ConfigPath != cfgPath {
		t.Errorf("backup ConfigPath = %q, want %q", bp.ConfigPath, cfgPath)
	}
	wantOp := filepath.Join(cfgDir, "op-refs.env")
	if bp.OpRefsPath != wantOp {
		t.Errorf("backup OpRefsPath = %q, want %q (must be the config-dir sibling, not CWD)", bp.OpRefsPath, wantOp)
	}
	// The old CWD-relative default must be gone.
	if strings.HasPrefix(bp.OpRefsPath, "config"+string(os.PathSeparator)) {
		t.Errorf("backup OpRefsPath is CWD-relative: %q", bp.OpRefsPath)
	}

	rp := resolveRestoreParams("some.tar.gz", false, time.Now())
	if rp.ConfigPath != cfgPath {
		t.Errorf("restore ConfigPath = %q, want %q", rp.ConfigPath, cfgPath)
	}
	if rp.OpRefsPath != wantOp {
		t.Errorf("restore OpRefsPath = %q, want %q", rp.OpRefsPath, wantOp)
	}
}

// TestResolveBackupParamsDerivesManifestNotes strengthens the (previously
// tautological) manifest test: resolveBackupParams must DERIVE the profiles list
// AND the knowledge note from a SEEDED config, not from hand-passed fields.
func TestResolveBackupParamsDerivesManifestNotes(t *testing.T) {
	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "config.toml")
	kb := filepath.Join(t.TempDir(), "bundle")
	cfg := "gog_account = \"x\"\n" +
		"knowledge_bundles = [\"" + kb + "\"]\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PI_STACK_CONFIG", cfgPath)
	t.Setenv("HOME", t.TempDir())

	bp := resolveBackupParams("", 7, time.Now())
	// Profiles were removed: the manifest no longer records any.
	if len(bp.Profiles) != 0 {
		t.Errorf("derived Profiles = %v, want none", bp.Profiles)
	}
	found := false
	for _, k := range bp.Knowledge {
		if k.Path == kb {
			found = true
		}
	}
	if !found {
		t.Errorf("derived Knowledge = %+v, want a note for %q", bp.Knowledge, kb)
	}
}

// TestResolveBackupParamsRedactsRemote is the B6 gate: a knowledge bundle whose
// git origin embeds userinfo/token is recorded in the manifest note REDACTED, so
// the credential never reaches disk (or the printed restore report).
func TestResolveBackupParamsRedactsRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	kb := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", "https://user:tok@github.com/me/kb.git"},
	} {
		cmd := exec.Command("git", append([]string{"-C", kb}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "config.toml")
	cfg := "knowledge_bundles = [\"" + kb + "\"]\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PI_STACK_CONFIG", cfgPath)
	t.Setenv("HOME", t.TempDir())

	bp := resolveBackupParams("", 7, time.Now())
	var remote string
	for _, k := range bp.Knowledge {
		if k.Path == kb {
			remote = k.Remote
		}
	}
	if want := "https://***@github.com/me/kb.git"; remote != want {
		t.Errorf("recorded remote = %q, want %q (userinfo must be redacted)", remote, want)
	}
}

// TestOpRefsHasPastedSecret is the B5 gate: a pasted literal secret VALUE is
// detected while op:// refs, comments, blanks, and the non-secret allowlist are
// NOT flagged.
func TestOpRefsHasPastedSecret(t *testing.T) {
	clean := "# a comment\n\nFOO=op://vault/item/field\nGOG_ACCOUNT=me@example.com\n"
	cleanPath := filepath.Join(t.TempDir(), "op-refs.env")
	if err := os.WriteFile(cleanPath, []byte(clean), 0o600); err != nil {
		t.Fatal(err)
	}
	if opRefsHasPastedSecret(cleanPath) {
		t.Error("clean op-refs (refs + allowlist only) flagged as containing a secret")
	}

	dirty := "FOO=op://vault/item/field\nSLACK_TOKEN=xoxb-1234567890-abcdefghijkl\n"
	dirtyPath := filepath.Join(t.TempDir(), "op-refs.env")
	if err := os.WriteFile(dirtyPath, []byte(dirty), 0o600); err != nil {
		t.Fatal(err)
	}
	if !opRefsHasPastedSecret(dirtyPath) {
		t.Error("op-refs with a pasted xoxb- token NOT flagged; want detection")
	}
}

// TestRestoreRefusesMalformedConfig is the B3+B4 gate (a): an archived config.toml
// that does not parse as TOML aborts the WHOLE restore BEFORE any change — the
// live config is unchanged AND the memory db is NOT swapped.
func TestRestoreRefusesMalformedConfig(t *testing.T) {
	st, dbPath := seedMemDB(t, 3)

	// Archive carries a MALFORMED config.toml.
	srcDir := t.TempDir()
	srcCfg := filepath.Join(srcDir, "config.toml")
	if err := os.WriteFile(srcCfg, []byte("this is = not valid toml = at all ][\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "pi-stack-backup-20260715-120000.tar.gz")
	if _, err := memoryBackup(backupParams{
		DBPath: dbPath, OutPath: archive, Keep: 7, Version: "test",
		ConfigPath: srcCfg, Now: time.Now(),
	}); err != nil {
		t.Fatalf("memoryBackup: %v", err)
	}
	st.db.Close()

	liveBefore, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	destDir := t.TempDir()
	destCfg := filepath.Join(destDir, "config.toml")
	cfgBefore := []byte("gog_account = \"keep-me\"\n")
	if err := os.WriteFile(destCfg, cfgBefore, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := memoryRestore(restoreParams{
		ArchivePath: archive, LiveDBPath: dbPath, ConfigPath: destCfg, Force: true,
		Now: time.Now(), ServeProbe: func() bool { return false },
	}); err == nil {
		t.Fatal("restore accepted a malformed archived config.toml; want refusal")
	}
	// Live config unchanged.
	if got, _ := os.ReadFile(destCfg); string(got) != string(cfgBefore) {
		t.Errorf("live config changed by a refused restore: %q", string(got))
	}
	// Memory NOT swapped.
	if got, _ := os.ReadFile(dbPath); len(got) != len(liveBefore) {
		t.Errorf("memory db mutated by a refused restore (%d -> %d bytes)", len(liveBefore), len(got))
	}
	if m, _ := filepath.Glob(destCfg + ".bak-*"); len(m) != 0 {
		t.Errorf("refused restore left a config .bak: %v", m)
	}
	if m, _ := filepath.Glob(dbPath + ".bak-*"); len(m) != 0 {
		t.Errorf("refused restore left a memory .bak: %v", m)
	}
}

// TestRestoreRollsBackConfigOnMemorySwapFailure is the B3+B4 gate (b): a failure
// in the LAST step (the memory swap) rolls back the already-restored config so
// there is NO split state — the live config is back to its original content.
func TestRestoreRollsBackConfigOnMemorySwapFailure(t *testing.T) {
	st, dbPath := seedMemDB(t, 3)

	srcDir := t.TempDir()
	srcCfg := filepath.Join(srcDir, "config.toml")
	if err := os.WriteFile(srcCfg, []byte("gog_account = \"archived\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srcOp := filepath.Join(srcDir, "op-refs.env")
	if err := os.WriteFile(srcOp, []byte("ARCHIVED=op://v/i/f\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "pi-stack-backup-20260715-120000.tar.gz")
	if _, err := memoryBackup(backupParams{
		DBPath: dbPath, OutPath: archive, Keep: 7, Version: "test",
		ConfigPath: srcCfg, OpRefsPath: srcOp, Now: time.Now(),
	}); err != nil {
		t.Fatalf("memoryBackup: %v", err)
	}
	st.db.Close()

	destDir := t.TempDir()
	destCfg := filepath.Join(destDir, "config.toml")
	cfgBefore := []byte("gog_account = \"original\"\n")
	if err := os.WriteFile(destCfg, cfgBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	destOp := filepath.Join(destDir, "op-refs.env")
	opBefore := []byte("ORIGINAL=op://o/o/o\n")
	if err := os.WriteFile(destOp, opBefore, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := memoryRestore(restoreParams{
		ArchivePath: archive, LiveDBPath: dbPath, ConfigPath: destCfg, OpRefsPath: destOp, Force: true,
		Now: time.Now(), ServeProbe: func() bool { return false },
		swapRename: func(_, _ string) error { return fmt.Errorf("injected swap failure") },
	})
	if err == nil {
		t.Fatal("restore succeeded despite an injected memory-swap failure; want error")
	}
	// Config rolled back to the ORIGINAL — no split state.
	if got, _ := os.ReadFile(destCfg); string(got) != string(cfgBefore) {
		t.Errorf("config not rolled back after swap failure: got %q, want %q", string(got), string(cfgBefore))
	}
	// Op-refs rolled back too.
	if got, _ := os.ReadFile(destOp); string(got) != string(opBefore) {
		t.Errorf("op-refs not rolled back after swap failure: got %q, want %q", string(got), string(opBefore))
	}
	// No orphan .bak files left from the rolled-back steps.
	if m, _ := filepath.Glob(destCfg + ".bak-*"); len(m) != 0 {
		t.Errorf("rollback left a config .bak: %v", m)
	}
	if m, _ := filepath.Glob(destOp + ".bak-*"); len(m) != 0 {
		t.Errorf("rollback left an op-refs .bak: %v", m)
	}
}

// TestRestoreOrderingRollsBackConfigOnOpRefsFailure is the B3+B4 gate for the
// ORDER (plain files FIRST, memory LAST) + full rollback. op-refs is made to fail
// (its parent path is a regular file, so MkdirAll errors). Under the correct
// order the config was already restored and is rolled back, AND the memory db was
// NOT swapped (it comes after op-refs). Under the OLD order (memory first) the db
// would already be swapped and the config left as the archived one — split state
// — so this test fails if the ordering/rollback is reverted.
func TestRestoreOrderingRollsBackConfigOnOpRefsFailure(t *testing.T) {
	st, dbPath := seedMemDB(t, 3)

	srcDir := t.TempDir()
	srcCfg := filepath.Join(srcDir, "config.toml")
	if err := os.WriteFile(srcCfg, []byte("gog_account = \"archived\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srcOp := filepath.Join(srcDir, "op-refs.env")
	if err := os.WriteFile(srcOp, []byte("ARCHIVED=op://v/i/f\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "pi-stack-backup-20260715-120000.tar.gz")
	if _, err := memoryBackup(backupParams{
		DBPath: dbPath, OutPath: archive, Keep: 7, Version: "test",
		ConfigPath: srcCfg, OpRefsPath: srcOp, Now: time.Now(),
	}); err != nil {
		t.Fatalf("memoryBackup: %v", err)
	}
	st.db.Close() // keep the live db FILE in place (existing live db)

	liveBefore, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	destDir := t.TempDir()
	destCfg := filepath.Join(destDir, "config.toml")
	cfgBefore := []byte("gog_account = \"original\"\n")
	if err := os.WriteFile(destCfg, cfgBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	// Make op-refs UNWRITABLE by giving it a parent that is a regular FILE, so
	// installPlainFile's MkdirAll fails before it can tighten or write.
	notADir := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(notADir, []byte("i am a file"), 0o600); err != nil {
		t.Fatal(err)
	}
	destOp := filepath.Join(notADir, "op-refs.env")

	if _, err := memoryRestore(restoreParams{
		ArchivePath: archive, LiveDBPath: dbPath, ConfigPath: destCfg, OpRefsPath: destOp, Force: true,
		Now: time.Now(), ServeProbe: func() bool { return false },
	}); err == nil {
		t.Fatal("restore succeeded despite an unwritable op-refs dest; want error")
	}
	// Config rolled back to the ORIGINAL (proves config was restored FIRST then undone).
	if got, _ := os.ReadFile(destCfg); string(got) != string(cfgBefore) {
		t.Errorf("config not rolled back: got %q, want %q", string(got), string(cfgBefore))
	}
	// Memory db NOT swapped (proves the memory swap is AFTER op-refs).
	if got, _ := os.ReadFile(dbPath); len(got) != len(liveBefore) {
		t.Errorf("memory db was swapped before op-refs failed (%d -> %d bytes); memory must be LAST", len(liveBefore), len(got))
	}
	if m, _ := filepath.Glob(destCfg + ".bak-*"); len(m) != 0 {
		t.Errorf("rollback left a config .bak: %v", m)
	}
	if m, _ := filepath.Glob(dbPath + ".bak-*"); len(m) != 0 {
		t.Errorf("failed restore left a memory .bak: %v", m)
	}
}

// TestRestoreOpRefsPermsAndDir is the B3+B4 gate (c): after a successful restore
// op-refs.env is 0600 and its parent dir is 0700 (MkdirAll does not tighten an
// already-loose existing dir, so restore must chmod it).
func TestRestoreOpRefsPermsAndDir(t *testing.T) {
	st, dbPath := seedMemDB(t, 2)
	srcDir := t.TempDir()
	srcOp := filepath.Join(srcDir, "op-refs.env")
	if err := os.WriteFile(srcOp, []byte("ARCHIVED=op://v/i/f\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "pi-stack-backup-20260715-120000.tar.gz")
	if _, err := memoryBackup(backupParams{
		DBPath: dbPath, OutPath: archive, Keep: 7, Version: "test",
		OpRefsPath: srcOp, Now: time.Now(),
	}); err != nil {
		t.Fatalf("memoryBackup: %v", err)
	}
	st.db.Close()
	for _, sc := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		_ = os.Remove(sc)
	}

	// A pre-existing, deliberately LOOSE (0755) dest dir that restore must tighten.
	destDir := filepath.Join(t.TempDir(), "cfg")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(destDir, 0o755); err != nil {
		t.Fatal(err)
	}
	destOp := filepath.Join(destDir, "op-refs.env")

	res, err := memoryRestore(restoreParams{
		ArchivePath: archive, LiveDBPath: dbPath, OpRefsPath: destOp,
		Now: time.Now(), ServeProbe: func() bool { return false },
	})
	if err != nil {
		t.Fatalf("memoryRestore: %v", err)
	}
	if !res.OpRefsRestored {
		t.Fatal("OpRefsRestored = false, want true")
	}
	fi, err := os.Stat(destOp)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("op-refs.env perm = %o, want 600", perm)
	}
	di, err := os.Stat(destDir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("op-refs dir perm = %o, want 700 (restore must tighten an existing dir)", perm)
	}
}
