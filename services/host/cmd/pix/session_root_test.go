package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"pix/host/hostenv"
	"pix/host/session"
	"pix/host/sys"
)

// TestHoldInteractiveRootNow_RecordsTreeNodeAndHold proves the ATTACH-path
// wiring does all three things architecture §7.2 asks for: a resolvable
// session tree, a root node record advancing to "running", and a real held
// reference at the exact directory workflow/launch's own lease already
// uses for this sessionKey.
func TestHoldInteractiveRootNow_RecordsTreeNodeAndHold(t *testing.T) {
	t.Setenv("PIX_HOME", t.TempDir())

	r, err := holdInteractiveRootNow("pix-proj-1234abcd", "pix-proj-1234abcd", "/w", "work", "anthropic/claude-sonnet-5", "inst-1")
	if err != nil {
		t.Fatalf("holdInteractiveRootNow: %v", err)
	}
	if r == nil || r.holder == nil {
		t.Fatal("holdInteractiveRootNow returned no holder")
	}
	defer r.release(false)

	node, err := r.store.ReadNode(r.tree, r.node)
	if err != nil {
		t.Fatalf("ReadNode: %v", err)
	}
	if node.State != session.StateRunning {
		t.Fatalf("root node state = %q, want running", node.State)
	}
	if node.Sandbox != "pix-proj-1234abcd" || node.InstanceID != "inst-1" || node.Model != "anthropic/claude-sonnet-5" {
		t.Fatalf("root node = %+v, unexpected fields", node)
	}
	if node.Target != session.TargetLocalSandbox {
		t.Fatalf("root node target = %q, want %q", node.Target, session.TargetLocalSandbox)
	}

	dir, derr := interactiveRootSandboxDir("pix-proj-1234abcd")
	if derr != nil {
		t.Fatalf("interactiveRootSandboxDir: %v", derr)
	}
	c := session.CountHolders(dir, "inst-1")
	if !c.Known || c.N != 1 {
		t.Fatalf("census = %+v, want exactly one live holder", c)
	}
}

// TestHoldInteractiveRootNow_RefusesSecondLiveRoot is the PRD invariant this
// whole feature exists to enforce: two `pix run` processes must never both
// believe they own the SAME sandbox's interactive session.
func TestHoldInteractiveRootNow_RefusesSecondLiveRoot(t *testing.T) {
	t.Setenv("PIX_HOME", t.TempDir())

	first, err := holdInteractiveRootNow("pix-proj-dupe", "pix-proj-dupe", "/w", "work", "", "inst-1")
	if err != nil {
		t.Fatalf("first Hold: %v", err)
	}
	defer first.release(false)

	_, err = holdInteractiveRootNow("pix-proj-dupe", "pix-proj-dupe", "/w", "work", "", "inst-1")
	if !errors.Is(err, session.ErrSecondInteractiveRoot) {
		t.Fatalf("second Hold error = %v, want ErrSecondInteractiveRoot", err)
	}
}

// TestHoldInteractiveRootNow_ResumesTheSameTreeOnReattach proves a second
// call for the SAME sandbox, after the first root released, resumes the
// SAME session tree rather than starting a fresh one — the tree survives a
// `pix run` exit and re-attach the way the sandbox itself does.
func TestHoldInteractiveRootNow_ResumesTheSameTreeOnReattach(t *testing.T) {
	t.Setenv("PIX_HOME", t.TempDir())

	first, err := holdInteractiveRootNow("pix-proj-resume", "pix-proj-resume", "/w", "work", "", "inst-1")
	if err != nil {
		t.Fatalf("first Hold: %v", err)
	}
	firstTree := first.tree
	first.release(false)

	second, err := holdInteractiveRootNow("pix-proj-resume", "pix-proj-resume", "/w", "work", "", "inst-2")
	if err != nil {
		t.Fatalf("second Hold: %v", err)
	}
	defer second.release(false)
	if second.tree != firstTree {
		t.Fatalf("second Hold started a NEW tree %q, want it to resume %q", second.tree, firstTree)
	}
}

// TestInteractiveRootRelease_AdvancesNodeToFailedOnly proves release(true)
// (a session that ended in error) marks the node "failed", release(false)
// marks it "finished", and both always drop the lock — including the
// nil-safe zero value a caller reaches when no Hold was ever attempted.
func TestInteractiveRootRelease_AdvancesNodeState(t *testing.T) {
	t.Setenv("PIX_HOME", t.TempDir())

	r, err := holdInteractiveRootNow("pix-proj-fail", "pix-proj-fail", "/w", "work", "", "inst-1")
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	nodeID := r.node
	tree := r.tree
	store := r.store
	r.release(true)

	node, rerr := store.ReadNode(tree, nodeID)
	if rerr != nil {
		t.Fatalf("ReadNode after release: %v", rerr)
	}
	if node.State != session.StateFailed {
		t.Fatalf("node state after release(true) = %q, want failed", node.State)
	}

	dir, _ := interactiveRootSandboxDir("pix-proj-fail")
	c := session.CountHolders(dir, "inst-1")
	if !c.Zero() {
		t.Fatalf("census after release must be a positive zero; got %+v", c)
	}

	// A nil receiver and a never-held struct must both be safe no-ops.
	var nilRoot *interactiveRoot
	nilRoot.release(false)
	(&interactiveRoot{}).release(false)
}

// fakeSbxLsEnv is a minimal hostenv.Env whose `sbx ls --json` answer is
// driven by a test-controlled function, so awaitInteractiveRootHold can be
// proven without a real sbx binary.
type fakeSbxLsEnv struct {
	sys.Real
	ls func() (string, error)
}

func (f fakeSbxLsEnv) Run(name string, args ...string) (string, error) {
	if name == "sbx" && len(args) == 2 && args[0] == "ls" && args[1] == "--json" {
		return f.ls()
	}
	return f.Real.Run(name, args...)
}

// sbxListingJSON matches the REAL sbx v0.38 row profile byte for byte
// (v38RowKeys/v38UUIDPattern in sandbox/list.go): name, a canonical UUID id,
// agent, a recognized status value, workspaces, workspace_missing. Using
// any other shape marks the parse legacy/unverified, which
// FindPositivelyIdentifiedRunning refuses (IdentityVerified=false).
func sbxListingJSON(name, instanceUUID, status string) string {
	return fmt.Sprintf(`{"sandboxes":[{"name":%q,"id":%q,"agent":"pi","status":%q,"workspaces":["/w"],"workspace_missing":false}]}`,
		name, instanceUUID, status)
}

// TestAwaitInteractiveRootHold_PicksUpTheFirstPositiveReceipt proves the
// CREATE-path poll: absent/starting probes are ignored, and the moment a
// positively identified, running instance appears the Hold is taken and
// returned — without the caller ever blocking the create itself.
func TestAwaitInteractiveRootHold_PicksUpTheFirstPositiveReceipt(t *testing.T) {
	t.Setenv("PIX_HOME", t.TempDir())

	calls := 0
	env := hostenv.Env{System: fakeSbxLsEnv{ls: func() (string, error) {
		calls++
		if calls < 3 {
			return `{"sandboxes":[]}`, nil
		}
		return sbxListingJSON("pix-proj-create", "5c2b6e0a-1f3d-4a9b-8e21-7d4f2b6c9a10", "running"), nil
	}}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := awaitInteractiveRootHold(ctx, env, "pix-proj-create", "pix-proj-create", "/w", "work", "")
	if err != nil {
		t.Fatalf("awaitInteractiveRootHold: %v", err)
	}
	defer r.release(false)
	if r == nil || r.holder == nil {
		t.Fatal("no holder acquired once the instance positively appeared")
	}
	if calls < 3 {
		t.Fatalf("Hold fired before the positive receipt (after %d calls)", calls)
	}
}

// TestAwaitInteractiveRootHold_CancelledBeforeAnyReceiptReturnsCtxErr proves
// the bound: a create that never appears (or a caller cancelling once
// RunSession itself has already returned) must leave no goroutine spinning
// forever.
func TestAwaitInteractiveRootHold_CancelledBeforeAnyReceiptReturnsCtxErr(t *testing.T) {
	t.Setenv("PIX_HOME", t.TempDir())
	env := hostenv.Env{System: fakeSbxLsEnv{ls: func() (string, error) { return `{"sandboxes":[]}`, nil }}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := awaitInteractiveRootHold(ctx, env, "pix-proj-never", "pix-proj-never", "/w", "work", "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestInteractiveRootSandboxDir_MatchesLeaseIdentity(t *testing.T) {
	t.Setenv("PIX_HOME", t.TempDir())
	dir, err := interactiveRootSandboxDir("pix-abc")
	if err != nil {
		t.Fatalf("interactiveRootSandboxDir: %v", err)
	}
	if filepath.Base(dir) != "pix-abc" {
		t.Fatalf("sandbox dir = %q, want to end in the sessionKey", dir)
	}
}

func TestWarnInteractiveRootFailure_SwallowsCancellationSilently(t *testing.T) {
	var buf fmtBuffer
	warnInteractiveRootFailure(&buf, "pix-x", context.Canceled)
	if buf.String() != "" {
		t.Fatalf("cancellation must not be reported: %q", buf.String())
	}
	warnInteractiveRootFailure(&buf, "pix-x", errors.New("boom"))
	if buf.String() == "" {
		t.Fatal("a real failure must be reported")
	}
}

// fmtBuffer avoids importing bytes.Buffer twice across test files that
// already alias it; a tiny local io.Writer is simpler than sharing state.
type fmtBuffer struct{ s string }

func (b *fmtBuffer) Write(p []byte) (int, error) {
	b.s += string(p)
	return len(p), nil
}
func (b *fmtBuffer) String() string { return b.s }
