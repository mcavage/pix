//go:build unix

package lease

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// These tests exercise REAL kernel flock semantics across REAL OS processes:
// no mocks, no fakes standing in for the kernel. TestHelperProcess is not a
// test in its own right — it is this same compiled test binary re-invoked as
// a subprocess (the standard Go pattern also used by os/exec_test.go's
// helperCommand), selected by -test.run and dispatched by the
// LEASE_HELPER_ACTION env var. A plain `go test` leaves that env var unset,
// so TestHelperProcess does nothing and passes trivially.

const helperActionEnv = "LEASE_HELPER_ACTION"

// helperCommand builds a Cmd that re-execs this test binary as a helper.
// Deliberately takes no *testing.T: several callers below invoke it from
// worker goroutines, and only the test's own goroutine may call
// t.Fatal/FailNow.
func helperCommand(env ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), env...)
	return cmd
}

// helperActions is the LEASE_HELPER_ACTION dispatch registry. Split across
// files (this one seeds "hold"/"check-fds"; ordering_process_test.go adds
// "attach"/"lifecycle"/"reap" via its own init) so each set of helper
// process behaviors lives next to the tests that exercise it, rather than
// all funneling through one giant switch.
var helperActions = map[string]func(){
	"hold":      helperHold,
	"check-fds": helperCheckFDs,
}

// TestHelperProcess dispatches on LEASE_HELPER_ACTION; see helperCommand.
func TestHelperProcess(t *testing.T) {
	action := os.Getenv(helperActionEnv)
	if action == "" {
		return // not invoked as a helper; a plain `go test` no-ops here.
	}
	fn, ok := helperActions[action]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown %s=%s\n", helperActionEnv, action)
		os.Exit(2)
	}
	fn()
}

// helperHold opens the lease at LEASE_HELPER_DIR, acquires it in
// LEASE_HELPER_MODE ("sh" or "ex"), prints "ACQUIRED\n" once held, then
// blocks reading a line from stdin. "release\n" causes a clean Close (an
// explicit Unlock) then exit(0). Anything else — EOF because the parent
// closed the pipe, or nothing at all because the parent is about to SIGKILL
// this process — leaves it blocked so an external process-death event is
// what ends it, which is exactly what TestSIGKILL_ReleasesLock drives: the
// kernel, not this program's own cleanup code, must be what releases the
// flock.
func helperHold() {
	dir := os.Getenv("LEASE_HELPER_DIR")
	mode := os.Getenv("LEASE_HELPER_MODE")
	l, err := OpenRefLease(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper Open: %v\n", err)
		os.Exit(1)
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

	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if line == "release\n" {
		l.Close()
		os.Exit(0)
	}
	select {} // wait to be killed
}

// helperCheckFDs reports, for each of two EXACT fd numbers passed in
// LEASE_HELPER_TARGET_A / _B, whether that fd number is still open in this
// (post-exec) process. The parent already knows both fd numbers before it
// execs this helper: fork+exec preserves the fd table verbatim except for
// entries the kernel closes because they are O_CLOEXEC-marked, so nothing is
// renumbered across the exec. That makes "is fd N still open" a direct,
// one-syscall fcntl(F_GETFD) question (see fdOpenInThisProcess below), with
// no /dev/fd enumeration and no path resolution (fcntl F_GETPATH) needed on
// either platform this package builds for. scanFDsForTargets in
// lock_process_fds_linux_test.go is the older, Linux-only, path-based
// technique this replaced for the cross-process proof; it survives there only
// for its own direct, in-process self-test.
func helperCheckFDs() {
	for _, label := range []string{"A", "B"} {
		raw := os.Getenv("LEASE_HELPER_TARGET_" + label)
		fd, err := strconv.Atoi(raw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bad fd for %s=%q: %v\n", label, raw, err)
			os.Exit(2)
		}
		fmt.Printf("%s=%v\n", label, fdOpenInThisProcess(fd))
	}
}

// fdOpenInThisProcess reports whether fd currently names an open descriptor
// in THIS process, via fcntl(fd, F_GETFD): the kernel answers EBADF for a
// closed or never-opened fd number and the descriptor's flags word
// otherwise. syscall.SYS_FCNTL and syscall.F_GETFD are both POSIX and both
// defined by the syscall package on every unix this file builds for (linux
// and darwin), so this one function needs no platform split at all — unlike
// the abandoned Darwin scan-all-fds approach, which had to walk the entire
// numeric fd space and read each one back out via fcntl(fd, F_GETPATH, ...)
// because it did not yet know which fd number it was looking for.
func fdOpenInThisProcess(fd int) bool {
	_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(syscall.F_GETFD), 0)
	return errno == 0
}

// readAcquired reads exactly one line from r, bounded by timeout so a stuck
// helper fails the test instead of hanging the suite.
func readAcquired(r *bufio.Reader, timeout time.Duration) (string, error) {
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
		return res.line, res.err
	case <-time.After(timeout):
		return "", fmt.Errorf("timed out after %v waiting for a line", timeout)
	}
}

// TestSIGKILL_ReleasesLock spawns a real subprocess holding the EXCLUSIVE
// lease, confirms via a real non-blocking flock probe that it is genuinely
// held, SIGKILLs the process with zero chance to run its own defer/Close,
// and then confirms this process can immediately acquire the same lock — the
// kernel released it on fd close at process death, not on any cooperative
// cleanup this package wrote.
func TestSIGKILL_ReleasesLock(t *testing.T) {
	dir := mustDir(t)
	cmd := helperCommand(helperActionEnv+"=hold", "LEASE_HELPER_DIR="+dir, "LEASE_HELPER_MODE=ex")
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
		t.Fatalf("waiting for helper ACQUIRED: %v", err)
	}
	if line != "ACQUIRED\n" {
		t.Fatalf("helper said %q, want ACQUIRED", line)
	}

	prober, err := OpenRefLease(dir)
	if err != nil {
		t.Fatalf("Open prober: %v", err)
	}
	defer prober.Close()
	if err := prober.TryExclusive(); !errors.Is(err, ErrHeld) {
		t.Fatalf("TryExclusive while helper holds exclusive = %v, want ErrHeld", err)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("Kill (SIGKILL): %v", err)
	}
	if waitErr := cmd.Wait(); waitErr == nil {
		t.Fatal("Wait on a SIGKILLed process returned nil, want a signal-exit error")
	}

	if err := prober.TryExclusive(); err != nil {
		t.Fatalf("TryExclusive after SIGKILLing the exclusive holder = %v, want success (kernel releases flock on process death)", err)
	}
}

// TestCLOEXEC_ChildDoesNotInheritLeaseFd proves the lease fd's O_CLOEXEC is
// real, load-bearing behavior rather than an incidental default: a REAL
// child process spawned via a plain exec.Command inherits an "unprotected"
// fd opened without O_CLOEXEC at the SAME fd table (fork+exec duplicates the
// whole table; exec only closes cloexec-marked entries) while the lease fd,
// opened through this package's openNoFollow, does not appear at all. This
// process already knows both fd numbers (rawFd and l.Fd()) before it execs
// the helper, so it hands them over directly rather than having the child
// rediscover them by scanning; see helperCheckFDs/fdOpenInThisProcess above
// for why that makes this proof identical on Linux and Darwin.
func TestCLOEXEC_ChildDoesNotInheritLeaseFd(t *testing.T) {
	dir := mustDir(t)
	l, err := OpenRefLease(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	unprotectedPath := filepath.Join(dir, "unprotected-comparison")
	rawFd, err := syscall.Open(unprotectedPath, syscall.O_RDWR|syscall.O_CREAT, 0o600)
	if err != nil {
		t.Fatalf("syscall.Open unprotected: %v", err)
	}
	defer syscall.Close(rawFd)

	cmd := helperCommand(helperActionEnv+"=check-fds",
		"LEASE_HELPER_TARGET_A="+strconv.Itoa(rawFd),
		"LEASE_HELPER_TARGET_B="+strconv.Itoa(int(l.Fd())),
	)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helper check-fds: %v (output: %s)", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "A=true") {
		t.Errorf("child fd scan = %q, want A=true (the unprotected fd DOES leak without O_CLOEXEC — this is what proves the methodology is real)", got)
	}
	if !strings.Contains(got, "B=false") {
		t.Errorf("child fd scan = %q, want B=false (the lease fd must NOT be inherited: O_CLOEXEC)", got)
	}
}

// TestEightHolders_RaceThenZeroHolderProof starts 8 real subprocesses
// concurrently, racing them to acquire the SHARED lease at roughly the same
// instant, confirms all 8 hold it simultaneously (and that the exclusive
// zero-holder proof correctly refuses while they do), then releases all 8
// and confirms the proof then succeeds.
func TestEightHolders_RaceThenZeroHolderProof(t *testing.T) {
	dir := mustDir(t)
	const n = 8

	type child struct {
		cmd   *exec.Cmd
		stdin io.WriteCloser
	}
	children := make([]*child, n)
	errs := make([]error, n)
	acquired := make([]bool, n)

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := helperCommand(helperActionEnv+"=hold", "LEASE_HELPER_DIR="+dir, "LEASE_HELPER_MODE=sh")
			stdin, err := cmd.StdinPipe()
			if err != nil {
				errs[i] = err
				return
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				errs[i] = err
				return
			}
			cmd.Stderr = os.Stderr
			if err := cmd.Start(); err != nil {
				errs[i] = err
				return
			}
			line, err := readAcquired(bufio.NewReader(stdout), 5*time.Second)
			if err != nil {
				errs[i] = err
				return
			}
			if line != "ACQUIRED\n" {
				errs[i] = fmt.Errorf("holder %d said %q, want ACQUIRED", i, line)
				return
			}
			acquired[i] = true
			children[i] = &child{cmd: cmd, stdin: stdin}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("holder %d: %v", i, err)
		}
	}
	for i, ok := range acquired {
		if !ok {
			t.Fatalf("holder %d never reported ACQUIRED", i)
		}
	}

	prober, err := OpenRefLease(dir)
	if err != nil {
		t.Fatalf("Open prober: %v", err)
	}
	defer prober.Close()
	if err := prober.TryExclusive(); !errors.Is(err, ErrHeld) {
		t.Fatalf("TryExclusive with %d shared holders = %v, want ErrHeld", n, err)
	}

	for i, c := range children {
		if _, err := io.WriteString(c.stdin, "release\n"); err != nil {
			t.Fatalf("release holder %d: %v", i, err)
		}
		if err := c.cmd.Wait(); err != nil {
			t.Fatalf("holder %d exit: %v", i, err)
		}
	}

	if err := prober.TryExclusive(); err != nil {
		t.Fatalf("TryExclusive after all %d holders released = %v, want success (zero-holder proof)", n, err)
	}
}
