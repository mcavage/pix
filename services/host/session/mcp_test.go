package session

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

func newTestServer(t *testing.T) (*Server, *[]ChildRequest) {
	t.Helper()
	dir := t.TempDir()
	storeRoot := filepath.Join(dir, "trees")
	tree := mustTree(t, storeRoot) // also plants the "root1" parent node
	ctx := ServerContext{
		StoreRoot:  storeRoot,
		SandboxDir: filepath.Join(dir, "sandbox"),
		TreeID:     tree,
		ParentID:   "root1",
		Sandbox:    "pix-proj-1234abcd",
		InstanceID: "inst-1",
	}
	var spawned []ChildRequest
	ids := 0
	s := NewServer(ctx, func(ctx ServerContext, treeID, nodeID string, req ChildRequest) error {
		spawned = append(spawned, req)
		return nil
	}, nil, nil)
	s.NewID = func() (string, error) { ids++; return "node-" + string(rune('0'+ids)), nil }
	return s, &spawned
}

func TestDelegate_RecordsNodeAndSpawns(t *testing.T) {
	s, spawned := newTestServer(t)
	raw := json.RawMessage(`{"agent":"fanout","task":"do it","target":"local-process"}`)
	res, err := s.Delegate(raw)
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if res.Tree != s.Ctx.TreeID || res.Node == "" {
		t.Fatalf("result = %+v", res)
	}
	if len(*spawned) != 1 || (*spawned)[0].Agent != "fanout" {
		t.Fatalf("spawned = %+v", *spawned)
	}
	node, err := (Store{Root: s.Ctx.StoreRoot}).ReadNode(s.Ctx.TreeID, res.Node)
	if err != nil {
		t.Fatalf("ReadNode: %v", err)
	}
	if node.Parent != "root1" {
		t.Fatalf("node parent = %q, want root1", node.Parent)
	}
}

func TestDelegate_RefusesGeneralCommandField(t *testing.T) {
	s, spawned := newTestServer(t)
	raw := json.RawMessage(`{"agent":"fanout","task":"do it","target":"local-process","shell":"rm -rf /"}`)
	if _, err := s.Delegate(raw); err == nil {
		t.Fatal("expected a refusal for an unbounded field")
	}
	if len(*spawned) != 0 {
		t.Fatal("must not spawn anything for a refused request")
	}
}

func TestDelegate_RefusesUnsupportedTarget(t *testing.T) {
	s, spawned := newTestServer(t)
	raw := json.RawMessage(`{"agent":"fanout","task":"do it","target":"cloud-sandbox"}`)
	_, err := s.Delegate(raw)
	var unsupported *UnsupportedTargetError
	if !errors.As(err, &unsupported) {
		t.Fatalf("Delegate err = %v, want *UnsupportedTargetError", err)
	}
	if len(*spawned) != 0 {
		t.Fatal("must not spawn for an unsupported target")
	}
}

func TestDelegate_RefusesUnknownTarget(t *testing.T) {
	s, spawned := newTestServer(t)
	raw := json.RawMessage(`{"agent":"fanout","task":"do it","target":"orbital"}`)
	_, err := s.Delegate(raw)
	var unknown *UnknownTargetError
	if !errors.As(err, &unknown) {
		t.Fatalf("Delegate err = %v, want *UnknownTargetError", err)
	}
	if len(*spawned) != 0 {
		t.Fatal("must not spawn for an unknown target")
	}
}

func TestDelegate_SpawnFailureMarksNodeFailed(t *testing.T) {
	s, _ := newTestServer(t)
	s.Spawn = func(ctx ServerContext, treeID, nodeID string, req ChildRequest) error {
		return errors.New("spawn boom")
	}
	raw := json.RawMessage(`{"agent":"fanout","task":"do it","target":"local-process"}`)
	_, err := s.Delegate(raw)
	if err == nil {
		t.Fatal("expected an error when spawn fails")
	}
}

func TestSessionDelegateTool_HasNoGeneralCommandProperty(t *testing.T) {
	tool := sessionDelegateTool()
	schema := tool["inputSchema"].(map[string]interface{})
	props := schema["properties"].(map[string]interface{})
	for _, forbidden := range []string{"command", "argv", "shell", "cmd"} {
		if _, ok := props[forbidden]; ok {
			t.Fatalf("tool schema must never declare %q", forbidden)
		}
	}
	if schema["additionalProperties"] != false {
		t.Fatal("tool schema must set additionalProperties: false")
	}
}
