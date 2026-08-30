// Package session persists the Pix session tree: one tree per `pix run`,
// one node per live agent, and the parentage between them
// (docs/design/pix-v2-architecture.md §7, docs/design/pix-v2-surface.md
// §3.1).
//
// The split this package enforces everywhere is: RECORDS ARE HISTORY,
// LOCKS ARE LIVENESS. A node file says what a node was doing when it last
// wrote; a crash leaves a stale `running` behind and that is allowed. What
// a sandbox teardown consults is never the node file but the reference
// lock held by the process responsible for that node (holder.go), because
// a lock closes on normal exit, on a signal, and on SIGKILL, while a
// record does not. PIDs are diagnostics and are never proof.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Schema is the on-disk version of a tree/node record. A record written by
// a NEWER Pix is refused rather than guessed at (see SchemaError): reading
// an unknown shape as if it were this one is how a future field silently
// becomes a lost holder.
const Schema = 1

// Target is where a node executes. All three values exist in the schema
// from the first implementation on purpose (architecture §7.2): a future
// local- or cloud-sandbox child must fit THIS session model rather than
// growing a parallel one, so the vocabulary ships before the capability.
type Target string

const (
	TargetLocalProcess Target = "local-process"
	TargetLocalSandbox Target = "local-sandbox"
	TargetCloudSandbox Target = "cloud-sandbox"
)

// KnownTargets are the values the schema accepts. Supported() is the
// narrower set this build can actually run.
func KnownTargets() []Target {
	return []Target{TargetLocalProcess, TargetLocalSandbox, TargetCloudSandbox}
}

// Supported reports whether this build can execute a node on t. Only
// local-process is implemented; the other two are schema-valid and return
// a capability error at the point of use.
func (t Target) Supported() bool { return t == TargetLocalProcess }

// Known reports whether t is a schema-valid target at all.
func (t Target) Known() bool {
	for _, k := range KnownTargets() {
		if t == k {
			return true
		}
	}
	return false
}

// UnsupportedTargetError is the CAPABILITY error for a schema-valid target
// this build cannot run. It is deliberately distinct from an unknown-value
// error: "Pix cannot do this yet" and "this is not a target" are different
// answers, and only the first one is a promise about the future.
type UnsupportedTargetError struct{ Target Target }

func (e *UnsupportedTargetError) Error() string {
	return fmt.Sprintf("pix: session target %q is not supported by this build (supported: %s)",
		string(e.Target), string(TargetLocalProcess))
}

// UnknownTargetError is the schema error for a value that is not a target.
type UnknownTargetError struct{ Value string }

func (e *UnknownTargetError) Error() string {
	names := make([]string, 0, 3)
	for _, t := range KnownTargets() {
		names = append(names, string(t))
	}
	return fmt.Sprintf("pix: unknown session target %q (known: %s)", e.Value, strings.Join(names, ", "))
}

// CheckTarget validates a requested target: unknown values are schema
// errors, known-but-unimplemented values are capability errors.
func CheckTarget(t Target) error {
	if !t.Known() {
		return &UnknownTargetError{Value: string(t)}
	}
	if !t.Supported() {
		return &UnsupportedTargetError{Target: t}
	}
	return nil
}

// SchemaError is the refusal for a record written by a different Pix.
type SchemaError struct {
	Path string
	Got  int
}

func (e *SchemaError) Error() string {
	return fmt.Sprintf("pix: session record %s has schema %d, this build understands %d; refusing to read it",
		e.Path, e.Got, Schema)
}

// State is a node's lifecycle state. Transitions are monotonic:
// starting -> running -> finished|failed, and never backwards.
type State string

const (
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateFinished State = "finished"
	StateFailed   State = "failed"
)

func stateRank(s State) (int, bool) {
	switch s {
	case StateStarting:
		return 0, true
	case StateRunning:
		return 1, true
	case StateFinished, StateFailed:
		return 2, true
	}
	return 0, false
}

// Terminal reports whether s is an end state.
func (s State) Terminal() bool { return s == StateFinished || s == StateFailed }

// Tree is one `pix run`'s session tree.
type Tree struct {
	Schema      int    `json:"schema"`
	ID          string `json:"id"`
	Environment string `json:"environment"`
	Workspace   string `json:"workspace"`
	CreatedAt   string `json:"created_at"`
}

// Node is one live (or historical) agent in a tree. The field set is
// architecture §7.1's record, byte for byte.
type Node struct {
	Schema      int    `json:"schema"`
	ID          string `json:"id"`
	Parent      string `json:"parent"`
	Environment string `json:"environment"`
	Model       string `json:"model"`
	Workspace   string `json:"workspace"`
	Target      Target `json:"target"`
	Sandbox     string `json:"sandbox"`
	InstanceID  string `json:"instance_id"`
	State       State  `json:"state"`
	CreatedAt   string `json:"created_at"`
	FinishedAt  string `json:"finished_at"`
}

// Root reports whether n is a tree's root node (no parent).
func (n Node) Root() bool { return strings.TrimSpace(n.Parent) == "" }

// Store is the session record store, rooted at PIX_HOME/state/sessions.
type Store struct{ Root string }

// NewID returns a random, stable, filesystem-safe identifier. Tree and node
// IDs are random rather than derived so nothing about a session leaks into
// a path, and two trees for the same workspace never collide.
func NewID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func (s Store) treeDir(id string) string  { return filepath.Join(s.Root, id) }
func (s Store) treePath(id string) string { return filepath.Join(s.treeDir(id), "tree.json") }
func (s Store) nodesDir(id string) string { return filepath.Join(s.treeDir(id), "nodes") }
func (s Store) nodePath(treeID, nodeID string) string {
	return filepath.Join(s.nodesDir(treeID), nodeID+".json")
}

// safeID refuses anything that is not a bare identifier, so no caller can
// steer a record write out of the store with a traversal or a separator.
func safeID(kind, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("pix: %s id is required", kind)
	}
	if strings.ContainsAny(id, `/\.`) || id != strings.TrimSpace(id) {
		return fmt.Errorf("pix: invalid %s id %q", kind, id)
	}
	return nil
}

// CreateTree writes a new tree record and returns it.
func (s Store) CreateTree(environment, workspace string) (Tree, error) {
	id, err := NewID()
	if err != nil {
		return Tree{}, err
	}
	t := Tree{Schema: Schema, ID: id, Environment: environment, Workspace: workspace, CreatedAt: now()}
	if err := os.MkdirAll(s.nodesDir(id), 0o700); err != nil {
		return Tree{}, err
	}
	if err := writeJSON(s.treePath(id), t); err != nil {
		return Tree{}, err
	}
	return t, nil
}

// ReadTree reads one tree record.
func (s Store) ReadTree(id string) (Tree, error) {
	if err := safeID("tree", id); err != nil {
		return Tree{}, err
	}
	var t Tree
	if err := readJSON(s.treePath(id), &t); err != nil {
		return Tree{}, err
	}
	if t.Schema != Schema {
		return Tree{}, &SchemaError{Path: s.treePath(id), Got: t.Schema}
	}
	return t, nil
}

// PutNode writes (or advances) one node record. It refuses an unknown
// target, an unknown state, a non-monotonic transition, and a parent that
// does not exist in the same tree — parentage is the property `pix ls`
// renders and teardown reasons about, so a dangling parent is a bug worth
// refusing at write time rather than rendering as an orphan later.
func (s Store) PutNode(treeID string, n Node) error {
	if err := safeID("tree", treeID); err != nil {
		return err
	}
	if err := safeID("node", n.ID); err != nil {
		return err
	}
	if !n.Target.Known() {
		return &UnknownTargetError{Value: string(n.Target)}
	}
	rank, ok := stateRank(n.State)
	if !ok {
		return fmt.Errorf("pix: unknown session node state %q", string(n.State))
	}
	if p := strings.TrimSpace(n.Parent); p != "" {
		if err := safeID("parent node", p); err != nil {
			return err
		}
		if _, err := s.ReadNode(treeID, p); err != nil {
			return fmt.Errorf("pix: session node %s names parent %s, which is not in tree %s", n.ID, p, treeID)
		}
	}
	if prev, err := s.ReadNode(treeID, n.ID); err == nil {
		prevRank, _ := stateRank(prev.State)
		if rank < prevRank {
			return fmt.Errorf("pix: refusing to move session node %s backwards from %q to %q",
				n.ID, string(prev.State), string(n.State))
		}
		if prev.CreatedAt != "" {
			n.CreatedAt = prev.CreatedAt
		}
	}
	n.Schema = Schema
	if n.CreatedAt == "" {
		n.CreatedAt = now()
	}
	if n.State.Terminal() && n.FinishedAt == "" {
		n.FinishedAt = now()
	}
	if err := os.MkdirAll(s.nodesDir(treeID), 0o700); err != nil {
		return err
	}
	return writeJSON(s.nodePath(treeID, n.ID), n)
}

// ReadNode reads one node record.
func (s Store) ReadNode(treeID, nodeID string) (Node, error) {
	if err := safeID("tree", treeID); err != nil {
		return Node{}, err
	}
	if err := safeID("node", nodeID); err != nil {
		return Node{}, err
	}
	var n Node
	if err := readJSON(s.nodePath(treeID, nodeID), &n); err != nil {
		return Node{}, err
	}
	if n.Schema != Schema {
		return Node{}, &SchemaError{Path: s.nodePath(treeID, nodeID), Got: n.Schema}
	}
	return n, nil
}

// ListNodes returns every node in a tree, sorted by ID for determinism.
func (s Store) ListNodes(treeID string) ([]Node, error) {
	if err := safeID("tree", treeID); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.nodesDir(treeID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Node
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		n, err := s.ReadNode(treeID, strings.TrimSuffix(name, ".json"))
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// ListTrees returns every tree id present in the store, in directory order
// filtered to well-formed ids.
func (s Store) ListTrees() ([]string, error) {
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if safeID("tree", e.Name()) != nil {
			continue
		}
		out = append(out, e.Name())
	}
	return out, nil
}

// writeJSON writes v atomically (same-directory temp file, fsync, rename)
// at mode 0600, the durability rule architecture §5 states for every file
// carrying machine state.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".session-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
