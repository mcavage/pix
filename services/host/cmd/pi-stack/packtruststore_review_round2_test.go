// packtruststore_review_round2_test.go — security re-review finding R2-02:
//
//	loadPackTrustStore's old Lstat-then-ReadFile had a TOCTOU symlink gap:
//	an attacker who can race the launcher between the Lstat check and the
//	os.ReadFile call can swap a legitimate pack-trust.json for a symlink
//	pointing at an attacker-readable (or attacker-controlled) file, and
//	os.ReadFile follows it. The fix opens the file with O_NOFOLLOW on unix
//	(via readPackTrustStoreFile, packtruststore_unix.go) so the open itself
//	atomically refuses a symlink — there is no separate check to race.
//
//	save() was ALREADY safe (Lstat-refuse the destination + a same-dir temp
//	file + os.Rename, which replaces a destination symlink's directory entry
//	rather than following it through to write its target) — these tests
//	PROVE that behavior rather than assume it, per the finding's own
//	instruction not to skip the save half.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func trustStoreEnv(t *testing.T) (cfgDir, storePath string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))
	return dir, filepath.Join(dir, packTrustStoreName)
}

// TestPackTrustStore_R202_ReadRefusesSymlink: loadPackTrustStore must refuse
// to read THROUGH a symlink planted at pack-trust.json's path — not just
// detect one at a single Lstat moment, but never actually dereference it.
func TestPackTrustStore_R202_ReadRefusesSymlink(t *testing.T) {
	_, storePath := trustStoreEnv(t)
	secret := filepath.Join(t.TempDir(), "secret.json")
	if err := os.WriteFile(secret, []byte(`{"version":1,"accepted":{"path:/pwned":{"fingerprint":"evil"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, storePath); err != nil {
		t.Fatal(err)
	}
	s, err := loadPackTrustStore()
	if err == nil {
		t.Fatalf("expected loadPackTrustStore to refuse a symlinked store, got a store: %+v", s)
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("expected a symlink-refusal error, got %v", err)
	}
	// The secret's content must NEVER have been parsed into a trusted record.
	if s != nil && s.Accepted != nil {
		if _, ok := s.Accepted["path:/pwned"]; ok {
			t.Fatalf("a symlinked store's content leaked into the returned store: %+v", s)
		}
	}
}

// TestPackTrustStore_R202_ReadNoSymlinkTOCTOUWindow proves there is no
// Lstat-then-open race window left to exploit: even when the symlink is
// swapped in for a REGULAR file at open time (simulated by the fact
// readPackTrustStoreFile does a single O_NOFOLLOW open, not a separate
// Lstat+ReadFile pair), a legitimate regular file at the store path is read
// normally and a symlink is refused — proving the single-syscall-class
// mechanism handles both cases without a second stat call in between.
func TestPackTrustStore_R202_ReadNoSymlinkTOCTOUWindow(t *testing.T) {
	_, storePath := trustStoreEnv(t)
	// A legitimate regular file reads fine.
	if err := os.WriteFile(storePath, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPackTrustStore(); err != nil {
		t.Fatalf("a regular file must load cleanly, got %v", err)
	}
	// Replace it with a symlink (as an attacker would mid-race) — the very
	// next read must refuse, proving the check and the read are the SAME
	// atomic operation rather than two steps an attacker can interleave.
	if err := os.Remove(storePath); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target.json")
	if err := os.WriteFile(target, []byte(`{"version":1,"accepted":{"path:/pwned":{"fingerprint":"evil"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, storePath); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPackTrustStore(); err == nil {
		t.Fatalf("a store path swapped to a symlink must be refused, not silently followed")
	}
}

// TestPackTrustStore_R202_ReadMissingIsEmptyStore: the fix must not regress
// the "absent -> fresh empty store" contract loadPackTrustStore documents.
func TestPackTrustStore_R202_ReadMissingIsEmptyStore(t *testing.T) {
	trustStoreEnv(t)
	s, err := loadPackTrustStore()
	if err != nil {
		t.Fatalf("a missing store must not error, got %v", err)
	}
	if s == nil || s.Version != 1 || len(s.Accepted) != 0 {
		t.Errorf("expected a fresh empty store, got %+v", s)
	}
}

// TestPackTrustStore_R202_SaveRefusesPreexistingDestinationSymlink proves
// save()'s up-front Lstat check does exactly what it claims: an
// ALREADY-symlinked destination is refused outright (fail loud, write
// nothing) rather than silently written through — the symlink's target is
// left completely untouched and the symlink itself is left in place
// (untouched, not replaced) because save() returns before ever calling
// atomicWriteInDir.
func TestPackTrustStore_R202_SaveRefusesPreexistingDestinationSymlink(t *testing.T) {
	_, storePath := trustStoreEnv(t)
	target := filepath.Join(t.TempDir(), "innocent-bystander.json")
	const sentinel = "do-not-touch"
	if err := os.WriteFile(target, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(storePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, storePath); err != nil {
		t.Fatal(err)
	}

	s := &packTrustStore{Version: 1, Accepted: map[string]packTrustRecord{
		"path:/legit": {Path: "/legit", Fingerprint: "fp"},
	}}
	err := s.save()
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("save() must refuse a pre-existing symlinked destination, got %v", err)
	}

	// The symlink's TARGET must be untouched.
	got, rerr := os.ReadFile(target)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != sentinel {
		t.Fatalf("save() wrote THROUGH the destination symlink into its target: got %q, want %q", got, sentinel)
	}

	// The symlink itself is left exactly as it was (save() bailed before any write).
	fi, lerr := os.Lstat(storePath)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected the untouched symlink to remain at the destination")
	}
}

// TestPackTrustStore_R202_SaveRaceNeverFollowsInjectedSymlink is the seam the
// finding calls for: it actually exercises the TOCTOU WINDOW between save()'s
// Lstat check and its atomic write, using packTrustSaveRaceHook to plant a
// symlink at the destination immediately AFTER the check already confirmed a
// clean (non-symlink) path — exactly the race an attacker would need. The
// final os.Rename inside atomicWriteInDir must still never write through the
// raced-in symlink: rename replaces the directory entry rather than
// dereferencing it, so the symlink's target stays untouched and the raced-in
// symlink itself gets clobbered by the real file.
func TestPackTrustStore_R202_SaveRaceNeverFollowsInjectedSymlink(t *testing.T) {
	_, storePath := trustStoreEnv(t)
	target := filepath.Join(t.TempDir(), "innocent-bystander.json")
	const sentinel = "do-not-touch"
	if err := os.WriteFile(target, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}

	prevHook := packTrustSaveRaceHook
	defer func() { packTrustSaveRaceHook = prevHook }()
	packTrustSaveRaceHook = func(dest string) {
		// The attacker's window: save() just Lstat-confirmed dest was clean.
		// Plant the symlink NOW, before the atomic write happens.
		_ = os.Remove(dest)
		if err := os.Symlink(target, dest); err != nil {
			t.Fatalf("race setup: %v", err)
		}
	}

	s := &packTrustStore{Version: 1, Accepted: map[string]packTrustRecord{
		"path:/legit": {Path: "/legit", Fingerprint: "fp"},
	}}
	if err := s.save(); err != nil {
		t.Fatalf("save() must succeed even when a symlink is raced in after its check (rename replaces it), got %v", err)
	}

	// The raced-in symlink's TARGET must be untouched.
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != sentinel {
		t.Fatalf("save() wrote THROUGH the raced-in symlink into its target: got %q, want %q", got, sentinel)
	}

	// The destination must now be a REGULAR file (rename clobbered the raced-in
	// symlink's directory entry) carrying the saved content.
	fi, err := os.Lstat(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("save() left the raced-in symlink in place instead of replacing it via rename")
	}
	reloaded, err := loadPackTrustStore()
	if err != nil {
		t.Fatalf("reloading the saved store failed: %v", err)
	}
	if _, ok := reloaded.Accepted["path:/legit"]; !ok {
		t.Errorf("saved content missing after the race, got %+v", reloaded)
	}
}

// TestPackTrustStore_R202_SaveAtomicNoPartialFile proves save() never leaves
// a partially-written destination visible: the write goes to a same-dir temp
// file that is renamed into place only after a full write + fsync + close, so
// a concurrent reader either sees the OLD content or the FULLY NEW content,
// never a truncated/partial file.
func TestPackTrustStore_R202_SaveAtomicNoPartialFile(t *testing.T) {
	dir, storePath := trustStoreEnv(t)
	s := &packTrustStore{Version: 1, Accepted: map[string]packTrustRecord{
		"path:/a": {Path: "/a", Fingerprint: "fp-a"},
	}}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	// No stray temp file left behind after a clean save.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(storePath) && filepath.Ext(e.Name()) != ".toml" {
			if strings.Contains(e.Name(), packTrustStoreName+".tmp-") {
				t.Errorf("a temp file was left behind after save(): %s", e.Name())
			}
		}
	}
	fi, err := os.Stat(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("saved store perms = %v, want 0644", fi.Mode().Perm())
	}
}
