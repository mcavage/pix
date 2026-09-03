package secret

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"pix/host/pixhome"
)

func testHome(t *testing.T) pixhome.Paths {
	t.Helper()
	return pixhome.New(t.TempDir())
}

func TestLoadRefs_AbsentIsEmpty(t *testing.T) {
	home := testHome(t)
	refs, err := LoadRefs(home)
	if err != nil {
		t.Fatalf("LoadRefs: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("expected no refs, got %v", refs)
	}
}

func TestSetRef_RejectsLiteralValue(t *testing.T) {
	home := testHome(t)
	err := SetRef(home, "API_KEY", "sk-not-a-reference")
	if err == nil {
		t.Fatal("expected an error for a literal secret value")
	}
	var target *NotAnOpRefError
	if !errors.As(err, &target) {
		t.Fatalf("expected *NotAnOpRefError, got %T: %v", err, err)
	}
	// The rejected value must never appear written to disk.
	if _, statErr := os.Stat(RefsEnvPath(home)); statErr == nil {
		data, _ := os.ReadFile(RefsEnvPath(home))
		if strings.Contains(string(data), "sk-not-a-reference") {
			t.Fatal("a rejected literal value was written to secrets.env")
		}
	}
}

func TestSetPlainValue_RoundTripsAndNeverRequiresAnOpRef(t *testing.T) {
	home := testHome(t)
	if err := SetPlainValue(home, "GOG_ACCOUNT", "you@docker.com"); err != nil {
		t.Fatalf("SetPlainValue: %v", err)
	}
	val, present := PlainValue(home, "GOG_ACCOUNT")
	if !present || val != "you@docker.com" {
		t.Fatalf("PlainValue = %q, %v", val, present)
	}
	data, err := os.ReadFile(RefsEnvPath(home))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "op://") {
		t.Fatalf("a plain value must never be recorded as an op:// reference:\n%s", data)
	}
}

func TestSetPlainValue_RejectsAnOpRefValue(t *testing.T) {
	home := testHome(t)
	if err := SetPlainValue(home, "GOG_ACCOUNT", "op://Vault/gog/account"); err == nil {
		t.Fatal("an op:// value passed to SetPlainValue must be refused (it belongs in env_keys, not plain_keys)")
	}
}

func TestSetPlainValue_RejectsEmptyValue(t *testing.T) {
	home := testHome(t)
	if err := SetPlainValue(home, "GOG_ACCOUNT", "   "); err == nil {
		t.Fatal("an empty non-secret value must be refused")
	}
}

func TestPlainValue_AbsentIsNotPresent(t *testing.T) {
	home := testHome(t)
	if _, present := PlainValue(home, "GOG_ACCOUNT"); present {
		t.Fatal("expected not present for a name never recorded")
	}
}

func TestSetRef_RejectsInvalidKey(t *testing.T) {
	home := testHome(t)
	err := SetRef(home, "not a var!", "op://vault/item/field")
	if err == nil {
		t.Fatal("expected an error for an invalid env var name")
	}
}

func TestSetRef_AcceptsOpRefAndRoundTrips(t *testing.T) {
	home := testHome(t)
	if err := SetRef(home, "GITHUB_TOKEN", "op://Vault/GitHub/token"); err != nil {
		t.Fatalf("SetRef: %v", err)
	}
	refs, err := LoadRefs(home)
	if err != nil {
		t.Fatalf("LoadRefs: %v", err)
	}
	if len(refs) != 1 || refs[0].Key != "GITHUB_TOKEN" || refs[0].Value != "op://Vault/GitHub/token" || !refs[0].IsRef {
		t.Fatalf("refs = %+v", refs)
	}
}

func TestSetRef_StripsQuotedPaste(t *testing.T) {
	home := testHome(t)
	if err := SetRef(home, "KEY", `"op://Vault/Item/field"`); err != nil {
		t.Fatalf("SetRef: %v", err)
	}
	refs, _ := LoadRefs(home)
	if len(refs) != 1 || refs[0].Value != "op://Vault/Item/field" {
		t.Fatalf("refs = %+v", refs)
	}
}

func TestSetRef_FileMode0600(t *testing.T) {
	home := testHome(t)
	if err := SetRef(home, "KEY", "op://Vault/Item/field"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(RefsEnvPath(home))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestSetRef_UpsertPreservesOtherLines(t *testing.T) {
	home := testHome(t)
	if err := SetRef(home, "FIRST", "op://Vault/First/field"); err != nil {
		t.Fatal(err)
	}
	if err := SetRef(home, "SECOND", "op://Vault/Second/field"); err != nil {
		t.Fatal(err)
	}
	if err := SetRef(home, "FIRST", "op://Vault/First/updated"); err != nil {
		t.Fatal(err)
	}
	refs, _ := LoadRefs(home)
	byKey := map[string]string{}
	for _, r := range refs {
		byKey[r.Key] = r.Value
	}
	if byKey["FIRST"] != "op://Vault/First/updated" || byKey["SECOND"] != "op://Vault/Second/field" {
		t.Fatalf("refs = %+v", byKey)
	}
}

func TestRemoveRef_IdempotentOnMissingFile(t *testing.T) {
	home := testHome(t)
	if err := RemoveRef(home, "NOPE"); err != nil {
		t.Fatalf("RemoveRef on a missing file should be a no-op, got %v", err)
	}
}

func TestRemoveRef_RemovesOnlyNamedKey(t *testing.T) {
	home := testHome(t)
	if err := SetRef(home, "A", "op://V/A/f"); err != nil {
		t.Fatal(err)
	}
	if err := SetRef(home, "B", "op://V/B/f"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveRef(home, "A"); err != nil {
		t.Fatalf("RemoveRef: %v", err)
	}
	refs, _ := LoadRefs(home)
	if len(refs) != 1 || refs[0].Key != "B" {
		t.Fatalf("refs = %+v", refs)
	}
}

type fakeOpReader struct {
	fail map[string]bool
}

func (f fakeOpReader) ReadRef(ref string) error {
	if f.fail[ref] {
		return errors.New("op: item not found")
	}
	return nil
}

func TestCheckRef_NeverReturnsValue(t *testing.T) {
	reader := fakeOpReader{}
	err := CheckRef(reader, "op://Vault/Item/field")
	if err != nil {
		t.Fatalf("CheckRef: %v", err)
	}
}

func TestCheckRef_RejectsNonOpRef(t *testing.T) {
	err := CheckRef(fakeOpReader{}, "plain-literal")
	if err == nil {
		t.Fatal("expected an error for a non-op:// value")
	}
}

func TestCheckRef_PropagatesFailureWithoutValue(t *testing.T) {
	reader := fakeOpReader{fail: map[string]bool{"op://Vault/Missing/field": true}}
	err := CheckRef(reader, "op://Vault/Missing/field")
	if err == nil {
		t.Fatal("expected an error for a failing reference")
	}
}

// TestSetRef_ConcurrentWritesNeverLoseAReference is the security re-review
// MEDIUM fix's proof: N goroutines each SetRef a DIFFERENT key concurrently
// (the unlocked v1-style read-modify-write this finding named would
// interleave two of these and silently drop one), and every single key must
// still be present afterward.
func TestSetRef_ConcurrentWritesNeverLoseAReference(t *testing.T) {
	home := testHome(t)
	const n = 25
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("KEY_%d", i)
			val := fmt.Sprintf("op://Vault/Item%d/field", i)
			errs[i] = SetRef(home, key, val)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("SetRef(KEY_%d): %v", i, err)
		}
	}

	refs, err := LoadRefs(home)
	if err != nil {
		t.Fatalf("LoadRefs: %v", err)
	}
	if len(refs) != n {
		t.Fatalf("secrets.env has %d refs after %d concurrent SetRef calls, want %d (a race dropped one) — got %+v", len(refs), n, n, refs)
	}
	seen := map[string]bool{}
	for _, r := range refs {
		seen[r.Key] = true
	}
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("KEY_%d", i)
		if !seen[key] {
			t.Errorf("%s missing after concurrent SetRef — a race dropped it", key)
		}
	}
}

// TestSetRefRemoveRef_ConcurrentMixDoesNotCorruptFile interleaves SetRef and
// RemoveRef across many goroutines and proves the file is left in SOME
// internally-consistent state (every surviving line still parses as a valid
// ref) rather than a torn write from two unlocked writers racing each
// other's temp-file-then-rename.
func TestSetRefRemoveRef_ConcurrentMixDoesNotCorruptFile(t *testing.T) {
	home := testHome(t)
	// Seed every key first so RemoveRef has something to race against.
	const n = 20
	for i := 0; i < n; i++ {
		if err := SetRef(home, fmt.Sprintf("MIX_%d", i), fmt.Sprintf("op://Vault/Mix%d/field", i)); err != nil {
			t.Fatalf("seed SetRef: %v", err)
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("MIX_%d", i)
			if i%2 == 0 {
				_ = RemoveRef(home, key)
			} else {
				_ = SetRef(home, key, fmt.Sprintf("op://Vault/Mix%d/updated", i))
			}
		}(i)
	}
	wg.Wait()

	refs, err := LoadRefs(home)
	if err != nil {
		t.Fatalf("LoadRefs after concurrent mix: %v", err)
	}
	for _, r := range refs {
		if !r.IsRef {
			t.Fatalf("secrets.env corrupted: %+v is not a clean op:// ref", r)
		}
	}
	// Every odd key (SetRef) must have survived; every even key (RemoveRef)
	// must be gone. Wrong on either side means a lost or resurrected update.
	byKey := map[string]OpRef{}
	for _, r := range refs {
		byKey[r.Key] = r
	}
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("MIX_%d", i)
		r, present := byKey[key]
		if i%2 == 0 {
			if present {
				t.Errorf("%s: RemoveRef lost a race and the key is still present", key)
			}
			continue
		}
		if !present || r.Value != fmt.Sprintf("op://Vault/Mix%d/updated", i) {
			t.Errorf("%s: SetRef update lost a race, got %+v", key, r)
		}
	}
}
