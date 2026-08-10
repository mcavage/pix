package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"pix/host/monitor"
	"pix/host/service"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/rpc"
)

// syncBuffer is a bytes.Buffer safe for the test goroutine to read while a
// backgrounded monitor run writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// runMonitorCore parses argv through the REAL root and runs the follow loop
// with an injected context + default store root. The verb has no other core:
// argv parsing is the root's, following is monitor's.
func runMonitorCore(ctx context.Context, argv []string, out io.Writer, defaultRoot string, tty bool) error {
	var ran error
	testSeams.monitor = func(c *monitorCmd, d *cli.Deps) error {
		if c.Path == "" {
			c.Path = defaultRoot
		}
		ran = c.follow(ctx, d, tty)
		return ran
	}
	defer func() { testSeams.monitor = nil }()
	d := &cli.Deps{Out: out, Err: out}
	if code := dispatch(append([]string{"monitor"}, argv...), d); code == 2 {
		return cli.Usagef("monitor: bad invocation")
	}
	return ran
}

func TestMonitorHelpAndUsageErrors(t *testing.T) {
	for _, argv := range [][]string{{"-h"}, {"--help"}, {"--help", "mybox"}} {
		var out bytes.Buffer
		if err := runMonitorCore(context.Background(), argv, &out, t.TempDir(), false); err != nil {
			t.Fatalf("runMonitorCore(%v): %v", argv, err)
		}
		// The usage text is the only place these operational facts are
		// discoverable, so pin them rather than just "usage:". It must also
		// say plainly this is a reader with no listener of its own, and point
		// at `pix serve` for the ingest side.
		// Generated help re-WRAPS its prose, so a fact can land across a line
		// break. Collapse whitespace before asserting: what must survive is the
		// fact, not the column it happens to fall in.
		flat := strings.Join(strings.Fields(out.String()), " ")
		for _, want := range []string{"Usage: pix monitor", "PURE READER", "never binds a port", "pix serve", "make load", "PIX_MONITOR=0", "PIX_MONITOR_URL", "CASE-SENSITIVE", "--path", "--json"} {
			if !strings.Contains(flat, want) {
				t.Errorf("monitor usage missing %q", want)
			}
		}
		// --bind/--port moved to `pix serve`; monitor no longer recognizes them.
		if strings.Contains(out.String(), "--bind ADDR") || strings.Contains(out.String(), "--port N") {
			t.Errorf("monitor usage still advertises --bind/--port, which moved to `pix serve`:\n%s", out.String())
		}
	}
	for _, argv := range [][]string{{"--nope"}, {"one", "two"}, {"--bind", "0.0.0.0"}, {"--port", "0"}} {
		err := runMonitorCore(context.Background(), argv, io.Discard, t.TempDir(), false)
		if err == nil {
			t.Fatalf("runMonitorCore(%v) = nil error, want a usage error", argv)
		}
		var usage cli.UsageError
		if !errors.As(err, &usage) {
			t.Errorf("runMonitorCore(%v) error = %v (%T), want a usage error (exit 2)", argv, err, err)
		}
	}
}

// storeFixture writes one captured stream into dir the way the store does,
// so a run has something real to read.
func storeFixture(t *testing.T, dir, sandboxID, sessionID string, events ...string) {
	t.Helper()
	streamDir := filepath.Join(dir, sandboxID+"="+sessionID)
	if err := os.MkdirAll(streamDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := strings.Join(events, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(streamDir, "events.ndjson"), []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func toolEndLine(sandboxID, sessionID, toolID string) string {
	return `{"kind":"tool_end","sandboxId":"` + sandboxID + `","sessionId":"` + sessionID +
		`","turnId":"t1","seq":1,"ts":1700000000000,"toolId":"` + toolID + `","ok":true,"resultBytes":7,"durationMs":3}`
}

// runMonitorUntil runs runMonitorCore in the background until out contains
// want, then cancels — Follow polls, so a test waits rather than calling it
// once.
func runMonitorUntil(t *testing.T, argv []string, defaultRoot string, want string) string {
	t.Helper()
	var out syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runMonitorCore(ctx, argv, &out, defaultRoot, false) }()
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(out.String(), want) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runMonitorCore: %v", err)
	}
	return out.String()
}

// monitor is a PURE reader: it must read a pre-existing store with no
// listener ever started, whether or not --path is given.
func TestMonitorDefaultRootReadsWithoutAListener(t *testing.T) {
	dir := t.TempDir()
	storeFixture(t, dir, "sbx", "sess", toolEndLine("sbx", "sess", "tool-1"))
	out := runMonitorUntil(t, nil, dir, "tool_end")
	if !strings.Contains(out, "sbx/sess") || !strings.Contains(out, "tool_end") {
		t.Fatalf("output = %q, want a concise tool_end line", out)
	}
}

func TestMonitorPathModeReadsAnArbitraryRoot(t *testing.T) {
	defaultRoot := t.TempDir() // deliberately empty: --path must override it
	dir := t.TempDir()
	storeFixture(t, dir, "sbx", "sess", toolEndLine("sbx", "sess", "tool-1"))
	out := runMonitorUntil(t, []string{"--path", dir}, defaultRoot, "tool_end")
	if !strings.Contains(out, "sbx/sess") || !strings.Contains(out, "tool_end") {
		t.Fatalf("output = %q, want a concise tool_end line", out)
	}
}

func TestMonitorPathModeJSONAndFilter(t *testing.T) {
	dir := t.TempDir()
	storeFixture(t, dir, "keep-me", "sess", toolEndLine("keep-me", "sess", "kept"))
	storeFixture(t, dir, "other", "sess", toolEndLine("other", "sess", "dropped"))

	out := runMonitorUntil(t, []string{"keep", "--path", dir, "--json"}, t.TempDir(), "kept")
	if strings.Contains(out, "dropped") {
		t.Errorf("filtered output %q includes a non-matching stream", out)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("--json emitted a non-JSON line %q: %v", line, err)
		}
		if m["kind"] != "tool_end" {
			t.Errorf("--json line kind = %v, want tool_end", m["kind"])
		}
	}
}

func TestMonitorIsAKnownVerb(t *testing.T) {
	if !knownVerbs()["monitor"] {
		t.Fatal(`knownVerbs()["monitor"] = false, want true`)
	}
}

// startFakeServeProc spawns a real, live process whose argv genuinely
// verifies as `pix-host serve` (see service.cmdlineIsServe: argv[0]'s base is
// "pix-host", argv[1] is "serve") by symlinking sh under that name, so a test
// can exercise the real ServeIdentityUp identity check instead of a stub.
// The process is killed and reaped on test cleanup.
func startFakeServeProc(t *testing.T) (pid int) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no sh on PATH: %v", err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "pix-host")
	if err := os.Symlink(sh, bin); err != nil {
		t.Fatalf("symlink %s -> %s: %v", bin, sh, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "serve"), []byte("while :; do sleep 0.05; done\n"), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}
	cmd := exec.Command(bin, "serve")
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s serve: %v", bin, err)
	}
	reaped := make(chan struct{})
	go func() { _ = cmd.Wait(); close(reaped) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-reaped
	})
	return cmd.Process.Pid
}

// isolateServeState points config.ServePidPath/StateDir at an empty temp
// dir for the duration of a test, so "is `pix-host serve` running" always
// reads false regardless of what happens to be running on the machine that
// executes the test suite.
func isolateServeState(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
}

// TestMonitorOneShotIsTheNonTTYDefault is the core fix: `pix monitor --json |
// head -5` must NOT block forever. With no --follow and tty=false (a pipe),
// follow must return on its own, with no context cancellation at all, once
// it has printed what is already stored.
func TestMonitorOneShotIsTheNonTTYDefault(t *testing.T) {
	isolateServeState(t)
	dir := t.TempDir()
	storeFixture(t, dir, "sbx", "sess",
		toolEndLine("sbx", "sess", "tool-1"), toolEndLine("sbx", "sess", "tool-2"))

	var out bytes.Buffer
	// A context that is NEVER canceled: if this hangs, it hangs on the test
	// deadline, not on us politely stopping it — which is exactly the
	// behavior this test exists to rule out.
	done := make(chan error, 1)
	go func() {
		done <- runMonitorCore(context.Background(), []string{"--path", dir, "--json"}, &out, t.TempDir(), false)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runMonitorCore: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("one-shot monitor did not return on its own; the non-TTY default must not block waiting for --follow")
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want exactly 2 (the two stored events, no more): %q", len(lines), out.String())
	}
	for _, line := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("one-shot --json emitted a non-JSON line %q: %v", line, err)
		}
	}
}

// pinIngest fixes the one input these tests cannot isolate: the ingest
// listener lives on a fixed, machine-wide port, so without this a developer
// running `pix serve` with monitor enabled and a CI box running nothing grade
// the same code differently.
func pinIngest(t *testing.T, up bool) {
	t.Helper()
	testSeams.ingestUp = func() bool { return up }
	t.Cleanup(func() { testSeams.ingestUp = nil })
}

// TestIngestUp_AnswersTheListenerNotTheServeProcess is the regression. `serve`
// starts only the services named in config's `services`, so `services =
// ["memory"]` is a live, healthy serve with NO ingest listener — and reading
// "serve is up" as "events may still arrive" made `pix monitor` print nothing
// and exit 0 on that host, forever, which is the exact ambiguity the one-shot
// error path exists to remove.
func TestIngestUp_AnswersTheListenerNotTheServeProcess(t *testing.T) {
	if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", monitor.DefaultPort), 250*time.Millisecond); err == nil {
		_ = c.Close()
		t.Skipf("something is already listening on :%d — the premise (no ingest) cannot be established here", monitor.DefaultPort)
	}
	isolateServeState(t)
	// A pid that ACTUALLY verifies as `pix-host serve` (argv[0] base "pix-host",
	// argv[1] "serve") — the same identity check ServeIdentityUp uses. Under the
	// old definition this alone made ingestUp() true.
	pid := startFakeServeProc(t)
	if err := os.MkdirAll(filepath.Dir(config.ServePidPath()), 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	if err := os.WriteFile(config.ServePidPath(), []byte(strconv.Itoa(pid)), 0o600); err != nil {
		t.Fatalf("write fake pidfile: %v", err)
	}
	if up, _ := service.ServeIdentityUp(service.ManagedActive, config.ServePidPath(), 0); !up {
		t.Fatal("premise not established: the fake serve must read as a running serve")
	}
	if ingestUp() {
		t.Error("ingestUp() = true from a serve process alone; it must answer the ingest LISTENER")
	}
}

// TestMonitorOneShotErrorsOnAbsentStoreWithIngestDown is the OTHER half of
// the fix: a one-shot read against a store that was never created, with no
// `pix-host serve` running to ever fill it, must fail loudly and
// actionably — not print nothing and exit 0, which is indistinguishable
// from "ran fine, there's just nothing to see".
func TestMonitorOneShotErrorsOnAbsentStoreWithIngestDown(t *testing.T) {
	isolateServeState(t)
	pinIngest(t, false)
	absent := filepath.Join(t.TempDir(), "never-created")

	var out bytes.Buffer
	err := runMonitorCore(context.Background(), []string{"--path", absent, "--json"}, &out, t.TempDir(), false)
	if err == nil {
		t.Fatalf("want a nonzero error for an absent store with no ingest running, got nil (output=%q)", out.String())
	}
	if code := cli.ExitCode(err); code != rpc.ExitServiceDown {
		t.Errorf("ExitCode(err) = %d, want %d (rpc.ExitServiceDown)", code, rpc.ExitServiceDown)
	}
	if !strings.Contains(out.String(), "pix serve") {
		t.Errorf("error output %q must name the fix (`pix serve`)", out.String())
	}
	if !strings.Contains(out.String(), "--follow") {
		t.Errorf("error output %q must mention --follow as the explicit opt-in to wait instead", out.String())
	}
}

// TestMonitorOneShotEmptyStoreWithIngestUpIsQuietSuccess is the documented
// exception: an empty store is legitimate, not broken, when an ingest
// listener IS running (nothing has arrived yet) — that is empty success,
// not an error, and must not be confused with the down case above.
func TestMonitorOneShotEmptyStoreWithIngestUpIsQuietSuccess(t *testing.T) {
	isolateServeState(t)
	// "Up" now means the INGEST LISTENER answers, not that some serve process
	// exists — see ingestUp. Pinned rather than dialled, because the real port
	// is shared with whatever this machine happens to be running.
	pinIngest(t, true)

	absent := filepath.Join(t.TempDir(), "never-created")
	var out bytes.Buffer
	err := runMonitorCore(context.Background(), []string{"--path", absent, "--json"}, &out, t.TempDir(), false)
	if err != nil {
		t.Fatalf("runMonitorCore: %v (output=%q)", err, out.String())
	}
	if out.String() != "" {
		t.Errorf("output = %q, want nothing: an empty store with a live ingest listener is quiet success", out.String())
	}
}

// TestMonitorFollowRunsUntilCanceled proves --follow (and the TTY-implied
// equivalent) is a real streaming mode, not just a no-op flag: with an
// empty store it must NOT return on its own — only ctx cancellation ends
// it — and a TTY run must print its honest banner before it starts
// waiting.
func TestMonitorFollowRunsUntilCanceled(t *testing.T) {
	isolateServeState(t)
	dir := t.TempDir() // deliberately empty

	var out syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runMonitorCore(ctx, []string{"--path", dir, "--follow"}, &out, t.TempDir(), true /* tty */)
	}()

	// It must still be running a moment later: nothing to read, no signal to
	// stop, so a one-shot implementation would have already returned.
	time.Sleep(200 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("follow returned on its own (err=%v) before cancellation; --follow must keep running on an empty store", err)
	default:
	}
	if !strings.Contains(out.String(), "Ctrl-C to stop") {
		t.Errorf("TTY follow banner %q must honestly say it is waiting and how to stop", out.String())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runMonitorCore after cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follow did not stop within 2s of context cancellation")
	}
}
