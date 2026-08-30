package secret

import (
	"errors"
	"os"
	"strings"
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
