// session_root.go wires the interactive-root reference (architecture
// §7.2, docs/design/pix-v2-surface.md §3.1: "Every `pix run` creates or
// resumes a session tree. Its root is the interactive Pi session.") into
// the ACTUAL `pix run` lifecycle: acquired once this launch has a POSITIVE
// instance receipt for the sandbox it is about to attach the interactive
// session to, refused when a second interactive root is already live for
// the SAME sandbox (session.ErrSecondInteractiveRoot), and released on
// every path out.
//
// It deliberately does NOT re-implement workflow/launch's own lifecycle
// lock / shared-refs ordering (lease.AttachRefUnderLifecycle,
// docs/design/architecture invariant "do not duplicate old lease
// ordering"): it only takes session's own, separate, EXCLUSIVE
// "root-interactive" reference file, keyed by the EXACT SAME lease
// directory launch already established for sessionKey (lease.SandboxDir),
// so it lands beside — never instead of — launch's own refs.lock in that
// one directory.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/lease"
	"pix/host/session"
	"pix/host/workflow/launch"
)

// interactiveRootPollInterval bounds how often awaitInteractiveRootHold
// re-probes for a fresh sandbox's positive instance receipt while
// launch.RunSession is creating it concurrently in the foreground.
const interactiveRootPollInterval = 500 * time.Millisecond

// sessionStoreRoot is where session tree/node records live: this host's
// per-user state dir, alongside (never inside) the sandbox lease tree
// (config.StateDir()/sandboxes) launch already owns.
func sessionStoreRoot() (string, error) {
	state, err := config.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, "sessions"), nil
}

// interactiveRootSandboxDir recomputes the SAME lease directory identity
// workflow/launch's own lifecycle lock already uses for sessionKey, from
// the exported lease.SandboxDir — never a second, independently derived
// path — so session's own reference file always lands in launch's own
// sandbox directory.
func interactiveRootSandboxDir(sessionKey string) (string, error) {
	state, err := config.StateDir()
	if err != nil {
		return "", err
	}
	return lease.SandboxDir(filepath.Join(state, "sandboxes"), sessionKey)
}

// interactiveRoot is one acquired interactive-root reference plus enough
// identity to advance and release its node record. The zero value (a nil
// *interactiveRoot, or one with a nil holder) is always safe to release:
// a Hold attempt that never happened, or that failed, must never crash the
// caller's cleanup path.
type interactiveRoot struct {
	holder *session.Holder
	store  session.Store
	tree   string
	// node is this OCCURRENCE's own node id, distinct from the fixed
	// "root-interactive" lock name every interactive root ever takes as its
	// holder path: a resumed tree's root node record must get a FRESH id each
	// time a new interactive session starts, or a second occurrence's first
	// PutNode would be refused as a backwards transition from the PRIOR
	// occurrence's already-"finished" record. The lock name stays fixed on
	// purpose (that fixed name is what makes a second live root detectable
	// at all); the record's identity does not need to, and must not, reuse
	// it across occurrences.
	node string
}

// release advances the root's node record to its terminal state and drops
// the reference lock. It is best-effort on the record write (a failure to
// write the LAST node transition must never leave the reference lock
// itself held — Release always runs), matching childrunner.go's own
// release-on-every-path-out contract.
func (r *interactiveRoot) release(failed bool) {
	if r == nil || r.holder == nil {
		return
	}
	if node, err := r.store.ReadNode(r.tree, r.node); err == nil {
		node.State = session.StateFinished
		if failed {
			node.State = session.StateFailed
		}
		_ = r.store.PutNode(r.tree, node)
	}
	_ = r.holder.Release()
}

// holdInteractiveRootNow takes the interactive-root reference for a
// POSITIVELY identified instance: the caller already has a real instance
// id in hand (an attach to an already-running, identity-verified sandbox,
// or a freshly created one whose instance this launch just observed), so
// a second live interactive root for the SAME sandbox is refused right
// here — session.ErrSecondInteractiveRoot — rather than discovered later.
func holdInteractiveRootNow(sessionKey, sandboxName, workspace, environment, model, instanceID string) (*interactiveRoot, error) {
	dir, err := interactiveRootSandboxDir(sessionKey)
	if err != nil {
		return nil, fmt.Errorf("pix: could not resolve %s's session directory: %w", sandboxName, err)
	}
	storeRoot, err := sessionStoreRoot()
	if err != nil {
		return nil, fmt.Errorf("pix: could not resolve the session store: %w", err)
	}
	store := session.Store{Root: storeRoot}
	treeID, terr := session.EnsureTree(store, dir, environment, workspace)
	if terr != nil {
		return nil, fmt.Errorf("pix: could not resolve %s's session tree: %w", sandboxName, terr)
	}
	holder, herr := session.HoldInteractiveRoot(dir, treeID, instanceID)
	if herr != nil {
		// session.ErrSecondInteractiveRoot travels verbatim: it is already the
		// exact refusal wording a caller should show.
		return nil, herr
	}
	nodeID, nerr := session.NewID()
	if nerr != nil {
		_ = holder.Release()
		return nil, fmt.Errorf("pix: could not allocate %s's session root node id: %w", sandboxName, nerr)
	}
	node := session.Node{
		ID:         nodeID,
		Model:      model,
		Workspace:  workspace,
		Target:     session.TargetLocalSandbox,
		Sandbox:    sandboxName,
		InstanceID: instanceID,
		State:      session.StateRunning,
	}
	if perr := store.PutNode(treeID, node); perr != nil {
		_ = holder.Release()
		return nil, fmt.Errorf("pix: could not record %s's session root node: %w", sandboxName, perr)
	}
	return &interactiveRoot{holder: holder, store: store, tree: treeID, node: nodeID}, nil
}

// awaitInteractiveRootHold is the CREATE-path half: launch.RunSession is
// about to create (or is already creating) sandboxName, so there is no
// positive instance id yet, and none of the identity this Hold binds to
// can exist before creation succeeds. It polls for the first positive
// receipt (launch.FindPositivelyIdentifiedRunning) concurrently with that
// creation, bounded by ctx (the caller cancels it once RunSession
// returns, whatever the outcome, so a create that never appears leaves no
// goroutine behind). A second live interactive root can never be found
// here in practice — nothing could already hold "root-interactive" for a
// sandbox name workflow/launch just positively probed absent moments ago
// — but the same refusal path is still honored if it somehow is.
func awaitInteractiveRootHold(ctx context.Context, env hostenv.Env, sessionKey, sandboxName, workspace, environment, model string) (*interactiveRoot, error) {
	ticker := time.NewTicker(interactiveRootPollInterval)
	defer ticker.Stop()
	for {
		if entry, ok := launch.FindPositivelyIdentifiedRunning(env, sandboxName); ok && entry.InstanceID != nil && *entry.InstanceID != "" {
			return holdInteractiveRootNow(sessionKey, sandboxName, workspace, environment, model, *entry.InstanceID)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// warnInteractiveRootFailure reports a failed Hold without ever aborting
// the launch: workflow/launch.RunSession is the sole owner of the actual
// sandbox lifecycle, so a session-tree bookkeeping failure discovered
// asynchronously (the create-path race) can only be surfaced, never used
// to tear down a session already underway. A cancelled poll (RunSession
// already returned, positively or not) is expected and silent.
func warnInteractiveRootFailure(errOut io.Writer, sandboxName string, err error) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	fmt.Fprintf(errOut, "pix: warning: %s's session-tree root reference could not be recorded: %v\n", sandboxName, err)
}
