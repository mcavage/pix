package session

import (
	"errors"
	"path/filepath"
	"testing"
)

func newRunOpts(t *testing.T, req ChildRequest) ChildRunOpts {
	t.Helper()
	dir := t.TempDir()
	return ChildRunOpts{
		SandboxDir: filepath.Join(dir, "sandbox"),
		StoreRoot:  filepath.Join(dir, "trees"),
		TreeID:     "", // filled by caller after CreateTree
		NodeID:     "child1",
		ParentID:   "root1",
		Sandbox:    "pix-proj-1234abcd",
		InstanceID: "inst-1",
		Request:    req,
	}
}

func mustTree(t *testing.T, storeRoot string) string {
	t.Helper()
	tree, err := (Store{Root: storeRoot}).CreateTree("work", "/home/u/proj")
	if err != nil {
		t.Fatalf("CreateTree: %v", err)
	}
	root := Node{ID: "root1", Model: "anthropic/claude-sonnet-5", Target: TargetLocalProcess,
		Sandbox: "pix-proj-1234abcd", InstanceID: "inst-1", State: StateRunning}
	if err := (Store{Root: storeRoot}).PutNode(tree.ID, root); err != nil {
		t.Fatalf("PutNode(root): %v", err)
	}
	return tree.ID
}

func TestRunChild_HoldsWhileExecutorRuns_ThenFinishes(t *testing.T) {
	o := newRunOpts(t, ChildRequest{Agent: "fanout", Task: "t", Target: "local-process"})
	o.TreeID = mustTree(t, o.StoreRoot)

	var sawHeld bool
	err := RunChild(o, func(req ChildRequest) error {
		c := CountHolders(o.SandboxDir, o.InstanceID)
		sawHeld = c.Known && c.N == 1 && len(c.Nodes) == 1 && c.Nodes[0] == "child1"
		return nil
	})
	if err != nil {
		t.Fatalf("RunChild: %v", err)
	}
	if !sawHeld {
		t.Fatal("executor did not observe its own node held")
	}
	if c := CountHolders(o.SandboxDir, o.InstanceID); !c.Zero() {
		t.Fatalf("after RunChild returns the reference must be released; census = %+v", c)
	}
	store := Store{Root: o.StoreRoot}
	node, err := store.ReadNode(o.TreeID, "child1")
	if err != nil {
		t.Fatalf("ReadNode: %v", err)
	}
	if node.State != StateFinished {
		t.Fatalf("node state = %q, want finished", node.State)
	}
	if node.Parent != "root1" {
		t.Fatalf("node parent = %q, want root1", node.Parent)
	}
}

func TestRunChild_ExecutorFailureIsRecordedAndReleased(t *testing.T) {
	o := newRunOpts(t, ChildRequest{Agent: "fanout", Task: "t", Target: "local-process"})
	o.TreeID = mustTree(t, o.StoreRoot)

	wantErr := errors.New("boom")
	err := RunChild(o, func(req ChildRequest) error { return wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("RunChild err = %v, want %v", err, wantErr)
	}
	if c := CountHolders(o.SandboxDir, o.InstanceID); !c.Zero() {
		t.Fatalf("a failed executor must still release; census = %+v", c)
	}
	node, rerr := (Store{Root: o.StoreRoot}).ReadNode(o.TreeID, "child1")
	if rerr != nil {
		t.Fatalf("ReadNode: %v", rerr)
	}
	if node.State != StateFailed {
		t.Fatalf("node state = %q, want failed", node.State)
	}
}

func TestRunChild_RefusesUnsupportedTarget(t *testing.T) {
	o := newRunOpts(t, ChildRequest{Agent: "fanout", Task: "t", Target: "local-sandbox"})
	o.TreeID = mustTree(t, o.StoreRoot)

	called := false
	err := RunChild(o, func(req ChildRequest) error { called = true; return nil })
	var unsupported *UnsupportedTargetError
	if !errors.As(err, &unsupported) {
		t.Fatalf("RunChild err = %v, want *UnsupportedTargetError", err)
	}
	if called {
		t.Fatal("executor must not run for an unsupported target")
	}
	if c := CountHolders(o.SandboxDir, o.InstanceID); !c.Zero() {
		t.Fatalf("refused target must never take a reference; census = %+v", c)
	}
}

func TestRunChild_RefusesSecondHolderOfSameNode(t *testing.T) {
	o := newRunOpts(t, ChildRequest{Agent: "fanout", Task: "t", Target: "local-process"})
	o.TreeID = mustTree(t, o.StoreRoot)

	holder, err := Hold(o.SandboxDir, o.TreeID, o.NodeID, o.InstanceID)
	if err != nil {
		t.Fatalf("pre-hold: %v", err)
	}
	defer holder.Release()

	err = RunChild(o, func(req ChildRequest) error { return nil })
	if err == nil {
		t.Fatal("expected RunChild to refuse when its own node ref is already held")
	}
	node, rerr := (Store{Root: o.StoreRoot}).ReadNode(o.TreeID, "child1")
	if rerr != nil {
		t.Fatalf("ReadNode: %v", rerr)
	}
	if node.State != StateFailed {
		t.Fatalf("node state = %q, want failed", node.State)
	}
}
