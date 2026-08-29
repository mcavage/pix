// hmackey_test.go — E2.2's red-first proof for the ONE launcher-owned
// creation-fingerprint HMAC key record: generated once, private (0600),
// symlink-safe, atomic, and never a low-entropy-resolved-value
// offline-guessing oracle.
package hosttrust

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestLoadCreationHMACKey_MissingIsExactSentinel(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadCreationHMACKey(dir); !errors.Is(err, ErrCreationHMACKeyMissing) {
		t.Fatalf("LoadCreationHMACKey(empty dir) error = %v, want errors.Is ErrCreationHMACKeyMissing", err)
	}
}

func TestEnsureCreationHMACKey_GeneratedOnce(t *testing.T) {
	dir := t.TempDir()
	first, err := EnsureCreationHMACKey(dir)
	if err != nil {
		t.Fatalf("EnsureCreationHMACKey (first): %v", err)
	}
	if len(first) != creationHMACKeyLen {
		t.Fatalf("key length = %d, want %d", len(first), creationHMACKeyLen)
	}
	second, err := EnsureCreationHMACKey(dir)
	if err != nil {
		t.Fatalf("EnsureCreationHMACKey (second): %v", err)
	}
	if string(first) != string(second) {
		t.Error("a second call generated a DIFFERENT key; it must load the one already stored")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	keyFiles := 0
	for _, e := range entries {
		if e.Name() == creationHMACKeyName {
			keyFiles++
		}
	}
	if keyFiles != 1 {
		t.Errorf("found %d files named %q in %s, want exactly 1", keyFiles, creationHMACKeyName, dir)
	}
}

func TestEnsureCreationHMACKey_ConcurrentFirstLaunchesConverge(t *testing.T) {
	dir := t.TempDir()
	const n = 8
	results := make([][]byte, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = EnsureCreationHMACKey(dir)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: EnsureCreationHMACKey: %v", i, err)
		}
	}
	for i := 1; i < n; i++ {
		if string(results[i]) != string(results[0]) {
			t.Fatalf("goroutine %d generated a DIFFERENT key than goroutine 0; concurrent first launches must converge on ONE key", i)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	keyFiles := 0
	for _, e := range entries {
		if e.Name() == creationHMACKeyName {
			keyFiles++
		}
	}
	if keyFiles != 1 {
		t.Errorf("found %d files named %q after %d concurrent generators, want exactly 1 — no second key file anywhere", keyFiles, creationHMACKeyName, n)
	}
}

func TestEnsureCreationHMACKey_FilePrivateMode0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file mode bits don't apply on windows")
	}
	dir := t.TempDir()
	if _, err := EnsureCreationHMACKey(dir); err != nil {
		t.Fatalf("EnsureCreationHMACKey: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, creationHMACKeyName))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("key file mode = %o, want 0600 (private: owner read/write only)", got)
	}
}

func TestSaveCreationHMACKey_RefusesSymlinkedDestination(t *testing.T) {
	dir := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "attacker-target")
	if err := os.WriteFile(elsewhere, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, creationHMACKeyName)
	if err := os.Symlink(elsewhere, dest); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := saveCreationHMACKey(dir, make([]byte, creationHMACKeyLen)); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("saveCreationHMACKey through a symlinked destination = %v, want a symlink-refusal error", err)
	}
	// The attacker's file must be untouched — the refusal happened before
	// any write, atomic or otherwise.
	got, err := os.ReadFile(elsewhere)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "not a key" {
		t.Error("a refused symlinked destination must never have its target's content modified")
	}
}

func TestLoadCreationHMACKey_RefusesSymlinkedSource(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(t.TempDir(), "real-key.json")
	if err := os.WriteFile(real, []byte(`{"key_hex":"00"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, creationHMACKeyName)
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := LoadCreationHMACKey(dir); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("LoadCreationHMACKey(symlinked source) = %v, want a symlink-refusal error", err)
	}
}

// ── SignResolvedValue: keyed HMAC, not an offline-guessable oracle ──────

// TestSignResolvedValue_LowEntropyValueNotOfflineGuessable is the sentinel
// E2.2 calls for explicitly: a real resolved value is often a short,
// low-entropy token ("prod", "admin123", a 6-digit PIN). An UNKEYED hash of
// one is trivially reproduced offline by brute-forcing the small candidate
// space and comparing digests — the digest itself becomes an oracle for
// the value. This test proves SignResolvedValue is NOT that: the same
// low-entropy value produces a DIFFERENT digest under a different key, so
// an attacker who only has the persisted digest (never the key — it is
// 0600 and launcher-only) cannot confirm a guessed value by brute-forcing
// SHA-256 alone; they would also have to guess the 32-byte random key,
// which is computationally infeasible.
func TestSignResolvedValue_LowEntropyValueNotOfflineGuessable(t *testing.T) {
	const lowEntropyValue = "admin123" // an 8-char, easily-brute-forced token

	keyA := make([]byte, creationHMACKeyLen)
	keyB := make([]byte, creationHMACKeyLen)
	for i := range keyA {
		keyA[i] = byte(i)
		keyB[i] = byte(i + 1)
	}

	digestA := SignResolvedValue(keyA, lowEntropyValue)
	digestB := SignResolvedValue(keyB, lowEntropyValue)
	if digestA == digestB {
		t.Fatal("the same low-entropy value produced the SAME digest under two different keys — the key is not load-bearing")
	}

	// The classic offline-guessing attack: hash every value in a small
	// candidate dictionary WITHOUT the key and compare to the persisted
	// digest. Against an UNKEYED SHA-256 this attack trivially recovers
	// "admin123"; against SignResolvedValue it must find no match at all,
	// because the digest space it is comparing against was never plain
	// SHA-256 of the value.
	candidates := []string{"admin123", "password", "letmein", "123456", "qwerty", "admin", "guest"}
	for _, c := range candidates {
		sum := sha256.Sum256([]byte(c))
		unkeyed := hex.EncodeToString(sum[:])
		if unkeyed == digestA {
			t.Fatalf("candidate %q's UNKEYED sha256 matched the keyed digest — the key contributed nothing", c)
		}
	}
}

func TestSignResolvedValue_NeverReturnsTheRawValue(t *testing.T) {
	key := make([]byte, creationHMACKeyLen)
	const value = "s3cr3t-raw-value"
	digest := SignResolvedValue(key, value)
	if strings.Contains(digest, value) {
		t.Fatalf("digest %q contains the raw resolved value", digest)
	}
	if digest == value {
		t.Fatal("digest must never equal the raw resolved value")
	}
}
