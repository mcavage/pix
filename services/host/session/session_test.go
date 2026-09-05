package session

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newStore(t *testing.T) Store {
	t.Helper()
	return Store{Root: filepath.Join(t.TempDir(), "sessions")}
}

func TestTreeAndNodeParentage(t *testing.T) {
	s := newStore(t)
	tree, err := s.CreateTree("work", "/home/u/proj")
	if err != nil {
		t.Fatalf("CreateTree: %v", err)
	}
	root := Node{ID: "root1", Environment: "work", Model: "anthropic/claude-sonnet-5",
		Workspace: "/home/u/proj", Target: TargetLocalProcess, Sandbox: "pix-proj-1234abcd",
		InstanceID: "inst-1", State: StateRunning}
	if err := s.PutNode(tree.ID, root); err != nil {
		t.Fatalf("PutNode(root): %v", err)
	}
	child := root
	child.ID = "child1"
	child.Parent = "root1"
	if err := s.PutNode(tree.ID, child); err != nil {
		t.Fatalf("PutNode(child): %v", err)
	}

	got, err := s.ReadNode(tree.ID, "child1")
	if err != nil {
		t.Fatalf("ReadNode: %v", err)
	}
	if got.Parent != "root1" || got.Root() {
		t.Fatalf("child lost its parentage: %+v", got)
	}
	nodes, err := s.ListNodes(tree.ID)
	if err != nil || len(nodes) != 2 {
		t.Fatalf("ListNodes = %d nodes, %v; want 2", len(nodes), err)
	}

	orphan := root
	orphan.ID = "orphan1"
	orphan.Parent = "nope"
	if err := s.PutNode(tree.ID, orphan); err == nil {
		t.Fatalf("a node naming a parent that is not in the tree must be refused")
	}
}

func TestNodeStateIsMonotonic(t *testing.T) {
	s := newStore(t)
	tree, _ := s.CreateTree("work", "/w")
	n := Node{ID: "n1", Target: TargetLocalProcess, State: StateRunning, Sandbox: "pix-a-1", InstanceID: "i"}
	if err := s.PutNode(tree.ID, n); err != nil {
		t.Fatalf("PutNode: %v", err)
	}
	n.State = StateFinished
	if err := s.PutNode(tree.ID, n); err != nil {
		t.Fatalf("running -> finished must be allowed: %v", err)
	}
	fin, _ := s.ReadNode(tree.ID, "n1")
	if fin.FinishedAt == "" {
		t.Fatalf("a terminal node must record finished_at")
	}
	n.State = StateRunning
	if err := s.PutNode(tree.ID, n); err == nil {
		t.Fatalf("finished -> running must be refused")
	}
	n.State = "wedged"
	if err := s.PutNode(tree.ID, n); err == nil {
		t.Fatalf("an unknown state must be refused")
	}
}

func TestTargetsSchemaVersusCapability(t *testing.T) {
	if err := CheckTarget(TargetLocalProcess); err != nil {
		t.Fatalf("local-process must be supported: %v", err)
	}
	for _, target := range []Target{TargetLocalSandbox, TargetCloudSandbox} {
		err := CheckTarget(target)
		var unsupported *UnsupportedTargetError
		if !errors.As(err, &unsupported) {
			t.Fatalf("%s must be a CAPABILITY error, got %v", target, err)
		}
	}
	var unknown *UnknownTargetError
	if err := CheckTarget(Target("somewhere-else")); !errors.As(err, &unknown) {
		t.Fatalf("an unknown target must be a schema error, got %v", err)
	}
	// A future target is schema-valid, so it can be PERSISTED even though
	// this build cannot run it. That is what stops a later cloud child
	// from needing a second session model.
	s := newStore(t)
	tree, _ := s.CreateTree("work", "/w")
	if err := s.PutNode(tree.ID, Node{ID: "n1", Target: TargetCloudSandbox, State: StateStarting}); err != nil {
		t.Fatalf("a known-but-unsupported target must still be schema-valid: %v", err)
	}
}

func TestUnknownSchemaIsRefused(t *testing.T) {
	s := newStore(t)
	tree, _ := s.CreateTree("work", "/w")
	if err := os.WriteFile(s.treePath(tree.ID), []byte(`{"schema":99,"id":"`+tree.ID+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var se *SchemaError
	if _, err := s.ReadTree(tree.ID); !errors.As(err, &se) {
		t.Fatalf("a future schema must be refused, got %v", err)
	}
}

func TestIDsCannotEscapeTheStore(t *testing.T) {
	s := newStore(t)
	if _, err := s.ReadTree("../../etc"); err == nil {
		t.Fatalf("a traversal id must be refused")
	}
	if err := s.PutNode("tree", Node{ID: "../oops", Target: TargetLocalProcess, State: StateRunning}); err == nil {
		t.Fatalf("a traversal node id must be refused")
	}
}
