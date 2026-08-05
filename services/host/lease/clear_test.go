//go:build unix

package lease

import (
	"os"
	"path/filepath"
	"testing"
)

// TestClearState_RemovesEveryLeaseFileAndTheDir: the whole state this package
// owns goes, dir included — the record FIRST, so a later create for the same
// key is not refused as a relabel.
func TestClearState_RemovesEveryLeaseFileAndTheDir(t *testing.T) {
	root := t.TempDir()
	dir, err := SandboxDir(root, "pix-demo-a1b2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CreateRecord(dir, "inst-1"); err != nil {
		t.Fatal(err)
	}
	if err := SetKeep(dir, "me@host"); err != nil {
		t.Fatal(err)
	}
	// Both locks exist on disk once anything has opened them.
	rl, err := OpenRefLease(dir)
	if err != nil {
		t.Fatal(err)
	}
	rl.Close()
	lc, err := OpenLifecycleLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	lc.Close()

	if err := ClearState(dir); err != nil {
		t.Fatalf("ClearState: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		entries, _ := os.ReadDir(dir)
		t.Fatalf("lease dir survived ClearState (err %v), leftovers: %v", err, entries)
	}
	if _, err := ReadRecord(dir); err == nil {
		t.Fatal("the record survived ClearState; the next create would be refused as a relabel")
	}
}

// TestClearState_UnderTheReapProof: clearing while HOLDING both locks (the
// only state a real teardown clears from) works, and the unlinked lock files
// do not leave a future acquirer holding a lock that excludes nobody.
func TestClearState_UnderTheReapProof(t *testing.T) {
	root := t.TempDir()
	dir, err := SandboxDir(root, "pix-demo-a1b2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CreateRecord(dir, "inst-1"); err != nil {
		t.Fatal(err)
	}
	var cleared error
	if err := TryReapProof(dir, func() error {
		cleared = ClearState(dir)
		return nil
	}); err != nil {
		t.Fatalf("TryReapProof: %v", err)
	}
	if cleared != nil {
		t.Fatalf("ClearState under the proof: %v", cleared)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("lease dir survived a proven clear: %v", err)
	}
	// A fresh lifetime for the same key starts clean.
	if err := EnsureSandboxDir(dir); err != nil {
		t.Fatal(err)
	}
	if rec, err := CreateRecord(dir, "inst-2"); err != nil || rec.InstanceID != "inst-2" {
		t.Fatalf("re-create after clear = (%+v, %v), want instance inst-2", rec, err)
	}
}

// TestClearState_LeavesADirWithForeignState: a file this package never wrote
// keeps the directory — ClearState deletes lease state, not somebody else's.
func TestClearState_LeavesADirWithForeignState(t *testing.T) {
	root := t.TempDir()
	dir, err := SandboxDir(root, "pix-demo-a1b2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CreateRecord(dir, "inst-1"); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(dir, "fingerprint.json")
	if err := os.WriteFile(foreign, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ClearState(dir); err != nil {
		t.Fatalf("ClearState: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir with foreign state must survive: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("foreign file must survive: %v", err)
	}
	if _, err := ReadRecord(dir); err == nil {
		t.Fatal("the record must still be gone")
	}
}

// TestClearState_RefusesASymlinkedLeaseDir: the same posture every other path
// in this package holds — a symlink where a lease dir belongs is refused, not
// followed into somebody else's tree.
func TestClearState_RefusesASymlinkedLeaseDir(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "pix-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := ClearState(link); err == nil {
		t.Fatal("ClearState followed a symlinked lease dir")
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the symlink target must be untouched: %v", err)
	}
}
