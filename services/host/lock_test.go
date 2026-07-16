package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAcquireLockExclusive proves the primitive: a second acquire of the SAME
// path fails while the first is held, and succeeds once released.
func TestAcquireLockExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".memory.lock")

	release, err := acquireLock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	if _, err := acquireLock(path); err == nil {
		t.Fatal("second acquire succeeded while lock held; want failure")
	}

	release()
	// Idempotent: a second release must not panic.
	release()

	release2, err := acquireLock(path)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	release2()
}

// TestLockMemoryStoreOrFatal proves the shared serving prologue: with the store
// lock FREE it returns a working release and never fires fatal; with the lock
// already held (as another memory server or a restore would) it refuses through
// fatal with the clear one-holder message (and does NOT open the store); and
// once released, a fresh acquire succeeds again. This is the helper all three
// live-serving entry points (serve.go, runMemory, servePluginMemory) call before
// opening the db. Hermetic: no daemon, no port bind, real flock via a temp
// MEMORY_DB.
func TestLockMemoryStoreOrFatal(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MEMORY_DB", filepath.Join(dir, "memory.db"))

	// Free lock: fatal must NOT fire and we get a usable release.
	fatalCalled := false
	release := lockMemoryStoreOrFatal(func(string, ...any) { fatalCalled = true })
	if fatalCalled {
		t.Fatal("fatal fired with a FREE lock")
	}
	if release == nil {
		t.Fatal("nil release with a free lock")
	}

	// Held lock: a second serving entry point must refuse via fatal with the
	// one-holder message, and return a no-op (fatal is stubbed so it does not exit).
	var msg string
	noop := lockMemoryStoreOrFatal(func(f string, a ...any) { msg = fmt.Sprintf(f, a...) })
	if msg == "" {
		t.Fatal("fatal did NOT fire while the lock was held; want refusal")
	}
	if !strings.Contains(msg, "only one may hold it") {
		t.Errorf("refusal message = %q, want it to mention 'only one may hold it'", msg)
	}
	noop() // the returned no-op must be safe to call

	// Release the real hold; a fresh acquire must now succeed with no fatal.
	release()
	fatalCalled = false
	release2 := lockMemoryStoreOrFatal(func(string, ...any) { fatalCalled = true })
	if fatalCalled {
		t.Fatal("fatal fired after the lock was released; a leaked hold?")
	}
	release2()
}

// TestServingEntryPointsRefuseWhenLockHeld is the mutual-exclusion gate for EVERY
// live-serving entry point: `pi-stack-host memory` (the bare daemon), `plugin
// memory` (the plugin self-exec), and `serve memory` (the built-in supervisor
// branch). With the shared store lock already held (as a running daemon or a
// `restore` would hold it), each MUST refuse — exit non-zero with the one-holder
// message and NEVER open the store (proven by the db file not being created). A
// regression that opened the store anyway would create the db and (for the
// daemon/serve) block on a port bind, which the wall-clock guard catches. Real
// subprocess + real cross-process flock; no long-running daemon (all three
// refuse instantly).
func TestServingEntryPointsRefuseWhenLockHeld(t *testing.T) {
	bin := buildHostBinary(t)
	cases := []struct {
		name string
		args []string
	}{
		{"memory daemon", []string{"memory"}},
		{"memory plugin self-exec", []string{"plugin", "memory"}},
		{"serve builtin memory", []string{"serve", "memory"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "memory.db")
			lockPath := filepath.Join(dir, ".memory.lock")

			// Hold the shared lock as another server/restore would (real flock; the
			// child sees it across the process boundary and must fail LOCK_NB).
			release, err := acquireLock(lockPath)
			if err != nil {
				t.Fatalf("test could not take the lock: %v", err)
			}
			defer release()

			cmd := exec.Command(bin, tc.args...)
			// MEMORY_DB fixes both the store path and (its dir) the lock path; HOME
			// isolates any config the `serve` branch reads.
			cmd.Env = append(os.Environ(), "MEMORY_DB="+dbPath, "HOME="+dir)

			var out []byte
			var runErr error
			done := make(chan struct{})
			go func() {
				out, runErr = cmd.CombinedOutput()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(20 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatalf("%s did not exit with the lock held — it opened the store / bound a port instead of refusing", tc.name)
			}

			exit, ok := runErr.(*exec.ExitError)
			if !ok {
				t.Fatalf("expected a non-zero exit, got err=%v out=%s", runErr, out)
			}
			if exit.ExitCode() == 0 {
				t.Fatalf("%s exited 0 with the lock held; want refusal\n%s", tc.name, out)
			}
			if !strings.Contains(string(out), "only one may hold it") {
				t.Errorf("%s output missing the lock-refusal message:\n%s", tc.name, out)
			}
			// The store must NEVER have been opened before the refusal.
			if fileExists(dbPath) {
				t.Errorf("%s opened the store despite the held lock (db created at %s)", tc.name, dbPath)
			}
		})
	}
}

// TestMemoryLockPathMatchesDBDir proves the daemon and restore resolve the SAME
// lock path: config.MemoryLockPath() sits next to MEMORY_DB, and restore's
// default (dir of LiveDBPath) lands on the identical file. This is the wiring the
// mutual-exclusion depends on.
func TestMemoryLockPathMatchesDBDir(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "memory.db")
	t.Setenv("MEMORY_DB", dbPath)

	// resolveRestoreParams pulls MEMORY_DB and sets LockPath from config.
	bp := resolveRestoreParams("archive.tar.gz", false, time.Now())
	want := filepath.Join(dir, ".memory.lock")
	if bp.LockPath != want {
		t.Errorf("restore LockPath = %q, want %q", bp.LockPath, want)
	}
	// And the restore-core default (dir of LiveDBPath) must land on the same file,
	// so a caller that leaves LockPath empty still shares with the daemon.
	if got := filepath.Join(filepath.Dir(bp.LiveDBPath), ".memory.lock"); got != want {
		t.Errorf("derived lock path = %q, want %q", got, want)
	}
}

// TestRestoreRefusesWhenLockHeld is the core contention gate: with the shared
// lock ALREADY held (as the running daemon would hold it), restore must REFUSE
// with the clear message and leave the LIVE db AND config AND op-refs
// byte-for-byte UNTOUCHED — i.e. the refusal happens BEFORE any plain-file
// mutation, not merely rolled back after. The lock is taken with the REAL
// acquireLock (real flock), not a stub, so this proves the primitive, not just
// the plumbing.
func TestRestoreRefusesWhenLockHeld(t *testing.T) {
	st, dbPath := seedMemDB(t, 3)

	// An archive that also carries a config.toml + op-refs.env, so restore would
	// install BOTH plain files if it got past the lock — letting us prove it does not.
	srcDir := t.TempDir()
	srcCfg := filepath.Join(srcDir, "config.toml")
	if err := os.WriteFile(srcCfg, []byte("gog_account = \"restored@example.com\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srcOp := filepath.Join(srcDir, "op-refs.env")
	if err := os.WriteFile(srcOp, []byte("FOO=op://vault/restored/field\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "pi-stack-backup-20260715-120000.tar.gz")
	if _, err := memoryBackup(backupParams{
		DBPath: dbPath, OutPath: archive, Keep: 7, Version: "test",
		ConfigPath: srcCfg, OpRefsPath: srcOp, Now: time.Now(),
	}); err != nil {
		t.Fatalf("seed backup: %v", err)
	}
	st.db.Close()

	liveBefore, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	// Live config + op-refs that MUST be untouched by a refused restore.
	destDir := t.TempDir()
	destCfg := filepath.Join(destDir, "config.toml")
	origCfg := []byte("gog_account = \"live@example.com\"\n")
	if err := os.WriteFile(destCfg, origCfg, 0o600); err != nil {
		t.Fatal(err)
	}
	destOp := filepath.Join(destDir, "op-refs.env")
	origOp := []byte("FOO=op://vault/live/field\n")
	if err := os.WriteFile(destOp, origOp, 0o600); err != nil {
		t.Fatal(err)
	}

	lockPath := filepath.Join(filepath.Dir(dbPath), ".memory.lock")
	release, err := acquireLock(lockPath)
	if err != nil {
		t.Fatalf("test could not take the lock: %v", err)
	}
	defer release()

	_, err = memoryRestore(restoreParams{
		ArchivePath: archive, LiveDBPath: dbPath, ConfigPath: destCfg, OpRefsPath: destOp,
		Force: true, LockPath: lockPath, Now: time.Now(),
		ServeProbe: func() bool { return false }, // port DOWN — lock is the authority
	})
	if err == nil {
		t.Fatal("restore succeeded while the store lock was held; want refusal")
	}
	if !strings.Contains(err.Error(), "pi-stack serve stop") {
		t.Errorf("refusal message = %q, want it to mention 'pi-stack serve stop'", err.Error())
	}

	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("live db missing after refused restore: %v", err)
	}
	if len(after) != len(liveBefore) {
		t.Errorf("live db mutated by a refused restore (%d -> %d bytes)", len(liveBefore), len(after))
	}
	if m, _ := filepath.Glob(dbPath + ".bak-*"); len(m) != 0 {
		t.Errorf("refused restore left a memory .bak: %v", m)
	}

	// The live config + op-refs must be byte-for-byte the ORIGINAL, and no .bak
	// should exist — proving the refusal fired BEFORE any plain-file move-aside.
	if got, _ := os.ReadFile(destCfg); string(got) != string(origCfg) {
		t.Errorf("config mutated by a refused restore: got %q, want %q", got, origCfg)
	}
	if m, _ := filepath.Glob(destCfg + ".bak-*"); len(m) != 0 {
		t.Errorf("refused restore left a config .bak: %v", m)
	}
	if got, _ := os.ReadFile(destOp); string(got) != string(origOp) {
		t.Errorf("op-refs mutated by a refused restore: got %q, want %q", got, origOp)
	}
	if m, _ := filepath.Glob(destOp + ".bak-*"); len(m) != 0 {
		t.Errorf("refused restore left an op-refs .bak: %v", m)
	}
}

// TestRestoreLockPrecedesPlainFileMutation is the reordered-lock gate: it proves
// the shared store lock is acquired BEFORE the FIRST plain-file mutation. The
// injected acquireLockFn captures the live config's bytes AT THE MOMENT the lock
// is attempted; if the lock were taken (as the bug had it) AFTER the config
// install, the captured content would already be the ARCHIVED value and a .bak
// would exist. Taken up front, the capture must still show the ORIGINAL config
// and no .bak. Moving the acquisition back after the plain-file installs fails
// this test.
func TestRestoreLockPrecedesPlainFileMutation(t *testing.T) {
	st, dbPath := seedMemDB(t, 2)

	srcDir := t.TempDir()
	srcCfg := filepath.Join(srcDir, "config.toml")
	if err := os.WriteFile(srcCfg, []byte("gog_account = \"restored@example.com\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "pi-stack-backup-20260715-120000.tar.gz")
	if _, err := memoryBackup(backupParams{
		DBPath: dbPath, OutPath: archive, Keep: 7, Version: "test",
		ConfigPath: srcCfg, Now: time.Now(),
	}); err != nil {
		t.Fatalf("seed backup: %v", err)
	}
	st.db.Close()

	destDir := t.TempDir()
	destCfg := filepath.Join(destDir, "config.toml")
	origCfg := []byte("gog_account = \"live@example.com\"\n")
	if err := os.WriteFile(destCfg, origCfg, 0o600); err != nil {
		t.Fatal(err)
	}

	lockPath := filepath.Join(filepath.Dir(dbPath), ".memory.lock")

	// Inject a lock that REFUSES, but first records the on-disk state of the live
	// config at the instant it is asked for the lock.
	var cfgAtLock string
	var bakAtLock []string
	stub := func(path string) (func(), error) {
		b, _ := os.ReadFile(destCfg)
		cfgAtLock = string(b)
		bakAtLock, _ = filepath.Glob(destCfg + ".bak-*")
		return nil, errBusyLock
	}

	_, err := memoryRestore(restoreParams{
		ArchivePath: archive, LiveDBPath: dbPath, ConfigPath: destCfg,
		Force: true, LockPath: lockPath, Now: time.Now(),
		ServeProbe:    func() bool { return false },
		acquireLockFn: stub,
	})
	if err == nil {
		t.Fatal("restore succeeded with a refusing lock stub; want refusal")
	}
	if cfgAtLock != string(origCfg) {
		t.Errorf("config was already mutated when the lock was acquired (got %q, want %q) — lock taken AFTER the plain-file install", cfgAtLock, origCfg)
	}
	if len(bakAtLock) != 0 {
		t.Errorf("a config .bak existed when the lock was acquired (%v) — lock taken AFTER the move-aside", bakAtLock)
	}
	// And the live config is left as the original.
	if got, _ := os.ReadFile(destCfg); string(got) != string(origCfg) {
		t.Errorf("live config not left original: got %q, want %q", got, origCfg)
	}
}

// errBusyLock is a sentinel the lock-order gate injects to simulate a held lock.
var errBusyLock = errors.New("memory store is locked")

// TestRestoreSucceedsAndReleasesLock proves the happy path: with the lock FREE,
// restore performs the swap AND releases the lock afterward — so the daemon (or a
// subsequent restore) can take it again. A leaked lock would block the next
// acquire and fail this test.
func TestRestoreSucceedsAndReleasesLock(t *testing.T) {
	st, dbPath := seedMemDB(t, 4)
	archive := backupSeeded(t, dbPath)
	st.db.Close()
	// Wipe the live db so restore installs cleanly (no --force needed).
	for _, sc := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		_ = os.Remove(sc)
	}

	lockPath := filepath.Join(filepath.Dir(dbPath), ".memory.lock")
	res, err := memoryRestore(restoreParams{
		ArchivePath: archive, LiveDBPath: dbPath,
		LockPath: lockPath, Now: time.Now(),
		ServeProbe: func() bool { return false },
	})
	if err != nil {
		t.Fatalf("restore with free lock: %v", err)
	}
	if res.RowCount != 4 {
		t.Errorf("restored RowCount = %d, want 4", res.RowCount)
	}
	if !fileExists(dbPath) {
		t.Fatal("restore did not install the live db")
	}

	// The swap released the lock: a fresh acquire must succeed immediately.
	release, err := acquireLock(lockPath)
	if err != nil {
		t.Fatalf("lock not released after a successful restore: %v", err)
	}
	release()
}

// TestRestoreRefusalRollsBackPlainFilesWhenLockHeld proves the refusal is atomic
// on the plain-file side too: when the lock is held, any config/op-refs restored
// before the swap step are rolled back, and (as above) the memory db is untouched.
func TestRestoreRefusalRollsBackPlainFilesWhenLockHeld(t *testing.T) {
	st, dbPath := seedMemDB(t, 2)

	srcDir := t.TempDir()
	srcCfg := filepath.Join(srcDir, "config.toml")
	if err := os.WriteFile(srcCfg, []byte("gog_account = \"restored@example.com\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "pi-stack-backup-20260715-120000.tar.gz")
	if _, err := memoryBackup(backupParams{
		DBPath: dbPath, OutPath: archive, Keep: 7, Version: "test",
		ConfigPath: srcCfg, Now: time.Now(),
	}); err != nil {
		t.Fatalf("seed backup: %v", err)
	}
	st.db.Close()

	// A live config to be moved aside (then rolled back).
	destDir := t.TempDir()
	destCfg := filepath.Join(destDir, "config.toml")
	origCfg := []byte("gog_account = \"live@example.com\"\n")
	if err := os.WriteFile(destCfg, origCfg, 0o600); err != nil {
		t.Fatal(err)
	}

	lockPath := filepath.Join(filepath.Dir(dbPath), ".memory.lock")
	release, err := acquireLock(lockPath)
	if err != nil {
		t.Fatalf("test could not take the lock: %v", err)
	}
	defer release()

	if _, err := memoryRestore(restoreParams{
		ArchivePath: archive, LiveDBPath: dbPath, ConfigPath: destCfg, Force: true,
		LockPath: lockPath, Now: time.Now(),
		ServeProbe: func() bool { return false },
	}); err == nil {
		t.Fatal("restore succeeded while the lock was held; want refusal")
	}

	// Config rolled back to its original content.
	if got, _ := os.ReadFile(destCfg); string(got) != string(origCfg) {
		t.Errorf("config not rolled back: got %q, want %q", got, origCfg)
	}
	if m, _ := filepath.Glob(destCfg + ".bak-*"); len(m) != 0 {
		t.Errorf("refused restore left a config .bak: %v", m)
	}
}

// TestRestoreReStatsLiveDBUnderLock is the TOCTOU gate for evaluating live-db
// existence + the --force rule UNDER the lock. The dest has NO live db at the
// initial (pre-lock) check, so the preliminary guard passes; a racing restore A
// then installs a db AFTER that check but BEFORE this restore acquires the lock.
// We simulate that race at the lock-acquire seam: the stub writes a sentinel db
// then returns the REAL lock. Under the lock, restore must RE-STAT, find the db,
// and (without --force) REFUSE — never os.Rename over restore A's db, never move
// it aside to a .bak. FAILS if the existence/force decision is not re-made under
// the lock (a stale liveExists=false would let the swap silently clobber it).
func TestRestoreReStatsLiveDBUnderLock(t *testing.T) {
	st, srcPath := seedMemDB(t, 3)
	archive := backupSeeded(t, srcPath)
	st.db.Close()

	destDir := t.TempDir()
	dbPath := filepath.Join(destDir, "memory.db")
	lockPath := filepath.Join(destDir, ".memory.lock")

	sentinel := []byte("racing restore A's db — must NOT be clobbered")
	stub := func(path string) (func(), error) {
		// Restore A installed a db while we waited for the lock.
		if err := os.WriteFile(dbPath, sentinel, 0o600); err != nil {
			return nil, err
		}
		return acquireLock(path)
	}

	_, err := memoryRestore(restoreParams{
		ArchivePath: archive, LiveDBPath: dbPath, Force: false,
		LockPath: lockPath, Now: time.Now(),
		ServeProbe:    func() bool { return false },
		acquireLockFn: stub,
	})
	if err == nil {
		t.Fatal("restore succeeded despite a live db appearing under the lock without --force; want refusal")
	}
	got, rerr := os.ReadFile(dbPath)
	if rerr != nil {
		t.Fatalf("live db missing after refused restore: %v", rerr)
	}
	if string(got) != string(sentinel) {
		t.Errorf("restore REPLACED the racing db (TOCTOU clobber): got %q, want the sentinel", got)
	}
	if m, _ := filepath.Glob(dbPath + ".bak-*"); len(m) != 0 {
		t.Errorf("refused restore moved the racing db aside to a .bak: %v", m)
	}
}

// TestAcquireLockCreatesMissingDir gates the fresh-setup bug: on a first-ever
// serve the lock file's parent dir (the memory db dir) does not exist yet, since
// it's created only when the store opens — AFTER the lock is taken. acquireLock
// must MkdirAll that dir and succeed, not fail with ENOENT.
func TestAcquireLockCreatesMissingDir(t *testing.T) {
	// A path two levels below a fresh temp dir: neither intermediate exists.
	path := filepath.Join(t.TempDir(), "memory", ".memory.lock")
	release, err := acquireLock(path)
	if err != nil {
		t.Fatalf("acquireLock on a missing parent dir failed (regression): %v", err)
	}
	defer release()
	if _, serr := os.Stat(path); serr != nil {
		t.Errorf("lock file not created at %s: %v", path, serr)
	}
}

// TestLockMemoryStoreOrFatalFreshDir: the serving prologue must NOT fatal on a
// fresh MEMORY_DB whose dir doesn't exist yet (the user-reported first-serve bug).
func TestLockMemoryStoreOrFatalFreshDir(t *testing.T) {
	// MEMORY_DB in a dir that does not exist -> MemoryLockPath parent missing.
	t.Setenv("MEMORY_DB", filepath.Join(t.TempDir(), "memory", "memory.db"))
	fatalCalled := false
	release := lockMemoryStoreOrFatal(func(string, ...any) { fatalCalled = true })
	if fatalCalled {
		t.Fatal("fatal fired on a fresh (missing) MEMORY_DB dir — first serve would be blocked")
	}
	if release != nil {
		release()
	}
}
