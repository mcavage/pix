// mcp_receipt_lifecycle_test.go — the corrected create-receipt LIFECYCLE
// (review findings 1/3/4/5 redrive). Pins, with fake processes and injected
// poll seams:
//
//   - A. the receipt is recorded WHILE the interactive `sbx run` process is
//     still alive (Start -> evidence poll -> commit -> Wait), never after it
//     exits; early process death and a poll timeout each degrade honestly.
//   - B. a concurrent explicit `mcp load` landing between the pre-create
//     clear and the create commit is MERGED, while a prior lifetime's loads
//     can never survive the clear.
//   - E. every successful launcher-side removal (pi-stack rm, task teardown,
//     replace pre-remove) clears the receipt; a failed/aborted removal
//     retains it.
//   - D. IsPartial semantics at the receipt layer (the join-layer half lives
//     in mcpjoin_test.go).
//   - a -race exerciser for loads racing the create commit under the
//     per-sandbox lock.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blockingCmd returns a portable *exec.Cmd that runs until the returned
// release func is called — a stand-in for the long-lived interactive `sbx
// run` session.
func blockingCmd(t *testing.T) (*exec.Cmd, func()) {
	t.Helper()
	flag := filepath.Join(t.TempDir(), "release")
	cmd := exec.Command("sh", "-c", fmt.Sprintf("while [ ! -e %q ]; do sleep 0.02; done", flag))
	return cmd, func() {
		if err := os.WriteFile(flag, nil, 0o644); err != nil {
			t.Fatalf("release: %v", err)
		}
	}
}

// --- A: receipt exists BEFORE the interactive process is released ----------

func TestCreateReceipt_RecordedWhileSessionAlive(t *testing.T) {
	dir := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return dir, nil })
	withCreatePollSeams(t, probeAlways(sbxRunning), time.Millisecond, 5*time.Second)

	cmd, release := blockingCmd(t)
	done := make(chan error, 1)
	go func() { done <- execSbxRunAndRecordCreate(cmd, true, "pi-stack-live", []string{"slack"}) }()

	// The receipt must become readable while the session is still running.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, status, _ := readSandboxMCPReceipt(dir, "pi-stack-live"); status == sandboxMCPStateOK {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("execSbxRunAndRecordCreate returned before the receipt was observed: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("receipt never appeared while the interactive session was alive")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// The interactive process is still held: the wrapper must NOT have
	// returned (Wait comes after the record, not instead of it).
	select {
	case err := <-done:
		t.Fatalf("interactive process released before the session ended: %v", err)
	default:
	}

	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("clean session exit after a recorded create must be nil, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("execSbxRunAndRecordCreate never returned after the session ended")
	}
}

// --- B: concurrent load merged; prior lifetime's loads dropped -------------

func TestCreateReceipt_MergesConcurrentLoadDropsPriorLifetime(t *testing.T) {
	dir := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return dir, nil })
	sandbox := "pi-stack-merge"

	// Prior lifetime: a create receipt with a load that must NOT survive.
	if err := writeCreateReceipt(dir, sandbox, []string{"old"}, receiptClock); err != nil {
		t.Fatal(err)
	}
	if err := appendLoadReceipt(dir, sandbox, "stale", receiptClock); err != nil {
		t.Fatal(err)
	}

	// The probe fires AFTER the pre-create clear and BEFORE the create commit
	// — exactly the window a concurrent `pi-stack mcp load` races into.
	probe := func(string) sbxState {
		if err := appendLoadReceipt(dir, sandbox, "fresh", receiptClock); err != nil {
			t.Errorf("concurrent appendLoadReceipt: %v", err)
		}
		return sbxRunning
	}
	withCreatePollSeams(t, probe, time.Millisecond, 5*time.Second)

	if err := execSbxRunAndRecordCreate(trueCmd(t), true, sandbox, []string{"gog"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r, status, err := readSandboxMCPReceipt(dir, sandbox)
	if err != nil || status != sandboxMCPStateOK {
		t.Fatalf("status=%v err=%v", status, err)
	}
	if r.CreatedAt == "" || len(r.Preloaded) != 1 || r.Preloaded[0] != "gog" {
		t.Errorf("create commit = created_at %q, preloaded %v; want a fresh create with [gog]", r.CreatedAt, r.Preloaded)
	}
	if len(r.Loads) != 1 || r.Loads[0].Name != "fresh" {
		t.Errorf("Loads = %+v, want exactly [fresh]: the concurrent load preserved, the prior lifetime's %q gone", r.Loads, "stale")
	}
}

// --- A: early process death / clean exit without evidence ------------------

// A clean exit with NO creation evidence writes nothing and returns the
// process's own (nil) result — never a fabricated receipt.
func TestCreateReceipt_CleanExitWithoutEvidenceWritesNothing(t *testing.T) {
	dir := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return dir, nil })
	withCreatePollSeams(t, probeAlways(sbxAbsent), time.Millisecond, 5*time.Second)

	if err := execSbxRunAndRecordCreate(trueCmd(t), true, "pi-stack-noev", []string{"slack"}); err != nil {
		t.Fatalf("clean exit must surface the process's own nil result, got %v", err)
	}
	if _, status, _ := readSandboxMCPReceipt(dir, "pi-stack-noev"); status != sandboxMCPStateAbsent {
		t.Fatalf("status = %v, want absent (no creation evidence, no receipt)", status)
	}
}

// Evidence that appears only as the process exits is still evidence: the
// final probe records it (a detached/fast create must not lose provenance).
func TestCreateReceipt_EvidenceAtExitStillRecorded(t *testing.T) {
	dir := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return dir, nil })
	var calls atomic.Int64
	probe := func(string) sbxState {
		if calls.Add(1) == 1 {
			return sbxAbsent
		}
		return sbxRunning
	}
	withCreatePollSeams(t, probe, time.Millisecond, 5*time.Second)

	if err := execSbxRunAndRecordCreate(trueCmd(t), true, "pi-stack-lateev", []string{"slack"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, status, _ := readSandboxMCPReceipt(dir, "pi-stack-lateev"); status != sandboxMCPStateOK {
		t.Fatalf("status = %v, want ok (evidence at exit is evidence)", status)
	}
}

// --- A: poll timeout with a live session --------------------------------

// The poll timing out while the session is still running must (1) keep the
// session interactive to its natural end, (2) write no receipt, and (3)
// surface a *receiptRecordError so the caller exits non-zero with the honest
// "launched, state unrecorded" report.
func TestCreateReceipt_PollTimeoutReportsUnrecorded(t *testing.T) {
	dir := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return dir, nil })
	withCreatePollSeams(t, probeAlways(sbxAbsent), time.Millisecond, 30*time.Millisecond)

	start := time.Now()
	err := execSbxRunAndRecordCreate(exec.Command("sleep", "0.4"), true, "pi-stack-timeout", []string{"slack"})
	var rerr *receiptRecordError
	if !errors.As(err, &rerr) {
		t.Fatalf("want a *receiptRecordError on poll timeout, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error should name the timeout, got %q", err.Error())
	}
	if elapsed := time.Since(start); elapsed < 350*time.Millisecond {
		t.Errorf("returned after %v — the session must still be waited to completion after the poll times out", elapsed)
	}
	if _, status, _ := readSandboxMCPReceipt(dir, "pi-stack-timeout"); status != sandboxMCPStateAbsent {
		t.Fatalf("status = %v, want absent (a timed-out poll must not fabricate a receipt)", status)
	}
}

// --- E: launcher removals clear; failed removals retain --------------------

func TestApplyReplaceRm_ClearsReceiptOnSuccessRetainsOnFailure(t *testing.T) {
	dir := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return dir, nil })
	sandbox := "pi-stack-replaceclear"
	plan := runLaunchPlan{RmFirst: true}

	if err := writeCreateReceipt(dir, sandbox, []string{"slack"}, receiptClock); err != nil {
		t.Fatal(err)
	}
	ok := shellEnv{run: func(string, ...string) (string, error) { return "", nil }}
	if err := applyReplaceRm(ok, plan, sandbox); err != nil {
		t.Fatalf("applyReplaceRm: %v", err)
	}
	if _, status, _ := readSandboxMCPReceipt(dir, sandbox); status != sandboxMCPStateAbsent {
		t.Fatalf("status = %v, want absent after a successful replace pre-remove", status)
	}

	if err := writeCreateReceipt(dir, sandbox, []string{"slack"}, receiptClock); err != nil {
		t.Fatal(err)
	}
	bad := shellEnv{run: func(string, ...string) (string, error) { return "", errors.New("rm failed") }}
	if err := applyReplaceRm(bad, plan, sandbox); err == nil {
		t.Fatal("want the rm failure surfaced")
	}
	if _, status, _ := readSandboxMCPReceipt(dir, sandbox); status != sandboxMCPStateOK {
		t.Fatalf("status = %v, want ok: a FAILED removal must retain the receipt", status)
	}
}

func TestRemovePiStackSandbox_ClearsReceiptOnSuccessRetainsOnFailure(t *testing.T) {
	dir := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return dir, nil })
	sandbox := "pi-stack-rmclear"

	if err := writeCreateReceipt(dir, sandbox, []string{"slack"}, receiptClock); err != nil {
		t.Fatal(err)
	}
	ok := shellEnv{run: func(string, ...string) (string, error) { return "", nil }}
	if err := removePiStackSandbox(ok, sandbox); err != nil {
		t.Fatalf("removePiStackSandbox: %v", err)
	}
	if _, status, _ := readSandboxMCPReceipt(dir, sandbox); status != sandboxMCPStateAbsent {
		t.Fatalf("status = %v, want absent after `pi-stack rm` succeeded", status)
	}

	if err := writeCreateReceipt(dir, sandbox, []string{"slack"}, receiptClock); err != nil {
		t.Fatal(err)
	}
	bad := shellEnv{run: func(string, ...string) (string, error) { return "", errors.New("rm failed") }}
	if err := removePiStackSandbox(bad, sandbox); err == nil {
		t.Fatal("want the rm failure surfaced")
	}
	if _, status, _ := readSandboxMCPReceipt(dir, sandbox); status != sandboxMCPStateOK {
		t.Fatalf("status = %v, want ok: a FAILED `pi-stack rm` must retain the receipt", status)
	}
}

// A task teardown that positively removes its sandbox clears the receipt; an
// aborted teardown (sandbox still running, non-force) retains it.
func TestExecuteTaskTeardown_ClearsReceiptOnRemovalRetainsOnAbort(t *testing.T) {
	dir := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return dir, nil })
	sandbox := "pi-stack-t-x-work"
	m := taskMeta{Name: "work", Sandbox: sandbox, Mainroot: t.TempDir(), Branch: "pi-stack/work"}

	// Success: git snapshot ok, `sbx ls` reads absent, `sbx rm -f` succeeds.
	if err := writeCreateReceipt(dir, sandbox, []string{"slack"}, receiptClock); err != nil {
		t.Fatal(err)
	}
	ok := shellEnv{run: func(string, ...string) (string, error) { return "", nil }}
	var out bytes.Buffer
	if rc := executeTaskTeardown(ok, &out, m, t.TempDir(), "work", "refs/pi-stack/recovered/work", true, taskState{}); rc != 0 {
		t.Fatalf("rc = %d, want 0, out:\n%s", rc, out.String())
	}
	if _, status, _ := readSandboxMCPReceipt(dir, sandbox); status != sandboxMCPStateAbsent {
		t.Fatalf("status = %v, want absent after task teardown removed the sandbox", status)
	}

	// Abort: non-force with the sandbox running — teardown refuses before any
	// rm, so the receipt (a live lifetime's evidence) must survive.
	if err := writeCreateReceipt(dir, sandbox, []string{"slack"}, receiptClock); err != nil {
		t.Fatal(err)
	}
	running := shellEnv{run: func(name string, args ...string) (string, error) {
		if name == "sbx" && len(args) == 1 && args[0] == "ls" {
			return sandbox + "  abc123  running\n", nil
		}
		return "", nil
	}}
	out.Reset()
	if rc := executeTaskTeardown(running, &out, m, t.TempDir(), "work", "refs/pi-stack/recovered/work", false, taskState{}); rc == 0 {
		t.Fatalf("rc = 0, want non-zero (running sandbox, non-force), out:\n%s", out.String())
	}
	if _, status, _ := readSandboxMCPReceipt(dir, sandbox); status != sandboxMCPStateOK {
		t.Fatalf("status = %v, want ok: an ABORTED teardown must retain the receipt", status)
	}
}

// --- D: partial receipt semantics at the receipt layer ---------------------

func TestReceiptIsPartial(t *testing.T) {
	dir := t.TempDir()
	sandbox := "pi-stack-partial"

	// Load-only (no create ever observed): partial.
	if err := appendLoadReceipt(dir, sandbox, "slack", receiptClock); err != nil {
		t.Fatal(err)
	}
	r, status, err := readSandboxMCPReceipt(dir, sandbox)
	if err != nil || status != sandboxMCPStateOK {
		t.Fatalf("status=%v err=%v", status, err)
	}
	if !r.IsPartial() {
		t.Error("a load-only receipt (empty CreatedAt) must be IsPartial")
	}

	// A committed create makes it full — even with an empty preload set.
	if err := commitCreateReceipt(dir, sandbox, nil, receiptClock); err != nil {
		t.Fatal(err)
	}
	r, _, _ = readSandboxMCPReceipt(dir, sandbox)
	if r.IsPartial() {
		t.Error("a receipt with CreatedAt set must not be IsPartial")
	}
	if len(r.Loads) != 1 || r.Loads[0].Name != "slack" {
		t.Errorf("Loads = %+v, want the pre-commit load merged, not erased", r.Loads)
	}

	var nilReceipt *sandboxMCPReceipt
	if nilReceipt.IsPartial() {
		t.Error("a nil receipt is no receipt at all, not a partial one")
	}
}

// --- race: loads racing the create commit under the per-sandbox lock -------

func TestCreateCommitRacesLoads(t *testing.T) {
	dir := t.TempDir()
	sandbox := "pi-stack-race"
	if err := clearSandboxMCPReceipt(dir, sandbox); err != nil {
		t.Fatal(err)
	}

	const loaders = 8
	var wg sync.WaitGroup
	errs := make([]error, loaders+1)
	for i := 0; i < loaders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = appendLoadReceipt(dir, sandbox, fmt.Sprintf("srv-%d", i), receiptClock)
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		errs[loaders] = commitCreateReceipt(dir, sandbox, []string{"gog"}, receiptClock)
	}()
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}

	// Whatever the interleaving: the create is committed and NO load was lost
	// (commit merges those before it; those after append to the committed
	// receipt) — all serialized by the per-sandbox flock.
	r, status, err := readSandboxMCPReceipt(dir, sandbox)
	if err != nil || status != sandboxMCPStateOK {
		t.Fatalf("status=%v err=%v", status, err)
	}
	if r.CreatedAt == "" || len(r.Preloaded) != 1 || r.Preloaded[0] != "gog" {
		t.Errorf("create commit lost: created_at %q, preloaded %v", r.CreatedAt, r.Preloaded)
	}
	if len(r.Loads) != loaders {
		t.Errorf("Loads = %d entries (%+v), want %d — a concurrent load was lost", len(r.Loads), r.Loads, loaders)
	}
}
