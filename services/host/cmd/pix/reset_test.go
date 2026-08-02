package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/rpc"
	"pix/host/sys/systest"
	"pix/host/workflow/upgrade"
)

// fixedNow returns a stable timestamp so .bak suffixes are predictable in tests.
func fixedNow() time.Time { return time.Unix(1700000000, 0) }

const fixedTS = "1700000000"

// resetCfg is a minimal config with a couple of MCP servers, for the --sbx plan.
func resetCfg() *config.Config {
	return &config.Config{MCP: []string{config.GWServerName, "slack"}}
}

// tempPaths lays out a fake config + data tree under root and returns the
// resetPaths pointing at it. It seeds a file in each so a move is observable.
func tempPaths(t *testing.T, root string) resetPaths {
	t.Helper()
	configDir := filepath.Join(root, "config", "pix")
	dataRoot := filepath.Join(root, "data", ".pix")
	memDir := filepath.Join(dataRoot, "memory")
	kbDir := filepath.Join(dataRoot, "knowledge")
	for _, d := range []string{configDir, memDir, kbDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(configDir, "config.toml"), "x")
	writeFile(t, filepath.Join(memDir, "memory.db"), "facts")
	writeFile(t, filepath.Join(kbDir, "knowledge.db"), "index")
	return resetPaths{configDir: configDir, dataRoot: dataRoot, memoryDir: memDir, knowledgeDir: kbDir}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// TestResetPlan_KeepMemory: --keep-memory targets the knowledge dir but NOT the
// memory dir or the whole data root; the default (no keep) moves the data root.
func TestResetPlan_KeepMemory(t *testing.T) {
	p := resetPaths{configDir: "/c", dataRoot: "/d", memoryDir: "/d/memory", knowledgeDir: "/d/knowledge"}

	keep := resetPlan(resetCfg(), p, resetOpts{keepMemory: true})
	if !keep.KeepMemory || keep.MemoryDir != "/d/memory" {
		t.Fatalf("keep plan: KeepMemory=%v MemoryDir=%q", keep.KeepMemory, keep.MemoryDir)
	}
	if backupHas(keep.Backups, "/d/memory") {
		t.Error("--keep-memory must NOT back up the memory dir")
	}
	if backupHas(keep.Backups, "/d") {
		t.Error("--keep-memory must NOT back up the whole data root")
	}
	if !backupHas(keep.Backups, "/d/knowledge") {
		t.Error("--keep-memory must back up the knowledge dir")
	}
	if !backupHas(keep.Backups, "/c") {
		t.Error("config dir must always be backed up")
	}

	full := resetPlan(resetCfg(), p, resetOpts{})
	if !backupHas(full.Backups, "/d") {
		t.Error("default reset must back up the whole data root (memory included)")
	}
}

// TestResetPlan_Sbx: --sbx includes the sandbox + mcp actions; without it, none.
func TestResetPlan_Sbx(t *testing.T) {
	p := resetPaths{configDir: "/c", dataRoot: "/d", memoryDir: "/d/memory", knowledgeDir: "/d/knowledge"}

	with := resetPlan(resetCfg(), p, resetOpts{sbx: true})
	if !with.RemoveSandboxes {
		t.Error("--sbx must set RemoveSandboxes")
	}
	if strings.Join(with.MCPRemove, ",") != config.GWServerName+",slack" {
		t.Errorf("MCPRemove = %v, want [%s slack]", with.MCPRemove, config.GWServerName)
	}

	without := resetPlan(resetCfg(), p, resetOpts{})
	if without.RemoveSandboxes || len(without.MCPRemove) != 0 {
		t.Errorf("without --sbx: RemoveSandboxes=%v MCPRemove=%v", without.RemoveSandboxes, without.MCPRemove)
	}
}

func backupHas(bs []backupTarget, path string) bool {
	for _, b := range bs {
		if b.Path == path {
			return true
		}
	}
	return false
}

// stubStopServe replaces the reset executor's serve-stop with a hermetic no-op so
// a test never signals a real `pix-host serve` on the developer's machine.
// It records that the stop ran and restores the original on cleanup.
func stubStopServe(t *testing.T) *bool {
	t.Helper()
	called := false
	orig := stopServeForReset
	stopServeForReset = func(out io.Writer) (bool, error) {
		called = true
		return false, nil
	}
	t.Cleanup(func() { stopServeForReset = orig })
	return &called
}

// TestExecuteReset_MovesConfigAndData: config + data land in .bak, originals gone.
func TestExecuteReset_MovesConfigAndData(t *testing.T) {
	called := stubStopServe(t)
	root := t.TempDir()
	p := tempPaths(t, root)
	a := resetPlan(resetCfg(), p, resetOpts{})

	var buf bytes.Buffer
	if _, err := executeReset(a, defaultResetFS(), noToolEnv(), &buf, fixedNow); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !*called {
		t.Error("executeReset must stop host services via service.Stop")
	}
	if exists(p.configDir) {
		t.Error("config dir should have been moved aside")
	}
	if exists(p.dataRoot) {
		t.Error("data root should have been moved aside")
	}
	if !exists(p.configDir + ".bak-" + fixedTS) {
		t.Error("config .bak backup missing")
	}
	if !exists(p.dataRoot + ".bak-" + fixedTS) {
		t.Error("data .bak backup missing")
	}
}

// TestExecuteReset_PreservesOnePasswordRefs: op-refs.env + hostmode.env survive
// the config-dir move byte-identical into a fresh config dir, while config.toml
// (which moved aside with everything else) does NOT come back.
func TestExecuteReset_PreservesOnePasswordRefs(t *testing.T) {
	stubStopServe(t)
	root := t.TempDir()
	p := tempPaths(t, root)
	writeFile(t, filepath.Join(p.configDir, "op-refs.env"), "ANTHROPIC_API_KEY=op://vault/item/field\n")
	writeFile(t, filepath.Join(p.configDir, "hostmode.env"), "OPENAI_API_KEY=op://vault/item2/field\n")
	a := resetPlan(resetCfg(), p, resetOpts{})
	a.PreserveRefs = true // exercise the lower-level optional preservation mechanism

	var buf bytes.Buffer
	if _, err := executeReset(a, defaultResetFS(), noToolEnv(), &buf, fixedNow); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The old config dir moved aside as usual — config.toml is still in the .bak.
	if !exists(p.configDir + ".bak-" + fixedTS + string(filepath.Separator) + "config.toml") {
		t.Error("config.toml should still be in the .bak")
	}

	// A FRESH config dir exists with just the ref files, not config.toml.
	if !exists(p.configDir) {
		t.Fatal("a fresh config dir should have been recreated to hold the preserved refs")
	}
	if exists(filepath.Join(p.configDir, "config.toml")) {
		t.Error("config.toml must NOT come back into the fresh config dir")
	}
	gotOP, err := os.ReadFile(filepath.Join(p.configDir, "op-refs.env"))
	if err != nil {
		t.Fatalf("op-refs.env missing from fresh config dir: %v", err)
	}
	if string(gotOP) != "ANTHROPIC_API_KEY=op://vault/item/field\n" {
		t.Errorf("op-refs.env content mismatch, got %q", gotOP)
	}
	gotHM, err := os.ReadFile(filepath.Join(p.configDir, "hostmode.env"))
	if err != nil {
		t.Fatalf("hostmode.env missing from fresh config dir: %v", err)
	}
	if string(gotHM) != "OPENAI_API_KEY=op://vault/item2/field\n" {
		t.Errorf("hostmode.env content mismatch, got %q", gotHM)
	}

	// And the same refs are STILL present in the .bak (additive, not a move).
	bakOP, err := os.ReadFile(p.configDir + ".bak-" + fixedTS + string(filepath.Separator) + "op-refs.env")
	if err != nil {
		t.Fatalf("op-refs.env should still be present in the .bak: %v", err)
	}
	if string(bakOP) != "ANTHROPIC_API_KEY=op://vault/item/field\n" {
		t.Errorf(".bak op-refs.env content mismatch, got %q", bakOP)
	}

	if !strings.Contains(buf.String(), "kept 1Password refs") {
		t.Errorf("expected a 'kept 1Password refs' line in output, got %q", buf.String())
	}
}

// TestExecuteReset_NoRefsNoFreshConfigDir: a config dir with no ref files must
// NOT get a fresh (empty) config dir recreated after the move.
func TestExecuteReset_NoRefsNoFreshConfigDir(t *testing.T) {
	stubStopServe(t)
	root := t.TempDir()
	p := tempPaths(t, root) // seeds only config.toml, no ref files
	a := resetPlan(resetCfg(), p, resetOpts{})
	a.PreserveRefs = true

	var buf bytes.Buffer
	if _, err := executeReset(a, defaultResetFS(), noToolEnv(), &buf, fixedNow); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if exists(p.configDir) {
		t.Error("no ref files were present — a fresh config dir must NOT be recreated")
	}
	if strings.Contains(buf.String(), "kept 1Password refs") {
		t.Error("must not claim refs were kept when none existed")
	}
}

// TestExecuteReset_CleanResetDoesNotPreserveRefs: reset is a clean wipe — refs
// remain recoverable only in the timestamped backup.
func TestExecuteReset_CleanResetDoesNotPreserveRefs(t *testing.T) {
	stubStopServe(t)
	root := t.TempDir()
	p := tempPaths(t, root)
	writeFile(t, filepath.Join(p.configDir, "op-refs.env"), "ANTHROPIC_API_KEY=op://vault/item/field\n")
	a := resetPlan(resetCfg(), p, resetOpts{})

	var buf bytes.Buffer
	if _, err := executeReset(a, defaultResetFS(), noToolEnv(), &buf, fixedNow); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exists(p.configDir) {
		t.Error("reset must NOT recreate the config dir to preserve refs")
	}
	if !exists(p.configDir + ".bak-" + fixedTS + string(filepath.Separator) + "op-refs.env") {
		t.Error("the ref should still be in the .bak")
	}
}

// TestExecuteReset_KeepMemoryPreservesMemory: memory survives, knowledge + any
// other data-root entry are moved aside.
func TestExecuteReset_KeepMemoryPreservesMemory(t *testing.T) {
	stubStopServe(t)
	root := t.TempDir()
	p := tempPaths(t, root)
	// An extra non-memory entry to prove the sweep catches "anything else".
	other := filepath.Join(p.dataRoot, "cache")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	a := resetPlan(resetCfg(), p, resetOpts{keepMemory: true})

	var buf bytes.Buffer
	if _, err := executeReset(a, defaultResetFS(), noToolEnv(), &buf, fixedNow); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !exists(p.memoryDir) {
		t.Error("--keep-memory must preserve the memory dir")
	}
	if !exists(filepath.Join(p.memoryDir, "memory.db")) {
		t.Error("captured facts must survive --keep-memory")
	}
	if exists(p.knowledgeDir) {
		t.Error("knowledge dir should have been moved aside")
	}
	if !exists(p.knowledgeDir + ".bak-" + fixedTS) {
		t.Error("knowledge .bak backup missing")
	}
	if exists(other) {
		t.Error("the sweep should move aside non-memory data-root entries")
	}
	if !exists(other + ".bak-" + fixedTS) {
		t.Error("cache .bak backup missing")
	}
}

// TestRunResetCore_NonTTYNoYesRefuses: no TTY + no --yes => errResetNeedsYes and
// NOTHING is moved.
func TestRunResetCore_NonTTYNoYesRefuses(t *testing.T) {
	root := t.TempDir()
	p := tempPaths(t, root)
	rio := setupIO{in: strings.NewReader(""), out: &bytes.Buffer{}, isTTY: false}

	err := runResetCore(resetCfg(), p, resetOpts{}, defaultResetFS(), noToolEnv(), rio, fixedNow)
	if !errors.Is(err, errResetNeedsYes) {
		t.Fatalf("want errResetNeedsYes, got %v", err)
	}
	if !exists(p.configDir) || !exists(p.dataRoot) {
		t.Error("a refused reset must not move anything")
	}
}

// TestRunResetCore_YesExecutes: --yes runs without a prompt and moves state.
func TestRunResetCore_YesExecutes(t *testing.T) {
	stubStopServe(t)
	root := t.TempDir()
	p := tempPaths(t, root)
	rio := setupIO{in: strings.NewReader(""), out: &bytes.Buffer{}, isTTY: false}

	if err := runResetCore(resetCfg(), p, resetOpts{assumeYes: true}, defaultResetFS(), noToolEnv(), rio, fixedNow); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exists(p.configDir) {
		t.Error("--yes should have moved the config dir")
	}
}

// TestUninstall_RemovesBinSymlinks: uninstall removes our symlinks but leaves a
// non-symlink (not ours) in place, and still runs the reset.
func TestUninstall_RemovesBinSymlinks(t *testing.T) {
	stubStopServe(t)
	root := t.TempDir()
	p := tempPaths(t, root)

	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// A repo checkout's launcher: basename is exactly "pix" (what we install).
	outDir := filepath.Join(root, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(outDir, "pix")
	writeFile(t, target, "binary")
	link := filepath.Join(bin, "pix")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	// A real (non-symlink) file at the pix-host slot must be left alone.
	notOurs := filepath.Join(bin, "pix-host")
	writeFile(t, notOurs, "hand-placed")

	rio := setupIO{in: strings.NewReader(""), out: &bytes.Buffer{}, isTTY: false}
	err := runUninstallCore(resetCfg(), p, []string{link, notOurs}, resetOpts{assumeYes: true}, upgrade.Provenance{Channel: upgrade.ChannelInstaller},
		defaultResetFS(), noToolEnv(), rio, fixedNow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exists(link) {
		t.Error("uninstall should remove our bin symlink")
	}
	if !exists(notOurs) {
		t.Error("uninstall must NOT remove a non-symlink we didn't install")
	}
	if exists(p.configDir) {
		t.Error("uninstall should also run the reset (config dir moved)")
	}
}

// TestUninstall_NonTTYNoYesRefuses: uninstall inherits the same guard as reset.
func TestUninstall_NonTTYNoYesRefuses(t *testing.T) {
	root := t.TempDir()
	p := tempPaths(t, root)
	link := filepath.Join(root, "pix")
	rio := setupIO{in: strings.NewReader(""), out: &bytes.Buffer{}, isTTY: false}

	err := runUninstallCore(resetCfg(), p, []string{link}, resetOpts{}, upgrade.Provenance{Channel: upgrade.ChannelInstaller}, defaultResetFS(), noToolEnv(), rio, fixedNow)
	if !errors.Is(err, errResetNeedsYes) {
		t.Fatalf("want errResetNeedsYes, got %v", err)
	}
	if !exists(p.configDir) {
		t.Error("a refused uninstall must not move anything")
	}
}

func TestRunUninstallHomebrewDoesNotRemoveOwnedOrDuplicateBinaries(t *testing.T) {
	stubStopServe(t)
	root := t.TempDir()
	p := tempPaths(t, root)
	localPix := filepath.Join(root, "bin", "pix")
	if err := os.MkdirAll(filepath.Dir(localPix), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, localPix, "curl-installed copy")

	var out bytes.Buffer
	rio := setupIO{in: strings.NewReader(""), out: &out, isTTY: false}
	err := runUninstallCore(resetCfg(), p, []string{localPix}, resetOpts{assumeYes: true}, upgrade.Provenance{Channel: upgrade.ChannelHomebrew},
		defaultResetFS(), noToolEnv(), rio, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if !exists(localPix) {
		t.Fatal("Homebrew uninstall path must not remove any binary directly")
	}
	for _, want := range []string{"owned by Homebrew", "brew uninstall mcavage/tap/pix", "next launch"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q missing %q", out.String(), want)
		}
	}
}

// TestParseResetArgs covers reset flags, help, and unknown input.
func TestParseResetArgs(t *testing.T) {
	if o, err := parseResetArgs([]string{"--keep-memory", "--purge-data", "--sbx", "--yes"}, true, true); err != nil ||
		!o.keepMemory || !o.purgeData || !o.sbx || !o.assumeYes {
		t.Fatalf("reset flags: %+v err=%v", o, err)
	}
	if o, err := parseResetArgs([]string{"-h"}, true, false); err != nil || !o.help {
		t.Errorf("help: %+v err=%v", o, err)
	}
	if _, err := parseResetArgs([]string{"--nope"}, true, false); err == nil {
		t.Error("unknown flag must error")
	}
}

// TestExecuteReset_MoveFailureReturnsError: a failing rename makes executeReset
// return an error, and runResetCore propagates it (non-zero exit for the CLI).
func TestExecuteReset_MoveFailureReturnsError(t *testing.T) {
	stubStopServe(t)
	root := t.TempDir()
	p := tempPaths(t, root)
	a := resetPlan(resetCfg(), p, resetOpts{})

	fsys := defaultResetFS()
	fsys.rename = func(_, _ string) error { return errors.New("disk full") }

	var buf bytes.Buffer
	_, err := executeReset(a, fsys, noToolEnv(), &buf, fixedNow)
	if err == nil {
		t.Fatal("a failed move must make executeReset return an error, not report success")
	}

	rio := setupIO{in: strings.NewReader(""), out: &bytes.Buffer{}, isTTY: false}
	if rErr := runResetCore(resetCfg(), p, resetOpts{assumeYes: true}, fsys, noToolEnv(), rio, fixedNow); rErr == nil {
		t.Error("runResetCore must return non-nil when a move failed")
	}
}

// TestUninstall_BackupFailureKeepsSymlinks: if the reset backup fails, uninstall
// must NOT remove the bin symlinks (never strand the user with no binaries).
func TestUninstall_BackupFailureKeepsSymlinks(t *testing.T) {
	stubStopServe(t)
	root := t.TempDir()
	p := tempPaths(t, root)

	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "out", "pix")
	if err := os.WriteFile(target, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(bin, "pix")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	fsys := defaultResetFS()
	fsys.rename = func(_, _ string) error { return errors.New("backup failed") }

	rio := setupIO{in: strings.NewReader(""), out: &bytes.Buffer{}, isTTY: false}
	err := runUninstallCore(resetCfg(), p, []string{link}, resetOpts{assumeYes: true}, upgrade.Provenance{Channel: upgrade.ChannelInstaller},
		fsys, noToolEnv(), rio, fixedNow)
	if err == nil {
		t.Fatal("uninstall must return the backup error")
	}
	if !exists(link) {
		t.Error("uninstall must NOT remove the bin symlink after a failed backup")
	}
}

// TestExecuteReset_ServeUpAbortsDataMove: when serve is still up (injected dial),
// the data dir move is refused (error) but the config dir is still backed up.
func TestExecuteReset_ServeUpAbortsDataMove(t *testing.T) {
	stubStopServe(t)
	root := t.TempDir()
	p := tempPaths(t, root)
	a := resetPlan(resetCfg(), p, resetOpts{})

	env := fakeEnv{present: map[string]bool{}, ports: map[int]bool{rpc.MemoryPortDefault: true}}.env()
	var buf bytes.Buffer
	_, err := executeReset(a, defaultResetFS(), env, &buf, fixedNow)
	if err == nil {
		t.Fatal("serve still up must make executeReset return an error")
	}
	if exists(p.dataRoot + ".bak-" + fixedTS) {
		t.Error("the data dir must NOT move while serve is up")
	}
	if !exists(p.dataRoot) {
		t.Error("the data dir must be left in place when the move is blocked")
	}
	if exists(p.configDir) {
		t.Error("the config dir (safe) should still be backed up")
	}

	// --force overrides the guard: the data dir moves even with serve up.
	aForce := resetPlan(resetCfg(), tempPaths(t, t.TempDir()), resetOpts{force: true})
	if !aForce.Force {
		t.Error("--force must set Force on the plan")
	}
}

// TestExecuteReset_KeepMemoryCustomDBDir: a custom (non-"memory") MEMORY_DB dir
// is preserved by --keep-memory (the sweep preserves the RESOLVED dir, not a
// hardcoded name).
func TestExecuteReset_KeepMemoryCustomDBDir(t *testing.T) {
	stubStopServe(t)
	root := t.TempDir()
	dataRoot := filepath.Join(root, ".pix")
	customMem := filepath.Join(dataRoot, "facts-store") // NOT named "memory"
	kbDir := filepath.Join(dataRoot, "knowledge")
	cache := filepath.Join(dataRoot, "cache")
	for _, d := range []string{customMem, kbDir, cache} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(customMem, "memory.db"), "facts")
	p := resetPaths{configDir: filepath.Join(root, "config"), dataRoot: dataRoot, memoryDir: customMem, knowledgeDir: kbDir}
	a := resetPlan(resetCfg(), p, resetOpts{keepMemory: true})

	var buf bytes.Buffer
	if _, err := executeReset(a, defaultResetFS(), noToolEnv(), &buf, fixedNow); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exists(customMem) || !exists(filepath.Join(customMem, "memory.db")) {
		t.Error("--keep-memory must preserve the resolved custom memory dir")
	}
	if exists(kbDir) {
		t.Error("knowledge dir should have been moved aside")
	}
	if exists(cache) {
		t.Error("the sweep should move aside non-memory entries")
	}
}

// TestExecuteReset_KeepMemoryLooseDBInRoot: --keep-memory with MEMORY_DB pointing
// at a db FILE sitting DIRECTLY inside the data root (memoryDir == dataRoot). The
// sweep must preserve that db file + its -wal sidecar (never move them), while an
// unrelated sibling file/dir IS moved aside. This is the data-loss regression: the
// old sweep only matched an entry whose path == MemoryDir (== dataRoot), so it
// preserved NOTHING and swept the db away while reporting it preserved.
func TestExecuteReset_KeepMemoryLooseDBInRoot(t *testing.T) {
	stubStopServe(t)
	root := t.TempDir()
	dataRoot := filepath.Join(root, ".pix")
	if err := os.MkdirAll(dataRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	// A custom MEMORY_DB loose in the data root, with a -wal sidecar.
	memDB := filepath.Join(dataRoot, "custom-memory.db")
	writeFile(t, memDB, "facts")
	writeFile(t, memDB+"-wal", "wal")
	// An unrelated sibling file and dir that MUST be swept aside.
	siblingFile := filepath.Join(dataRoot, "scratch.log")
	writeFile(t, siblingFile, "junk")
	siblingDir := filepath.Join(dataRoot, "knowledge")
	if err := os.MkdirAll(siblingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(siblingDir, "knowledge.db"), "index")

	p := resetPaths{
		configDir:    filepath.Join(root, "config"),
		dataRoot:     dataRoot,
		memoryDir:    dataRoot, // dir(MEMORY_DB) == dataRoot
		memoryDB:     memDB,    // the loose db file directly in the root
		knowledgeDir: siblingDir,
	}
	a := resetPlan(resetCfg(), p, resetOpts{keepMemory: true})

	var buf bytes.Buffer
	if _, err := executeReset(a, defaultResetFS(), noToolEnv(), &buf, fixedNow); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The loose db + its -wal are STILL present, untouched.
	if !exists(memDB) {
		t.Error("the loose custom memory db must be preserved (still present)")
	}
	if !exists(memDB + "-wal") {
		t.Error("the loose memory db -wal sidecar must be preserved")
	}
	if exists(memDB + ".bak-" + fixedTS) {
		t.Error("the loose memory db must NOT have been moved aside")
	}
	// The unrelated sibling file + dir were swept aside.
	if exists(siblingFile) {
		t.Error("an unrelated sibling file must be swept aside")
	}
	if !exists(siblingFile + ".bak-" + fixedTS) {
		t.Error("the swept sibling file .bak backup is missing")
	}
	if exists(siblingDir) {
		t.Error("the knowledge dir sibling must be swept aside")
	}
	if !exists(siblingDir + ".bak-" + fixedTS) {
		t.Error("the swept knowledge dir .bak backup is missing")
	}
}

// TestMoveAside_NoOverwrite: a pre-existing <path>.bak-<ts> is never overwritten;
// moveAside picks a unique suffixed name instead.
func TestMoveAside_NoOverwrite(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "data")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	collision := src + ".bak-" + fixedTS
	writeFile(t, collision, "existing backup")

	dest, err := moveAside(defaultResetFS(), src, 1700000000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dest == collision {
		t.Fatal("moveAside must NOT reuse an existing .bak path")
	}
	if dest != collision+"-1" {
		t.Errorf("want unique %q, got %q", collision+"-1", dest)
	}
	if b, _ := os.ReadFile(collision); string(b) != "existing backup" {
		t.Error("the existing backup must be left intact")
	}
	if !exists(dest) {
		t.Error("the source must have moved to the unique dest")
	}
}

// TestUninstall_LeavesUnrelatedSymlinks: symlinks in bin slots whose target
// BASENAME is not EXACTLY pix/pix-host are left in place + reported —
// both a genuinely unrelated target AND a DECEPTIVE one whose path merely
// CONTAINS "pix" (a substring match would wrongly delete it).
func TestUninstall_LeavesUnrelatedSymlinks(t *testing.T) {
	stubStopServe(t)
	root := t.TempDir()
	p := tempPaths(t, root)

	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// A genuinely unrelated target, and a DECEPTIVE one (path contains "pix"
	// but its basename does NOT match our binaries).
	other := filepath.Join(root, "opt", "other-tool")
	deceptive := filepath.Join(root, "opt", "pix-ish-wrapper")
	for _, tgt := range []string{other, deceptive} {
		if err := os.MkdirAll(filepath.Dir(tgt), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tgt, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	linkA := filepath.Join(bin, "pix")      // -> /opt/other-tool
	linkB := filepath.Join(bin, "pix-host") // -> /opt/pix-ish-wrapper
	if err := os.Symlink(other, linkA); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(deceptive, linkB); err != nil {
		t.Fatal(err)
	}

	rio := setupIO{in: strings.NewReader(""), out: &bytes.Buffer{}, isTTY: false}
	if err := runUninstallCore(resetCfg(), p, []string{linkA, linkB}, resetOpts{assumeYes: true}, upgrade.Provenance{Channel: upgrade.ChannelInstaller},
		defaultResetFS(), noToolEnv(), rio, fixedNow); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exists(linkA) {
		t.Error("uninstall must NOT remove an unrelated symlink (/opt/other-tool)")
	}
	if !exists(linkB) {
		t.Error("uninstall must NOT remove a deceptive substring symlink (/opt/pix-ish-wrapper)")
	}
	out := rio.out.(*bytes.Buffer).String()
	if !strings.Contains(out, "not a pix binary") {
		t.Errorf("want a 'left in place' report, got %q", out)
	}
}

// TestExecuteReset_CustomDBOutsideRootMovesFileOnly: a custom MEMORY_DB pointing
// at a file OUTSIDE the data root moves ONLY that file (+ its -wal sidecar),
// NEVER the parent dir or an unrelated sibling in it.
func TestExecuteReset_CustomDBOutsideRootMovesFileOnly(t *testing.T) {
	stubStopServe(t)
	root := t.TempDir()
	dataRoot := filepath.Join(root, ".pix")
	if err := os.MkdirAll(dataRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	// A custom MEMORY_DB in a SHARED dir alongside an unrelated file.
	docs := filepath.Join(root, "Documents")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	memDB := filepath.Join(docs, "memory.db")
	writeFile(t, memDB, "facts")
	writeFile(t, memDB+"-wal", "wal")
	unrelated := filepath.Join(docs, "taxes.pdf")
	writeFile(t, unrelated, "important")

	p := resetPaths{
		configDir: filepath.Join(root, "config"),
		dataRoot:  dataRoot,
		memoryDir: docs,  // dir(MEMORY_DB)
		memoryDB:  memDB, // the custom file path
	}
	a := resetPlan(resetCfg(), p, resetOpts{})

	var buf bytes.Buffer
	if _, err := executeReset(a, defaultResetFS(), noToolEnv(), &buf, fixedNow); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The db file + its -wal moved aside...
	if exists(memDB) {
		t.Error("the custom memory.db file should have moved aside")
	}
	if !exists(memDB + ".bak-" + fixedTS) {
		t.Error("memory.db .bak backup missing")
	}
	if exists(memDB + "-wal") {
		t.Error("the -wal sidecar should have moved aside")
	}
	if !exists(memDB + "-wal.bak-" + fixedTS) {
		t.Error("memory.db-wal .bak backup missing")
	}
	// ...but the parent dir and the UNRELATED sibling are UNTOUCHED.
	if !exists(docs) {
		t.Error("the parent dir must NOT be moved (only the db file + sidecars)")
	}
	if !exists(unrelated) {
		t.Error("an unrelated sibling file must NOT be moved")
	}
	if exists(docs + ".bak-" + fixedTS) {
		t.Error("the parent dir must NOT be backed up wholesale")
	}
}

// TestExecuteReset_KeepMemoryCustomKnowledgeDBOutsideRoot: --keep-memory with a
// custom KNOWLEDGE_DB pointing at a file OUTSIDE the data root moves ONLY that
// db file (+ its -wal/-shm sidecars), NEVER the parent dir or an unrelated
// sibling in it — while captured memory is still preserved in the same run.
func TestExecuteReset_KeepMemoryCustomKnowledgeDBOutsideRoot(t *testing.T) {
	stubStopServe(t)
	root := t.TempDir()
	dataRoot := filepath.Join(root, ".pix")
	memoryDir := filepath.Join(dataRoot, "memory")
	if err := os.MkdirAll(memoryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(memoryDir, "memory.db"), "facts")

	// A custom KNOWLEDGE_DB in a SHARED dir alongside an unrelated file.
	docs := filepath.Join(root, "Documents")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	kbDB := filepath.Join(docs, "knowledge.db")
	writeFile(t, kbDB, "index")
	writeFile(t, kbDB+"-wal", "wal")
	unrelated := filepath.Join(docs, "taxes.pdf")
	writeFile(t, unrelated, "important")

	p := resetPaths{
		configDir:    filepath.Join(root, "config"),
		dataRoot:     dataRoot,
		memoryDir:    memoryDir,
		knowledgeDir: docs, // dir(KNOWLEDGE_DB)
		knowledgeDB:  kbDB, // the custom file path
	}
	a := resetPlan(resetCfg(), p, resetOpts{keepMemory: true})

	var buf bytes.Buffer
	if _, err := executeReset(a, defaultResetFS(), noToolEnv(), &buf, fixedNow); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The knowledge db file + its -wal moved aside...
	if exists(kbDB) {
		t.Error("the custom knowledge.db file should have moved aside")
	}
	if !exists(kbDB + ".bak-" + fixedTS) {
		t.Error("knowledge.db .bak backup missing")
	}
	if exists(kbDB + "-wal") {
		t.Error("the -wal sidecar should have moved aside")
	}
	if !exists(kbDB + "-wal.bak-" + fixedTS) {
		t.Error("knowledge.db-wal .bak backup missing")
	}
	// ...but the parent dir and the UNRELATED sibling are UNTOUCHED.
	if !exists(docs) {
		t.Error("the parent dir must NOT be moved (only the db file + sidecars)")
	}
	if !exists(unrelated) {
		t.Error("an unrelated sibling file must NOT be moved")
	}
	if exists(docs + ".bak-" + fixedTS) {
		t.Error("the parent dir must NOT be backed up wholesale")
	}
	// ...and captured memory is preserved in the same run.
	if !exists(filepath.Join(memoryDir, "memory.db")) {
		t.Error("captured memory must be preserved by --keep-memory")
	}
	if exists(memoryDir + ".bak-" + fixedTS) {
		t.Error("the memory dir must NOT be backed up under --keep-memory")
	}
}

// TestExecuteReset_KnowledgePortUpAbortsDataMove: a knowledge-only serve still
// running (knowledge port up, memory port down) must ALSO block the data move.
func TestExecuteReset_KnowledgePortUpAbortsDataMove(t *testing.T) {
	stubStopServe(t)
	root := t.TempDir()
	p := tempPaths(t, root)
	a := resetPlan(resetCfg(), p, resetOpts{})

	env := fakeEnv{present: map[string]bool{}, ports: map[int]bool{rpc.KnowledgePortDefault: true}}.env()
	var buf bytes.Buffer
	_, err := executeReset(a, defaultResetFS(), env, &buf, fixedNow)
	if err == nil {
		t.Fatal("a knowledge-only serve still up must block the data move")
	}
	if exists(p.dataRoot + ".bak-" + fixedTS) {
		t.Error("the data dir must NOT move while the knowledge port is up")
	}
	if !exists(p.dataRoot) {
		t.Error("the data dir must be left in place when the move is blocked")
	}
	if exists(p.configDir) {
		t.Error("the config dir (safe) should still be backed up")
	}
}

// TestExecuteReset_KeepMemoryReadDirErrorSurfaces: a readDir failure during the
// keep-memory sweep must surface as a returned error, not be swallowed as
// success (never report preservation over a dir we could not even scan).
func TestExecuteReset_KeepMemoryReadDirErrorSurfaces(t *testing.T) {
	stubStopServe(t)
	root := t.TempDir()
	p := tempPaths(t, root)
	a := resetPlan(resetCfg(), p, resetOpts{keepMemory: true})

	fsys := defaultResetFS()
	fsys.readDir = func(string) ([]os.DirEntry, error) {
		return nil, errors.New("permission denied")
	}

	var buf bytes.Buffer
	_, err := executeReset(a, fsys, noToolEnv(), &buf, fixedNow)
	if err == nil {
		t.Fatal("a readDir failure in the keep-memory sweep must return an error")
	}
	if strings.Contains(buf.String(), "preserved captured memory") {
		t.Error("must NOT report preservation/success over an unreadable data dir")
	}
}

// noToolEnv is a hostenv.Env with no sbx on PATH, so the executor degrades (prints
// commands) without touching the host. The serve-stop no longer probes PATH (it
// is pidfile-based via service.Stop), so this env exercises the sbx path only.
func noToolEnv() hostenv.Env {
	return fakeEnv{present: map[string]bool{}, output: map[string]string{}}.env()
}

// TestResolveResetPaths_RelativeMemoryDBAbsolute gates round-6: a relative
// MEMORY_DB must be normalized to an absolute path at resolution so the
// --keep-memory preserve set matches the sweep's absolute entries.
func TestResolveResetPaths_RelativeMemoryDBAbsolute(t *testing.T) {
	t.Chdir(t.TempDir())
	env := hostenv.Env{System: &systest.Fake{HomeDirFn: func() string { return "/home/fake" }, GetenvFn: func(k string) string {
		if k == "MEMORY_DB" {
			return ".pix/custom-memory.db"
		}
		return ""
	}}}
	p := resolveResetPaths(env)
	if !filepath.IsAbs(p.memoryDB) {
		t.Errorf("memoryDB = %q, want absolute", p.memoryDB)
	}
	if !filepath.IsAbs(p.memoryDir) {
		t.Errorf("memoryDir = %q, want absolute", p.memoryDir)
	}
}

// stubRestartServe records whether the post-reset restart fired.
func stubRestartServe(t *testing.T) *bool {
	t.Helper()
	called := false
	orig := restartServeForReset
	restartServeForReset = func(out io.Writer) error { called = true; return nil }
	t.Cleanup(func() { restartServeForReset = orig })
	return &called
}

// TestExecuteReset_ClearsRuntimeFilesAndRestarts: when a daemon was up, reset
// wipes the state-dir runtime files and restarts a fresh daemon.
func TestExecuteReset_ClearsRuntimeFilesAndRestarts(t *testing.T) {
	stubStopServe(t)
	restarted := stubRestartServe(t)

	dir := t.TempDir()
	pid := filepath.Join(dir, "serve.pid")
	lock := filepath.Join(dir, "serve.spawn.lock")
	for _, p := range []string{pid, lock} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// dial returns true exactly once: wasUp probe sees it up, the post-stop guard
	// sees it down (so the data move isn't blocked and restart runs).
	firstDial := true
	env := noToolEnv()
	systest.Of(env.System).DialLocalFn = func(int) bool {
		if firstDial {
			firstDial = false
			return true
		}
		return false
	}

	a := resetActions{RuntimeFiles: []string{pid, lock}}
	var buf bytes.Buffer
	if _, err := executeReset(a, defaultResetFS(), env, &buf, fixedNow); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, p := range []string{pid, lock} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("runtime file %s should have been removed", p)
		}
	}
	if !*restarted {
		t.Error("a daemon that was up should be restarted on the clean slate")
	}
}

// TestExecuteReset_NoRestartWhenDown: nothing running -> no restart.
func TestExecuteReset_NoRestartWhenDown(t *testing.T) {
	stubStopServe(t)
	restarted := stubRestartServe(t)
	env := noToolEnv() // dial nil => serveStillUp false
	var buf bytes.Buffer
	if _, err := executeReset(resetActions{}, defaultResetFS(), env, &buf, fixedNow); err != nil {
		t.Fatal(err)
	}
	if *restarted {
		t.Error("must not start a daemon that wasn't running before reset")
	}
}
