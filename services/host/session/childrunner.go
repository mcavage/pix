package session

import (
	"fmt"
)

// ChildRunOpts identifies exactly which node a child-runner invocation is
// responsible for and what it should run. It is the parsed form of the
// hidden `pix` child-runner invocation's flags (cmd/pix/sessionctl.go),
// kept here — beside Hold/PutNode — so the identity fields a runner must
// get right (tree/node/parent/sandbox/instance) are validated in exactly
// one place, independent of how they arrived on the command line.
type ChildRunOpts struct {
	SandboxDir string // directory CountHolders/Hold key their refs under
	StoreRoot  string // session.Store root the node record lives under
	TreeID     string
	NodeID     string
	ParentID   string
	Sandbox    string
	InstanceID string
	Request    ChildRequest
}

// Executor runs the bounded work a child node was created for. The first
// implementation's only supported Target is local-process
// (architecture §7.2); a real Executor for it re-enters the same sandbox
// (`sbx exec <sandbox> -- pi ...`) built from the request's bounded fields
// only, never from a caller-supplied argv. Tests substitute a fake so the
// hold/release timing this package guarantees can be proven without a real
// sandbox.
type Executor func(req ChildRequest) error

// RunChild is the child-runner's whole job, architecture §7.2's shape made
// literal: acquire the instance-bound reference (so a live child is always
// counted), advance the node running -> finished|failed exactly once
// (monotonic — PutNode already refuses to move backwards), run the bounded
// work, and release the reference on every path out, INCLUDING a failure to
// acquire it at all (nothing runs unheld). The reference is held for the
// executor's ENTIRE duration, which is what lets this process keep the
// sandbox alive after an interactive root's own reference has already
// closed (TestChildSurvivesRootClose proves this across real processes).
func RunChild(o ChildRunOpts, exec Executor) error {
	if err := o.Request.Validate(); err != nil {
		return err
	}
	if err := CheckTarget(Target(o.Request.Target)); err != nil {
		return err
	}
	store := Store{Root: o.StoreRoot}
	node := Node{
		ID:         o.NodeID,
		Parent:     o.ParentID,
		Model:      o.Request.Model,
		Target:     Target(o.Request.Target),
		Sandbox:    o.Sandbox,
		InstanceID: o.InstanceID,
		State:      StateStarting,
	}
	if err := store.PutNode(o.TreeID, node); err != nil {
		return fmt.Errorf("pix: session child could not record its starting node: %w", err)
	}

	holder, err := Hold(o.SandboxDir, o.TreeID, o.NodeID, o.InstanceID)
	if err != nil {
		node.State = StateFailed
		_ = store.PutNode(o.TreeID, node)
		return fmt.Errorf("pix: session child could not take its reference: %w", err)
	}
	defer holder.Release()

	node.State = StateRunning
	if err := store.PutNode(o.TreeID, node); err != nil {
		return fmt.Errorf("pix: session child could not record its running node: %w", err)
	}

	runErr := exec(o.Request)

	node.State = StateFinished
	if runErr != nil {
		node.State = StateFailed
	}
	if perr := store.PutNode(o.TreeID, node); perr != nil {
		// The reference still releases (the defer above); a record-write
		// failure at the very end must not turn into an unreleased holder.
		if runErr != nil {
			return fmt.Errorf("%w (and could not record finish: %v)", runErr, perr)
		}
		return fmt.Errorf("pix: session child could not record its finish: %w", perr)
	}
	return runErr
}
