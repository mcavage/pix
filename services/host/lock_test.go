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
// live-serving entry point: `pix-host memory` (the bare daemon), `plugin
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
