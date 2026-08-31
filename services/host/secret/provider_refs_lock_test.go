// provider_refs_lock_test.go — the provider-refs transaction lock
// (providerrefslock.go): the ONE advisory cross-process lock serializing every
// both-file (secrets.env + hostmode.env) credential transaction.
//
// Coverage here:
//   - deterministic lock-window tests (a recording fake flock proves every
//     file read/write and sbx call of a transaction happens INSIDE exactly one
//     lock acquisition — and that no nested acquisition ever happens, which
//     with a real flock would deadlock);
//   - a real-flock concurrency test (lost-update-prone naive writes stay
//     correct because RunSecretSet serializes);
//   - real-flock no-deadlock tests for the two-file provider-key paths;
//   - reconcile-uses-the-snapshot tests (never rereads CurrentOpRef);
//   - lock-acquisition-failure tests (operations fail honestly, never proceed
//     unlocked).
//
// All hermetic: lock paths derive from DefaultOpRefsPath, which every env here
// fakes (PIX_CONFIG via getenv, or a t.TempDir()) — no real HOME writes.
package secret

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"pix/host/hostenv"
	"pix/host/sys"
	"pix/host/sys/systest"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockWindow is systest.LockWindow plus a fatal when the window is missing.
func lockWindow(t *testing.T, events []string, lockPath string) (first, last int) {
	t.Helper()
	first, last = systest.LockWindow(events, lockPath)
	if first < 0 || last < 0 {
		t.Fatalf("no acquire/release of %s in events: %v", lockPath, events)
	}
	return first, last
}

// --- deterministic lock-window: secret set/rm are ONE locked transaction ---

func TestSecretSetHoldsLockAcrossFileTransaction(t *testing.T) {
	files := map[string]string{fakeRefsPath: "# header\n"}
	var events []string
	env := memEnv(t, files)
	origRead, origWrite := systest.Of(env.System).ReadFileFn, systest.Of(env.System).WriteFileFn
	systest.Of(env.System).ReadFileFn = func(p string) (string, error) {
		events = append(events, "read "+p)
		return origRead(p)
	}
	systest.Of(env.System).WriteFileFn = func(p string, d []byte, perm os.FileMode) error {
		events = append(events, "write "+p)
		return origWrite(p, d, perm)
	}
	systest.Of(env.System).LockFn = systest.NewLockRecorder(t.Fatalf, &events).Lock

	var out bytes.Buffer
	if err := RunSecretSet(env, &out, "ANTHROPIC_API_KEY", "op://v/anthropic/key", nil); err != nil {
		t.Fatalf("RunSecretSet: %v (out=%q)", err, out.String())
	}

	lockPath := ProviderRefsLockPath(env)
	if got := systest.CountEvents(events, "acquire "); got != 1 {
		t.Fatalf("want exactly 1 lock acquisition (writes must use the Locked variant), got %d: %v", got, events)
	}
	first, last := lockWindow(t, events, lockPath)
	for i, e := range events {
		if strings.HasPrefix(e, "read ") || strings.HasPrefix(e, "write ") {
			if i < first || i > last {
				t.Errorf("file I/O outside the lock window: %q (index %d, window %d..%d)", e, i, first, last)
			}
		}
	}
	if !strings.Contains(files[fakeRefsPath], "ANTHROPIC_API_KEY=op://v/anthropic/key") {
		t.Errorf("secrets.env missing the key: %q", files[fakeRefsPath])
	}
}

func TestSecretRmHoldsLockAcrossFileTransaction(t *testing.T) {
	files := map[string]string{fakeRefsPath: "ANTHROPIC_API_KEY=op://v/anthropic/key\n"}
	var events []string
	env := memEnv(t, files)
	origRead, origWrite := systest.Of(env.System).ReadFileFn, systest.Of(env.System).WriteFileFn
	systest.Of(env.System).ReadFileFn = func(p string) (string, error) {
		events = append(events, "read "+p)
		return origRead(p)
	}
	systest.Of(env.System).WriteFileFn = func(p string, d []byte, perm os.FileMode) error {
		events = append(events, "write "+p)
		return origWrite(p, d, perm)
	}
	systest.Of(env.System).LockFn = systest.NewLockRecorder(t.Fatalf, &events).Lock

	var out bytes.Buffer
	if err := RunSecretRm(env, &out, "ANTHROPIC_API_KEY"); err != nil {
		t.Fatalf("RunSecretRm: %v (out=%q)", err, out.String())
	}
	if got := systest.CountEvents(events, "acquire "); got != 1 {
		t.Fatalf("want exactly 1 lock acquisition, got %d: %v", got, events)
	}
	first, last := lockWindow(t, events, ProviderRefsLockPath(env))
	for i, e := range events {
		if strings.HasPrefix(e, "read ") || strings.HasPrefix(e, "write ") {
			if i < first || i > last {
				t.Errorf("file I/O outside the lock window: %q (index %d, window %d..%d)", e, i, first, last)
			}
		}
	}
	if strings.Contains(files[fakeRefsPath], "ANTHROPIC_API_KEY") {
		t.Errorf("key not removed from secrets.env: %q", files[fakeRefsPath])
	}
}

// syncTestHome is the VIRTUAL PIX_HOME these lock tests point at: every read
// below is served by the fake ReadFileFn keyed on the resolved path, so no
// real directory is ever touched. Each test sets $PIX_HOME to it, because
// secrets.env resolves under PIX_HOME alone now (QA F5) — the old
// XDG_CONFIG_HOME fake this file used is not consulted by anything any more.
const syncTestHome = "/cfg-synctest"

// --- deterministic lock-window: syncProviderKeys / EnsureProviderKeysFromRefs ---

// syncEnv builds a hostenv.Env over a hermetic temp config dir for
// syncProviderKeys / EnsureProviderKeysFromRefs lock tests: op
// installed+signed-in, sbx on PATH, and a recording run/readFile so the test
// can assert every secrets.env read and op/sbx call landed inside the lock
// window. opReadVal is returned for every `op read`; sbxLsOut is returned for
// `sbx secret ls`.
func syncEnv(refsContent, opReadVal, sbxLsOut string, events *[]string) (hostenv.Env, string) {
	dir := syncTestHome
	refsPath := filepath.Join(dir, "secrets.env")
	env := hostenv.Env{System: &systest.Fake{GetenvFn: func(k string) string {
		if k == "XDG_CONFIG_HOME" {
			return dir
		}
		return ""
	}, ReadFileFn: func(p string) (string, error) {
		*events = append(*events, "read "+p)
		if p == refsPath {
			return refsContent, nil
		}
		return "", os.ErrNotExist
	}, LookPathFn: func(name string) (string, error) { return "/usr/bin/" + name, nil }, RunFn: func(name string, args ...string) (string, error) {
		*events = append(*events, "run "+name+" "+strings.Join(args, " "))
		switch {
		case name == "op" && len(args) >= 1 && args[0] == "account":
			return "acct", nil
		case name == "op" && len(args) >= 1 && args[0] == "read":
			return opReadVal, nil
		case name == "sbx" && len(args) >= 2 && args[0] == "secret" && args[1] == "ls":
			return sbxLsOut, nil
		}
		return "", nil
	}}}
	return env, refsPath
}

// syncProviderKeys must hold the provider-refs lock across its WHOLE
// read-op-refs / op-read / sbx-set transaction — one acquisition, everything
// inside the window — so a concurrent `secret set`/`secret rm` can never
// change secrets.env between the snapshot it reads and the sbx values it
// pushes.
func TestSyncProviderKeysHoldsLockAcrossReadResolveSbxSync(t *testing.T) {
	var events []string
	t.Setenv("PIX_HOME", syncTestHome)
	env, refsPath := syncEnv("ANTHROPIC_API_KEY=op://v/anthropic/key\n", "sk-val", "", &events)
	systest.Of(env.System).LockFn = systest.NewLockRecorder(t.Fatalf, &events).Lock

	var out bytes.Buffer
	synced, failed, fatal := syncProviderKeys(env, &out)
	if fatal != nil || failed != 0 || synced != 1 {
		t.Fatalf("synced=%d failed=%d fatal=%v out=%q", synced, failed, fatal, out.String())
	}

	lockPath := ProviderRefsLockPath(env)
	if got := systest.CountEvents(events, "acquire "); got != 1 {
		t.Fatalf("want exactly 1 lock acquisition, got %d: %v", got, events)
	}
	first, last := lockWindow(t, events, lockPath)
	sawRefsRead := false
	for i, e := range events {
		if e == "read "+refsPath {
			sawRefsRead = true
		}
		if strings.HasPrefix(e, "read ") || strings.HasPrefix(e, "run ") {
			if i < first || i > last {
				t.Errorf("event outside the lock window: %q (index %d, window %d..%d)", e, i, first, last)
			}
		}
	}
	if !sawRefsRead {
		t.Fatalf("expected secrets.env read, events: %v", events)
	}
}

// EnsureProviderKeysFromRefs must likewise hold the lock across its whole
// read/sbx-ls/op-read/sbx-set pass.
func TestEnsureProviderKeysFromRefsHoldsLockAcrossReadResolveSbxSync(t *testing.T) {
	var events []string
	t.Setenv("PIX_HOME", syncTestHome)
	env, refsPath := syncEnv("GEMINI_API_KEY=op://v/gemini/key\n", "sk-val", "", &events)
	systest.Of(env.System).LockFn = systest.NewLockRecorder(t.Fatalf, &events).Lock

	var out bytes.Buffer
	EnsureProviderKeysFromRefs(env, &out)
	if !strings.Contains(out.String(), "resolved google") {
		t.Fatalf("expected google to be resolved, got: %s", out.String())
	}

	lockPath := ProviderRefsLockPath(env)
	if got := systest.CountEvents(events, "acquire "); got != 1 {
		t.Fatalf("want exactly 1 lock acquisition, got %d: %v", got, events)
	}
	first, last := lockWindow(t, events, lockPath)
	sawRefsRead := false
	for i, e := range events {
		if e == "read "+refsPath {
			sawRefsRead = true
		}
		if strings.HasPrefix(e, "read ") || strings.HasPrefix(e, "run ") {
			if i < first || i > last {
				t.Errorf("event outside the lock window: %q (index %d, window %d..%d)", e, i, first, last)
			}
		}
	}
	if !sawRefsRead {
		t.Fatalf("expected secrets.env read, events: %v", events)
	}
}

// OfferOnePasswordKeys must write each provider's secrets.env + hostmode.env
// pair under ONE lock acquisition (never two separate windows for the same
// pair, never nested), and its final syncProviderKeys call must acquire its
// OWN lock only AFTER every per-provider write lock has already been
// released — calling it from inside a still-held lock would deadlock the
// real flock.
func TestOfferOnePasswordKeysWritesRefUnderOneLockThenSyncsAfterRelease(t *testing.T) {
	dir := "/cfg-offertest"
	t.Setenv("PIX_HOME", dir) // secrets.env resolves under PIX_HOME alone (QA F5)
	refsPath := filepath.Join(dir, "secrets.env")
	files := map[string]string{}
	var events []string
	env := hostenv.Env{System: &systest.Fake{GetenvFn: func(k string) string {
		if k == "XDG_CONFIG_HOME" {
			return dir
		}
		return ""
	}, ReadFileFn: func(p string) (string, error) {
		events = append(events, "read "+p)
		if v, ok := files[p]; ok {
			return v, nil
		}
		return "", os.ErrNotExist
	}, WriteFileFn: func(p string, d []byte, _ os.FileMode) error {
		events = append(events, "write "+p)
		files[p] = string(d)
		return nil
	}, LookPathFn: func(name string) (string, error) { return "/usr/bin/" + name, nil }, RunFn: func(name string, args ...string) (string, error) {
		events = append(events, "run "+name+" "+strings.Join(args, " "))
		switch {
		case name == "op" && len(args) >= 1 && args[0] == "--version":
			return "2.0", nil
		case name == "op" && len(args) >= 1 && args[0] == "account":
			return "acct", nil
		case name == "op" && len(args) >= 1 && args[0] == "read":
			return "sk-val", nil
		}
		return "", nil
	}}}
	systest.Of(env.System).LockFn = systest.NewLockRecorder(t.Fatalf, &events).Lock

	// Accept the offer, paste ONE ref (anthropic), blank the other two so the
	// pair-write happens exactly once before the loop finishes and falls
	// through to the final sync call.
	var out bytes.Buffer
	OfferOnePasswordKeys(env, strings.NewReader("y\nop://v/anthropic/key\n\n\n"), &out, true)

	lockPath := ProviderRefsLockPath(env)
	// Exactly two acquisitions: the anthropic pair-write, then the final sync
	// — never more (a correct pair-write is ONE lock call, not two) and
	// recordingFlock already fails the test outright on any NESTED
	// acquisition (the deadlock case), so surviving to this assertion is
	// itself proof the two acquisitions never overlapped.
	if got := systest.CountEvents(events, "acquire "); got != 2 {
		t.Fatalf("want exactly 2 lock acquisitions (1 ref-write + 1 sync), got %d: %v", got, events)
	}
	var acquireIdx, releaseIdx []int
	for i, e := range events {
		if e == "acquire "+lockPath {
			acquireIdx = append(acquireIdx, i)
		}
		if e == "release "+lockPath {
			releaseIdx = append(releaseIdx, i)
		}
	}
	if len(acquireIdx) != 2 || len(releaseIdx) != 2 {
		t.Fatalf("expected 2 acquire/release pairs, got acquire=%v release=%v", acquireIdx, releaseIdx)
	}
	firstAcquire, firstRelease := acquireIdx[0], releaseIdx[0]
	sawRefsWrite := false
	for i, e := range events {
		if e == "write "+refsPath {
			sawRefsWrite = true
			if i < firstAcquire || i > firstRelease {
				t.Errorf("secrets.env write outside the first (ref-write) lock window: index %d, window %d..%d", i, firstAcquire, firstRelease)
			}
		}
	}
	if !sawRefsWrite {
		t.Fatalf("expected an secrets.env write, events: %v", events)
	}
	// The sync call's lock acquisition must start strictly after the
	// ref-write's lock released — sequential, never overlapping.
	if secondAcquire := acquireIdx[1]; secondAcquire <= firstRelease {
		t.Fatalf("sync's lock acquisition (index %d) must start after the ref-write lock released (index %d)", secondAcquire, firstRelease)
	}
}

// --- lock acquisition failure fails syncProviderKeys / EnsureProviderKeysFromRefs / offer honestly ---

func TestLockAcquisitionErrorFailsSyncProviderKeys(t *testing.T) {
	var events []string
	t.Setenv("PIX_HOME", syncTestHome)
	env, _ := syncEnv("ANTHROPIC_API_KEY=op://v/a/k\n", "sk-val", "", &events)
	systest.Of(env.System).LockFn = func(string, func() error) error { return errors.New("lock dir unwritable") }

	var out bytes.Buffer
	_, _, fatal := syncProviderKeys(env, &out)
	if fatal == nil {
		t.Fatal("syncProviderKeys must fail when the lock cannot be acquired")
	}
	if !strings.Contains(out.String(), "could not lock provider refs") {
		t.Errorf("output = %q, want a lock failure message", out.String())
	}
	if len(events) != 0 {
		t.Errorf("must not read/act on secrets.env when the lock cannot be acquired, events: %v", events)
	}
}

func TestLockAcquisitionErrorMakesEnsureProviderKeysFromRefsANoOp(t *testing.T) {
	var events []string
	t.Setenv("PIX_HOME", syncTestHome)
	env, _ := syncEnv("ANTHROPIC_API_KEY=op://v/a/k\n", "sk-val", "", &events)
	systest.Of(env.System).LockFn = func(string, func() error) error { return errors.New("lock dir unwritable") }

	var out bytes.Buffer
	EnsureProviderKeysFromRefs(env, &out)
	if !strings.Contains(out.String(), "could not lock provider refs") {
		t.Errorf("output = %q, want a lock failure message", out.String())
	}
	if len(events) != 0 {
		t.Errorf("must not read/act on secrets.env when the lock cannot be acquired, events: %v", events)
	}
}

func TestLockAcquisitionErrorSkipsProviderInOfferOnePasswordKeys(t *testing.T) {
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(name string) (string, error) { return "/usr/bin/" + name, nil }, RunFn: func(name string, args ...string) (string, error) {
		if name == "op" && len(args) >= 1 && args[0] == "--version" {
			return "2.0", nil
		}
		return "", nil
	}, ReadFileFn: func(string) (string, error) { return "", os.ErrNotExist }, LockFn: func(string, func() error) error { return errors.New("lock dir unwritable") }}}
	var out bytes.Buffer
	OfferOnePasswordKeys(env, strings.NewReader("y\nop://v/anthropic/key\n\n\n"), &out, true)
	if !strings.Contains(out.String(), "could not lock provider refs for anthropic") {
		t.Errorf("output = %q, want a per-provider lock failure message", out.String())
	}
	if strings.Contains(out.String(), "Resolving from 1Password") {
		t.Error("must not proceed to sync when every provider write failed to lock")
	}
}

// --- real flock: concurrency + no-deadlock ---

// realFileEnv is a hostenv.Env over REAL temp files with the REAL sys.Lock and a
// deliberately lost-update-prone writer (plain WriteFile after a small delay),
// so unserialized concurrent read-modify-writes would drop keys.
func realFileEnv(t *testing.T) (hostenv.Env, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	t.Setenv("PIX_CONFIG", cfg)             // keeps any config.* fallback in the temp dir too
	t.Setenv("PIX_HOME", filepath.Dir(cfg)) // secrets.env resolves under PIX_HOME alone (QA F5)
	env := hostenv.Env{System: &systest.Fake{GetenvFn: func(k string) string {
		if k == "PIX_CONFIG" {
			return cfg
		}
		return ""
	}, ReadFileFn: func(p string) (string, error) {
		b, err := os.ReadFile(p)
		return string(b), err
	}, WriteFileFn: func(p string, d []byte, perm os.FileMode) error {
		time.Sleep(2 * time.Millisecond) // widen the read..write race window
		return os.WriteFile(p, d, perm)
	}, LockFn: sys.Lock}}
	if err := os.WriteFile(DefaultOpRefsPath(), []byte("# header\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return env, dir
}

func TestConcurrentSecretSetSerializedByRealFlock(t *testing.T) {
	env, dir := realFileEnv(t)
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var out bytes.Buffer
			errs[i] = RunSecretSet(env, &out, fmt.Sprintf("TEST_REF_%d", i), fmt.Sprintf("op://v/item%d/field", i), nil)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent set %d failed: %v", i, err)
		}
	}
	content, err := os.ReadFile(DefaultOpRefsPath())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		want := fmt.Sprintf("TEST_REF_%d=op://v/item%d/field", i, i)
		if !strings.Contains(string(content), want) {
			t.Errorf("lost update: %s missing from secrets.env:\n%s", want, content)
		}
	}
	// The lock file exists adjacent to the refs files, mode 0600.
	fi, err := os.Stat(filepath.Join(dir, providerRefsLockName))
	if err != nil {
		t.Fatalf("lock file not created adjacent to secrets.env: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("lock file mode = %o, want 0600", perm)
	}
}

// A provider key exercises the two-file path (secrets.env write + hostmode.env
// mirror) INSIDE the held lock; if the mirror went through the public locking
// wrapper this would deadlock forever against the real flock.
func TestProviderKeySetAndRmNoDeadlockUnderRealFlock(t *testing.T) {
	env, dir := realFileEnv(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var out bytes.Buffer
		if err := RunSecretSet(env, &out, "ANTHROPIC_API_KEY", "op://v/anthropic/key", nil); err != nil {
			t.Errorf("RunSecretSet: %v (out=%q)", err, out.String())
			return
		}
		if err := RunSecretRm(env, &out, "ANTHROPIC_API_KEY"); err != nil {
			t.Errorf("RunSecretRm: %v (out=%q)", err, out.String())
		}
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("provider-key set/rm deadlocked under the real flock (nested lock acquisition?)")
	}
	hm, err := os.ReadFile(filepath.Join(dir, "hostmode.env"))
	if err == nil && strings.Contains(string(hm), "ANTHROPIC_API_KEY") {
		t.Errorf("hostmode.env still carries the removed key: %q", hm)
	}
}

// --- lock acquisition failure fails honestly ---

func TestLockAcquisitionErrorFailsSecretSetAndRm(t *testing.T) {
	files := map[string]string{fakeRefsPath: "ANTHROPIC_API_KEY=op://v/anthropic/key\n"}
	env := memEnv(t, files)
	systest.Of(env.System).LockFn = func(string, func() error) error { return errors.New("lock dir unwritable") }

	var out bytes.Buffer
	if err := RunSecretSet(env, &out, "ANTHROPIC_API_KEY", "op://v/anthropic/new", nil); err == nil {
		t.Fatal("secret set must fail when the lock cannot be acquired")
	}
	if !strings.Contains(out.String(), "could not lock provider refs") {
		t.Errorf("secret set output = %q, want a lock failure message", out.String())
	}
	if strings.Contains(files[fakeRefsPath], "op://v/anthropic/new") {
		t.Error("secret set wrote despite the lock failure")
	}

	out.Reset()
	if err := RunSecretRm(env, &out, "ANTHROPIC_API_KEY"); err == nil {
		t.Fatal("secret rm must fail when the lock cannot be acquired")
	}
	if !strings.Contains(out.String(), "could not lock provider refs") {
		t.Errorf("secret rm output = %q, want a lock failure message", out.String())
	}
	if !strings.Contains(files[fakeRefsPath], "ANTHROPIC_API_KEY") {
		t.Error("secret rm removed the key despite the lock failure")
	}
}
