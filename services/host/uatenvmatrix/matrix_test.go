// matrix_test.go proves Run/CheckNames end to end using an INJECTED fake
// Executor — deliberately never a real `sbx` binary, since unit tests in
// this package must never need one (the whole point of the Executor seam).
package uatenvmatrix_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"pix/host/uatenvmatrix"
)

func writeFakeCandidateBinaries(t *testing.T) string {
	t.Helper()
	outDir := t.TempDir()
	for _, name := range []string{"pix", "pix-host"} {
		if err := os.WriteFile(filepath.Join(outDir, name), []byte("#!/bin/sh\n"), 0755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	return outDir
}

type recordedCall struct {
	name string
	args []string
	env  []string
	dir  string
}

// fakeExecutor is the injected command-execution seam every test in this
// file uses instead of a real `sbx` binary. onCall decides the response for
// each recorded call, keyed by whatever the test needs to distinguish
// (typically args[0]: "env" for create, "exec" for name-based exec).
type fakeExecutor struct {
	mu     sync.Mutex
	calls  []recordedCall
	onCall func(call recordedCall) (stdout, stderr string, err error)
}

func (f *fakeExecutor) Run(ctx context.Context, name string, args, env []string, dir string) (string, string, error) {
	call := recordedCall{name: name, args: append([]string(nil), args...), env: append([]string(nil), env...), dir: dir}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
	return f.onCall(call)
}

func (f *fakeExecutor) snapshot() []recordedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedCall(nil), f.calls...)
}

func TestCheckNames_NonEmptyAndDerivesFromRegistry(t *testing.T) {
	names := uatenvmatrix.CheckNames()
	if len(names) == 0 {
		t.Fatal("CheckNames() is empty; capabilities.named_checks must be non-empty")
	}
	want := []string{"environment_create_then_exec_invocation", "environment_uses_local_candidate_image", "environment_recreate_boundary", "environment_failed_create_cleanup", "environment_rm_scope_refusal", "environment_custom_agent_ollama"}
	if len(names) != len(want) {
		t.Fatalf("CheckNames() = %#v, want %#v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("CheckNames()[%d] = %q, want %q", i, names[i], want[i])
		}
	}
}

func TestRun_MissingCandidateBinaryFailsClosed(t *testing.T) {
	empty := t.TempDir()
	stepsDir := filepath.Join(t.TempDir(), "steps")
	if err := os.MkdirAll(stepsDir, 0700); err != nil {
		t.Fatal(err)
	}
	err := uatenvmatrix.Run(context.Background(), uatenvmatrix.Inputs{OutDir: empty, StepsDir: stepsDir})
	if err == nil {
		t.Fatal("expected an error for a candidate dir with no real binaries, got nil")
	}
	if !strings.Contains(err.Error(), "candidate binary missing") {
		t.Fatalf("expected a 'candidate binary missing' error, got: %v", err)
	}
}

// successfulFixtureNames is every pix-uatenv-* instance a fully successful
// Run() creates: successfulExecutor's `sbx ls --json` fresh-probe response
// reports all of them at once (cleanup's own probe only ever checks for ONE
// name at a time via strings.Contains, so a combined listing is exactly what
// a real `sbx ls` would show while more than one of this run's fixtures is
// still live).
const successfulFixtureNames = "pix-uatenv-fixture-0 pix-uatenv-fixture-image pix-uatenv-fixture-recreate pix-uatenv-fixture-ollama"

// successfulExecutor answers every command this package's checks (and their
// shared cleanupCreatedFixture teardown) can issue, keyed on ARGV SHAPE, not
// on guessing from fixture content — the deterministic success path,
// including full receipt-gated cleanup, for every check that creates a real
// fixture instance: environment_create_then_exec_invocation,
// environment_uses_local_candidate_image, environment_recreate_boundary,
// environment_failed_create_cleanup, and environment_custom_agent_ollama
// (every registered check except environment_rm_scope_refusal, which never
// touches the injected Executor at all). A `docker image inspect` call
// answers with a fixed digest; each `sbx env create` call is routed by the
// fixture file's own basename to the exact receipted response its check
// expects (the one carrying environment_recreate_boundary's drifted facet is
// refused — the expected native contract that check's whole assertion rests
// on; the one carrying environment_failed_create_cleanup's fixture fails
// outright with no instance ever positively identified, the before-receipt
// scenario that check's whole assertion rests on); a fresh `sbx ls --json`
// probe reports every successfully-receipted instance still live; and an
// environment-scoped `sbx env rm -f <path>` removal always succeeds. This
// executor exists to prove Run() end to end, not to exercise every
// capability branch (check_custom_agent_ollama_test.go and cleanup_test.go
// do that in isolation).
func successfulExecutor() *fakeExecutor {
	fe := &fakeExecutor{}
	fe.onCall = func(call recordedCall) (string, string, error) {
		if call.name == "docker" {
			return fakeCandidateDigest + "\n", "", nil
		}
		if len(call.args) > 0 && call.args[0] == "ls" {
			return successfulFixtureNames + "\n", "", nil
		}
		if len(call.args) > 1 && call.args[0] == "env" && call.args[1] == "rm" {
			return "removed\n", "", nil
		}
		if len(call.args) > 1 && call.args[0] == "env" && call.args[1] == "create" {
			fixturePath := call.args[len(call.args)-1]
			switch filepath.Base(fixturePath) {
			case "authored.sbxenv.yaml":
				return "created pix-uatenv-fixture-0 (positively identified) image digest: " + fakeCandidateDigest + "\n", "", nil
			case "candidate-image.sbxenv.yaml":
				return "created pix-uatenv-fixture-image (positively identified) image digest: " + fakeCandidateDigest + "\n", "", nil
			case "recreate-boundary.sbxenv.yaml":
				fixtureBytes, _ := os.ReadFile(fixturePath)
				if strings.Contains(string(fixtureBytes), "memory: 60g") {
					return "", "environment declaration changed since creation", errors.New("refused: drifted declaration")
				}
				return "created pix-uatenv-fixture-recreate (positively identified) image digest: " + fakeCandidateDigest + "\n", "", nil
			case "failed-create-cleanup.sbxenv.yaml":
				return "", "sbx: internal error resolving scoped secrets", errors.New("create failed before receipt")
			case "ollama-capability.sbxenv.yaml":
				return "created pix-uatenv-fixture-ollama (positively identified) image digest: " + fakeCandidateDigest + "\n", "", nil
			}
			return "", "", fmt.Errorf("successfulExecutor: unrecognized create fixture path %s", fixturePath)
		}
		return "", "", nil
	}
	return fe
}

// fakeCandidateDigest is the one digest matrix_test.go's shared fake
// executors answer with, both from `docker image inspect` and from the
// digest line embedded in a fake `sbx env create` log — kept identical so
// environment_uses_local_candidate_image's equality check passes.
const fakeCandidateDigest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

func TestRun_EnvironmentCreateThenExecInvocation_Success(t *testing.T) {
	outDir := writeFakeCandidateBinaries(t)
	runDir := t.TempDir()
	stepsDir := filepath.Join(runDir, "steps")
	if err := os.MkdirAll(stepsDir, 0700); err != nil {
		t.Fatal(err)
	}
	fe := successfulExecutor()

	if err := uatenvmatrix.Run(context.Background(), uatenvmatrix.Inputs{OutDir: outDir, StepsDir: stepsDir, Executor: fe, ImageTag: "docker.io/mcavage/pix:test-candidate"}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	logPath := filepath.Join(stepsDir, "env_environment_create_then_exec_invocation.log")
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("missing bounded artifact: %v", err)
	}
	if !strings.Contains(string(b), "RESULT: PASS") {
		t.Errorf("artifact does not record PASS: %s", b)
	}

	// Run() now executes all six registered checks, each of the four that
	// creates a real fixture instance also running its own receipt-gated
	// cleanup (a fresh `sbx ls --json` probe, then `sbx env rm -f <path>`):
	// this check's own create+exec+probe+remove calls (calls[0..3]),
	// environment_uses_local_candidate_image's docker-inspect+create+probe+remove
	// calls (calls[4..7]), environment_recreate_boundary's baseline+drifted
	// create calls plus its one cleanup probe+remove pair keyed on the
	// baseline's own receipt (calls[8..11]), environment_failed_create_cleanup's
	// single failed create call with no cleanup calls at all — no receipt,
	// no removal authority (calls[12]), environment_rm_scope_refusal's zero
	// calls (it never touches the injected Executor), and
	// environment_custom_agent_ollama's create+probe pair plus its own
	// cleanup probe+remove pair (calls[13..16]). This test asserts only on
	// the first check's own create+exec calls.
	calls := fe.snapshot()
	if len(calls) != 17 {
		t.Fatalf("expected exactly 17 executor calls, got %d: %#v", len(calls), calls)
	}
	create := calls[0]
	if create.name != "sbx" || len(create.args) != 3 || create.args[0] != "env" || create.args[1] != "create" {
		t.Fatalf("create call = %#v, want `sbx env create <fixture>`", create)
	}
	if !strings.HasSuffix(create.args[2], "authored.sbxenv.yaml") {
		t.Errorf("create call did not pass the authored fixture path: %#v", create)
	}
	fixtureBytes, err := os.ReadFile(create.args[2])
	if err != nil {
		t.Fatalf("read authored fixture back: %v", err)
	}
	if !strings.Contains(string(fixtureBytes), "agent: pix") {
		t.Errorf("authored fixture does not declare the Pix custom agent: %s", fixtureBytes)
	}

	execCall := calls[1]
	wantExecArgs := []string{
		"exec", "-it", "pix-uatenv-fixture-0", "--", "pi",
		"--kit", "/opt/pix/kit",
		"--skill", "/opt/pix/kit/skills",
		"--skill", "/home/uat/personal-context/skills",
		"--model", "anthropic/claude-sonnet-5",
		"--resume", "session-fixture-1",
	}
	if execCall.name != "sbx" || len(execCall.args) != len(wantExecArgs) {
		t.Fatalf("exec call = %#v, want name %q with %d args", execCall, "sbx", len(wantExecArgs))
	}
	for i, want := range wantExecArgs {
		if execCall.args[i] != want {
			t.Errorf("exec call args[%d] = %q, want %q (full: %#v)", i, execCall.args[i], want, execCall.args)
		}
	}

	for _, e := range execCall.env {
		if strings.HasPrefix(e, "MEMORY_PORT=") {
			t.Errorf("exec call environment referenced the memory port: %q", e)
		}
	}
}

func TestRun_EnvironmentCreateThenExecInvocation_CreateFailure(t *testing.T) {
	outDir := writeFakeCandidateBinaries(t)
	runDir := t.TempDir()
	stepsDir := filepath.Join(runDir, "steps")
	if err := os.MkdirAll(stepsDir, 0700); err != nil {
		t.Fatal(err)
	}
	fe := &fakeExecutor{onCall: func(call recordedCall) (string, string, error) {
		return "", "no such command\n", context.DeadlineExceeded
	}}

	err := uatenvmatrix.Run(context.Background(), uatenvmatrix.Inputs{OutDir: outDir, StepsDir: stepsDir, Executor: fe, ImageTag: "docker.io/mcavage/pix:test-candidate"})
	if err == nil {
		t.Fatal("expected Run to fail when sbx env create errors, got nil")
	}
	if !strings.Contains(err.Error(), "environment_create_then_exec_invocation") {
		t.Errorf("error does not name the failing check: %v", err)
	}

	logPath := filepath.Join(stepsDir, "env_environment_create_then_exec_invocation.log")
	b, rerr := os.ReadFile(logPath)
	if rerr != nil {
		t.Fatalf("missing bounded artifact even on failure: %v", rerr)
	}
	if !strings.Contains(string(b), "RESULT: FAIL") {
		t.Errorf("artifact does not record FAIL: %s", b)
	}

	// Only the create call should ever have been attempted: a failed create
	// must not proceed to a name-based exec against an instance that was
	// never positively identified.
	if calls := fe.snapshot(); len(calls) != 1 {
		t.Fatalf("expected exactly 1 executor call after a failed create, got %d: %#v", len(calls), calls)
	}
}

func TestRun_EnvironmentCreateThenExecInvocation_UnidentifiedInstanceNameFails(t *testing.T) {
	outDir := writeFakeCandidateBinaries(t)
	runDir := t.TempDir()
	stepsDir := filepath.Join(runDir, "steps")
	if err := os.MkdirAll(stepsDir, 0700); err != nil {
		t.Fatal(err)
	}
	fe := &fakeExecutor{onCall: func(call recordedCall) (string, string, error) {
		// A create call that succeeds (err == nil) but never reports the
		// expected instance name must still fail the check: an accepted
		// call is not the same as a positively identified instance.
		return "accepted\n", "", nil
	}}

	err := uatenvmatrix.Run(context.Background(), uatenvmatrix.Inputs{OutDir: outDir, StepsDir: stepsDir, Executor: fe, ImageTag: "docker.io/mcavage/pix:test-candidate"})
	if err == nil {
		t.Fatal("expected Run to fail when create never reports a positively identified instance")
	}
	if calls := fe.snapshot(); len(calls) != 1 {
		t.Fatalf("expected exactly 1 executor call, got %d: %#v", len(calls), calls)
	}
}

func TestRun_EnvironmentCreateThenExecInvocation_ExecFailure(t *testing.T) {
	outDir := writeFakeCandidateBinaries(t)
	runDir := t.TempDir()
	stepsDir := filepath.Join(runDir, "steps")
	if err := os.MkdirAll(stepsDir, 0700); err != nil {
		t.Fatal(err)
	}
	fe := &fakeExecutor{onCall: func(call recordedCall) (string, string, error) {
		if len(call.args) > 0 && call.args[0] == "env" {
			return "created pix-uatenv-fixture-0 (positively identified)\n", "", nil
		}
		return "", "connection refused\n", context.Canceled
	}}

	err := uatenvmatrix.Run(context.Background(), uatenvmatrix.Inputs{OutDir: outDir, StepsDir: stepsDir, Executor: fe, ImageTag: "docker.io/mcavage/pix:test-candidate"})
	if err == nil {
		t.Fatal("expected Run to fail when the name-based exec errors")
	}
	logPath := filepath.Join(stepsDir, "env_environment_create_then_exec_invocation.log")
	b, rerr := os.ReadFile(logPath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(b), "RESULT: FAIL") {
		t.Errorf("artifact does not record FAIL: %s", b)
	}
}

// TestRun_BoundedArtifact proves the per-check log artifact is capped rather
// than growing without bound when a check (or the command it drives) emits
// an oversized amount of output.
func TestRun_BoundedArtifact(t *testing.T) {
	outDir := writeFakeCandidateBinaries(t)
	runDir := t.TempDir()
	stepsDir := filepath.Join(runDir, "steps")
	if err := os.MkdirAll(stepsDir, 0700); err != nil {
		t.Fatal(err)
	}
	huge := strings.Repeat("x", 2*1024*1024)
	fe := &fakeExecutor{onCall: func(call recordedCall) (string, string, error) {
		if call.name == "docker" {
			return fakeCandidateDigest + "\n", "", nil
		}
		if len(call.args) > 0 && call.args[0] == "ls" {
			return successfulFixtureNames + "\n", "", nil
		}
		if len(call.args) > 1 && call.args[0] == "env" && call.args[1] == "rm" {
			return "removed\n", "", nil
		}
		if len(call.args) > 1 && call.args[0] == "env" && call.args[1] == "create" {
			fixturePath := call.args[len(call.args)-1]
			switch filepath.Base(fixturePath) {
			case "authored.sbxenv.yaml":
				return "pix-uatenv-fixture-0 (positively identified) image digest: " + fakeCandidateDigest + " " + huge, "", nil
			case "candidate-image.sbxenv.yaml":
				return "pix-uatenv-fixture-image (positively identified) image digest: " + fakeCandidateDigest + " " + huge, "", nil
			case "recreate-boundary.sbxenv.yaml":
				fixtureBytes, _ := os.ReadFile(fixturePath)
				if strings.Contains(string(fixtureBytes), "memory: 60g") {
					return "", "environment declaration changed since creation", errors.New("refused: drifted declaration")
				}
				return "pix-uatenv-fixture-recreate (positively identified) image digest: " + fakeCandidateDigest + " " + huge, "", nil
			case "failed-create-cleanup.sbxenv.yaml":
				return "", "sbx: internal error resolving scoped secrets", errors.New("create failed before receipt")
			case "ollama-capability.sbxenv.yaml":
				return "pix-uatenv-fixture-ollama (positively identified) image digest: " + fakeCandidateDigest + " " + huge, "", nil
			}
			return "", "", fmt.Errorf("TestRun_BoundedArtifact: unrecognized create fixture path %s", fixturePath)
		}
		return "", "", nil
	}}

	if err := uatenvmatrix.Run(context.Background(), uatenvmatrix.Inputs{OutDir: outDir, StepsDir: stepsDir, Executor: fe, ImageTag: "docker.io/mcavage/pix:test-candidate"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	logPath := filepath.Join(stepsDir, "env_environment_create_then_exec_invocation.log")
	fi, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	// A generous margin over the 1 MiB cap for the truncation marker and
	// framing lines this check writes around the captured stdout.
	const margin = 4096
	if fi.Size() > 1024*1024+margin {
		t.Errorf("artifact size %d exceeds the bounded cap plus margin", fi.Size())
	}
	b, _ := os.ReadFile(logPath)
	if !strings.Contains(string(b), "output truncated") {
		t.Errorf("artifact does not record truncation despite oversized input")
	}
}
