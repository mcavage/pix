package reset

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pix/host/cli"
)

// ── the plan (pure) ─────────────────────────────────────────────────────────

// TestPlan_DefaultIsTheWholeStack: bare `pix reset` targets config, data,
// sandboxes and the configured MCP registrations. This is the shape the verb
// exists for — every flag on it only ever subtracts.
func TestPlan_DefaultIsTheWholeStack(t *testing.T) {
	s := newStack(t)
	a := plan(defaultCfg(), ResolvePaths(s.env), NewOpts(false, false, true, false))

	if got := labels(a); got != "config|data (memory, packs, context)" {
		t.Errorf("backup labels = %q", got)
	}
	if a.StateDir != s.stateDir {
		t.Errorf("StateDir = %q, want %q", a.StateDir, s.stateDir)
	}
	if !a.RemoveSandboxes {
		t.Error("a default reset must sweep sandboxes")
	}
	if len(a.MCPRemove) != 1 || a.MCPRemove[0] != "gog" {
		t.Errorf("MCPRemove = %v, want [gog]", a.MCPRemove)
	}
}

// TestPlan_KeepMemory_DropsTheDataRootTarget: --keep-memory must not put the
// whole data root on the move list; the executor's sweep handles it entry by
// entry so the memory store survives.
func TestPlan_KeepMemory_DropsTheDataRootTarget(t *testing.T) {
	s := newStack(t)
	a := plan(defaultCfg(), ResolvePaths(s.env), NewOpts(true, false, true, false))
	if got := labels(a); got != "config" {
		t.Errorf("backup labels = %q, want just config", got)
	}
	if !a.KeepMemory {
		t.Error("KeepMemory must reach the executor")
	}
}

// TestPlan_KeepSandboxes_LeavesSbxAlone: --keep-sandboxes drops BOTH sbx-facing
// actions, not just the sandbox one. Unregistering the MCP servers of a host
// whose sandboxes are still running would break them for no reason.
func TestPlan_KeepSandboxes_LeavesSbxAlone(t *testing.T) {
	s := newStack(t)
	a := plan(defaultCfg(), ResolvePaths(s.env), NewOpts(false, true, true, false))
	if a.RemoveSandboxes {
		t.Error("--keep-sandboxes must not sweep sandboxes")
	}
	if len(a.MCPRemove) != 0 {
		t.Errorf("MCPRemove = %v, want none", a.MCPRemove)
	}
}

// TestPlan_CustomMemoryDBOutsideDataRoot_MovesTheFileNotTheDir: a MEMORY_DB in
// the user's own directory is moved as a FILE (plus sidecars). Renaming that
// parent would take unrelated files with it.
func TestPlan_CustomMemoryDBOutsideDataRoot_MovesTheFileNotTheDir(t *testing.T) {
	s := newStack(t)
	db := filepath.Join(s.home, "notes", "mem.db")
	s.setenv("MEMORY_DB", db)

	a := plan(defaultCfg(), ResolvePaths(s.env), NewOpts(false, false, true, false))
	var found *backupTarget
	for i := range a.Backups {
		if a.Backups[i].Path == db {
			found = &a.Backups[i]
		}
	}
	if found == nil {
		t.Fatalf("custom MEMORY_DB %s is not a backup target: %v", db, a.Backups)
	}
	if !found.WithSidecars {
		t.Error("a db outside the data root must move as a file + its -wal/-shm sidecars, not as a directory")
	}
}

func labels(a actions) string {
	var out []string
	for _, b := range a.Backups {
		out = append(out, b.Label)
	}
	return strings.Join(out, "|")
}

// ── the whole verb ──────────────────────────────────────────────────────────

// TestRun_MovesEveryDirectoryAside is the headline behaviour: config, data and
// runtime state all leave the live path, all three land in a .bak- sibling, and
// the sandboxes are swept.
func TestRun_MovesEveryDirectoryAside(t *testing.T) {
	s := newStack(t)
	stubServeProbe(t, false, false)

	if err := s.run(t, NewOpts(false, false, true, false), false, ""); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, dir := range []string{s.configDir, s.dataRoot, s.stateDir} {
		if exists(dir) {
			t.Errorf("%s still exists after a reset", dir)
		}
		backupOf(t, dir) // fails unless exactly one .bak- sibling exists
	}
	if s.sweeps != 1 {
		t.Errorf("sandbox sweeps = %d, want 1", s.sweeps)
	}
}

// TestRun_TaskCheckoutSurvivesInsideTheBackup is the reason this verb renames
// instead of deleting: `<state>/tasks/<repo>/co/<name>` is a real git checkout
// that can carry uncommitted work. A reset relocates it; it never destroys it.
func TestRun_TaskCheckoutSurvivesInsideTheBackup(t *testing.T) {
	s := newStack(t)
	stubServeProbe(t, false, false)

	if err := s.run(t, NewOpts(false, false, true, false), false, ""); err != nil {
		t.Fatalf("Run: %v", err)
	}

	rel, err := filepath.Rel(s.stateDir, s.taskFile)
	if err != nil {
		t.Fatalf("rel: %v", err)
	}
	moved := filepath.Join(backupOf(t, s.stateDir), rel)
	if got := readFile(t, moved); got != "uncommitted work\n" {
		t.Errorf("task checkout content = %q, want it preserved verbatim", got)
	}
}

// TestReset_NeverDeletes is a SOURCE-level guard, and it is the cheapest way to
// keep the package's one safety property true: reset owns no delete. If a
// future change needs os.Remove/os.RemoveAll here, that is a design decision to
// argue for in review, not something to add and discover later from a bug
// report about a lost checkout.
func TestReset_NeverDeletes(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "os" {
				return true
			}
			if sel.Sel.Name == "Remove" || sel.Sel.Name == "RemoveAll" {
				t.Errorf("%s calls os.%s: reset moves state aside, it never deletes it", name, sel.Sel.Name)
			}
			return true
		})
	}
}

// TestRun_KeepMemory_PreservesTheStoreAndMovesTheRest: the memory dir stays put
// while packs and context go, and the config dir goes with them.
func TestRun_KeepMemory_PreservesTheStoreAndMovesTheRest(t *testing.T) {
	s := newStack(t)
	stubServeProbe(t, false, false)

	if err := s.run(t, NewOpts(true, false, true, false), false, ""); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !exists(filepath.Join(s.dataRoot, "memory", "memory.db")) {
		t.Error("--keep-memory must leave the memory store in place")
	}
	for _, gone := range []string{filepath.Join(s.dataRoot, "packs"), filepath.Join(s.dataRoot, "context"), s.configDir} {
		if exists(gone) {
			t.Errorf("%s should have been moved aside", gone)
		}
	}
}

// TestRun_ServeStillUp_RefusesTheDataMove: an unconfirmed-down daemon blocks the
// data move (a live sqlite writer would be split from its wal) AND the state-dir
// move (renaming a live daemon's pidfile orphans it from `pix serve stop`).
func TestRun_ServeStillUp_RefusesTheDataMove(t *testing.T) {
	s := newStack(t)
	stubServeProbe(t, true, true) // up before the stop, still up after it

	err := s.run(t, NewOpts(false, false, true, false), false, "")
	if err == nil {
		t.Fatal("a reset that could not stop the daemon must report a failure")
	}
	if !strings.Contains(err.Error(), "STILL running") {
		t.Errorf("error = %v, want it to name the still-running daemon", err)
	}
	if !exists(filepath.Join(s.dataRoot, "memory", "memory.db")) {
		t.Error("the data root must not move under a live daemon")
	}
	if !exists(s.stateDir) {
		t.Error("the state dir must not move under a live daemon")
	}
}

// TestRun_ForceMovesDataButNeverTheStateDir: --force is an override of the DATA
// move only. The live daemon keeps its pidfile either way — that is AGENTS.md
// invariant #4, and it is the one refusal --force deliberately cannot buy.
func TestRun_ForceMovesDataButNeverTheStateDir(t *testing.T) {
	s := newStack(t)
	stubServeProbe(t, true, true)

	_ = s.run(t, NewOpts(false, false, true, true), false, "")

	if exists(filepath.Join(s.dataRoot, "memory", "memory.db")) {
		t.Error("--force must move the data root")
	}
	if !exists(s.stateDir) {
		t.Error("--force must NOT move the state dir out from under a live daemon")
	}
	if !exists(filepath.Join(s.stateDir, "serve.pid")) {
		t.Error("the live daemon must keep its pidfile so `pix serve stop` can still reach it")
	}
}

// TestRun_SweepRunsBeforeTheStateDirMoves: sandbox teardown reads the lease
// records under the state dir, so the sweep has to happen first. The fixture's
// sweep asserts the dir is still there when it is called; this test proves the
// sweep actually ran (an ordering test that never fires proves nothing).
func TestRun_SweepRunsBeforeTheStateDirMoves(t *testing.T) {
	s := newStack(t)
	stubServeProbe(t, false, false)

	if err := s.run(t, NewOpts(false, false, true, false), false, ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if s.sweeps != 1 {
		t.Fatalf("sandbox sweeps = %d, want 1", s.sweeps)
	}
	if exists(s.stateDir) {
		t.Error("the state dir should be gone by the end")
	}
}

// TestRun_SweepFailure_StillResetsHostState: a sandbox that refuses to go (a
// live shell still references it) is reported and returned, but it must not
// abandon the durable half the user actually typed the verb for.
func TestRun_SweepFailure_StillResetsHostState(t *testing.T) {
	s := newStack(t)
	stubServeProbe(t, false, false)
	s.sweepErr = errors.New("pix-demo is busy")

	err := s.run(t, NewOpts(false, false, true, false), false, "")
	if err == nil {
		t.Fatal("a failed sandbox sweep must surface")
	}
	if exists(s.configDir) || exists(s.dataRoot) {
		t.Error("host state must still be reset when only the sandbox sweep failed")
	}
	if !strings.Contains(s.out.String(), "pix rm --all") {
		t.Error("the failure must name the retry command")
	}
}

// TestRun_KeepSandboxes_TouchesNoSbx: with --keep-sandboxes nothing sbx-facing
// runs — not the sweep, not an `sbx mcp rm`.
func TestRun_KeepSandboxes_TouchesNoSbx(t *testing.T) {
	s := newStack(t)
	stubServeProbe(t, false, false)
	s.env.binaries["sbx"] = true

	if err := s.run(t, NewOpts(false, true, true, false), false, ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if s.sweeps != 0 {
		t.Errorf("sandbox sweeps = %d, want 0", s.sweeps)
	}
	if len(s.ran) != 0 {
		t.Errorf("ran %v, want nothing sbx-facing", s.ran)
	}
}

// TestRun_UnregistersConfiguredMCP: the gateway's HOST registration list is
// derived from the config we just moved aside, so it is cleaned up with it.
func TestRun_UnregistersConfiguredMCP(t *testing.T) {
	s := newStack(t)
	stubServeProbe(t, false, false)
	s.env.binaries["sbx"] = true

	if err := s.run(t, NewOpts(false, false, true, false), false, ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(s.ran) != 1 || s.ran[0] != "sbx mcp rm gog" {
		t.Errorf("ran %v, want [sbx mcp rm gog]", s.ran)
	}
}

// TestRun_SbxAbsent_PrintsTheManualCommands: inside a sandbox (or on a host
// without sbx) reset still resets host state and hands back the commands it
// could not run, rather than failing or silently skipping them.
func TestRun_SbxAbsent_PrintsTheManualCommands(t *testing.T) {
	s := newStack(t)
	stubServeProbe(t, false, false)

	if err := s.run(t, NewOpts(false, false, true, false), false, ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(s.out.String(), "sbx mcp rm gog") {
		t.Errorf("output must print the unrunnable command:\n%s", s.out.String())
	}
}

// TestRun_RestartsOnlyWhatWasRunning: a daemon that was up before comes back on
// the clean slate; one that was never up does not get started as a side effect.
func TestRun_RestartsOnlyWhatWasRunning(t *testing.T) {
	for _, tc := range []struct {
		name      string
		wasUp     bool
		stopped   bool
		wantFired bool
	}{
		{"was running", true, true, true},
		{"never running", false, false, false},
		// A pidfile-less ORPHAN is invisible to the probe but is exactly what the
		// stop's discovery finds and kills, and it deserves the same restart.
		{"orphan the probe could not see", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStack(t)
			stubServeProbe(t, tc.wasUp, false) // whatever it was, it is down after the stop
			stubStop(t, tc.stopped, nil)
			fired := stubRestart(t)

			if err := s.run(t, NewOpts(false, false, true, false), false, ""); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if *fired != tc.wantFired {
				t.Errorf("restart fired = %v, want %v", *fired, tc.wantFired)
			}
		})
	}
}

// ── the guard ───────────────────────────────────────────────────────────────

// TestRun_NonInteractiveWithoutYes_ChangesNothing: the refusal is a REFUSAL,
// not a warning — it happens before the first rename.
func TestRun_NonInteractiveWithoutYes_ChangesNothing(t *testing.T) {
	s := newStack(t)
	stubServeProbe(t, false, false)

	err := s.run(t, NewOpts(false, false, false, false), false, "")
	if !errors.Is(err, ErrNeedsYes) {
		t.Fatalf("err = %v, want ErrNeedsYes", err)
	}
	for _, dir := range []string{s.configDir, s.dataRoot, s.stateDir} {
		if !exists(dir) {
			t.Errorf("%s was touched by a refused reset", dir)
		}
	}
	if s.sweeps != 0 {
		t.Error("a refused reset must not sweep sandboxes")
	}
}

// TestRun_InteractiveNo_Aborts: a bare Enter (the [y/N] default) is NO, and no
// answer other than yes proceeds.
func TestRun_InteractiveNo_Aborts(t *testing.T) {
	for _, answer := range []string{"\n", "n\n", "no\n", "later\n"} {
		s := newStack(t)
		stubServeProbe(t, false, false)

		if err := s.run(t, NewOpts(false, false, false, false), true, answer); err != nil {
			t.Fatalf("answer %q: Run: %v", answer, err)
		}
		if !exists(s.configDir) {
			t.Errorf("answer %q proceeded with the reset", answer)
		}
		if !strings.Contains(s.out.String(), "Aborted") {
			t.Errorf("answer %q: want an explicit abort message", answer)
		}
	}
}

// TestRun_InteractiveYes_Proceeds is the other half: the guard must be passable.
func TestRun_InteractiveYes_Proceeds(t *testing.T) {
	s := newStack(t)
	stubServeProbe(t, false, false)

	if err := s.run(t, NewOpts(false, false, false, false), true, "y\n"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exists(s.configDir) {
		t.Error("an accepted reset must move the config dir aside")
	}
}

// TestRun_PlanIsPrintedBeforeThePrompt: the confirmation is only informed if the
// paths appear above it.
func TestRun_PlanIsPrintedBeforeThePrompt(t *testing.T) {
	s := newStack(t)
	stubServeProbe(t, false, false)
	_ = s.run(t, NewOpts(false, false, false, false), true, "n\n")

	got := s.out.String()
	prompt := strings.Index(got, "Proceed?")
	if prompt < 0 {
		t.Fatalf("no prompt in output:\n%s", got)
	}
	for _, want := range []string{s.configDir, s.dataRoot, s.stateDir} {
		at := strings.Index(got, want)
		if at < 0 || at > prompt {
			t.Errorf("%s is not listed before the prompt:\n%s", want, got)
		}
	}
}

// ── path resolution ─────────────────────────────────────────────────────────

// TestResolvePaths_XDGWins locks the precedence every path in this verb depends
// on. Getting it wrong points a directory RENAME at the wrong tree.
func TestResolvePaths_XDGWins(t *testing.T) {
	s := newStack(t)
	s.setenv("XDG_CONFIG_HOME", "/xdg/config")
	s.setenv("XDG_DATA_HOME", "/xdg/data")
	s.setenv("XDG_STATE_HOME", "/xdg/state")

	p := ResolvePaths(s.env)
	for _, tc := range []struct{ got, want string }{
		{p.ConfigDir, "/xdg/config/pix"},
		{p.DataRoot, "/xdg/data/pix"},
		{p.StateDir, "/xdg/state/pix"},
		{p.PidFile, "/xdg/state/pix/serve.pid"},
		{p.MemoryDir, "/xdg/data/pix/memory"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

// TestResolvePaths_PixConfigOverride: $PIX_CONFIG names the config FILE, so the
// config dir is its parent.
func TestResolvePaths_PixConfigOverride(t *testing.T) {
	s := newStack(t)
	s.setenv("PIX_CONFIG", "/somewhere/else/config.toml")
	if got := ResolvePaths(s.env).ConfigDir; got != "/somewhere/else" {
		t.Errorf("ConfigDir = %q, want /somewhere/else", got)
	}
}

// TestResettableStateDir refuses anything that is not provably this program's
// own state dir — the shape an unresolvable $HOME produces, where
// config.StateDir() itself degrades to bare relative filenames.
func TestResettableStateDir(t *testing.T) {
	for _, tc := range []struct {
		dir  string
		want bool
	}{
		{"/home/u/.local/state/pix", true},
		{"/xdg/state/pix", true},
		{"", false},
		{"pix", false},                        // relative: an unresolvable $HOME
		{"/home/u/.local/state", false},       // the XDG parent, not ours
		{"/", false},                          //
		{"/home/u/.local/state/other", false}, // someone else's
	} {
		if got := resettableStateDir(tc.dir); got != tc.want {
			t.Errorf("resettableStateDir(%q) = %v, want %v", tc.dir, got, tc.want)
		}
	}
}

// TestMoveAside_NeverOverwritesAnExistingBackup: two resets in the same second
// must not have the second one clobber the first one's backup.
func TestMoveAside_NeverOverwritesAnExistingBackup(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "state")
	write(t, filepath.Join(target, "a.txt"), "first")

	first, err := moveAside(DefaultResetFS(), target, 1700000000)
	if err != nil {
		t.Fatalf("first moveAside: %v", err)
	}
	write(t, filepath.Join(target, "b.txt"), "second")
	second, err := moveAside(DefaultResetFS(), target, 1700000000)
	if err != nil {
		t.Fatalf("second moveAside: %v", err)
	}
	if first == second {
		t.Fatalf("both moves landed on %s: the first backup was overwritten", first)
	}
	if got := readFile(t, filepath.Join(first, "a.txt")); got != "first" {
		t.Errorf("first backup content = %q", got)
	}
	if got := readFile(t, filepath.Join(second, "b.txt")); got != "second" {
		t.Errorf("second backup content = %q", got)
	}
}

// TestMoveAside_MissingPathIsSoft: "nothing to move" is the normal answer on a
// clean machine, not an error.
func TestMoveAside_MissingPathIsSoft(t *testing.T) {
	_, err := moveAside(DefaultResetFS(), filepath.Join(t.TempDir(), "absent"), 1)
	if !errors.Is(err, errNotExist) {
		t.Errorf("err = %v, want errNotExist", err)
	}
}

// TestRun_CleanMachine_SaysSo: a reset with nothing to reset must not print a
// backup list it did not create.
func TestRun_CleanMachine_SaysSo(t *testing.T) {
	home := t.TempDir()
	env := resetHost{home: home, envVars: map[string]string{}, binaries: map[string]bool{}, output: map[string]string{}, ports: map[int]bool{}}
	s := &stack{home: home, env: env}
	stubServeProbe(t, false, false)

	if err := Run(defaultCfg(), ResolvePaths(env), NewOpts(false, false, true, false), s.runtime(false, "")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(s.out.String(), "already clean") {
		t.Errorf("want the already-clean summary:\n%s", s.out.String())
	}
}

// TestExecute_PostStopSettleIsGenerousEnoughForALaunchdBootOut pins the race
// that made a real `pix reset` come out HALF done.
//
// A launchd boot-out returns when the unit is unloaded, not when the process has
// exited, and this daemon closes a sqlite writer and reaps a supervised child on
// the way out. With a 2s ceiling, reset printed "serve is STILL running (pid N)",
// refused to move the data directory — and the pid was gone moments later. The
// host was left with config moved aside, data and state kept, and every MCP
// registration removed: a state no single command produces on purpose.
//
// The probe POLLS and returns the instant the process is gone, so a generous
// ceiling costs nothing when the exit is quick. This asserts execute() asks for
// one, which is the part a future tightening would undo.
func TestExecute_PostStopSettleIsGenerousEnoughForALaunchdBootOut(t *testing.T) {
	restoreProbe, restoreStop := probeServeUp, stopServeForReset
	defer func() { probeServeUp, stopServeForReset = restoreProbe, restoreStop }()

	stopServeForReset = func(io.Writer) (bool, error) { return true, nil }
	var settles []time.Duration
	probeServeUp = func(_ string, settle time.Duration) (bool, int) {
		settles = append(settles, settle)
		// Up before the stop; down once a real settle window is honoured.
		return settle == 0, 4242
	}

	var buf bytes.Buffer
	execute(actions{PidFile: filepath.Join(t.TempDir(), "serve.pid")}, Runtime{FS: DefaultResetFS(), IO: cli.IO{Out: &buf}, ErrW: &buf, Now: time.Now}, &buf)

	if len(settles) < 2 {
		t.Fatalf("expected a pre-stop and a post-stop probe, got %d: %v", len(settles), settles)
	}
	post := settles[len(settles)-1]
	if post < 10*time.Second {
		t.Errorf("post-stop settle is %v — too short for a launchd boot-out plus a sqlite close "+
			"and a child reap; 2s is what produced the half-reset", post)
	}
	if out := buf.String(); strings.Contains(out, "STILL running") {
		t.Errorf("a daemon that exits within the settle window must not be reported as still running:\n%s", out)
	}
}
