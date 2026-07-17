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

	"pi-stack/host/config"
)

// fixedNow returns a stable timestamp so .bak suffixes are predictable in tests.
func fixedNow() time.Time { return time.Unix(1700000000, 0) }

const fixedTS = "1700000000"

// resetCfg is a minimal config with a couple of MCP servers, for the --sbx plan.
func resetCfg() *config.Config {
	return &config.Config{MCP: []string{"gog", "slack"}}
}

// tempPaths lays out a fake config + data tree under root and returns the
// resetPaths pointing at it. It seeds a file in each so a move is observable.
func tempPaths(t *testing.T, root string) resetPaths {
	t.Helper()
	configDir := filepath.Join(root, "config", "pi-stack")
	dataRoot := filepath.Join(root, "data", ".pi-stack")
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
	if strings.Join(with.MCPRemove, ",") != "gog,slack" {
		t.Errorf("MCPRemove = %v, want [gog slack]", with.MCPRemove)
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

// backupDangerous reports the Dangerous flag of the backup target at path (false
// if the target is absent), so a test can assert the fail-closed classification.
func backupDangerous(bs []backupTarget, path string) bool {
	for _, b := range bs {
		if b.Path == path {
			return b.Dangerous
		}
	}
	return false
}

// stubStopServe replaces the reset executor's serve-stop with a hermetic no-op so
// a test never signals a real `pi-stack-host serve` on the developer's machine.
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
		t.Error("executeReset must stop host services via stopServe")
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
	// A repo checkout's launcher: basename is exactly "pi-stack" (what we install).
	outDir := filepath.Join(root, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(outDir, "pi-stack")
	writeFile(t, target, "binary")
	link := filepath.Join(bin, "pi-stack")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	// A real (non-symlink) file at the pi-stack-host slot must be left alone.
	notOurs := filepath.Join(bin, "pi-stack-host")
	writeFile(t, notOurs, "hand-placed")

	rio := setupIO{in: strings.NewReader(""), out: &bytes.Buffer{}, isTTY: false}
	err := runUninstallCore(resetCfg(), p, []string{link, notOurs}, resetOpts{assumeYes: true},
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
	link := filepath.Join(root, "pi-stack")
	rio := setupIO{in: strings.NewReader(""), out: &bytes.Buffer{}, isTTY: false}

	err := runUninstallCore(resetCfg(), p, []string{link}, resetOpts{}, defaultResetFS(), noToolEnv(), rio, fixedNow)
	if !errors.Is(err, errResetNeedsYes) {
		t.Fatalf("want errResetNeedsYes, got %v", err)
	}
	if !exists(p.configDir) {
		t.Error("a refused uninstall must not move anything")
	}
}

// TestParseResetArgs: --sbx is reset-only; uninstall rejects it. Help + unknown.
func TestParseResetArgs(t *testing.T) {
	if o, err := parseResetArgs([]string{"--keep-memory", "--sbx", "--yes"}, true); err != nil ||
		!o.keepMemory || !o.sbx || !o.assumeYes {
		t.Fatalf("reset flags: %+v err=%v", o, err)
	}
	if _, err := parseResetArgs([]string{"--sbx"}, false); err == nil {
		t.Error("uninstall must reject --sbx")
	}
	if o, err := parseResetArgs([]string{"-h"}, true); err != nil || !o.help {
		t.Errorf("help: %+v err=%v", o, err)
	}
	if _, err := parseResetArgs([]string{"--nope"}, true); err == nil {
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
	target := filepath.Join(root, "out", "pi-stack")
	if err := os.WriteFile(target, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(bin, "pi-stack")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	fsys := defaultResetFS()
	fsys.rename = func(_, _ string) error { return errors.New("backup failed") }

	rio := setupIO{in: strings.NewReader(""), out: &bytes.Buffer{}, isTTY: false}
	err := runUninstallCore(resetCfg(), p, []string{link}, resetOpts{assumeYes: true},
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

	env := fakeEnv{present: map[string]bool{}, ports: map[int]bool{memoryPortDefault: true}}.env()
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
	dataRoot := filepath.Join(root, ".pi-stack")
	customMem := filepath.Join(dataRoot, "facts-store") // NOT named "memory"
	kbDir := filepath.Join(dataRoot, "knowledge")
	cache := filepath.Join(dataRoot, "cache")
	for _, d := range []string{customMem, kbDir, cache} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(customMem, "memory.db"), "facts")
	// The .memory.lock flock file sits inside the memory subdir; it must ride along
	// with the preserved subdir, never be swept aside (R4-1).
	writeFile(t, filepath.Join(customMem, ".memory.lock"), "")
	p := resetPaths{configDir: filepath.Join(root, "config"), dataRoot: dataRoot, memoryDir: customMem, knowledgeDir: kbDir}
	a := resetPlan(resetCfg(), p, resetOpts{keepMemory: true})

	var buf bytes.Buffer
	if _, err := executeReset(a, defaultResetFS(), noToolEnv(), &buf, fixedNow); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exists(customMem) || !exists(filepath.Join(customMem, "memory.db")) {
		t.Error("--keep-memory must preserve the resolved custom memory dir")
	}
	if !exists(filepath.Join(customMem, ".memory.lock")) {
		t.Error("--keep-memory must preserve the .memory.lock inside the memory subdir")
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
	dataRoot := filepath.Join(root, ".pi-stack")
	if err := os.MkdirAll(dataRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	// A custom MEMORY_DB loose in the data root, with a -wal sidecar and the
	// sibling .memory.lock flock file reset holds during the sweep (R4-1).
	memDB := filepath.Join(dataRoot, "custom-memory.db")
	writeFile(t, memDB, "facts")
	writeFile(t, memDB+"-wal", "wal")
	lockFile := filepath.Join(dataRoot, ".memory.lock")
	writeFile(t, lockFile, "")
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
	// The sibling .memory.lock is preserved IN PLACE — never swept aside — so the
	// flock reset holds on that inode stays the canonical-path authority (R4-1).
	if !exists(lockFile) {
		t.Error("the sibling .memory.lock must be preserved in place")
	}
	if exists(lockFile + ".bak-" + fixedTS) {
		t.Error("the sibling .memory.lock must NOT have been moved aside")
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
// BASENAME is not EXACTLY pi-stack/pi-stack-host are left in place + reported —
// both a genuinely unrelated target AND a DECEPTIVE one whose path merely
// CONTAINS "pi-stack" (a substring match would wrongly delete it).
func TestUninstall_LeavesUnrelatedSymlinks(t *testing.T) {
	stubStopServe(t)
	root := t.TempDir()
	p := tempPaths(t, root)

	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// A genuinely unrelated target, and a DECEPTIVE one (path contains "pi-stack"
	// but its basename does NOT match our binaries).
	other := filepath.Join(root, "opt", "other-tool")
	deceptive := filepath.Join(root, "opt", "pi-stack-ish-wrapper")
	for _, tgt := range []string{other, deceptive} {
		if err := os.MkdirAll(filepath.Dir(tgt), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tgt, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	linkA := filepath.Join(bin, "pi-stack")      // -> /opt/other-tool
	linkB := filepath.Join(bin, "pi-stack-host") // -> /opt/pi-stack-ish-wrapper
	if err := os.Symlink(other, linkA); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(deceptive, linkB); err != nil {
		t.Fatal(err)
	}

	rio := setupIO{in: strings.NewReader(""), out: &bytes.Buffer{}, isTTY: false}
	if err := runUninstallCore(resetCfg(), p, []string{linkA, linkB}, resetOpts{assumeYes: true},
		defaultResetFS(), noToolEnv(), rio, fixedNow); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exists(linkA) {
		t.Error("uninstall must NOT remove an unrelated symlink (/opt/other-tool)")
	}
	if !exists(linkB) {
		t.Error("uninstall must NOT remove a deceptive substring symlink (/opt/pi-stack-ish-wrapper)")
	}
	out := rio.out.(*bytes.Buffer).String()
	if !strings.Contains(out, "not a pi-stack binary") {
		t.Errorf("want a 'left in place' report, got %q", out)
	}
}

// TestExecuteReset_CustomDBOutsideRootMovesFileOnly: a custom MEMORY_DB pointing
// at a file OUTSIDE the data root moves ONLY that file (+ its -wal sidecar),
// NEVER the parent dir or an unrelated sibling in it.
func TestExecuteReset_CustomDBOutsideRootMovesFileOnly(t *testing.T) {
	stubStopServe(t)
	root := t.TempDir()
	dataRoot := filepath.Join(root, ".pi-stack")
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
	dataRoot := filepath.Join(root, ".pi-stack")
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

	env := fakeEnv{present: map[string]bool{}, ports: map[int]bool{knowledgePortDefault: true}}.env()
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

// TestResetPlan_XDGBasesMovedAside: a default reset moves aside the new DATA and
// STATE bases too (not just the legacy ~/.pi-stack), so the XDG layout is swept.
func TestResetPlan_XDGBasesMovedAside(t *testing.T) {
	p := resetPaths{
		configDir: "/c", dataRoot: "/legacy", dataDir: "/data", stateDir: "/state",
		memoryDir: "/legacy/memory", knowledgeDir: "/legacy/knowledge",
	}
	full := resetPlan(resetCfg(), p, resetOpts{})
	for _, want := range []string{"/legacy", "/data", "/state"} {
		if !backupHas(full.Backups, want) {
			t.Errorf("default reset must move aside %s", want)
		}
	}
}

// TestResetPlan_KeepMemorySweepsBothRoots: --keep-memory sweeps BOTH the legacy
// data root and the new DATA base (each preserving memory), and moves STATE aside
// wholesale.
func TestResetPlan_KeepMemorySweepsBothRoots(t *testing.T) {
	p := resetPaths{
		configDir: "/c", dataRoot: "/legacy", dataDir: "/data", stateDir: "/state",
		memoryDir: "/legacy/memory", knowledgeDir: "/legacy/knowledge",
	}
	keep := resetPlan(resetCfg(), p, resetOpts{keepMemory: true})
	if !backupHas(keep.Backups, "/state") {
		t.Error("--keep-memory must move the STATE base aside wholesale")
	}
	if backupHas(keep.Backups, "/data") || backupHas(keep.Backups, "/legacy") {
		t.Error("--keep-memory must SWEEP the data roots, not move them wholesale (memory lives there)")
	}
	roots := map[string]bool{}
	for _, sr := range keep.SweepRoots {
		roots[sr.Root] = true
	}
	if !roots["/legacy"] || !roots["/data"] {
		t.Errorf("SweepRoots = %v, want both /legacy and /data", keep.SweepRoots)
	}
}

// TestExecuteReset_KeepMemoryPreservesEveryAuthority (finding 10): --keep-memory
// preserves memory in the legacy root, the new DATA base, AND a memory.pre-xdg-*
// safety copy, while sweeping knowledge + backups in each.
func TestExecuteReset_KeepMemoryPreservesEveryAuthority(t *testing.T) {
	stubStopServe(t)
	root := t.TempDir()
	legacy := filepath.Join(root, ".pi-stack")
	data := filepath.Join(root, ".local", "share", "pi-stack")
	state := filepath.Join(root, ".local", "state", "pi-stack")
	legMem := filepath.Join(legacy, "memory")
	legSafety := filepath.Join(legacy, "memory.pre-xdg-1699999999")
	legKb := filepath.Join(legacy, "knowledge")
	dataMem := filepath.Join(data, "memory")
	dataBackups := filepath.Join(data, "backups")
	for _, d := range []string{legMem, legSafety, legKb, dataMem, dataBackups, state} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(legMem, "memory.db"), "legacy facts")
	writeFile(t, filepath.Join(legSafety, "memory.db"), "safety facts")
	writeFile(t, filepath.Join(dataMem, "memory.db"), "new facts")
	writeFile(t, filepath.Join(dataBackups, "b.tar.gz"), "backup")

	p := resetPaths{
		configDir: filepath.Join(root, "config"), dataRoot: legacy, dataDir: data, stateDir: state,
		memoryDir: legMem, knowledgeDir: legKb,
	}
	a := resetPlan(resetCfg(), p, resetOpts{keepMemory: true})

	var buf bytes.Buffer
	if _, err := executeReset(a, defaultResetFS(), noToolEnv(), &buf, fixedNow); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Every memory authority survives.
	for _, keep := range []string{
		filepath.Join(legMem, "memory.db"),
		filepath.Join(legSafety, "memory.db"),
		filepath.Join(dataMem, "memory.db"),
	} {
		if !exists(keep) {
			t.Errorf("memory authority must be preserved: %s", keep)
		}
	}
	// Knowledge (legacy) + backups (new DATA) are swept aside.
	if exists(legKb) {
		t.Error("legacy knowledge must be swept aside")
	}
	if exists(dataBackups) {
		t.Error("new DATA backups must be swept aside under --keep-memory")
	}
	// STATE moved wholesale.
	if exists(state) {
		t.Error("STATE base must be moved aside")
	}
	if !exists(state + ".bak-" + fixedTS) {
		t.Error("STATE .bak backup missing")
	}
}

// TestExecuteReset_MemoryFlockBlocksDataMove (LOW-2): when the memory flock
// cannot be ACQUIRED (a running serve holds it), reset refuses the dangerous
// data move even when the port probe is down.
func TestExecuteReset_MemoryFlockBlocksDataMove(t *testing.T) {
	stubStopServe(t)
	root := t.TempDir()
	p := tempPaths(t, root)
	a := resetPlan(resetCfg(), p, resetOpts{})

	env := noToolEnv()
	env.memLockAcquire = func() (func(), bool) { return nil, false } // held => refuse
	var buf bytes.Buffer
	_, err := executeReset(a, defaultResetFS(), env, &buf, fixedNow)
	if err == nil {
		t.Fatal("a held memory flock must block the dangerous data move")
	}
	if exists(p.dataRoot + ".bak-" + fixedTS) {
		t.Error("the data dir must NOT move while the memory flock is held")
	}
	if exists(p.configDir) {
		t.Error("the config dir (safe) should still be backed up")
	}
	// --force overrides the flock guard (acquire is never even attempted).
	aForce := resetPlan(resetCfg(), tempPaths(t, t.TempDir()), resetOpts{force: true})
	var buf2 bytes.Buffer
	env2 := noToolEnv()
	acquireCalled := false
	env2.memLockAcquire = func() (func(), bool) { acquireCalled = true; return nil, false }
	if _, err := executeReset(aForce, defaultResetFS(), env2, &buf2, fixedNow); err != nil {
		t.Fatalf("--force must override the flock guard: %v", err)
	}
	if acquireCalled {
		t.Error("--force must NOT even attempt to acquire the memory flock")
	}
}

// TestExecuteReset_MemoryFlockHeldAcrossMove (LOW-2): reset ACQUIRES the memory
// flock before the data move and RELEASES it only after every move completes, so
// a serve cannot start mid-move.
func TestExecuteReset_MemoryFlockHeldAcrossMove(t *testing.T) {
	stubStopServe(t)
	root := t.TempDir()
	p := tempPaths(t, root)
	a := resetPlan(resetCfg(), p, resetOpts{})

	env := noToolEnv()
	acquired, releasedAt, moveCount := false, -1, 0
	// The fs rename records the order of moves so we can prove release ran LAST.
	fsys := defaultResetFS()
	baseRename := fsys.rename
	fsys.rename = func(oldpath, newpath string) error {
		if err := baseRename(oldpath, newpath); err != nil {
			return err
		}
		moveCount++
		return nil
	}
	env.memLockAcquire = func() (func(), bool) {
		acquired = true
		return func() { releasedAt = moveCount }, true
	}
	var buf bytes.Buffer
	if _, err := executeReset(a, fsys, env, &buf, fixedNow); err != nil {
		t.Fatalf("executeReset: %v", err)
	}
	if !acquired {
		t.Fatal("reset must ACQUIRE the memory flock before the data move")
	}
	if releasedAt == -1 {
		t.Fatal("reset must RELEASE the memory flock after the data move")
	}
	if releasedAt != moveCount || moveCount == 0 {
		t.Fatalf("flock released after %d moves, but %d moves ran — must release LAST", releasedAt, moveCount)
	}
	if !exists(p.dataRoot + ".bak-" + fixedTS) {
		t.Error("the data dir should have moved aside while the flock was held")
	}
}

// TestReset_CustomMemoryDBUnderConfigProtectedOnFlockFail (R2-1): a custom
// MEMORY_DB living UNDER the config dir must be protected by the fail-closed
// flock-acquire refusal. resetPlan classifies the config-dir target Dangerous
// because it CONTAINS the memory authority, so executeReset SKIPS it when the
// flock cannot be acquired — the config dir (and the memory db inside it) is NOT
// moved out from under a live sqlite writer.
func TestReset_CustomMemoryDBUnderConfigProtectedOnFlockFail(t *testing.T) {
	stubStopServe(t)
	root := t.TempDir()
	configDir := filepath.Join(root, ".config", "pi-stack")
	memDir := filepath.Join(configDir, "memory")
	memoryDB := filepath.Join(memDir, "memory.db")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(configDir, "config.toml"), "x")
	writeFile(t, memoryDB, "facts")
	p := resetPaths{
		configDir: configDir,
		dataRoot:  filepath.Join(root, ".pi-stack"),
		dataDir:   filepath.Join(root, ".local", "share", "pi-stack"),
		stateDir:  filepath.Join(root, ".local", "state", "pi-stack"),
		memoryDir: memDir,
		memoryDB:  memoryDB,
	}

	// A DEFAULT reset (no --keep-memory) would move the config dir wholesale. The
	// plan must mark it Dangerous because it holds the memory authority.
	a := resetPlan(resetCfg(), p, resetOpts{})
	if !backupDangerous(a.Backups, configDir) {
		t.Fatal("a config dir that CONTAINS the memory authority must be classified Dangerous")
	}

	env := noToolEnv()
	env.memLockAcquire = func() (func(), bool) { return nil, false } // held/failed => refuse
	var buf bytes.Buffer
	_, err := executeReset(a, defaultResetFS(), env, &buf, fixedNow)
	if err == nil {
		t.Fatal("a failed flock acquire must refuse moving the memory-bearing config dir")
	}
	if !exists(configDir) || !exists(memoryDB) {
		t.Error("the config dir (and the custom MEMORY_DB inside it) must NOT move while the flock is held")
	}
	if exists(configDir + ".bak-" + fixedTS) {
		t.Error("the config dir must NOT have been backed up under a held flock")
	}
}

// TestExecuteReset_KeepMemoryCustomDBUnderXDGBase (R1-2): a custom MEMORY_DB that
// resolves UNDER any of the XDG bases (DATA, STATE, CONFIG) is preserved by
// --keep-memory, while the regenerable siblings in that base are swept aside.
func TestExecuteReset_KeepMemoryCustomDBUnderXDGBase(t *testing.T) {
	cases := []struct {
		name string
		// base selects which XDG base holds the custom memory db.
		base func(root string) (baseDir string, p resetPaths)
	}{
		{
			name: "DATA",
			base: func(root string) (string, resetPaths) {
				dataDir := filepath.Join(root, ".local", "share", "pi-stack")
				mem := filepath.Join(dataDir, "facts-store")
				return dataDir, resetPaths{
					configDir: filepath.Join(root, ".config", "pi-stack"),
					dataRoot:  filepath.Join(root, ".pi-stack"),
					dataDir:   dataDir,
					stateDir:  filepath.Join(root, ".local", "state", "pi-stack"),
					memoryDir: mem, memoryDB: filepath.Join(mem, "memory.db"),
				}
			},
		},
		{
			name: "STATE",
			base: func(root string) (string, resetPaths) {
				stateDir := filepath.Join(root, ".local", "state", "pi-stack")
				mem := filepath.Join(stateDir, "memory")
				return stateDir, resetPaths{
					configDir: filepath.Join(root, ".config", "pi-stack"),
					dataRoot:  filepath.Join(root, ".pi-stack"),
					dataDir:   filepath.Join(root, ".local", "share", "pi-stack"),
					stateDir:  stateDir,
					memoryDir: mem, memoryDB: filepath.Join(mem, "memory.db"),
				}
			},
		},
		{
			name: "CONFIG",
			base: func(root string) (string, resetPaths) {
				configDir := filepath.Join(root, ".config", "pi-stack")
				mem := filepath.Join(configDir, "memory")
				return configDir, resetPaths{
					configDir: configDir,
					dataRoot:  filepath.Join(root, ".pi-stack"),
					dataDir:   filepath.Join(root, ".local", "share", "pi-stack"),
					stateDir:  filepath.Join(root, ".local", "state", "pi-stack"),
					memoryDir: mem, memoryDB: filepath.Join(mem, "memory.db"),
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubStopServe(t)
			root := t.TempDir()
			baseDir, p := tc.base(root)
			if err := os.MkdirAll(p.memoryDir, 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, p.memoryDB, "facts")
			// A regenerable sibling in the SAME base that must be swept aside.
			sibling := filepath.Join(baseDir, "regenerable")
			if err := os.MkdirAll(sibling, 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(sibling, "x"), "junk")

			a := resetPlan(resetCfg(), p, resetOpts{keepMemory: true})
			// The base holding the memory authority must NOT be a wholesale backup target.
			if backupHas(a.Backups, baseDir) {
				t.Fatalf("%s base holds a custom MEMORY_DB; it must be swept, not moved wholesale", tc.name)
			}
			var buf bytes.Buffer
			if _, err := executeReset(a, defaultResetFS(), noToolEnv(), &buf, fixedNow); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !exists(p.memoryDB) {
				t.Errorf("custom MEMORY_DB under %s must be preserved by --keep-memory", tc.name)
			}
			if exists(sibling) {
				t.Errorf("a regenerable sibling under %s must be swept aside", tc.name)
			}
			if !exists(sibling + ".bak-" + fixedTS) {
				t.Errorf("the swept sibling .bak backup is missing under %s", tc.name)
			}
		})
	}
}

// TestExecuteReset_KeepMemoryCustomDBUnderLegacyStray (R3-1): a custom MEMORY_DB
// nested UNDER a legacy config-sibling stray must be PRESERVED by --keep-memory on
// a SUCCESSFUL flock, while the rest of that stray IS still swept aside (sweep,
// not the wholesale move that would drag the live db with it).
func TestExecuteReset_KeepMemoryCustomDBUnderLegacyStray(t *testing.T) {
	stubStopServe(t)
	root := t.TempDir()
	legacyConfig := filepath.Join(root, ".config", "pi-stack")
	strayKb := filepath.Join(legacyConfig, "knowledge") // a legacy stray dir
	mem := filepath.Join(strayKb, "memory")             // MEMORY_DB nested UNDER the stray
	memoryDB := filepath.Join(mem, "memory.db")
	if err := os.MkdirAll(mem, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, memoryDB, "facts")
	// A regenerable sibling inside the SAME stray that MUST be swept aside.
	sibling := filepath.Join(strayKb, "index")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sibling, "x"), "junk")

	p := resetPaths{
		configDir: filepath.Join(root, "custom-config"), // moved elsewhere, so strays exist
		dataRoot:  filepath.Join(root, ".pi-stack"),
		dataDir:   filepath.Join(root, ".local", "share", "pi-stack"),
		stateDir:  filepath.Join(root, ".local", "state", "pi-stack"),
		memoryDir: mem,
		memoryDB:  memoryDB,
		legacyStrays: []backupTarget{
			{Path: strayKb, Label: "legacy knowledge bundle"},
			{Path: filepath.Join(legacyConfig, "knowledge-cache"), Label: "legacy knowledge cache"},
			{Path: filepath.Join(legacyConfig, "serve.pid"), Label: "legacy serve pidfile"},
		},
	}
	a := resetPlan(resetCfg(), p, resetOpts{keepMemory: true})
	// The stray holding the authority must NOT be a wholesale backup target.
	if backupHas(a.Backups, strayKb) {
		t.Fatal("a legacyStray holding the memory authority must be swept, not moved wholesale")
	}

	var buf bytes.Buffer
	if _, err := executeReset(a, defaultResetFS(), noToolEnv(), &buf, fixedNow); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exists(memoryDB) {
		t.Error("MEMORY_DB nested under a legacyStray must be preserved by --keep-memory on flock success")
	}
	if exists(sibling) {
		t.Error("a regenerable sibling in the stray must be swept aside")
	}
	if !exists(sibling + ".bak-" + fixedTS) {
		t.Error("the swept sibling .bak backup is missing")
	}
}

// TestExecuteReset_KeepMemoryCustomDBUnderKnowledgeDir (R3-1): a custom MEMORY_DB
// nested UNDER paths.knowledgeDir must be PRESERVED by --keep-memory on a
// SUCCESSFUL flock, while the knowledge index sibling IS still swept aside
// (sweep, not the wholesale move that would drag the live db with it).
func TestExecuteReset_KeepMemoryCustomDBUnderKnowledgeDir(t *testing.T) {
	stubStopServe(t)
	root := t.TempDir()
	dataRoot := filepath.Join(root, ".pi-stack")
	kbDir := filepath.Join(dataRoot, "knowledge")
	mem := filepath.Join(kbDir, "memory") // MEMORY_DB nested UNDER the knowledge dir
	memoryDB := filepath.Join(mem, "memory.db")
	if err := os.MkdirAll(mem, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, memoryDB, "facts")
	// The regenerable index sibling in the SAME dir that MUST be swept aside.
	kbIndex := filepath.Join(kbDir, "knowledge.db")
	writeFile(t, kbIndex, "index")

	p := resetPaths{
		configDir:    filepath.Join(root, "config"),
		dataRoot:     dataRoot,
		dataDir:      filepath.Join(root, ".local", "share", "pi-stack"),
		stateDir:     filepath.Join(root, ".local", "state", "pi-stack"),
		memoryDir:    mem,
		memoryDB:     memoryDB,
		knowledgeDir: kbDir,
	}
	a := resetPlan(resetCfg(), p, resetOpts{keepMemory: true})
	// The knowledge dir holding the authority must NOT be a wholesale backup target.
	if backupHas(a.Backups, kbDir) {
		t.Fatal("knowledgeDir holding the memory authority must be swept, not moved wholesale")
	}

	var buf bytes.Buffer
	if _, err := executeReset(a, defaultResetFS(), noToolEnv(), &buf, fixedNow); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exists(memoryDB) {
		t.Error("MEMORY_DB nested under knowledgeDir must be preserved by --keep-memory on flock success")
	}
	if exists(kbIndex) {
		t.Error("the knowledge index sibling must be swept aside")
	}
	if !exists(kbIndex + ".bak-" + fixedTS) {
		t.Error("the swept knowledge index .bak backup is missing")
	}
}

// TestExecuteReset_KeepMemoryCustomDBUnderConfigDirFlockSuccess (R3-1): a custom
// MEMORY_DB nested UNDER the config dir must be PRESERVED by --keep-memory on a
// SUCCESSFUL flock, while a regenerable config-dir sibling IS still swept aside
// (sweep, not the wholesale move that would drag the live db with it).
func TestExecuteReset_KeepMemoryCustomDBUnderConfigDirFlockSuccess(t *testing.T) {
	stubStopServe(t)
	root := t.TempDir()
	configDir := filepath.Join(root, ".config", "pi-stack")
	mem := filepath.Join(configDir, "memory") // MEMORY_DB nested UNDER the config dir
	memoryDB := filepath.Join(mem, "memory.db")
	if err := os.MkdirAll(mem, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, memoryDB, "facts")
	writeFile(t, filepath.Join(configDir, "config.toml"), "x")
	// A regenerable sibling in the config dir that MUST be swept aside.
	sibling := filepath.Join(configDir, "knowledge-cache")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sibling, "x"), "junk")

	p := resetPaths{
		configDir: configDir,
		dataRoot:  filepath.Join(root, ".pi-stack"),
		dataDir:   filepath.Join(root, ".local", "share", "pi-stack"),
		stateDir:  filepath.Join(root, ".local", "state", "pi-stack"),
		memoryDir: mem,
		memoryDB:  memoryDB,
	}
	a := resetPlan(resetCfg(), p, resetOpts{keepMemory: true})
	// The config dir holding the authority must NOT be a wholesale backup target.
	if backupHas(a.Backups, configDir) {
		t.Fatal("config dir holding the memory authority must be swept, not moved wholesale")
	}

	var buf bytes.Buffer
	if _, err := executeReset(a, defaultResetFS(), noToolEnv(), &buf, fixedNow); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exists(memoryDB) {
		t.Error("MEMORY_DB nested under the config dir must be preserved by --keep-memory on flock success")
	}
	if exists(sibling) {
		t.Error("a regenerable config-dir sibling must be swept aside")
	}
	if !exists(sibling + ".bak-" + fixedTS) {
		t.Error("the swept config-dir sibling .bak backup is missing")
	}
}

// TestAcquireMemoryFlock_ErrorRefusesDataMove (R1-3): acquireMemoryFlock FAILS
// CLOSED — an acquisition error (here a mkdir failure, because the lock dir's
// parent is a regular file) returns ok=false, and that refusal blocks the
// dangerous data move (no --force). The old code fail-OPENed on such errors and
// moved the data without the lock.
func TestAcquireMemoryFlock_ErrorRefusesDataMove(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "blocker")
	writeFile(t, blocker, "x") // a FILE where a dir would need to be created
	t.Setenv("MEMORY_DB", filepath.Join(blocker, "sub", "memory.db"))

	rel, ok := acquireMemoryFlock()
	if ok {
		t.Fatal("an acquire error must refuse (ok=false), not fail open")
	}
	if rel != nil {
		t.Error("no release func on a refused acquire")
	}

	stubStopServe(t)
	p := tempPaths(t, root)
	a := resetPlan(resetCfg(), p, resetOpts{})
	env := noToolEnv()
	env.memLockAcquire = acquireMemoryFlock // real; errors due to the bad MEMORY_DB path
	var buf bytes.Buffer
	_, err := executeReset(a, defaultResetFS(), env, &buf, fixedNow)
	if err == nil {
		t.Fatal("an acquire error must block the dangerous data move")
	}
	if exists(p.dataRoot + ".bak-" + fixedTS) {
		t.Error("the data dir must NOT move when the flock cannot be acquired")
	}
	if !exists(p.configDir + ".bak-" + fixedTS) {
		t.Error("the safe config dir should still be backed up")
	}
}

// noToolEnv is a shellEnv with no sbx on PATH, so the executor degrades (prints
// commands) without touching the host. The serve-stop no longer probes PATH (it
// is pidfile-based via stopServe), so this env exercises the sbx path only.
func noToolEnv() shellEnv {
	return fakeEnv{present: map[string]bool{}, output: map[string]string{}}.env()
}

// TestResolveResetPaths_RelativeMemoryDBAbsolute gates round-6: a relative
// MEMORY_DB must be normalized to an absolute path at resolution so the
// --keep-memory preserve set matches the sweep's absolute entries.
func TestResolveResetPaths_RelativeMemoryDBAbsolute(t *testing.T) {
	t.Chdir(t.TempDir())
	env := shellEnv{
		homeDir: func() string { return "/home/fake" },
		getenv: func(k string) string {
			if k == "MEMORY_DB" {
				return ".pi-stack/custom-memory.db"
			}
			return ""
		},
	}
	p := resolveResetPaths(env)
	if !filepath.IsAbs(p.memoryDB) {
		t.Errorf("memoryDB = %q, want absolute", p.memoryDB)
	}
	if !filepath.IsAbs(p.memoryDir) {
		t.Errorf("memoryDir = %q, want absolute", p.memoryDir)
	}
}
