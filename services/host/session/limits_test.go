package session

// limits_test.go — L2 (security re-review): pix_session_delegate's live-
// children-per-tree and max-depth caps, enforced atomically under
// Store.WithTreeLock. Tests cover sequential width, concurrent width (the
// actual race the lock exists for), depth, a released/finished node
// freeing its slot, and "refuse without spawning".

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func delegateRaw() json.RawMessage {
	return json.RawMessage(`{"agent":"fanout","task":"do it","target":"local-process"}`)
}

// TestDelegate_RefusesExcessWidth_SequentialWithoutSpawning proves the
// live-children cap: with MaxLiveChildren=2, a third concurrent (never-
// finished) child is refused, and refused BEFORE it is ever spawned or
// recorded as anything but absent.
func TestDelegate_RefusesExcessWidth_SequentialWithoutSpawning(t *testing.T) {
	s, spawned := newTestServer(t)
	s.Limits = Limits{MaxLiveChildren: 2, MaxDepth: DefaultMaxDepth}
	// Every spawned child stays "starting" forever (Spawn never advances it
	// to a terminal state) — the exact "live and never finishes" shape a
	// width cap must bound.

	for i := 0; i < 2; i++ {
		if _, err := s.Delegate(delegateRaw()); err != nil {
			t.Fatalf("delegate %d: unexpected refusal: %v", i, err)
		}
	}
	_, err := s.Delegate(delegateRaw())
	var tooMany *TooManyLiveChildrenError
	if !errors.As(err, &tooMany) {
		t.Fatalf("3rd delegate err = %v, want *TooManyLiveChildrenError", err)
	}
	if len(*spawned) != 2 {
		t.Fatalf("spawned %d children, want exactly 2 (the refused 3rd must never spawn)", len(*spawned))
	}
	nodes, err := (Store{Root: s.Ctx.StoreRoot}).ListNodes(s.Ctx.TreeID)
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	// root1 (planted by mustTree) + the 2 accepted children = 3 nodes total;
	// the refused 3rd delegate must not have recorded a 4th.
	if len(nodes) != 3 {
		t.Fatalf("tree has %d nodes, want 3 (root + 2 accepted children, refusal recorded nothing)", len(nodes))
	}
}

// TestDelegate_ReleaseFreesASlot proves a finished child's slot is
// reusable: filling the cap, finishing one child (a terminal PutNode, the
// same transition RunChild itself performs), then delegating one more must
// succeed.
func TestDelegate_ReleaseFreesASlot(t *testing.T) {
	s, _ := newTestServer(t)
	s.Limits = Limits{MaxLiveChildren: 1, MaxDepth: DefaultMaxDepth}

	res, err := s.Delegate(delegateRaw())
	if err != nil {
		t.Fatalf("first delegate: %v", err)
	}
	if _, err := s.Delegate(delegateRaw()); err == nil {
		t.Fatal("second delegate at the cap should have been refused")
	}

	store := Store{Root: s.Ctx.StoreRoot}
	node, err := store.ReadNode(s.Ctx.TreeID, res.Node)
	if err != nil {
		t.Fatalf("ReadNode: %v", err)
	}
	node.State = StateFinished
	if err := store.PutNode(s.Ctx.TreeID, node); err != nil {
		t.Fatalf("PutNode(finished): %v", err)
	}

	if _, err := s.Delegate(delegateRaw()); err != nil {
		t.Fatalf("delegate after release: %v", err)
	}
}

// TestDelegate_RefusesExcessDepth proves the depth cap: with MaxDepth=2, a
// delegate whose PARENT is already at depth 2 is refused, never spawned.
func TestDelegate_RefusesExcessDepth(t *testing.T) {
	s, spawned := newTestServer(t)
	s.Limits = Limits{MaxLiveChildren: 100, MaxDepth: 2}
	store := Store{Root: s.Ctx.StoreRoot}

	// root1 is depth 0 (planted by mustTree). Build a depth-1 node (d1,
	// parent root1) and a depth-2 node (d2, parent d1) directly, so this
	// test pins depth accounting independent of Delegate's own node IDs.
	if err := store.PutNode(s.Ctx.TreeID, Node{ID: "d1", Parent: "root1", Target: TargetLocalProcess, State: StateRunning}); err != nil {
		t.Fatalf("PutNode(d1): %v", err)
	}
	if err := store.PutNode(s.Ctx.TreeID, Node{ID: "d2", Parent: "d1", Target: TargetLocalProcess, State: StateRunning}); err != nil {
		t.Fatalf("PutNode(d2): %v", err)
	}

	// A delegate from d1 (depth 1 < MaxDepth 2) must succeed — its child
	// lands at depth 2.
	s.Ctx.ParentID = "d1"
	if _, err := s.Delegate(delegateRaw()); err != nil {
		t.Fatalf("delegate from depth 1: unexpected refusal: %v", err)
	}
	// A delegate from d2 (depth 2 >= MaxDepth 2) must be refused.
	s.Ctx.ParentID = "d2"
	_, err := s.Delegate(delegateRaw())
	var depthErr *DepthExceededError
	if !errors.As(err, &depthErr) {
		t.Fatalf("delegate from depth 2 err = %v, want *DepthExceededError", err)
	}
	if depthErr.Depth != 2 || depthErr.Limit != 2 {
		t.Fatalf("DepthExceededError = %+v, want Depth=2 Limit=2", depthErr)
	}
	if len(*spawned) != 1 {
		t.Fatalf("spawned %d children, want exactly 1 (the depth-2 attempt must never spawn)", len(*spawned))
	}
}

// TestDelegate_ConcurrentWidth_NeverExceedsTheCap is the actual race the
// per-tree lock exists for: N goroutines call Delegate simultaneously
// against the SAME tree with MaxLiveChildren < N; without WithTreeLock
// serializing "check then reserve", more than the cap could all observe
// "one slot free" and all win. A NewID that is safe to call concurrently
// (atomic counter) is substituted so the race under test is the cap logic
// itself, not NewID's own default (which is not documented as
// goroutine-safe).
func TestDelegate_ConcurrentWidth_NeverExceedsTheCap(t *testing.T) {
	s, spawned := newTestServer(t)
	const limit = 3
	const attempts = 12
	s.Limits = Limits{MaxLiveChildren: limit, MaxDepth: DefaultMaxDepth}

	var idCounter int64
	var idMu sync.Mutex
	s.NewID = func() (string, error) {
		idMu.Lock()
		defer idMu.Unlock()
		idCounter++
		return fmt.Sprintf("conc-%d", idCounter), nil
	}
	var spawnMu sync.Mutex
	s.Spawn = func(ctx ServerContext, treeID, nodeID string, req ChildRequest) error {
		spawnMu.Lock()
		defer spawnMu.Unlock()
		*spawned = append(*spawned, req)
		return nil
	}

	var wg sync.WaitGroup
	var accepted, refused atomic.Int32
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Delegate(delegateRaw())
			if err != nil {
				var tooMany *TooManyLiveChildrenError
				if !errors.As(err, &tooMany) {
					t.Errorf("unexpected delegate error: %v", err)
				}
				refused.Add(1)
				return
			}
			accepted.Add(1)
		}()
	}
	wg.Wait()

	if int(accepted.Load()) != limit {
		t.Fatalf("accepted = %d, want exactly %d (the cap)", accepted.Load(), limit)
	}
	if int(refused.Load()) != attempts-limit {
		t.Fatalf("refused = %d, want %d", refused.Load(), attempts-limit)
	}
	spawnMu.Lock()
	n := len(*spawned)
	spawnMu.Unlock()
	if n != limit {
		t.Fatalf("spawned %d children concurrently, want exactly %d (never more than the cap, even under a race)", n, limit)
	}
	live, err := (Store{Root: s.Ctx.StoreRoot}).LiveNodeCount(s.Ctx.TreeID)
	if err != nil {
		t.Fatalf("LiveNodeCount: %v", err)
	}
	if live != limit {
		t.Fatalf("LiveNodeCount = %d, want %d (the root itself never counts as a child)", live, limit)
	}
}
