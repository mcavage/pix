// check_custom_agent_ollama_test.go proves environment_custom_agent_ollama
// (docs/design/environments.md section 4.1 / section 11, item 10) in
// isolation against an injected fake Executor and this check's own
// function — the same posture check_rm_scope_refusal_test.go and
// check_failed_create_cleanup_test.go use, so this unit's tests never need a
// real `sbx` binary or Run()'s full registry.
//
// This is a CAPABILITY observation, not a pass/fail gate: docs/design/
// environments.md section 4.1 is explicit that local-Ollama routing for a
// custom agent is undocumented upstream, so the check must distinguish three
// outcomes, and only one of them is a check failure:
//
//  1. supported — sbx accepted the transport; the check succeeds and says so.
//  2. recognized unsupported — sbx refused with one of this package's own
//     literal, explicitly-owned rejection markers; the check still succeeds
//     (an accurate "no" is not a check failure) and the artifact states
//     plainly that extensions/ollama-bridge.ts remains required.
//  3. anything else — an executor/infrastructure error that does not match
//     a recognized marker; the check FAILS, because a transient or unrelated
//     failure must never be mislabeled as "unsupported capability".
package uatenvmatrix

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCheckEnvironmentCustomAgentOllama_SupportedSucceedsAndRecordsCapability(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	executor := recordingExecutor{fn: func(args []string) (string, string, error) {
		if len(args) > 0 && args[0] == "ls" {
			return "created " + ollamaCapabilityFixtureName + " (positively identified)\n", "", nil
		}
		if len(args) > 1 && args[0] == "env" && args[1] == "rm" {
			return "removed\n", "", nil
		}
		if len(args) > 0 && args[0] == "env" {
			return "created " + ollamaCapabilityFixtureName + " (positively identified)\n", "", nil
		}
		// The exec probe itself: sbx accepts the transport cleanly.
		return "ollama transport ready\n", "", nil
	}}

	if err := checkEnvironmentCustomAgentOllama(context.Background(), &lw, executor, phaseDir); err != nil {
		t.Fatalf("expected success on the supported branch, got: %v", err)
	}

	logged := lw.String()
	if !strings.Contains(logged, "capability: supported") {
		t.Errorf("artifact does not record the structured capability field for the supported branch: %s", logged)
	}
	if strings.Contains(logged, "extensions/ollama-bridge.ts remains required") {
		t.Errorf("supported branch must not claim the bridge remains required: %s", logged)
	}
}

func TestCheckEnvironmentCustomAgentOllama_RecognizedUnsupportedSucceedsAndRecordsBridgeRequirement(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	executor := recordingExecutor{fn: func(args []string) (string, string, error) {
		if len(args) > 0 && args[0] == "ls" {
			return "created " + ollamaCapabilityFixtureName + " (positively identified)\n", "", nil
		}
		if len(args) > 1 && args[0] == "env" && args[1] == "rm" {
			return "removed\n", "", nil
		}
		if len(args) > 0 && args[0] == "env" {
			return "created " + ollamaCapabilityFixtureName + " (positively identified)\n", "", nil
		}
		return "", "sbx: unknown flag: --provider", errors.New("exit status 2")
	}}

	err := checkEnvironmentCustomAgentOllama(context.Background(), &lw, executor, phaseDir)
	if err != nil {
		t.Fatalf("a recognized unsupported response must not fail the check, got: %v", err)
	}

	logged := lw.String()
	if !strings.Contains(logged, "capability: unsupported") {
		t.Errorf("artifact does not record the structured capability field for the unsupported branch: %s", logged)
	}
	if !strings.Contains(logged, "extensions/ollama-bridge.ts remains required") {
		t.Errorf("artifact does not state that extensions/ollama-bridge.ts remains required: %s", logged)
	}
}

func TestCheckEnvironmentCustomAgentOllama_UnrelatedExecutorErrorFailsTheCheck(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	executor := recordingExecutor{fn: func(args []string) (string, string, error) {
		if len(args) > 0 && args[0] == "env" {
			return "created " + ollamaCapabilityFixtureName + " (positively identified)\n", "", nil
		}
		return "", "connection refused", errors.New("dial tcp: connection refused")
	}}

	err := checkEnvironmentCustomAgentOllama(context.Background(), &lw, executor, phaseDir)
	if err == nil {
		t.Fatal("expected an unrelated executor error to fail the check, got nil")
	}
	if strings.Contains(err.Error(), "unsupported") {
		t.Errorf("an unrelated infrastructure error must not be phrased as an unsupported-capability result: %v", err)
	}
	logged := lw.String()
	if strings.Contains(logged, "capability: unsupported") {
		t.Errorf("artifact must not record an unsupported capability for an unrecognized infrastructure error: %s", logged)
	}
}

func TestCheckEnvironmentCustomAgentOllama_CreateFailureFailsTheCheck(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	calls := 0
	executor := recordingExecutor{fn: func(args []string) (string, string, error) {
		calls++
		return "", "sbx: internal error", errors.New("create failed")
	}}

	err := checkEnvironmentCustomAgentOllama(context.Background(), &lw, executor, phaseDir)
	if err == nil {
		t.Fatal("expected a failed environment create to fail the check (it is infrastructure, not a capability signal)")
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 executor call when create fails, got %d", calls)
	}
}

func TestCheckEnvironmentCustomAgentOllama_CreateNeverIdentifiedFailsTheCheck(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	executor := recordingExecutor{fn: func(args []string) (string, string, error) {
		return "accepted\n", "", nil
	}}

	err := checkEnvironmentCustomAgentOllama(context.Background(), &lw, executor, phaseDir)
	if err == nil {
		t.Fatal("expected an error when create never reports the expected positively-identified instance")
	}
}

// TestRecognizedOllamaUnsupportedReason_KnownMarkersRecognized proves the
// literal, package-owned marker set this check treats as a legitimate
// "unsupported" signal — never a claim that a real sbx binary emits exactly
// this text (see the check's own doc comment on the E0.7 host-assumption
// review this pins).
func TestRecognizedOllamaUnsupportedReason_KnownMarkersRecognized(t *testing.T) {
	for _, out := range []string{
		"sbx: unknown flag: --provider",
		"error: unsupported for custom agent",
		"--provider is only supported for the built-in claude agent",
		"provider ollama is experimental and unavailable for agent: pix",
	} {
		if recognizedOllamaUnsupportedReason(out) == "" {
			t.Errorf("expected %q to be recognized as an unsupported-capability response", out)
		}
	}
}

// TestRecognizedOllamaUnsupportedReason_UnrelatedErrorsNotRecognized proves
// the marker set does not swallow ordinary infrastructure failures.
func TestRecognizedOllamaUnsupportedReason_UnrelatedErrorsNotRecognized(t *testing.T) {
	for _, out := range []string{
		"connection refused",
		"context deadline exceeded",
		"sbx: command not found",
		"panic: runtime error",
		// A genuinely different missing-executable failure (e.g. `pi` itself
		// missing from $PATH inside the sandbox) must never be swallowed by
		// the observed `--model`-forwarded-as-executable marker below: only
		// the exact attempted executable `--model` is recognized.
		"OCI runtime exec failed: executable file `pi` not found in $PATH: No such file or directory",
		"exec: \"pi\": executable file not found in $PATH",
	} {
		if recognizedOllamaUnsupportedReason(out) != "" {
			t.Errorf("expected %q to NOT be recognized as an unsupported-capability response", out)
		}
	}
}

// observedOllamaModelFlagForwardedAsExecutableStdout is the EXACT stdout
// byte-for-byte (CRLF included) captured from a real host UAT run
// (run-20260824-100925-6a672010) probing this check's assumed
// `sbx exec -it <name> --model gemma4 --provider ollama -- pi --kit
// /opt/pix/kit` transport: sbx forwarded the literal `--model` flag to the
// container runtime as the command to execute, rather than recognizing it
// as a pre-command transport flag. stderr was empty; the process exited 127.
const observedOllamaModelFlagForwardedAsExecutableStdout = "OCI runtime exec failed: executable file `--model` not found in $PATH: No such file or directory\r\n"

// TestRecognizedOllamaUnsupportedReason_ObservedModelFlagForwardedAsExecutableRecognized
// pins the exact observed host evidence above as a recognized unsupported-
// transport marker: this is host evidence that `sbx exec` does not accept
// this check's assumed pre-command `--model`/`--provider` transport for a
// custom agent, forwarding `--model` as the executable instead.
func TestRecognizedOllamaUnsupportedReason_ObservedModelFlagForwardedAsExecutableRecognized(t *testing.T) {
	reason := recognizedOllamaUnsupportedReason(observedOllamaModelFlagForwardedAsExecutableStdout)
	if reason == "" {
		t.Fatalf("expected the exact observed run-20260824-100925-6a672010 stdout to be recognized as an unsupported-capability response, got no match for %q", observedOllamaModelFlagForwardedAsExecutableStdout)
	}
	if !strings.Contains(reason, "--model") {
		t.Errorf("reason does not name --model as the flag sbx forwarded as the executable: %q", reason)
	}
}

// TestCheckEnvironmentCustomAgentOllama_ObservedModelFlagForwardedAsExecutableSucceedsAndRecordsBridgeRequirement
// replays the exact observed host command/response from run-20260824-100925-
// 6a672010 through the full check (create, exec probe, cleanup) and proves
// it lands on the non-failing "unsupported" branch, states the bridge
// requirement, and still cleans up its fixture.
func TestCheckEnvironmentCustomAgentOllama_ObservedModelFlagForwardedAsExecutableSucceedsAndRecordsBridgeRequirement(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	rmCalled := false
	executor := recordingExecutor{fn: func(args []string) (string, string, error) {
		if len(args) > 0 && args[0] == "ls" {
			return "created " + ollamaCapabilityFixtureName + " (positively identified)\n", "", nil
		}
		if len(args) > 1 && args[0] == "env" && args[1] == "rm" {
			rmCalled = true
			return "removed\n", "", nil
		}
		if len(args) > 0 && args[0] == "env" {
			return "created " + ollamaCapabilityFixtureName + " (positively identified)\n", "", nil
		}
		// The exec probe itself: the exact observed host response.
		return observedOllamaModelFlagForwardedAsExecutableStdout, "", errors.New("exit status 127")
	}}

	err := checkEnvironmentCustomAgentOllama(context.Background(), &lw, executor, phaseDir)
	if err != nil {
		t.Fatalf("the exact observed run-20260824-100925-6a672010 response must not fail the check, got: %v", err)
	}
	if !rmCalled {
		t.Error("expected the fixture to be cleaned up (sbx env rm) even on the recognized-unsupported branch")
	}

	logged := lw.String()
	if !strings.Contains(logged, "capability: unsupported") {
		t.Errorf("artifact does not record the structured capability field for the unsupported branch: %s", logged)
	}
	if !strings.Contains(logged, "extensions/ollama-bridge.ts remains required") {
		t.Errorf("artifact does not state that extensions/ollama-bridge.ts remains required: %s", logged)
	}
}
