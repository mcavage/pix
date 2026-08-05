package reset

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/rpc"
)

// fixedNow returns a stable timestamp so .bak suffixes are predictable in tests.
func fixedNow() time.Time { return time.Unix(1700000000, 0) }

const fixedTS = "1700000000"

// resetCfg is a minimal config with a couple of MCP servers, for the --sbx plan.
func resetCfg() *config.Config {
	return &config.Config{MCP: []string{config.GWServerName, "slack"}}
}

// tempPaths lays out a fake config + data tree under root and returns the
// Paths pointing at it. It seeds a file in each so a move is observable.
func tempPaths(t *testing.T, root string) Paths {
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
	return Paths{ConfigDir: configDir, DataRoot: dataRoot, MemoryDir: memDir, knowledgeDir: kbDir}
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
	p := Paths{ConfigDir: "/c", DataRoot: "/d", MemoryDir: "/d/memory", knowledgeDir: "/d/knowledge"}

	keep := Plan(resetCfg(), p, Opts{keepMemory: true})
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

	full := Plan(resetCfg(), p, Opts{})
	if !backupHas(full.Backups, "/d") {
		t.Error("default reset must back up the whole data root (memory included)")
	}
}

// TestResetPlan_Sbx: --sbx includes the sandbox + mcp actions; without it, none.
func TestResetPlan_Sbx(t *testing.T) {
	p := Paths{ConfigDir: "/c", DataRoot: "/d", MemoryDir: "/d/memory", knowledgeDir: "/d/knowledge"}

	with := Plan(resetCfg(), p, Opts{sbx: true})
	if !with.RemoveSandboxes {
		t.Error("--sbx must set RemoveSandboxes")
	}
	if strings.Join(with.MCPRemove, ",") != config.GWServerName+",slack" {
		t.Errorf("MCPRemove = %v, want [%s slack]", with.MCPRemove, config.GWServerName)
	}

	without := Plan(resetCfg(), p, Opts{})
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
	a := Plan(resetCfg(), p, Opts{})

	var buf bytes.Buffer
	if _, err := executeReset(a, DefaultResetFS(), noToolEnv(), &buf, fixedNow); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !*called {
		t.Error("executeReset must stop host services via service.Stop")
	}
	if exists(p.ConfigDir) {
		t.Error("config dir should have been moved aside")
	}
	if exists(p.DataRoot) {
		t.Error("data root should have been moved aside")
	}
	if !exists(p.ConfigDir + ".bak-" + fixedTS) {
		t.Error("config .bak backup missing")
	}
	if !exists(p.DataRoot + ".bak-" + fixedTS) {
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
	writeFile(t, filepath.Join(p.ConfigDir, "op-refs.env"), "ANTHROPIC_API_KEY=op://vault/item/field\n")
	writeFile(t, filepath.Join(p.ConfigDir, "hostmode.env"), "OPENAI_API_KEY=op://vault/item2/field\n")
	a := Plan(resetCfg(), p, Opts{})
	a.PreserveRefs = true // exercise the lower-level optional preservation mechanism

	var buf bytes.Buffer
	if _, err := executeReset(a, DefaultResetFS(), noToolEnv(), &buf, fixedNow); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The old config dir moved aside as usual — config.toml is still in the .bak.
	if !exists(p.ConfigDir + ".bak-" + fixedTS + string(filepath.Separator) + "config.toml") {
		t.Error("config.toml should still be in the .bak")
	}

	// A FRESH config dir exists with just the ref files, not config.toml.
	if !exists(p.ConfigDir) {
		t.Fatal("a fresh config dir should have been recreated to hold the preserved refs")
	}
	if exists(filepath.Join(p.ConfigDir, "config.toml")) {
		t.Error("config.toml must NOT come back into the fresh config dir")
	}
	gotOP, err := os.ReadFile(filepath.Join(p.ConfigDir, "op-refs.env"))
	if err != nil {
		t.Fatalf("op-refs.env missing from fresh config dir: %v", err)
	}
	if string(gotOP) != "ANTHROPIC_API_KEY=op://vault/item/field\n" {
		t.Errorf("op-refs.env content mismatch, got %q", gotOP)
	}
	gotHM, err := os.ReadFile(filepath.Join(p.ConfigDir, "hostmode.env"))
	if err != nil {
		t.Fatalf("hostmode.env missing from fresh config dir: %v", err)
	}
	if string(gotHM) != "OPENAI_API_KEY=op://vault/item2/field\n" {
		t.Errorf("hostmode.env content mismatch, got %q", gotHM)
	}

	// And the same refs are STILL present in the .bak (additive, not a move).
	bakOP, err := os.ReadFile(p.ConfigDir + ".bak-" + fixedTS + string(filepath.Separator) + "op-refs.env")
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
	a := Plan(resetCfg(), p, Opts{})
	a.PreserveRefs = true

	var buf bytes.Buffer
	if _, err := executeReset(a, DefaultResetFS(), noToolEnv(), &buf, fixedNow); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if exists(p.ConfigDir) {
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
	writeFile(t, filepath.Join(p.ConfigDir, "op-refs.env"), "ANTHROPIC_API_KEY=op://vault/item/field\n")
	a := Plan(resetCfg(), p, Opts{})

	var buf bytes.Buffer
	if _, err := executeReset(a, DefaultResetFS(), noToolEnv(), &buf, fixedNow); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exists(p.ConfigDir) {
		t.Error("reset must NOT recreate the config dir to preserve refs")
	}
	if !exists(p.ConfigDir + ".bak-" + fixedTS + string(filepath.Separator) + "op-refs.env") {
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
	other := filepath.Join(p.DataRoot, "cache")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	a := Plan(resetCfg(), p, Opts{keepMemory: true})

	var buf bytes.Buffer
	if _, err := executeReset(a, DefaultResetFS(), noToolEnv(), &buf, fixedNow); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !exists(p.MemoryDir) {
		t.Error("--keep-memory must preserve the memory dir")
	}
	if !exists(filepath.Join(p.MemoryDir, "memory.db")) {
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

// TestRunResetCore_NonTTYNoYesRefuses: no TTY + no --yes => ErrResetNeedsYes and
// NOTHING is moved.
func TestRunResetCore_NonTTYNoYesRefuses(t *testing.T) {
	root := t.TempDir()
	p := tempPaths(t, root)
	rio := cli.IO{In: strings.NewReader(""), Out: &bytes.Buffer{}, IsTTY: false}

	err := RunCore(resetCfg(), p, Opts{}, DefaultResetFS(), noToolEnv(), rio, fixedNow)
	if !errors.Is(err, ErrResetNeedsYes) {
		t.Fatalf("want ErrResetNeedsYes, got %v", err)
	}
	if !exists(p.ConfigDir) || !exists(p.DataRoot) {
		t.Error("a refused reset must not move anything")
	}
}

// TestRunResetCore_YesExecutes: --yes runs without a prompt and moves state.
func TestRunResetCore_YesExecutes(t *testing.T) {
	stubStopServe(t)
	root := t.TempDir()
	p := tempPaths(t, root)
	rio := cli.IO{In: strings.NewReader(""), Out: &bytes.Buffer{}, IsTTY: false}

	if err := RunCore(resetCfg(), p, Opts{assumeYes: true}, DefaultResetFS(), noToolEnv(), rio, fixedNow); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exists(p.ConfigDir) {
		t.Error("--yes should have moved the config dir")
	}
}

// TestNewOpts: the flag SET is the root parser's (root.go's resetCmd); what
// stays here is that each flag reaches the field the plan reads.
func TestNewOpts(t *testing.T) {
	o := NewOpts(true, true, true, true)
	if !o.keepMemory || !o.sbx || !o.assumeYes || !o.force {
		t.Fatalf("NewOpts(all true) = %+v", o)
	}
	if z := NewOpts(false, false, false, false); z.keepMemory || z.sbx || z.assumeYes || z.force {
		t.Fatalf("NewOpts(all false) = %+v", z)
	}
}

// TestExecuteReset_MoveFailureReturnsError: a failing rename makes executeReset
// return an error, and RunCore propagates it (non-zero exit for the CLI).
func TestExecuteReset_MoveFailureReturnsError(t *testing.T) {
	stubStopServe(t)
	root := t.TempDir()
	p := tempPaths(t, root)
	a := Plan(resetCfg(), p, Opts{})

	fsys := DefaultResetFS()
	fsys.rename = func(_, _ string) error { return errors.New("disk full") }

	var buf bytes.Buffer
	_, err := executeReset(a, fsys, noToolEnv(), &buf, fixedNow)
	if err == nil {
		t.Fatal("a failed move must make executeReset return an error, not report success")
	}

	rio := cli.IO{In: strings.NewReader(""), Out: &bytes.Buffer{}, IsTTY: false}
	if rErr := RunCore(resetCfg(), p, Opts{assumeYes: true}, fsys, noToolEnv(), rio, fixedNow); rErr == nil {
		t.Error("RunCore must return non-nil when a move failed")
	}
}

// TestExecuteReset_ServeUpAbortsDataMove: when serve is still up (injected dial),
// the data dir move is refused (error) but the config dir is still backed up.
func TestExecuteReset_ServeUpAbortsDataMove(t *testing.T) {
	stubStopServe(t)
	root := t.TempDir()
	p := tempPaths(t, root)
	a := Plan(resetCfg(), p, Opts{})

	env := resetHost{ports: map[int]bool{rpc.MemoryPortDefault: true}}
	var buf bytes.Buffer
	_, err := executeReset(a, DefaultResetFS(), env, &buf, fixedNow)
	if err == nil {
		t.Fatal("serve still up must make executeReset return an error")
	}
	if exists(p.DataRoot + ".bak-" + fixedTS) {
		t.Error("the data dir must NOT move while serve is up")
	}
	if !exists(p.DataRoot) {
		t.Error("the data dir must be left in place when the move is blocked")
	}
	if exists(p.ConfigDir) {
		t.Error("the config dir (safe) should still be backed up")
	}

	// --force overrides the guard: the data dir moves even with serve up.
	aForce := Plan(resetCfg(), tempPaths(t, t.TempDir()), Opts{force: true})
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
	p := Paths{ConfigDir: filepath.Join(root, "config"), DataRoot: dataRoot, MemoryDir: customMem, knowledgeDir: kbDir}
	a := Plan(resetCfg(), p, Opts{keepMemory: true})

	var buf bytes.Buffer
	if _, err := executeReset(a, DefaultResetFS(), noToolEnv(), &buf, fixedNow); err != nil {
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

	p := Paths{
		ConfigDir:    filepath.Join(root, "config"),
		DataRoot:     dataRoot,
		MemoryDir:    dataRoot, // dir(MEMORY_DB) == dataRoot
		memoryDB:     memDB,    // the loose db file directly in the root
		knowledgeDir: siblingDir,
	}
	a := Plan(resetCfg(), p, Opts{keepMemory: true})

	var buf bytes.Buffer
	if _, err := executeReset(a, DefaultResetFS(), noToolEnv(), &buf, fixedNow); err != nil {
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

	dest, err := moveAside(DefaultResetFS(), src, 1700000000)
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

	p := Paths{
		ConfigDir: filepath.Join(root, "config"),
		DataRoot:  dataRoot,
		MemoryDir: docs,  // dir(MEMORY_DB)
		memoryDB:  memDB, // the custom file path
	}
	a := Plan(resetCfg(), p, Opts{})

	var buf bytes.Buffer
	if _, err := executeReset(a, DefaultResetFS(), noToolEnv(), &buf, fixedNow); err != nil {
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

	p := Paths{
		ConfigDir:    filepath.Join(root, "config"),
		DataRoot:     dataRoot,
		MemoryDir:    memoryDir,
		knowledgeDir: docs, // dir(KNOWLEDGE_DB)
		knowledgeDB:  kbDB, // the custom file path
	}
	a := Plan(resetCfg(), p, Opts{keepMemory: true})

	var buf bytes.Buffer
	if _, err := executeReset(a, DefaultResetFS(), noToolEnv(), &buf, fixedNow); err != nil {
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

// TestExecuteReset_KeepMemoryReadDirErrorSurfaces: a readDir failure during the
// keep-memory sweep must surface as a returned error, not be swallowed as
// success (never report preservation over a dir we could not even scan).
func TestExecuteReset_KeepMemoryReadDirErrorSurfaces(t *testing.T) {
	stubStopServe(t)
	root := t.TempDir()
	p := tempPaths(t, root)
	a := Plan(resetCfg(), p, Opts{keepMemory: true})

	fsys := DefaultResetFS()
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

// noToolEnv is a host with no sbx on PATH, so the executor degrades (prints
// commands) without touching the machine. The serve-stop no longer probes PATH
// (it is pidfile-based via service.Stop), so this exercises the sbx path only.
func noToolEnv() resetHost { return resetHost{} }

// TestResolveResetPaths_RelativeMemoryDBAbsolute gates round-6: a relative
// MEMORY_DB must be normalized to an absolute path at resolution so the
// --keep-memory preserve set matches the sweep's absolute entries.
func TestResolveResetPaths_RelativeMemoryDBAbsolute(t *testing.T) {
	t.Chdir(t.TempDir())
	env := resetHost{home: "/home/fake", envVars: map[string]string{"MEMORY_DB": ".pix/custom-memory.db"}}
	p := ResolveResetPaths(env)
	if !filepath.IsAbs(p.memoryDB) {
		t.Errorf("memoryDB = %q, want absolute", p.memoryDB)
	}
	if !filepath.IsAbs(p.MemoryDir) {
		t.Errorf("memoryDir = %q, want absolute", p.MemoryDir)
	}
}

// TestResolveResetPaths_DerivesFromInjectedEnvNotRealProcessEnv is the pure,
// no-filesystem half of the U11h safety gate: ResolveResetPaths must resolve
// ConfigDir/RuntimeFiles from the INJECTED sys.System, never by reaching past
// it into the real process environment (config.Path()/config.StateDir() read
// os.Getenv/os.UserHomeDir directly). This sets the REAL $HOME (via t.Setenv)
// to a canary tree and asserts the resolved paths land under the completely
// different injected host's home instead — proving the real env never leaks
// through. Fails before the fix (ConfigDir would resolve under realHome).
func TestResolveResetPaths_DerivesFromInjectedEnvNotRealProcessEnv(t *testing.T) {
	realHome := t.TempDir()
	t.Setenv("HOME", realHome)
	t.Setenv("PIX_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")

	injectedHome := t.TempDir()
	env := resetHost{home: injectedHome}
	p := ResolveResetPaths(env)

	wantConfigDir := filepath.Join(injectedHome, ".config", "pix")
	if p.ConfigDir != wantConfigDir {
		t.Errorf("ConfigDir = %q, want %q (derived from the injected host)", p.ConfigDir, wantConfigDir)
	}
	if strings.HasPrefix(p.ConfigDir, realHome) {
		t.Errorf("ConfigDir leaked the REAL process $HOME: %q", p.ConfigDir)
	}
	for _, rf := range p.RuntimeFiles {
		if strings.HasPrefix(rf, realHome) {
			t.Errorf("runtime file %q resolved under the REAL process $HOME, not the injected host", rf)
		}
		if !strings.HasPrefix(rf, injectedHome) {
			t.Errorf("runtime file %q did not resolve under the injected host %q", rf, injectedHome)
		}
	}
}

// TestResolveResetPaths_RealHomeCanarySurvivesReset is the full end-to-end
// U11h safety gate: it plants a canary config.toml at the path the REAL
// $HOME/$XDG_* env resolves to (simulating the operator's actual machine),
// injects a totally different temp-dir host, runs a real (--yes, real
// filesystem) reset against the paths ResolveResetPaths resolves for that
// injected host, and proves the REAL canary is never touched — only the
// injected tree moves aside. Before the fix, ResolveResetPaths.ConfigDir came
// from config.Path() (real os.Getenv/os.UserHomeDir), so this test would
// resolve the REAL canary directory and executeReset would move it aside;
// this test fails loudly in that world (the canary vanishes/gets a .bak).
func TestResolveResetPaths_RealHomeCanarySurvivesReset(t *testing.T) {
	stubStopServe(t)

	// The "real machine": every OS-level env var the OLD code path read
	// directly (config.Path()/config.StateDir()) now points at a canary tree.
	realHome := t.TempDir()
	t.Setenv("HOME", realHome)
	t.Setenv("PIX_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")

	realConfigDir := filepath.Join(realHome, ".config", "pix")
	if err := os.MkdirAll(realConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	canaryPath := filepath.Join(realConfigDir, "config.toml")
	writeFile(t, canaryPath, "REAL-CANARY-DO-NOT-TOUCH")

	realStatePid := filepath.Join(realHome, ".local", "state", "pix", "serve.pid")
	if err := os.MkdirAll(filepath.Dir(realStatePid), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, realStatePid, "REAL-PID-DO-NOT-TOUCH")

	// The INJECTED host: a completely different temp tree — the only one
	// reset is allowed to touch.
	injectedHome := t.TempDir()
	env := resetHost{home: injectedHome}

	p := ResolveResetPaths(env)
	if err := os.MkdirAll(p.ConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	injectedCanary := filepath.Join(p.ConfigDir, "config.toml")
	writeFile(t, injectedCanary, "injected")

	a := Plan(resetCfg(), p, Opts{})

	var buf bytes.Buffer
	if _, err := executeReset(a, DefaultResetFS(), env, &buf, fixedNow); err != nil {
		t.Fatalf("executeReset: %v", err)
	}

	// The injected config dir WAS moved aside (proves reset actually ran).
	if exists(p.ConfigDir) {
		t.Error("the injected config dir should have been moved aside")
	}
	if !exists(p.ConfigDir + ".bak-" + fixedTS) {
		t.Error("expected a .bak sibling for the injected config dir")
	}

	// The REAL canary is UNTOUCHED: present, unmoved, unmodified, no .bak sibling.
	data, err := os.ReadFile(canaryPath)
	if err != nil {
		t.Fatalf("real canary config.toml was removed/moved: %v", err)
	}
	if string(data) != "REAL-CANARY-DO-NOT-TOUCH" {
		t.Error("real canary config.toml content changed")
	}
	if exists(realConfigDir + ".bak-" + fixedTS) {
		t.Fatal("reset moved the REAL config dir aside — it must only touch the injected path")
	}
	if data, err := os.ReadFile(realStatePid); err != nil || string(data) != "REAL-PID-DO-NOT-TOUCH" {
		t.Fatalf("real serve.pid canary was touched: data=%q err=%v", data, err)
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
	probed := false
	env := dialUpThenDownHost{probed: &probed}

	a := Actions{RuntimeFiles: []string{pid, lock}}
	var buf bytes.Buffer
	if _, err := executeReset(a, DefaultResetFS(), env, &buf, fixedNow); err != nil {
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
	if _, err := executeReset(Actions{}, DefaultResetFS(), env, &buf, fixedNow); err != nil {
		t.Fatal(err)
	}
	if *restarted {
		t.Error("must not start a daemon that wasn't running before reset")
	}
}
