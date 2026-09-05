package session

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func sandboxDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sandboxes", "pix-proj-1234abcd")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestChildHolderOutlivesRoot is the session-tree property the whole lease
// design exists for: the interactive root may exit while a delegated child
// is still running, and the sandbox must remain held.
func TestChildHolderOutlivesRoot(t *testing.T) {
	dir := sandboxDir(t)
	root, err := HoldInteractiveRoot(dir, "tree1", "inst-1")
	if err != nil {
		t.Fatalf("HoldInteractiveRoot: %v", err)
	}
	child, err := Hold(dir, "tree1", "child1", "inst-1")
	if err != nil {
		t.Fatalf("Hold(child): %v", err)
	}

	if c := CountHolders(dir, "inst-1"); !c.Known || c.N != 2 {
		t.Fatalf("census with root+child = %+v; want 2 known holders", c)
	}
	if err := root.Release(); err != nil {
		t.Fatalf("root Release: %v", err)
	}
	c := CountHolders(dir, "inst-1")
	if !c.Known || c.N != 1 || c.Zero() {
		t.Fatalf("after root exit the child must still hold the sandbox; census = %+v", c)
	}
	if len(c.Nodes) != 1 || c.Nodes[0] != "child1" {
		t.Fatalf("census must name the surviving child, got %v", c.Nodes)
	}
	if err := child.Release(); err != nil {
		t.Fatalf("child Release: %v", err)
	}
	if c := CountHolders(dir, "inst-1"); !c.Zero() {
		t.Fatalf("after the last holder exits the census must be a POSITIVE zero; got %+v", c)
	}
}

// TestSecondInteractiveRootRefused: two interactive roots for one live
// sandbox is the case the PRD closes out explicitly.
func TestSecondInteractiveRootRefused(t *testing.T) {
	dir := sandboxDir(t)
	first, err := HoldInteractiveRoot(dir, "tree1", "inst-1")
	if err != nil {
		t.Fatalf("first root: %v", err)
	}
	defer first.Release()

	if _, err := HoldInteractiveRoot(dir, "tree2", "inst-1"); !errors.Is(err, ErrSecondInteractiveRoot) {
		t.Fatalf("second interactive root must be refused with ErrSecondInteractiveRoot, got %v", err)
	}
	// A delegated child is still allowed while the root is live.
	child, err := Hold(dir, "tree1", "child1", "inst-1")
	if err != nil {
		t.Fatalf("a child must be allowed alongside a live root: %v", err)
	}
	child.Release()
}

// TestStaleReferenceIsNotAHolder: a reference file left behind by a killed
// process holds no lock, so it must never keep a sandbox alive forever.
func TestStaleReferenceIsNotAHolder(t *testing.T) {
	dir := sandboxDir(t)
	refs := RefsDir(dir)
	if err := os.MkdirAll(refs, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(refs, "dead1.ref")
	if err := os.WriteFile(stale, []byte(`{"schema":"1","node":"dead1","instance_id":"inst-1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c := CountHolders(dir, "inst-1")
	if !c.Zero() {
		t.Fatalf("an unlocked reference must not count as a holder; census = %+v", c)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("a stale reference should be swept, stat err = %v", err)
	}
}

// TestCensusFailsClosed: every case where the census cannot see clearly
// must report UNKNOWN, never zero — zero is what authorizes teardown.
func TestCensusFailsClosed(t *testing.T) {
	t.Run("no instance id", func(t *testing.T) {
		if c := CountHolders(sandboxDir(t), " "); c.Known {
			t.Fatalf("a census with no instance to bind to must be unknown, got %+v", c)
		}
	})
	t.Run("reference bound to another instance", func(t *testing.T) {
		dir := sandboxDir(t)
		h, err := Hold(dir, "tree1", "n1", "inst-OLD")
		if err != nil {
			t.Fatal(err)
		}
		defer h.Release()
		c := CountHolders(dir, "inst-NEW")
		if c.Known || c.Zero() {
			t.Fatalf("a live reference bound to a different instance must be unknown, got %+v", c)
		}
	})
	t.Run("locked but unreadable payload", func(t *testing.T) {
		dir := sandboxDir(t)
		h, err := Hold(dir, "tree1", "n1", "inst-1")
		if err != nil {
			t.Fatal(err)
		}
		defer h.Release()
		if err := os.WriteFile(h.Path(), []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if c := CountHolders(dir, "inst-1"); c.Known {
			t.Fatalf("an unparseable live reference must be unknown, got %+v", c)
		}
	})
}

// TestNoRefsDirectoryIsPositiveZero: a sandbox nothing ever held is a
// positive zero, not an unknown — otherwise no sandbox could ever be torn
// down on a host that crashed before writing its first reference.
func TestNoRefsDirectoryIsPositiveZero(t *testing.T) {
	if c := CountHolders(sandboxDir(t), "inst-1"); !c.Zero() {
		t.Fatalf("a sandbox with no refs dir must be a positive zero, got %+v", c)
	}
}

// TestHoldRequiresInstanceBinding: a reference that is not bound to an sbx
// instance cannot be matched against a live sandbox later.
func TestHoldRequiresInstanceBinding(t *testing.T) {
	if _, err := Hold(sandboxDir(t), "tree1", "n1", ""); err == nil {
		t.Fatalf("an unbound reference must be refused")
	}
}
