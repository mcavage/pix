package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
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

// TestFromFacetMap_RoundTripsThroughDiff (E2.2) proves FromFacetMap is a
// plain type conversion, not a second comparison engine: a facet map from
// a richer key space (envinfo.ComputeFingerprint's composed keys, modeled
// here as a plain map literal since sandbox may not import envinfo, an L1
// sibling) drives Diff/Equal exactly the way a native Fingerprint literal
// already does.
func TestFromFacetMap_RoundTripsThroughDiff(t *testing.T) {
	storedFacets := map[string]string{"env.FOO": "bar", "mcp.servers[github].url": "https://a"}
	currentFacets := map[string]string{"env.FOO": "baz", "mcp.servers[github].url": "https://a"}

	stored := FromFacetMap(storedFacets)
	current := FromFacetMap(currentFacets)

	got := Diff(stored, current)
	want := []string{"env.FOO"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Diff(FromFacetMap(...)) = %v, want %v", got, want)
	}
	if Equal(stored, current) {
		t.Fatal("Equal must be false when env.FOO changed")
	}
	// Every reported key is a human-readable facet name, never a hash.
	for _, k := range got {
		if bareHashREForFingerprintTest.MatchString(k) {
			t.Errorf("diverged key %q looks hash-only, never allowed", k)
		}
	}
}

var bareHashREForFingerprintTest = regexp.MustCompile(`^[0-9a-f]{16,}$`)
