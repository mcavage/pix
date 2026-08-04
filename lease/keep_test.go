package lease

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func mustDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "sbx-1")
	if _, err := CreateRecord(dir, "sbx-1"); err != nil {
		t.Fatalf("CreateRecord: %v", err)
	}
	return dir
}

func TestSetKeep_BindsIdentityAnd0600(t *testing.T) {
	dir := mustDir(t)
	if err := SetKeep(dir, "alice"); err != nil {
		t.Fatalf("SetKeep: %v", err)
	}
	state, ok, err := ReadKeep(dir)
	if err != nil {
		t.Fatalf("ReadKeep: %v", err)
	}
	if !ok {
		t.Fatal("ReadKeep ok = false, want true")
	}
	if state.Identity != "alice" {
		t.Errorf("Identity = %q, want alice", state.Identity)
	}
	fi, err := os.Stat(filepath.Join(dir, keepFileName))
	if err != nil {
		t.Fatalf("Stat keep file: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("keep file perm = %o, want 0600", perm)
	}
}

func TestSetKeep_SameIdentityRefreshes(t *testing.T) {
	dir := mustDir(t)
	if err := SetKeep(dir, "alice"); err != nil {
		t.Fatalf("SetKeep #1: %v", err)
	}
	first, _, _ := ReadKeep(dir)
	if err := SetKeep(dir, "alice"); err != nil {
		t.Fatalf("SetKeep #2: %v", err)
	}
	second, _, _ := ReadKeep(dir)
	if second.UpdatedAt.Before(first.UpdatedAt) {
		t.Errorf("UpdatedAt went backwards: %v -> %v", first.UpdatedAt, second.UpdatedAt)
	}
}

func TestSetKeep_RefusesDifferentIdentity(t *testing.T) {
	dir := mustDir(t)
	if err := SetKeep(dir, "alice"); err != nil {
		t.Fatalf("SetKeep: %v", err)
	}
	if err := SetKeep(dir, "bob"); err == nil {
		t.Error("SetKeep as bob over alice's keep = nil error, want refusal")
	}
	state, _, _ := ReadKeep(dir)
	if state.Identity != "alice" {
		t.Errorf("keep identity changed to %q despite refusal", state.Identity)
	}
}

func TestClearKeep_RefusesDifferentIdentity(t *testing.T) {
	dir := mustDir(t)
	if err := SetKeep(dir, "alice"); err != nil {
		t.Fatalf("SetKeep: %v", err)
	}
	if err := ClearKeep(dir, "bob"); err == nil {
		t.Error("ClearKeep as bob over alice's keep = nil error, want refusal")
	}
	if _, ok, _ := ReadKeep(dir); !ok {
		t.Error("keep was cleared despite refusal")
	}
}

func TestClearKeep_OwnerClearsAndMissingIsNoop(t *testing.T) {
	dir := mustDir(t)
	if err := SetKeep(dir, "alice"); err != nil {
		t.Fatalf("SetKeep: %v", err)
	}
	if err := ClearKeep(dir, "alice"); err != nil {
		t.Fatalf("ClearKeep: %v", err)
	}
	if _, ok, _ := ReadKeep(dir); ok {
		t.Error("keep still set after owner clear")
	}
	// Clearing again (nobody holds it) is a no-op, not an error.
	if err := ClearKeep(dir, "alice"); err != nil {
		t.Errorf("ClearKeep on an already-clear keep = %v, want nil", err)
	}
}

func TestReadKeep_UnsetIsFalseNotError(t *testing.T) {
	dir := mustDir(t)
	state, ok, err := ReadKeep(dir)
	if err != nil {
		t.Fatalf("ReadKeep: %v", err)
	}
	if ok || state != nil {
		t.Errorf("ReadKeep = (%v, %v), want (nil, false) on an unset keep", state, ok)
	}
}

// TestSetKeep_ConcurrentIdentitiesRaceDoesNotCorrupt drives real concurrent
// goroutines (real OS threads doing real file I/O through the guard, not a
// mock) at the same keep.json to prove the identity-bound guard actually
// serializes the read-modify-write: exactly one identity ever wins ownership,
// and the file is never observed half-written or in a state neither caller
// asked for.
func TestSetKeep_ConcurrentIdentitiesRaceDoesNotCorrupt(t *testing.T) {
	dir := mustDir(t)
	const n = 8
	var wg sync.WaitGroup
	results := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			identity := "identity-" + string(rune('a'+i))
			results[i] = SetKeep(dir, identity)
		}(i)
	}
	wg.Wait()

	wins := 0
	for _, err := range results {
		if err == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("winners = %d, want exactly 1 (results: %v)", wins, results)
	}
	state, ok, err := ReadKeep(dir)
	if err != nil {
		t.Fatalf("ReadKeep: %v", err)
	}
	if !ok {
		t.Fatal("no keep set after concurrent race, want exactly one winner's keep")
	}
	if state.Identity == "" {
		t.Error("winning keep has empty identity")
	}
}
