package uatenvmatrix

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// ollamaCapabilityModel is the literal `--model` value this check probes
// with, taken verbatim from docs/design/environments.md section 4.1's own
// worked example (`sbx run --model gemma4 --provider ollama claude`) rather
// than invented here — the closest documented shape this package has to
// build a custom-agent analogue from.
const ollamaCapabilityModel = "gemma4"

// buildOllamaExecArgv composes this check's own ASSUMED shape for a
// name-based `sbx exec` carrying the local-Ollama transport flags section
// 4.1 documents only for the built-in Claude Code agent
// (`sbx run --model gemma4 --provider ollama claude`): flags placed between
// the target name and `--`, in the same `--model` then `--provider` order
// section 4.1's own example uses.
//
// Section 4.1 is explicit that this transport is "absent from the
// .sbxenv.yaml schema, and not documented for custom agents" — so BOTH the
// flag placement here and the recognized-refusal text
// recognizedOllamaUnsupportedReason matches are Pix's own literal
// assumption, not an observed contract. They are pinned here for
// docs/upstream/sbx-0.39-environments.md and Story 1's E0.7
// host-assumption review to independently verify against a real sbx binary,
// exactly like checkEnvironmentRecreateBoundary's create-never-attaches
// assumption.
func buildOllamaExecArgv(f EnvironmentFixture) []string {
	return []string{
		"exec", "-it", f.Name,
		"--model", ollamaCapabilityModel,
		"--provider", "ollama",
		"--", "pi", "--kit", f.Kit,
	}
}

// ollamaUnsupportedMarker is one literal substring this package treats as a
// legitimate "sbx explicitly refused this transport" signal, paired with the
// human-readable reason recorded in the check's bounded artifact. This is
// Pix's own explicit encoding of a PLAUSIBLE upstream refusal for an
// admittedly undocumented feature (docs/design/environments.md section
// 4.1) — never a claim that a real sbx binary emits this exact text. A
// mismatch here is precisely the kind of finding docs/upstream/
// sbx-0.39-environments.md and Story 1's E0.7 host-assumption review exist
// to catch and correct against a real sbx binary.
type ollamaUnsupportedMarker struct {
	substr string
	reason string
}

// ollamaUnsupportedMarkers is the full literal set
// recognizedOllamaUnsupportedReason checks, matched case-insensitively.
var ollamaUnsupportedMarkers = []ollamaUnsupportedMarker{
	{"unknown flag", "sbx rejected --model/--provider as unrecognized flags for a custom-agent `sbx exec`"},
	{"unsupported for custom agent", "sbx explicitly reports the Ollama provider transport as unsupported for a custom agent"},
	{"only supported for the built-in", "sbx explicitly scopes --provider ollama to its own built-in agent, excluding custom agents"},
	{"experimental and unavailable", "sbx reports the Ollama provider transport as experimental and unavailable for this invocation"},
}

// ollamaModelFlagForwardedAsExecutablePattern recognizes the OBSERVED
// concrete OCI runtime-exec failure this package saw in a real host UAT run
// (run-20260824-100925-6a672010): `sbx exec` forwarded the literal
// `--model` flag to the container runtime as if it were the sandbox's
// command to execute, rather than recognizing it as this check's own
// assumed pre-command transport flag
// (`sbx exec -it <name> --model gemma4 --provider ollama -- pi --kit ...`).
//
// The pattern intentionally requires the failing executable to be
// identified as exactly `--model` (in backticks, single, or double quotes)
// rather than matching any "executable ... not found" text: a genuinely
// different missing-executable failure (a typo'd binary, a missing `pi`, a
// broken PATH) must still fail the check as infrastructure breakage, never
// be silently absorbed here.
var ollamaModelFlagForwardedAsExecutablePattern = regexp.MustCompile(
	`(?i)executable file\s*[` + "`" + `'"]?--model[` + "`" + `'"]?\s*not found in \$path`,
)

// ollamaModelFlagForwardedAsExecutableReason is the human-readable reason
// recorded when ollamaModelFlagForwardedAsExecutablePattern matches: it
// names the flag sbx forwarded and states plainly this was NOT a real
// executor/infrastructure failure.
const ollamaModelFlagForwardedAsExecutableReason = "sbx forwarded --model as the sandbox exec command rather than recognizing the custom-agent model/provider transport (OCI runtime exec failed: executable file `--model` not found)"

// recognizedOllamaUnsupportedReason reports the human-readable reason this
// package recognizes combinedOutput as a legitimate "unsupported" response,
// or "" if combinedOutput does not match any known marker — in which case
// the caller must treat the failure as infrastructure/executor breakage,
// never as a silently-assumed unsupported capability.
func recognizedOllamaUnsupportedReason(combinedOutput string) string {
	if ollamaModelFlagForwardedAsExecutablePattern.MatchString(combinedOutput) {
		return ollamaModelFlagForwardedAsExecutableReason
	}
	lower := strings.ToLower(combinedOutput)
	for _, m := range ollamaUnsupportedMarkers {
		if strings.Contains(lower, m.substr) {
			return m.reason
		}
	}
	return ""
}

// checkEnvironmentCustomAgentOllama is Story 0's sixth named check
// (docs/design/environments.md section 4.1 / section 11, item 10): attempt
// the real custom Pix agent local-Ollama transport through the injected
// Executor and report a structured, non-failing CAPABILITY result —
// "supported" or "unsupported" — rather than a fake pass. It is not a
// pass/fail gate on sbx's behavior: section 4.1 is explicit that this
// transport is undocumented for custom agents, so BOTH outcomes are
// legitimate evidence.
//
// A recognized unsupported response (recognizedOllamaUnsupportedReason)
// succeeds the check and states plainly that extensions/ollama-bridge.ts
// remains required (section 4.1: "Delete the bridge only after host UAT
// proves that sbx exposes a stable local model transport to the Pix custom
// agent"). Any OTHER executor error — one that does not match a recognized
// marker — fails the check outright: an unrelated infrastructure failure
// (a crashed sbx, a network error, a timeout) must never be mislabeled as
// "unsupported capability", or a real regression would silently read as a
// benign capability gap.
//
// Every host command goes through the injected Executor, exactly like the
// other checks in this package: no real `sbx` binary is required under `go
// test`.
func checkEnvironmentCustomAgentOllama(ctx context.Context, lw io.Writer, executor Executor, phaseDir string) (retErr error) {
	fixture := ollamaCapabilityFixture()

	fixturePath, err := writeAuthoredFixture(phaseDir, "ollama-capability.sbxenv.yaml", fixture)
	if err != nil {
		return err
	}
	fmt.Fprintf(lw, "authored fixture written to %s\n", fixturePath)

	env := hostToolExecEnv()

	createArgs := []string{"env", "create", fixturePath}
	fmt.Fprintf(lw, "$ sbx %s\n", strings.Join(createArgs, " "))
	createOut, createErrOut, err := executor.Run(ctx, "sbx", createArgs, env, phaseDir)
	fmt.Fprintf(lw, "stdout: %s\nstderr: %s\nerr: %v\n", createOut, createErrOut, err)
	defer func() {
		if cleanupErr := cleanupCreatedFixture(ctx, lw, executor, env, phaseDir, fixturePath, fixture.Name, createOut, err); cleanupErr != nil && retErr == nil {
			retErr = cleanupErr
		}
	}()
	if err != nil {
		return fmt.Errorf("sbx env create (infrastructure, not a capability signal): %w", err)
	}
	if !strings.Contains(createOut, fixture.Name) {
		return fmt.Errorf("sbx env create did not report the expected positively-identified instance name %q (stdout=%q); infrastructure failure, not a capability signal", fixture.Name, createOut)
	}

	execArgs := buildOllamaExecArgv(fixture)
	fmt.Fprintf(lw, "$ sbx %s\n", strings.Join(execArgs, " "))
	execOut, execErrOut, execErr := executor.Run(ctx, "sbx", execArgs, env, phaseDir)
	fmt.Fprintf(lw, "stdout: %s\nstderr: %s\nerr: %v\n", execOut, execErrOut, execErr)

	if execErr == nil {
		fmt.Fprintf(lw, "capability: supported\n")
		fmt.Fprintf(lw, "custom Pix agent local-Ollama transport (--model %s --provider ollama) accepted by name-based sbx exec\n", ollamaCapabilityModel)
		return nil
	}

	combined := execOut + "\n" + execErrOut
	if reason := recognizedOllamaUnsupportedReason(combined); reason != "" {
		fmt.Fprintf(lw, "capability: unsupported\n")
		fmt.Fprintf(lw, "reason: %s\n", reason)
		fmt.Fprintf(lw, "extensions/ollama-bridge.ts remains required: docs/design/environments.md section 4.1 keeps the bridge until host UAT proves sbx exposes a stable local model transport to the Pix custom agent; this run did not observe one\n")
		return nil
	}

	return fmt.Errorf("sbx exec (ollama transport probe) failed with a response this package does not recognize as a legitimate capability refusal, so it is treated as infrastructure breakage: %w (stdout=%q stderr=%q)", execErr, execOut, execErrOut)
}
