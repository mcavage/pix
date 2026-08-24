//go:build unix

package recreatelog

// symlink_test.go is the SHIPPED proof for a claim QA previously only
// verified ad hoc: every file this package ever opens by path — the flock
// file, the data file, and the write-side temp file — is opened with
// openNoFollow (O_NOFOLLOW), so a symlink planted at any one of those three
// names is refused, never followed. Each test below plants the symlink at
// a DIFFERENT one of the three names and drives the public API (Append /
// Read) rather than calling openNoFollow directly, so the proof is that the
// public surface fails closed, not merely that the private helper does.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// elsewhereFile creates a file OUTSIDE dir and returns its path — the decoy
// target every symlink in this file points at, so a bug that DID follow the
// link would read or write attacker-chosen content living outside the log's
// own directory.
func elsewhereFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "decoy")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func symlinkOrSkip(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

// --- lock file (recreate.log.lock) ------------------------------------------

// TestAppend_RefusesSymlinkedLockFile: withAppendLock opens lockFileName with
// openNoFollow before ever taking the flock. A symlinked lock file must
// refuse with a symlink error, not follow the link and flock (and
// potentially corrupt) whatever it points at.
func TestAppend_RefusesSymlinkedLockFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	decoy := elsewhereFile(t, "")
	symlinkOrSkip(t, decoy, filepath.Join(dir, lockFileName))

	err := Append(dir, "home", []string{"a.b"})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Append with a symlinked lock file = %v, want a symlink-refusal error", err)
	}
	// The decoy the symlink points at must be untouched — no flock, no
	// truncation, nothing written through it.
	if fi, statErr := os.Stat(decoy); statErr != nil || fi.Size() != 0 {
		t.Errorf("decoy lock target was modified (size=%v, err=%v), want untouched", fi, statErr)
	}
	// And no data file must have been written either: the refusal must
	// happen before any record is appended.
	if _, statErr := os.Stat(filepath.Join(dir, fileName)); !os.IsNotExist(statErr) {
		t.Errorf("data file exists after a refused lock acquisition: %v", statErr)
	}
}

// TestAppend_ConcurrentAppendsWithSymlinkedLockAlwaysRefuse is the race-
// relevant form of the lock-file test: N goroutines hit the SAME symlinked
// lock file concurrently. Every single one must refuse — none may race past
// the O_NOFOLLOW open and win a flock on the decoy.
func TestAppend_ConcurrentAppendsWithSymlinkedLockAlwaysRefuse(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	decoy := elsewhereFile(t, "")
	symlinkOrSkip(t, decoy, filepath.Join(dir, lockFileName))

	const n = 32
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = Append(dir, "home", []string{"k"})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Errorf("goroutine %d: Append with a symlinked lock file = %v, want a symlink-refusal error", i, err)
		}
	}
	if fi, statErr := os.Stat(decoy); statErr != nil || fi.Size() != 0 {
		t.Errorf("decoy lock target was modified under concurrent refusal (size=%v, err=%v), want untouched", fi, statErr)
	}
}

// --- data file (recreate.log.json) ------------------------------------------

// TestRead_RefusesSymlinkedDataFile: readRecordsFile opens fileName with
// openNoFollow. Unlike a genuinely absent file (which Read reports as zero
// records, no error — see TestRead_MissingFileReturnsNoRecordsNoError), a
// SYMLINKED data file is a distinct, non-benign case and must be a hard
// error, never silently treated as "no log yet" nor followed to read
// whatever the link points at.
func TestRead_RefusesSymlinkedDataFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	decoy := elsewhereFile(t, `[{"timestamp":"2020-01-01T00:00:00Z","environment":"evil","changed_key_paths":["x"]}]`)
	symlinkOrSkip(t, decoy, filepath.Join(dir, fileName))

	records, err := Read(dir)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Read with a symlinked data file = (%v, %v), want a symlink-refusal error", records, err)
	}
	if records != nil {
		t.Errorf("Read with a symlinked data file returned %d records, want none (the decoy's content must never surface)", len(records))
	}
}

// TestAppend_RefusesSymlinkedDataFile proves the SAME refusal reaches
// Append, which reads the existing log (readRecordsFile) before appending:
// a symlinked data file must abort the whole read-modify-write, never fall
// through to writing a fresh log over (or through) the link.
func TestAppend_RefusesSymlinkedDataFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	decoy := elsewhereFile(t, `[{"timestamp":"2020-01-01T00:00:00Z","environment":"evil","changed_key_paths":["x"]}]`)
	symlinkOrSkip(t, decoy, filepath.Join(dir, fileName))

	if err := Append(dir, "home", []string{"a.b"}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Append with a symlinked data file = %v, want a symlink-refusal error", err)
	}
	// The symlink itself must still be exactly that: a link to the decoy,
	// never replaced by a fresh log (which would mean the temp-file+rename
	// path ran despite the read-side refusal).
	fi, err := os.Lstat(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatalf("Lstat data file after refused Append: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("data file is no longer a symlink after a refused Append; the write side must never have run")
	}
	if raw, err := os.ReadFile(decoy); err != nil || !strings.Contains(string(raw), "evil") {
		t.Errorf("decoy content changed after a refused Append: %q, err=%v", raw, err)
	}
}

// --- temp file (recreate.log.json.tmp) --------------------------------------

// TestAppend_RefusesSymlinkedTempFile: writeRecordsFile opens the temp file
// (path+".tmp") with openNoFollow before writing the marshaled records and
// renaming into place. A pre-existing symlink at the temp name — left over
// from a prior crash, or planted by an attacker predicting the fixed temp
// name — must be refused, never written through: writeRecordsFile uses a
// FIXED temp name (unlike sys.AtomicWriteInDir's randomized CreateTemp), so
// this is the one place in this package a predictable temp path is an
// actual attack surface, and openNoFollow is the entire defense.
func TestAppend_RefusesSymlinkedTempFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	decoy := elsewhereFile(t, "")
	symlinkOrSkip(t, decoy, filepath.Join(dir, fileName+".tmp"))

	if err := Append(dir, "home", []string{"a.b"}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Append with a symlinked temp file = %v, want a symlink-refusal error", err)
	}
	if fi, statErr := os.Stat(decoy); statErr != nil || fi.Size() != 0 {
		t.Errorf("decoy temp-file target was modified (size=%v, err=%v), want untouched", fi, statErr)
	}
	// No live data file may exist either: the rename that would publish it
	// never got a chance to run.
	if _, statErr := os.Stat(filepath.Join(dir, fileName)); !os.IsNotExist(statErr) {
		t.Errorf("data file exists after a refused temp-file open: %v", statErr)
	}
	// The symlink at the temp name itself must survive untouched — proof
	// the refusal happened at open(2), before any attempt to unlink or
	// overwrite it.
	fi, err := os.Lstat(filepath.Join(dir, fileName+".tmp"))
	if err != nil {
		t.Fatalf("Lstat temp file after refused Append: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("temp file is no longer a symlink after a refused Append")
	}
}

// TestOpenNoFollow_ELOOPIsTheRefusalMechanism pins the underlying syscall
// behavior the three tests above rely on: openNoFollow's refusal is
// syscall.ELOOP from O_NOFOLLOW hitting a symlink at open(2) itself — the
// same single syscall that would otherwise follow it — not a prior,
// separable Lstat check a racing replace could slip past.
func TestOpenNoFollow_ELOOPIsTheRefusalMechanism(t *testing.T) {
	dir := t.TempDir()
	decoy := elsewhereFile(t, "")
	link := filepath.Join(dir, "link")
	symlinkOrSkip(t, decoy, link)

	_, rawErr := syscall.Open(link, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if !errors.Is(rawErr, syscall.ELOOP) {
		t.Fatalf("raw syscall.Open(O_NOFOLLOW) on a symlink = %v, want syscall.ELOOP (the fixture is not exercising the refusal path this package relies on)", rawErr)
	}

	_, err := openNoFollow(link, syscall.O_RDONLY, 0)
	if err == nil {
		t.Fatal("openNoFollow on a symlink = nil error, want refusal")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("openNoFollow(%q) = %v, want an error mentioning 'symlink'", link, err)
	}
}
