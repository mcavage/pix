//go:build unix

package lease

// These tests exercise the U04c1 reshard (AttachRef / WithLifecycle /
// TryReapProof) across REAL OS processes, the same TestHelperProcess
// re-exec pattern lock_process_test.go uses for the raw RefLease primitive.
// See helperCommand/readAcquired there; this file adds the helper actions
// that exercise the composed ordering helpers specifically.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

func init() {
	// Registered here (rather than duplicating TestHelperProcess) so both
	// this file's actions and lock_process_test.go's stay reachable from the
	// one re-exec'd helper entry point.
	helperActions["attach"] = helperAttach
	helperActions["lifecycle"] = helperLifecycle
	helperActions["reap"] = helperReap
}

// helperAttach calls AttachRef, prints ACQUIRED once it holds the refs
// SHARED lock, then blocks reading a line from stdin exactly like
// helperHold: "release\n" causes a clean Close then exit(0); anything else
// (EOF, or nothing because the parent is about to SIGKILL) leaves it
// blocked so the kernel — not this program's cleanup — is what ends the
// hold.
func helperAttach() {
	dir := os.Getenv("LEASE_HELPER_DIR")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rl, err := AttachRef(ctx, dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper AttachRef: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("ACQUIRED")

	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if line == "release\n" {
		rl.Close()
		os.Exit(0)
	}
	select {} // wait to be killed
}

// helperLifecycle runs WithLifecycle, printing ACQUIRED once fn is entered
// (i.e. the lifecycle EXCLUSIVE lock is held) and then blocking inside fn
// until stdin delivers "release\n", at which point fn returns and
// WithLifecycle's defers release the lock before exit(0).
func helperLifecycle() {
	dir := os.Getenv("LEASE_HELPER_DIR")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := WithLifecycle(ctx, dir, func() error {
		fmt.Println("ACQUIRED")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if line != "release\n" {
			select {} // wait to be killed inside the held lock
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper WithLifecycle: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// helperReap makes exactly one TryReapProof attempt and reports the result
// on stdout as "REAPED" (fn ran) or "HELD" (ErrHeld, fn did not run), then
// exits — used to observe the non-blocking proof from a fresh process.
func helperReap() {
	dir := os.Getenv("LEASE_HELPER_DIR")
	err := TryReapProof(dir, func() error { return nil })
	// os.Exit (rather than a plain return) in every branch here matters: a
	// plain return lets TestHelperProcess return into the normal `go test`
	// harness, which then prints its own "PASS"/timing line to the SAME
	// stdout the parent is scanning for our one-word result — os.Exit skips
	// that harness completion output entirely, exactly like helperHold and
	// helperCheckFDs already do.
	if err == nil {
		fmt.Println("REAPED")
		os.Exit(0)
	}
	if errors.Is(err, ErrHeld) {
		fmt.Println("HELD")
		os.Exit(0)
	}
	fmt.Fprintf(os.Stderr, "helper TryReapProof: %v\n", err)
	os.Exit(1)
}

// startAttachHolder spawns a real "attach" helper subprocess against dir and
// waits (bounded by timeout) for it to report ACQUIRED, returning the
// elapsed time from process start to that report plus the cmd/stdin needed
// to release it later.
func startAttachHolder(t *testing.T, dir string, timeout time.Duration) (cmd *exec.Cmd, stdin io.WriteCloser, elapsed time.Duration) {
	t.Helper()
	cmd = helperCommand(helperActionEnv+"=attach", "LEASE_HELPER_DIR="+dir)
	var err error
	stdin, err = cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	start := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	line, err := readAcquired(bufio.NewReader(stdout), timeout)
	elapsed = time.Since(start)
	if err != nil {
		t.Fatalf("waiting for attach helper ACQUIRED: %v", err)
	}
	if line != "ACQUIRED\n" {
		t.Fatalf("attach helper said %q, want ACQUIRED", line)
	}
	return cmd, stdin, elapsed
}

func releaseHolder(t *testing.T, cmd *exec.Cmd, stdin io.WriteCloser) {
	t.Helper()
	if _, err := io.WriteString(stdin, "release\n"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("holder exit: %v", err)
	}
}

// TestTwoHolders_AttachRefPromptly spawns two independent real processes
// (holder1, holder2) that both call AttachRef against the SAME sandbox dir
// concurrently, and requires each to report ACQUIRED (i.e. hold refs
// SHARED) within 250ms — proving the brief lifecycle EX handshake inside
// AttachRef does not meaningfully serialize two unrelated attaches even
// though both must transiently take the same lifecycle lock.
func TestTwoHolders_AttachRefPromptly(t *testing.T) {
	dir := mustDir(t)
	const budget = 250 * time.Millisecond

	type result struct {
		cmd     *exec.Cmd
		stdin   io.WriteCloser
		elapsed time.Duration
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd, stdin, elapsed := startAttachHolder(t, dir, 2*time.Second)
			results <- result{cmd, stdin, elapsed}
		}()
	}
	wg.Wait()
	close(results)

	var holders []result
	for r := range results {
		holders = append(holders, r)
		if r.elapsed > budget {
			t.Errorf("holder took %v to report ACQUIRED, want < %v", r.elapsed, budget)
		}
	}
	if len(holders) != 2 {
		t.Fatalf("got %d holders, want 2", len(holders))
	}

	// Both really do hold refs SHARED concurrently: a zero-holder proof must
	// refuse while they do.
	if err := TryReapProof(dir, func() error { return nil }); !errors.Is(err, ErrHeld) {
		t.Errorf("TryReapProof with both holders attached = %v, want ErrHeld", err)
	}

	for _, h := range holders {
		releaseHolder(t, h.cmd, h.stdin)
	}
	if err := TryReapProof(dir, func() error { return nil }); err != nil {
		t.Errorf("TryReapProof after both holders released = %v, want nil", err)
	}
}

// TestLifecycleOperation_SerializesSeparatelyFromRefs proves the two locks
// really are independent axes: a live refs holder (attached, not doing
// anything lifecycle-related) does NOT block a lifecycle operation from
// acquiring the lifecycle lock and running — only a concurrent LIFECYCLE
// holder does that (see helperLifecycle usage below). This is the
// "lifecycle operation serializes separately [from refs]" requirement.
func TestLifecycleOperation_SerializesSeparatelyFromRefs(t *testing.T) {
	dir := mustDir(t)
	cmd, stdin, elapsed := startAttachHolder(t, dir, 2*time.Second)
	if elapsed > 250*time.Millisecond {
		t.Fatalf("attach holder took %v, want < 250ms", elapsed)
	}
	defer releaseHolder(t, cmd, stdin)

	// A lifecycle-only operation (no refs proof needed) must proceed
	// promptly despite the live refs holder.
	ranAt := time.Now()
	err := WithLifecycle(context.Background(), dir, func() error { return nil })
	if err != nil {
		t.Fatalf("WithLifecycle while a ref is attached = %v, want nil", err)
	}
	if elapsed := time.Since(ranAt); elapsed > 250*time.Millisecond {
		t.Errorf("WithLifecycle took %v while only a refs holder was live, want < 250ms", elapsed)
	}

	// But the REAP proof (which also checks refs) correctly still refuses.
	if err := TryReapProof(dir, func() error { return nil }); !errors.Is(err, ErrHeld) {
		t.Errorf("TryReapProof while a ref is attached = %v, want ErrHeld", err)
	}
}

// TestAttachVsTransitionOrdering is the end-to-end multiprocess proof of the
// attach-vs-transition invariant: two real attach holders register (each
// promptly), a real "reap" helper process observes HELD (not a race — every
// attempt while either holder is live must see it), then both holders
// release and a fresh reap helper observes REAPED.
func TestAttachVsTransitionOrdering(t *testing.T) {
	dir := mustDir(t)
	cmd1, stdin1, e1 := startAttachHolder(t, dir, 2*time.Second)
	if e1 > 250*time.Millisecond {
		t.Errorf("holder1 took %v to attach, want < 250ms", e1)
	}
	cmd2, stdin2, e2 := startAttachHolder(t, dir, 2*time.Second)
	if e2 > 250*time.Millisecond {
		t.Errorf("holder2 took %v to attach, want < 250ms", e2)
	}

	// Run the reap helper several times while both holders are live: it must
	// NEVER observe REAPED — the ordering guarantee is deterministic, not
	// merely "usually wins the race".
	for i := 0; i < 5; i++ {
		if got := runReapHelper(t, dir); got != "HELD" {
			t.Fatalf("reap attempt %d while both holders live = %q, want HELD", i, got)
		}
	}

	releaseHolder(t, cmd1, stdin1)
	if got := runReapHelper(t, dir); got != "HELD" {
		t.Fatalf("reap after only holder1 released = %q, want HELD (holder2 still live)", got)
	}

	releaseHolder(t, cmd2, stdin2)
	if got := runReapHelper(t, dir); got != "REAPED" {
		t.Fatalf("reap after both holders released = %q, want REAPED", got)
	}
}

func runReapHelper(t *testing.T, dir string) string {
	t.Helper()
	cmd := helperCommand(helperActionEnv+"=reap", "LEASE_HELPER_DIR="+dir)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("reap helper: %v (output: %s)", err, out)
	}
	return trimOneLine(string(out))
}

func trimOneLine(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// TestSIGKILL_ReleasesAttachedRef proves the SIGKILL-release property holds
// through the composed AttachRef helper too, not just the raw RefLease
// primitive: killing an attached holder with zero chance to run its own
// cleanup still lets a subsequent reap proof succeed, because the kernel
// releases the refs SHARED flock on fd close at process death.
func TestSIGKILL_ReleasesAttachedRef(t *testing.T) {
	dir := mustDir(t)
	cmd, _, elapsed := startAttachHolder(t, dir, 2*time.Second)
	if elapsed > 250*time.Millisecond {
		t.Errorf("attach took %v, want < 250ms", elapsed)
	}

	if got := runReapHelper(t, dir); got != "HELD" {
		t.Fatalf("reap while attached = %q, want HELD", got)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("Kill (SIGKILL): %v", err)
	}
	if waitErr := cmd.Wait(); waitErr == nil {
		t.Fatal("Wait on a SIGKILLed process returned nil, want a signal-exit error")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		if got := runReapHelper(t, dir); got == "REAPED" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("reap never observed REAPED after SIGKILLing the attached holder")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
