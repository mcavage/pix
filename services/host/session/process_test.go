//go:build unix

package session

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// These tests exercise the child-runner across REAL OS processes: the
// interactive root and the delegated child are each a separate process
// holding a REAL flock, so "root exits, child keeps the sandbox" is proven
// by the kernel releasing one fd while the other stays open — not by two
// in-process Go values whose lifetimes this same test controls. The pattern
// (a re-exec of this same test binary, dispatched by an env var) mirrors
// pix/host/lease's lock_process_test.go.

const helperActionEnv = "SESSION_HELPER_ACTION"

func helperCommand(env ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), env...)
	return cmd
}

// TestHelperProcess dispatches on SESSION_HELPER_ACTION; a plain `go test`
// leaves it unset and this no-ops.
func TestHelperProcess(t *testing.T) {
	switch os.Getenv(helperActionEnv) {
	case "root":
		helperRoot()
	case "child":
		helperChild()
	}
}

// helperRoot holds the interactive-root reference, prints ACQUIRED, then
// blocks reading stdin. It is killed by the test rather than asked to
// release cleanly, on purpose: root death is exactly the event a delegated
// child must survive.
func helperRoot() {
	dir := os.Getenv("SESSION_HELPER_DIR")
	instance := os.Getenv("SESSION_HELPER_INSTANCE")
	h, err := HoldInteractiveRoot(dir, "tree1", instance)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helperRoot Hold: %v\n", err)
		os.Exit(1)
	}
	_ = h
	fmt.Println("ACQUIRED")
	// Block on a real read, never on a bare `select{}`: the latter is the
	// ONLY goroutine parked on nothing at all, which the runtime's own
	// deadlock detector treats as a fatal error rather than a blocked
	// syscall. Nothing ever writes to this pipe; the point is to be killed.
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

// helperChild runs a real RunChild, holding its reference for
// SESSION_HELPER_SLEEP_MS before finishing normally. It prints ACQUIRED the
// moment the executor starts (i.e., after the reference is taken and the
// node is running), then FINISHED right before it exits, so the parent test
// can observe both edges.
func helperChild() {
	dir := os.Getenv("SESSION_HELPER_DIR")
	storeRoot := os.Getenv("SESSION_HELPER_STORE")
	instance := os.Getenv("SESSION_HELPER_INSTANCE")
	sleepMS := 200
	if v := os.Getenv("SESSION_HELPER_SLEEP_MS"); v != "" {
		fmt.Sscanf(v, "%d", &sleepMS)
	}
	o := ChildRunOpts{
		SandboxDir: dir,
		StoreRoot:  storeRoot,
		TreeID:     "tree1",
		NodeID:     "child1",
		ParentID:   "root-interactive-parent",
		Sandbox:    "pix-proj-1234abcd",
		InstanceID: instance,
		Request:    ChildRequest{Agent: "fanout", Task: "helper task", Target: "local-process"},
	}
	err := RunChild(o, func(req ChildRequest) error {
		fmt.Println("ACQUIRED")
		time.Sleep(time.Duration(sleepMS) * time.Millisecond)
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "helperChild RunChild: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("FINISHED")
}

func readLine(t *testing.T, r *bufio.Reader, want string, timeout time.Duration) {
	t.Helper()
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := r.ReadString('\n')
		ch <- result{line, err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("reading %q: %v", want, res.err)
		}
		if res.line != want+"\n" {
			t.Fatalf("got %q, want %q", res.line, want)
		}
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for %q", want)
	}
}

// TestChildSurvivesRootClose is the property the whole child-runner exists
// for (architecture §7.2 / this task's core proof): a delegated child,
// running as its OWN process holding its OWN instance-bound reference,
// keeps the sandbox referenced after the interactive root process has been
// killed out from under it — proven with two real OS processes, a real
// SIGKILL, and a real flock census, not a mock.
func TestChildSurvivesRootClose(t *testing.T) {
	base := t.TempDir()
	sandboxDir := filepath.Join(base, "sandboxes", "pix-proj-1234abcd")
	if err := os.MkdirAll(sandboxDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const instance = "inst-1"

	// The helpers hardcode "tree1"/"root-interactive-parent" for simplicity;
	// create that tree and its root node directly here so PutNode's parent
	// check passes for the child the helper writes.
	storeRoot := filepath.Join(base, "trees-fixed")
	if err := os.MkdirAll(storeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(storeRoot, "tree1", "nodes"), 0o700); err != nil {
		t.Fatal(err)
	}
	store := Store{Root: storeRoot}
	if err := os.WriteFile(filepath.Join(storeRoot, "tree1", "tree.json"),
		[]byte(`{"schema":1,"id":"tree1","environment":"work","workspace":"/w","created_at":"2020-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.PutNode("tree1", Node{ID: "root-interactive-parent", Target: TargetLocalProcess,
		Sandbox: "pix-proj-1234abcd", InstanceID: instance, State: StateRunning}); err != nil {
		t.Fatalf("seed root node: %v", err)
	}

	rootCmd := helperCommand(helperActionEnv+"=root", "SESSION_HELPER_DIR="+sandboxDir, "SESSION_HELPER_INSTANCE="+instance)
	rootOut, err := rootCmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	// A REAL open pipe, never written to and never closed until this test
	// kills the process: root's blocking read must be genuinely blocked, not
	// an instant EOF off /dev/null that would let root exit (and so release
	// its own reference) on its own, before this test ever gets to killing
	// it.
	rootIn, err := rootCmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	rootCmd.Stderr = os.Stderr
	if err := rootCmd.Start(); err != nil {
		t.Fatalf("start root: %v", err)
	}
	defer rootIn.Close()
	readLine(t, bufio.NewReader(rootOut), "ACQUIRED", 5*time.Second)

	childCmd := helperCommand(helperActionEnv+"=child",
		"SESSION_HELPER_DIR="+sandboxDir, "SESSION_HELPER_STORE="+storeRoot,
		"SESSION_HELPER_INSTANCE="+instance, "SESSION_HELPER_SLEEP_MS=500")
	childOut, err := childCmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	childCmd.Stderr = os.Stderr
	if err := childCmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	childReader := bufio.NewReader(childOut)
	readLine(t, childReader, "ACQUIRED", 5*time.Second)

	// Both root and child are live now.
	c := CountHolders(sandboxDir, instance)
	if !c.Known || c.N != 2 {
		t.Fatalf("census with root+child running = %+v, want 2", c)
	}

	// Kill the interactive root WITHOUT giving it a chance to clean up —
	// the exact event a delegated child must be immune to.
	if err := rootCmd.Process.Kill(); err != nil {
		t.Fatalf("kill root: %v", err)
	}
	_ = rootCmd.Wait()

	// The child must still be counted: root's death released only root's
	// own reference.
	c = CountHolders(sandboxDir, instance)
	if !c.Known || c.N != 1 || c.Zero() {
		t.Fatalf("after root is killed the child must still hold the sandbox; census = %+v", c)
	}
	if len(c.Nodes) != 1 || c.Nodes[0] != "child1" {
		t.Fatalf("surviving holder = %v, want [child1]", c.Nodes)
	}

	// Let the child finish on its own (monotonic finish), then confirm a
	// positive zero.
	readLine(t, childReader, "FINISHED", 5*time.Second)
	if err := childCmd.Wait(); err != nil {
		t.Fatalf("child process exit: %v", err)
	}
	c = CountHolders(sandboxDir, instance)
	if !c.Zero() {
		t.Fatalf("after the child finishes the census must be a POSITIVE zero; got %+v", c)
	}
	node, err := store.ReadNode("tree1", "child1")
	if err != nil {
		t.Fatalf("ReadNode(child1): %v", err)
	}
	if node.State != StateFinished {
		t.Fatalf("child node state = %q, want finished", node.State)
	}
}
