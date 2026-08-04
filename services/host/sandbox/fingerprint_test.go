package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func readFingerprint(t *testing.T, name string) Fingerprint {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var fp Fingerprint
	if err := json.Unmarshal(data, &fp); err != nil {
		t.Fatalf("unmarshal fixture %s: %v", name, err)
	}
	return fp
}

// TestDiff_RealFixtures loads two REAL stored/current fingerprint files (a
// changed "image", an added "static_mcp" entry — expressed as an unequal
// value since Fingerprint is flat string->string, and an unchanged
// "workspace_digest"/"kit") and asserts exactly the changed keys are
// reported.
func TestDiff_RealFixtures(t *testing.T) {
	stored := readFingerprint(t, "fingerprint_stored.json")
	current := readFingerprint(t, "fingerprint_current.json")

	got := Diff(stored, current)
	want := []string{"image", "static_mcp"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Diff = %v, want %v", got, want)
	}
	if Equal(stored, current) {
		t.Fatalf("Equal(stored, current) = true, want false")
	}
}

func TestDiff_IdenticalIsEmpty(t *testing.T) {
	stored := readFingerprint(t, "fingerprint_stored.json")
	same := readFingerprint(t, "fingerprint_stored.json")
	if got := Diff(stored, same); len(got) != 0 {
		t.Fatalf("Diff(identical) = %v, want empty", got)
	}
	if !Equal(stored, same) {
		t.Fatalf("Equal(identical) = false, want true")
	}
}

func TestDiff_AddedAndRemovedKeysCountAsDrift(t *testing.T) {
	stored := Fingerprint{"a": "1", "b": "2"}
	current := Fingerprint{"a": "1", "c": "3"}
	got := Diff(stored, current)
	want := []string{"b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Diff = %v, want %v", got, want)
	}
}
