// limits.go — L2 (security re-review): pix_session_delegate had NO bound at
// all on how many children a tree could accumulate or how deep a delegate
// chain could run — a single compromised or looping session could fan out
// an unbounded number of detached child-runners, each a full `pi` process
// outliving the interactive root. Two caps close that gap:
//
//   - MaxLiveChildren: how many of a tree's nodes may be non-terminal
//     (starting/running) at once. A node that finishes or fails frees its
//     slot immediately (Store.LiveNodeCount only counts non-Terminal
//     states), so this bounds CONCURRENT fan-out, not cumulative history.
//   - MaxDepth: how many parent hops a delegating node may already be at
//     before it is refused from creating one more. A node AT MaxDepth
//     refuses to delegate further; one below it may.
//
// Defaults mirror extensions/subagents.ts's own MAX_CONCURRENCY (8) and
// MAX_DEPTH (3): pix_session_delegate is the SAME "one more agent"
// primitive at the host/sandbox boundary that tool already bounds inside a
// single pi process, and a looser ceiling here would just move an
// attacker (or a runaway loop) to whichever surface has the weaker limit.
//
// Both checks, and the node write that reserves the slot they approve, run
// under ONE per-tree advisory lock (Store.WithTreeLock) so two concurrent
// pix_session_delegate calls against the SAME tree — this server dispatches
// tools/call on its own goroutine per request (mcp.go's Serve) — can never
// both observe "one slot free" and both take it.
package session

import (
	"fmt"
	"path/filepath"
	"strings"

	"pix/host/sys"
)

// DefaultMaxLiveChildren and DefaultMaxDepth are pix_session_delegate's own
// ceilings, aligned with extensions/subagents.ts's MAX_CONCURRENCY (8
// running at once) and MAX_DEPTH (3) defaults.
const (
	DefaultMaxLiveChildren = 8
	DefaultMaxDepth        = 3
)

// Limits is the delegate cap a Server enforces. DefaultLimits is the only
// sanctioned constructor: a caller building one field at a time by hand
// risks an accidental zero value, which MaxLiveChildrenExceeded/
// MaxDepthExceeded would then read as "no room ever" rather than
// "unbounded" — refusing everything is the safe failure direction, but a
// test or caller should say "no limit" explicitly (a very large number)
// rather than by accident.
type Limits struct {
	MaxLiveChildren int
	MaxDepth        int
}

// DefaultLimits returns pix_session_delegate's production ceilings.
func DefaultLimits() Limits {
	return Limits{MaxLiveChildren: DefaultMaxLiveChildren, MaxDepth: DefaultMaxDepth}
}

// TooManyLiveChildrenError is refused BEFORE any node is recorded or any
// child spawned (this file's own doc comment: "refuse excess without
// spawning/holding").
type TooManyLiveChildrenError struct {
	Tree  string
	Limit int
}

func (e *TooManyLiveChildrenError) Error() string {
	return fmt.Sprintf("pix: session tree %s already has %d live children (the maximum); a child must finish before another can start", e.Tree, e.Limit)
}

// DepthExceededError is refused for the same reason: the DELEGATING node
// (ParentID) is already at Limit hops from the tree's root, so one more
// would exceed it.
type DepthExceededError struct {
	Tree  string
	Depth int
	Limit int
}

func (e *DepthExceededError) Error() string {
	return fmt.Sprintf("pix: session tree %s: delegating node is already at depth %d (the maximum); it must do the work itself instead of delegating further", e.Tree, e.Depth)
}

// treeLockPath is the per-tree advisory lock file every delegate-cap check
// and the node write it gates serialize on.
func (s Store) treeLockPath(treeID string) string {
	return filepath.Join(s.treeDir(treeID), "delegate.lock")
}

// WithTreeLock runs fn with treeID's delegate lock held: the ONE
// serialization point for "check the caps, then write the node that
// claims a slot" so two concurrent callers can never both pass the same
// check. fn must not call WithTreeLock again for the SAME treeID (flock is
// per open file description; a nested acquire self-deadlocks against the
// same bounded wait sys.Lock already enforces elsewhere).
func (s Store) WithTreeLock(treeID string, fn func() error) error {
	if err := safeID("tree", treeID); err != nil {
		return err
	}
	return sys.Lock(s.treeLockPath(treeID), fn)
}

// LiveNodeCount reports how many of a tree's CHILD nodes (Root() false —
// the interactive root itself is not a delegated child and never counts
// against its own cap) are NOT in a terminal state (finished/failed) — the
// exact population MaxLiveChildren bounds, since a finished/failed child
// no longer holds a slot.
func (s Store) LiveNodeCount(treeID string) (int, error) {
	nodes, err := s.ListNodes(treeID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, node := range nodes {
		if !node.Root() && !node.State.Terminal() {
			n++
		}
	}
	return n, nil
}

// Depth reports how many parent hops separate nodeID from its tree's root
// (the root node itself is depth 0). An empty nodeID — no parent recorded
// at all, the conservative fallback for a delegate call made without a
// resolvable parent context — is treated as depth 0, the same base case a
// real root node has: the first delegate call from an interactive root (or
// from a context this server could not resolve a parent for) is refused
// only once MaxDepth itself is 0, never silently miscounted as already
// deep.
func (s Store) Depth(treeID, nodeID string) (int, error) {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return 0, nil
	}
	depth := 0
	seen := map[string]bool{}
	id := nodeID
	for {
		if seen[id] {
			return 0, fmt.Errorf("pix: session tree %s has a parent cycle at node %s", treeID, id)
		}
		seen[id] = true
		node, err := s.ReadNode(treeID, id)
		if err != nil {
			return 0, fmt.Errorf("pix: could not resolve session node %s's depth: %w", id, err)
		}
		if node.Root() {
			return depth, nil
		}
		parent := strings.TrimSpace(node.Parent)
		if parent == "" {
			return depth, nil // defensive: a non-root node somehow recorded with no parent
		}
		id = parent
		depth++
	}
}

// CheckDelegateCaps is the whole check-then-reserve section a delegate call
// runs under WithTreeLock, BEFORE any node is written or any child spawned:
// it refuses a delegate whose PARENT is already at limits.MaxDepth, and
// refuses one that would push the tree's live-child count past
// limits.MaxLiveChildren. Nothing here mutates the store; a caller that
// gets a nil error still owns writing the reserving node itself, inside
// the SAME locked section (mcp.go's Delegate).
func (s Store) CheckDelegateCaps(treeID, parentID string, limits Limits) error {
	depth, err := s.Depth(treeID, parentID)
	if err != nil {
		return err
	}
	if depth >= limits.MaxDepth {
		return &DepthExceededError{Tree: treeID, Depth: depth, Limit: limits.MaxDepth}
	}
	live, err := s.LiveNodeCount(treeID)
	if err != nil {
		return err
	}
	if live >= limits.MaxLiveChildren {
		return &TooManyLiveChildrenError{Tree: treeID, Limit: limits.MaxLiveChildren}
	}
	return nil
}
