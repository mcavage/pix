package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// A holder is one live host-side process that still depends on a sandbox.
// It is represented by an EXCLUSIVE flock on its own reference file under
// state/sandboxes/<sandbox>/refs/. The kernel releases that lock on normal
// exit, on a signal, and on SIGKILL, which is why the lock — never the
// node record, never a PID — is what teardown consults.
//
// One file per node (rather than one shared lock file) is what lets a
// delegated CHILD outlive the interactive ROOT: root closing its own file
// releases only its own lock, and a census still finds the child's.

// refFileMode keeps a reference file private to the user, like every other
// machine-state file under PIX_HOME.
const refFileMode = 0o600

// interactiveRootRef is the fixed reference name the interactive root
// takes. There is exactly one per sandbox, which is what makes a second
// interactive root detectable at all.
const interactiveRootRef = "root-interactive"

// refPayload is what a reference file carries: the sbx instance the holder
// is bound to. A reference that names a different instance is not this
// sandbox's holder — it is leftover state from a previous instance that
// happened to reuse the name.
type refPayload struct {
	Schema     string `json:"schema"`
	Node       string `json:"node"`
	Tree       string `json:"tree"`
	InstanceID string `json:"instance_id"`
}

// ErrSecondInteractiveRoot is the refusal for a second interactive `pix
// run` against the SAME live sandbox (PRD: "A second interactive root for
// the same live sandbox is refused; delegated child nodes are allowed").
// Two interactive roots would share one terminal-bound Pi session's
// sandbox while each believing it owns teardown.
var ErrSecondInteractiveRoot = errors.New("pix: this sandbox already has a live interactive session; refusing to start a second one (delegated child agents are allowed)")

// Holder is one acquired reference lock. Release closes it; so does
// process exit, which is the point.
type Holder struct {
	path string
	f    *os.File
	node string
}

// Path is the reference file this holder owns.
func (h *Holder) Path() string { return h.path }

// Release drops the lock and removes the reference file. Removal is best
// effort: a leftover FILE with no LOCK is correctly counted as not-a-holder
// by Census, so failing to unlink can never fabricate a holder.
func (h *Holder) Release() error {
	if h == nil || h.f == nil {
		return nil
	}
	err := h.f.Close()
	h.f = nil
	_ = os.Remove(h.path)
	return err
}

// RefsDir is the reference directory for one sandbox record dir.
func RefsDir(sandboxDir string) string { return filepath.Join(sandboxDir, "refs") }

// Hold acquires the reference lock for nodeID under sandboxDir, binding it
// to instanceID. A busy lock means another live process already holds THIS
// node, which is a caller bug for an ordinary node and the second-root
// case for the interactive root.
func Hold(sandboxDir, treeID, nodeID, instanceID string) (*Holder, error) {
	if err := safeID("node", nodeID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(instanceID) == "" {
		return nil, fmt.Errorf("pix: a session reference must be bound to an sbx instance id")
	}
	dir := RefsDir(sandboxDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, nodeID+".ref")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, refFileMode)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			if nodeID == interactiveRootRef {
				return nil, ErrSecondInteractiveRoot
			}
			return nil, fmt.Errorf("pix: session node %s is already held by a live process", nodeID)
		}
		return nil, err
	}
	payload, _ := json.Marshal(refPayload{Schema: "1", Node: nodeID, Tree: treeID, InstanceID: instanceID})
	if err := f.Truncate(0); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.WriteAt(append(payload, '\n'), 0); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, err
	}
	return &Holder{path: path, f: f, node: nodeID}, nil
}

// HoldInteractiveRoot takes the one interactive-root reference for this
// sandbox. A busy lock is ErrSecondInteractiveRoot, not a generic failure.
func HoldInteractiveRoot(sandboxDir, treeID, instanceID string) (*Holder, error) {
	return Hold(sandboxDir, treeID, interactiveRootRef, instanceID)
}

// Census is a holder count that can be UNKNOWN. An unreadable refs
// directory, a malformed reference, or a reference bound to a different
// instance all produce Known=false: a census that cannot see clearly must
// never report zero, because zero authorizes teardown.
type Census struct {
	Known bool
	N     int
	// Nodes are the node ids positively observed as live, sorted by the
	// order they were read. Diagnostics only.
	Nodes []string
	// Reason explains an unknown census.
	Reason string
}

// Zero reports a POSITIVE zero-holder answer.
func (c Census) Zero() bool { return c.Known && c.N == 0 }

// CountHolders takes the live-holder census for one sandbox record dir,
// bound to instanceID. A reference whose lock can be taken exclusively is
// STALE (its owner died) and is not counted; a reference whose lock is busy
// is LIVE.
func CountHolders(sandboxDir, instanceID string) Census {
	if strings.TrimSpace(instanceID) == "" {
		return Census{Reason: "no recorded sbx instance id to bind references to"}
	}
	dir := RefsDir(sandboxDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// No refs directory at all is a positive zero: nothing was
			// ever held for this sandbox on this host.
			return Census{Known: true}
		}
		return Census{Reason: fmt.Sprintf("session reference directory %s could not be read: %v", dir, err)}
	}
	c := Census{Known: true}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".ref") {
			continue
		}
		path := filepath.Join(dir, name)
		live, err := refIsLive(path, instanceID)
		if err != nil {
			return Census{Reason: fmt.Sprintf("session reference %s could not be classified: %v", path, err)}
		}
		if live {
			c.N++
			c.Nodes = append(c.Nodes, strings.TrimSuffix(name, ".ref"))
		}
	}
	return c
}

// refIsLive classifies one reference file. It is deliberately conservative
// in one direction only: a reference that cannot be parsed, or that names
// a DIFFERENT instance while still being locked, is an error (unknown
// census), never a silent "not a holder".
func refIsLive(path, instanceID string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR, refFileMode)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		// We took it, so nobody holds it: stale. Drop the lock and the
		// file so the next census does less work.
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = os.Remove(path)
		return false, nil
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) {
		return false, err
	}
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		return false, rerr
	}
	var p refPayload
	if len(data) == 0 || json.Unmarshal(data, &p) != nil {
		return false, fmt.Errorf("reference is locked but its payload is unreadable")
	}
	if p.InstanceID != instanceID {
		return false, fmt.Errorf("reference is bound to instance %q, not %q", p.InstanceID, instanceID)
	}
	return true, nil
}
