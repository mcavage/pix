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
	executeReset(a, defaultResetFS(), noToolEnv(), &buf, fixedNow)

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
	executeReset(a, defaultResetFS(), noToolEnv(), &buf, fixedNow)

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
	target := filepath.Join(root, "real-pi-stack")
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

// noToolEnv is a shellEnv with no sbx on PATH, so the executor degrades (prints
// commands) without touching the host. The serve-stop no longer probes PATH (it
// is pidfile-based via stopServe), so this env exercises the sbx path only.
func noToolEnv() shellEnv {
	return fakeEnv{present: map[string]bool{}, output: map[string]string{}}.env()
}
