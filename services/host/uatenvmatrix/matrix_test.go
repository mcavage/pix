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
// successfulCandidateImageFixtureName is the deterministic, run-unique
// name candidateImageFixtureName (check_local_image.go) derives from the
// fixed "docker.io/mcavage/pix:test-candidate" tag every test in this file
// passes as Inputs.ImageTag — pinned here as its own literal because
// matrix_test.go is package uatenvmatrix_test and cannot call the
// unexported derivation function directly.
const successfulCandidateImageFixtureName = "pix-uatenv-fixture-image-test-candidate"

const successfulFixtureNames = "pix-uatenv-fixture-0 " + successfulCandidateImageFixtureName + " pix-uatenv-fixture-recreate pix-uatenv-fixture-ollama"

// fakePrepareImageSection renders the REAL, line-separated "PREPARE IMAGE"
// section shape fresh UAT run run-20260824-092338-d4c384f5's own host
// receipt actually used: a boxed section-header line, then the section's
// own body lines each on their OWN indented line below it — never the
// single joined "PREPARE IMAGE \u2192 check <ref>" line a prior version of
// this package's parser invented. This is matrix_test.go's own copy of the
// package-internal create_receipt_test.go helper of the same name (this
// file lives in the external uatenvmatrix_test package and cannot see it).
func fakePrepareImageSection(ref string) string {
	return "\u2500\u2500 PREPARE IMAGE\n" +
		"   \u2192 check " + ref + "\n" +
		"   \u2713 image ready\n"
}

// fakeTemplateListOut models a real `sbx template ls`'s REPOSITORY/TAG
// table (docker images' own convention), listing exactly the run-unique
// candidate repo:tag environment_uses_local_candidate_image requires be
// present before it ever attempts create.
const fakeTemplateListOut = "REPOSITORY               TAG             IMAGE ID       CREATED         SIZE\ndocker.io/mcavage/pix    test-candidate  " + fakeCandidateDigest + "   2 minutes ago   1.2GB\n"

// successfulFixtureNamesJSON renders successfulFixtureNames as a minimal
// `sbx ls --json` body: a bare array of rows, each reporting "running", one
// per fixture name. environment_create_then_exec_invocation's own bounded
// poll (pollForRunningInstance) now parses this as JSON before proceeding to
// exec, so the fake `ls` response must be schema-usable JSON, not merely a
// substring cleanupCreatedFixture's own fresh probe can find its expected
// name inside of (which this JSON also still satisfies, since every name
// still appears verbatim).
// echoProbeFakeResponse answers environment_create_then_exec_invocation's
// actual transport probe (`sbx exec -i <name> -- sh -c <script> sh
// <intended...>`): it echoes back everything after the fixed 8-element
// `exec -i <name> -- sh -c <script> sh` prefix, one per line, exactly the
// way a real POSIX shell running echoProbeScript would. environment_custom_
// agent_ollama's own `exec` call (check_custom_agent_ollama.go) also lands
// here, but that check only ever inspects the returned ERROR (nil here),
// never this stdout, so echoing its differently-shaped argv back is inert.
func echoProbeFakeResponse(args []string) string {
	const prefixLen = 8
	if len(args) <= prefixLen {
		return ""
	}
	return strings.Join(args[prefixLen:], "\n") + "\n"
}

func successfulFixtureNamesJSON() string {
	names := strings.Fields(successfulFixtureNames)
	rows := make([]string, len(names))
	for i, n := range names {
		rows[i] = fmt.Sprintf(`{"name":%q,"status":"running"}`, n)
	}
	return "[" + strings.Join(rows, ",") + "]"
}

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
		if len(call.args) > 1 && call.args[0] == "template" && call.args[1] == "ls" {
			return fakeTemplateListOut, "", nil
		}
		if len(call.args) > 0 && call.args[0] == "exec" {
			return echoProbeFakeResponse(call.args), "", nil
		}
		if len(call.args) > 0 && call.args[0] == "ls" {
			return successfulFixtureNamesJSON() + "\n", "", nil
		}
		if len(call.args) > 1 && call.args[0] == "env" && call.args[1] == "rm" {
			return "removed\n", "", nil
		}
		if len(call.args) > 1 && call.args[0] == "env" && call.args[1] == "create" {
			fixturePath := call.args[len(call.args)-1]
			switch filepath.Base(fixturePath) {
			case "authored.sbxenv.yaml":
				return "created pix-uatenv-fixture-0 (positively identified) kit ./kit\n", "", nil
			case "candidate-image.sbxenv.yaml":
				// The verbatim, line-separated shape fresh UAT run
				// run-20260824-092338-d4c384f5 actually observed: a boxed
				// "── PREPARE IMAGE" section header, its own indented
				// "→ check <ref>" and "✓ image ready" body lines each on their
				// OWN line, and a positively identified instance line — never
				// a single joined "PREPARE IMAGE → check <ref>" line, a
				// fabricated "image digest: sha256:..." line, or a host-Docker
				// container reference.
				return fakePrepareImageSection("docker.io/mcavage/pix:test-candidate") + "created " + successfulCandidateImageFixtureName + " (positively identified)\n", "", nil
			case "recreate-boundary.sbxenv.yaml":
				fixtureBytes, _ := os.ReadFile(fixturePath)
				if strings.Contains(string(fixtureBytes), "memory: 60g") {
					return "", "environment declaration changed since creation", errors.New("refused: drifted declaration")
				}
				return "created pix-uatenv-fixture-recreate (positively identified)\n", "", nil
			case "failed-create-cleanup.sbxenv.yaml":
				return "", "sbx: internal error resolving scoped secrets", errors.New("create failed before receipt")
			case "ollama-capability.sbxenv.yaml":
				return "created pix-uatenv-fixture-ollama (positively identified)\n", "", nil
			}
			return "", "", fmt.Errorf("successfulExecutor: unrecognized create fixture path %s", fixturePath)
		}
		return "", "", nil
	}
	return fe
}

// fakeCandidateDigest is the local candidate image ID matrix_test.go's
// shared fake executors answer `docker image inspect` with — evidence-only
// logging inside environment_uses_local_candidate_image now (there is no
// created-sandbox digest field to compare it against; see check_local_image.go).
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
	// this check's own create+poll+probe+cleanup-probe+remove calls
	// (calls[0..4] — one extra call versus its siblings, the bounded
	// pre-exec poll for a positively identified running row),
	// environment_uses_local_candidate_image's docker-image-inspect+template-
	// list+create+poll+cleanup-probe+remove calls (calls[5..10] — the
	// corrected AC-2 evidence chain: `sbx template ls` proves the run-unique
	// candidate repo:tag is registered before create, and the bounded poll for
	// a positively identified running row replaces the prior version's
	// invented `docker inspect <sandbox name>` probe), environment_
	// recreate_boundary's baseline+drifted create calls plus its one cleanup
	// probe+remove pair keyed on the baseline's own receipt (calls[11..14]),
	// environment_failed_create_cleanup's single failed create call with no
	// cleanup calls at all — no receipt, no removal authority (calls[15]),
	// environment_rm_scope_refusal's zero calls (it never touches the
	// injected Executor), and environment_custom_agent_ollama's create+probe
	// pair plus its own cleanup probe+remove pair (calls[16..19]). This test
	// asserts only on the first check's own create+poll+exec calls.
	calls := fe.snapshot()
	if len(calls) != 20 {
		t.Fatalf("expected exactly 20 executor calls, got %d: %#v", len(calls), calls)
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

	pollCall := calls[1]
	if pollCall.name != "sbx" || len(pollCall.args) != 2 || pollCall.args[0] != "ls" || pollCall.args[1] != "--json" {
		t.Fatalf("poll call = %#v, want `sbx ls --json`, issued BEFORE any exec", pollCall)
	}

	execCall := calls[2]
	const echoProbeScript = `for a in "$@"; do printf '%s\n' "$a"; done`
	wantExecArgs := []string{
		"exec", "-i", "pix-uatenv-fixture-0", "--", "sh", "-c", echoProbeScript, "sh",
		"pi",
		"--skill", "/opt/pix/kit/skills",
		"--skill", "/home/uat/personal-context/skills",
		"--model", "anthropic/claude-sonnet-5",
		"--session", "session-fixture-1",
	}
	if execCall.name != "sbx" || len(execCall.args) != len(wantExecArgs) {
		t.Fatalf("exec call = %#v, want name %q with %d args (%#v)", execCall, "sbx", len(wantExecArgs), wantExecArgs)
	}
	for i, want := range wantExecArgs {
		if execCall.args[i] != want {
			t.Errorf("exec call args[%d] = %q, want %q (full: %#v)", i, execCall.args[i], want, execCall.args)
		}
	}
	joined := strings.Join(execCall.args, " ")
	if strings.Contains(joined, "--kit") {
		t.Errorf("actual exec/probe call must never pass pi --kit: %#v", execCall.args)
	}
	if strings.Contains(joined, "--resume") {
		t.Errorf("actual exec/probe call must never use --resume: %#v", execCall.args)
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
		if len(call.args) > 0 && call.args[0] == "ls" {
			// The bounded pre-exec poll must succeed fast so this test
			// exercises the actual probe's own failure, not a poll timeout.
			return `[{"name":"pix-uatenv-fixture-0","status":"running"}]` + "\n", "", nil
		}
		if len(call.args) > 0 && call.args[0] == "env" {
			return "created pix-uatenv-fixture-0 (positively identified) kit ./kit\n", "", nil
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
		if len(call.args) > 1 && call.args[0] == "template" && call.args[1] == "ls" {
			return fakeTemplateListOut, "", nil
		}
		if len(call.args) > 0 && call.args[0] == "exec" {
			return echoProbeFakeResponse(call.args), "", nil
		}
		if len(call.args) > 0 && call.args[0] == "ls" {
			return successfulFixtureNamesJSON() + "\n", "", nil
		}
		if len(call.args) > 1 && call.args[0] == "env" && call.args[1] == "rm" {
			return "removed\n", "", nil
		}
		if len(call.args) > 1 && call.args[0] == "env" && call.args[1] == "create" {
			fixturePath := call.args[len(call.args)-1]
			switch filepath.Base(fixturePath) {
			case "authored.sbxenv.yaml":
				return "pix-uatenv-fixture-0 (positively identified) kit ./kit " + huge, "", nil
			case "candidate-image.sbxenv.yaml":
				return fakePrepareImageSection("docker.io/mcavage/pix:test-candidate") + successfulCandidateImageFixtureName + " (positively identified) " + huge, "", nil
			case "recreate-boundary.sbxenv.yaml":
				fixtureBytes, _ := os.ReadFile(fixturePath)
				if strings.Contains(string(fixtureBytes), "memory: 60g") {
					return "", "environment declaration changed since creation", errors.New("refused: drifted declaration")
				}
				return "pix-uatenv-fixture-recreate (positively identified) " + huge, "", nil
			case "failed-create-cleanup.sbxenv.yaml":
				return "", "sbx: internal error resolving scoped secrets", errors.New("create failed before receipt")
			case "ollama-capability.sbxenv.yaml":
				return "pix-uatenv-fixture-ollama (positively identified) " + huge, "", nil
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
