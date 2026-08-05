//go:build unix

package lease

// TestRealProcess_UnlinkRecreateBetweenOpenAndAcquire is the real-OS-process
// counterpart to toctou_test.go's in-process controlled reproductions: it
// drives the exact same "blocked old inode, then path recreation" race, but
// with the open() and the flock() genuinely happening in TWO DIFFERENT
// processes, exercised through the same TestHelperProcess re-exec pattern as
// lock_process_test.go / ordering_process_test.go (see their doc comments).
//
// The helper opens the lease file and reports OPENED — holding an fd on the
// ORIGINAL inode — then blocks waiting for a "go\n" line. Only once the
// parent has replaced the file on disk (a real unlink(2)+creat(2), from a
// different process's perspective than the one about to flock) does it send
// "go\n", so the helper's flock(2) call is genuinely racing a real external
// replace, not a same-goroutine simulation of one.

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func init() {
	helperActions["open-then-acquire"] = helperOpenThenAcquire
}

// helperOpenThenAcquire opens the lease at LEASE_HELPER_DIR, reports
// "OPENED\n" while holding the fd but BEFORE acquiring any lock, waits for a
// "go\n" line, then acquires in LEASE_HELPER_MODE ("sh" or "ex") and reports
// "ACQUIRED\n". From there it behaves like helperHold: "release\n" causes a
// clean Close then exit(0); anything else leaves it blocked for the kernel
// to end via process death.
func helperOpenThenAcquire() {
	dir := os.Getenv("LEASE_HELPER_DIR")
	mode := os.Getenv("LEASE_HELPER_MODE")
	l, err := OpenRefLease(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper Open: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("OPENED")

	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	if line != "go\n" {
		fmt.Fprintf(os.Stderr, "helper expected go, got %q\n", line)
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	switch mode {
	case "sh":
		err = l.AcquireShared(ctx)
	case "ex":
		err = l.h.acquireValidated(ctx, syscall.LOCK_EX)
	default:
		fmt.Fprintf(os.Stderr, "unknown LEASE_HELPER_MODE=%s\n", mode)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper acquire: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("ACQUIRED")

	line, _ = reader.ReadString('\n')
	if line == "release\n" {
		l.Close()
		os.Exit(0)
	}
	select {} // wait to be killed
}

func testRealProcessUnlinkRecreateBetweenOpenAndAcquire(t *testing.T, mode string) {
	dir := mustDir(t)
	// Learn the real path up front, from this (parent) process, without
	// disturbing anything the child will open.
	probe, err := OpenRefLease(dir)
	if err != nil {
		t.Fatalf("Open probe: %v", err)
	}
	path := probe.Path()
	probe.Close()

	cmd := helperCommand(helperActionEnv+"=open-then-acquire", "LEASE_HELPER_DIR="+dir, "LEASE_HELPER_MODE="+mode)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	reader := bufio.NewReader(stdout)
	line, err := readAcquired(reader, 5*time.Second)
	if err != nil {
		t.Fatalf("waiting for helper OPENED: %v", err)
	}
	if line != "OPENED\n" {
		t.Fatalf("helper said %q, want OPENED", line)
	}

	// The REAL race: from a genuinely different OS process's perspective,
	// unlink the file the child already has open (leaving its fd pointing
	// at a now-orphaned inode with Nlink dropping to 0 the instant this
	// completes) and put a brand-new file at the same path. The child has
	// not yet called flock(2) — its very first flock will land on the
	// orphaned inode unless the fix's post-acquire validate+retry catches
	// it.
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove %s: %v", path, err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("recreate %s: %v", path, err)
	}

	if _, err := fmt.Fprint(stdin, "go\n"); err != nil {
		t.Fatalf("send go: %v", err)
	}

	line, err = readAcquired(reader, 5*time.Second)
	if err != nil {
		t.Fatalf("waiting for helper ACQUIRED: %v", err)
	}
	if line != "ACQUIRED\n" {
		t.Fatalf("helper said %q, want ACQUIRED", line)
	}

	// Prove the child's lock is genuinely on the CURRENT path's inode: a
	// fresh handle in THIS process must observe contention.
	assertGenuinelyHeld(t, dir)

	if _, err := fmt.Fprint(stdin, "release\n"); err != nil {
		t.Fatalf("send release: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper exit: %v", err)
	}

	prober, err := OpenRefLease(dir)
	if err != nil {
		t.Fatalf("Open prober: %v", err)
	}
	defer prober.Close()
	if err := prober.TryExclusive(); err != nil {
		t.Errorf("TryExclusive after helper released = %v, want nil (zero-holder proof)", err)
	}
}

func TestRealProcess_UnlinkRecreateBetweenOpenAndAcquire_Shared(t *testing.T) {
	testRealProcessUnlinkRecreateBetweenOpenAndAcquire(t, "sh")
}

func TestRealProcess_UnlinkRecreateBetweenOpenAndAcquire_Exclusive(t *testing.T) {
	testRealProcessUnlinkRecreateBetweenOpenAndAcquire(t, "ex")
}

// TestRealProcess_TwoProcessesSerializeAcrossRecreate is a stronger
// end-to-end check: process A opens-then-acquires EXCLUSIVE while the parent
// replaces the file underneath it (as above), then — while A still holds
// it — process B (a plain, un-raced "hold" helper, see lock_process_test.go)
// attempts the SAME lock and must genuinely block until A releases. If A's
// fix had failed (A silently held a lock on the orphaned old inode), B would
// acquire immediately despite A believing it holds the lock — the exact
// double-holder outcome this fix exists to prevent.
func TestRealProcess_TwoProcessesSerializeAcrossRecreate(t *testing.T) {
	dir := mustDir(t)
	probe, err := OpenRefLease(dir)
	if err != nil {
		t.Fatalf("Open probe: %v", err)
	}
	path := probe.Path()
	probe.Close()

	cmdA := helperCommand(helperActionEnv+"=open-then-acquire", "LEASE_HELPER_DIR="+dir, "LEASE_HELPER_MODE=ex")
	stdinA, err := cmdA.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe A: %v", err)
	}
	stdoutA, err := cmdA.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe A: %v", err)
	}
	cmdA.Stderr = os.Stderr
	if err := cmdA.Start(); err != nil {
		t.Fatalf("Start A: %v", err)
	}
	readerA := bufio.NewReader(stdoutA)
	if line, err := readAcquired(readerA, 5*time.Second); err != nil || line != "OPENED\n" {
		t.Fatalf("A OPENED: line=%q err=%v", line, err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	fmt.Fprint(stdinA, "go\n")
	if line, err := readAcquired(readerA, 5*time.Second); err != nil || line != "ACQUIRED\n" {
		t.Fatalf("A ACQUIRED: line=%q err=%v", line, err)
	}

	// Process B: a plain holder attempting the SAME lock. It must NOT
	// acquire while A holds it.
	cmdB := helperCommand(helperActionEnv+"=hold", "LEASE_HELPER_DIR="+dir, "LEASE_HELPER_MODE=ex")
	stdinB, err := cmdB.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe B: %v", err)
	}
	stdoutB, err := cmdB.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe B: %v", err)
	}
	cmdB.Stderr = os.Stderr
	if err := cmdB.Start(); err != nil {
		t.Fatalf("Start B: %v", err)
	}
	readerB := bufio.NewReader(stdoutB)

	bDone := make(chan struct{})
	go func() {
		line, _ := readAcquired(readerB, 10*time.Second)
		if strings.TrimSpace(line) == "ACQUIRED" {
			close(bDone)
		}
	}()

	select {
	case <-bDone:
		t.Fatal("process B acquired the exclusive lock while process A still holds it — the TOCTOU let two holders in at once")
	case <-time.After(300 * time.Millisecond):
		// expected: B is still blocked.
	}

	fmt.Fprint(stdinA, "release\n")
	if err := cmdA.Wait(); err != nil {
		t.Fatalf("A exit: %v", err)
	}

	select {
	case <-bDone:
	case <-time.After(5 * time.Second):
		t.Fatal("process B never acquired after A released")
	}
	fmt.Fprint(stdinB, "release\n")
	if err := cmdB.Wait(); err != nil {
		t.Fatalf("B exit: %v", err)
	}
}
