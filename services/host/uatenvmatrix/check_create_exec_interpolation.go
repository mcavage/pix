package uatenvmatrix

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// check_create_exec_interpolation.go closes the final E0.7 host-evidence gap
// (AC-7, docs/design/environments.md section 5: "Story 0 records exact
// undefined-variable behavior"; section 6.2: "Story 0 proves relative
// paths, `${VAR}` and `${VAR:-default}` interpolation, custom-agent
// selection, and candidate-image behavior before this adapter lands").
// checkEnvironmentCreateThenExecInvocation's existing create/exec proof
// (check_create_exec.go) says nothing about host-variable interpolation at
// all: none of its fixtures reference `${VAR}` anywhere, so the corrected
// docs this UAT run produces could never actually claim the exact observed
// `${VAR}`, `${VAR:-default}`, and undefined-variable behavior AC-7
// requires. This file adds a BOUNDED interpolation observation phase
// INSIDE that same first named check — never a seventh check name — run
// after the primary create/exec proof succeeds:
//
//  1. observeDefinedDefaultInterpolation: a defined `${VAR}` reference and a
//     missing-with-default `${VAR:-default}` reference in ONE fixture. Both
//     facets are PINNED, deterministic outcomes (docs/design/
//     environments.md section 5.1's own composition contract): create must
//     succeed, and the exec probe must report the exact known defined value
//     and the exact literal default. Any other outcome is a real check
//     failure, not ambiguous evidence.
//  2. observeUndefinedVariableBehavior: a bare `${VAR}` reference with NO
//     default, where sbx's own behavior is genuinely unknown upstream (the
//     same admittedly-undocumented posture check_custom_agent_ollama.go
//     already uses for the local-Ollama transport). BOTH a loader/create
//     refusal and a create success are legitimate observation evidence;
//     only an unrelated/inconclusive outcome (an executor error this
//     package cannot attribute to a real refusal, or exec probe output this
//     package cannot classify into one of its three known shapes) fails the
//     check.
//
// Both phases route every host command through the injected Executor and
// reuse this check's OWN existing bounded primitives — pollForRunningInstance
// and echoProbeTimeout (requirement: comfortably under smoke's 5m scenario
// timeout, reusing the existing poll and bounded exec timeout rather than
// inventing new ones) — and both are receipt-gated through the SAME shared
// cleanupCreatedFixture teardown every other fixture-creating check in this
// package uses.

// interpolationDefinedDefaultProbeScript is the terminating, non-TTY POSIX
// shell probe observeDefinedDefaultInterpolation hands to a real `sbx exec`:
// it prints both interpolated sandbox-side values in one unambiguous,
// labeled line per value, so the check can assert the EXACT known defined
// value and the EXACT literal default, never merely "the command exited 0".
const interpolationDefinedDefaultProbeScript = `printf '` + interpolationDefinedEnvKey + `=%s\n` + interpolationDefaultEnvKey + `=%s\n' "$` + interpolationDefinedEnvKey + `" "$` + interpolationDefaultEnvKey + `"`

// expectedInterpolationDefinedDefaultProbeOutput is the exact stdout
// interpolationDefinedDefaultProbeScript must produce once host
// interpolation resolved both facets correctly.
func expectedInterpolationDefinedDefaultProbeOutput() string {
	return fmt.Sprintf("%s=%s\n%s=%s\n", interpolationDefinedEnvKey, interpolationDefinedHostValue, interpolationDefaultEnvKey, interpolationDefaultFallbackValue)
}

// undefinedVariableLiteralUnexpanded is the exact literal text a sandbox-side
// value would carry if sbx passed the `${VAR}` reference through completely
// unresolved rather than interpolating it — one of the three shapes
// classifyUndefinedVariableProbeOutput recognizes as legitimate evidence.
const undefinedVariableLiteralUnexpanded = "${" + interpolationMissingHostVar + "}"

// undefinedVariableProbeUnsetToken / undefinedVariableProbeEmptyToken /
// undefinedVariableProbeValuePrefix are the exact labeled tokens
// interpolationMissingProbeScript prints for each of the three states a
// POSIX shell can distinguish for one variable: entirely unset (`${VAR+x}`
// unset), set to the empty string, or set to some non-empty value (printed
// verbatim after the prefix, so a literal/unexpanded `${VAR}` reference is
// still observable).
const (
	undefinedVariableProbeUnsetToken  = interpolationMissingEnvKey + ":UNSET"
	undefinedVariableProbeEmptyToken  = interpolationMissingEnvKey + ":EMPTY"
	undefinedVariableProbeValuePrefix = interpolationMissingEnvKey + ":VALUE="
)

// interpolationMissingProbeScript is the terminating, non-TTY POSIX shell
// probe observeUndefinedVariableBehavior hands to a real `sbx exec` on its
// success branch: it distinguishes unset vs set-empty vs a literal/
// unexpanded value using the POSIX `${VAR+x}` existence test (never merely
// `-z "$VAR"`, which cannot tell "unset" from "set empty" on its own), and
// prints exactly one labeled line naming which state it observed.
const interpolationMissingProbeScript = `if [ -z "${` + interpolationMissingEnvKey + `+x}" ]; then
  printf '` + undefinedVariableProbeUnsetToken + `\n'
elif [ -z "$` + interpolationMissingEnvKey + `" ]; then
  printf '` + undefinedVariableProbeEmptyToken + `\n'
else
  printf '` + undefinedVariableProbeValuePrefix + `%s\n' "$` + interpolationMissingEnvKey + `"
fi`

// classifyUndefinedVariableProbeOutput reports the normalized, human-readable
// undefined-variable behavior interpolationMissingProbeScript's output
// describes, or a non-nil error if out matches none of the three recognized
// shapes — an unrelated/inconclusive outcome this package refuses to guess
// at (this file's own doc comment: "only an unrelated/inconclusive outcome
// fails the check").
func classifyUndefinedVariableProbeOutput(out string) (string, error) {
	trimmed := strings.TrimSpace(out)
	switch {
	case trimmed == undefinedVariableProbeUnsetToken:
		return "unset (the undefined variable reference resolved to no sandbox-side environment variable at all)", nil
	case trimmed == undefinedVariableProbeEmptyToken:
		return "set-empty (the undefined variable reference resolved to a sandbox-side environment variable set to the empty string)", nil
	case strings.HasPrefix(trimmed, undefinedVariableProbeValuePrefix):
		value := strings.TrimPrefix(trimmed, undefinedVariableProbeValuePrefix)
		if value == undefinedVariableLiteralUnexpanded {
			return "literal/unexpanded (the `${VAR}` reference was passed through to the sandbox environment unresolved, as literal text)", nil
		}
		return "", fmt.Errorf("probe reported a non-empty value %q that is neither empty, unset, nor the literal unexpanded reference %q; unrelated/inconclusive outcome", value, undefinedVariableLiteralUnexpanded)
	default:
		return "", fmt.Errorf("probe output did not match any recognized classification shape: %q; unrelated/inconclusive outcome", trimmed)
	}
}

// undefinedVariableRefusalReasonMaxLen bounds the normalized refusal reason
// observeUndefinedVariableBehavior logs on its refusal branch — this
// package's own artifacts are already bounded by envMatrixLogMaxBytes, but
// the ONE LINE this check normalizes and reports must itself stay short
// enough to read at a glance, never a raw, arbitrarily long stack dump.
const undefinedVariableRefusalReasonMaxLen = 400

// boundedRefusalReason normalizes createOut/createErrOut/createErr into one
// short, whitespace-collapsed line describing WHY sbx refused, truncated to
// undefinedVariableRefusalReasonMaxLen. It never returns an empty string: a
// refusal this package cannot describe from any of the three inputs still
// reports the bare error text.
func boundedRefusalReason(createOut, createErrOut string, createErr error) string {
	reason := strings.TrimSpace(createOut + " " + createErrOut)
	reason = strings.Join(strings.Fields(reason), " ")
	if reason == "" && createErr != nil {
		reason = createErr.Error()
	}
	if reason == "" {
		reason = "sbx env create failed with no output"
	}
	if len(reason) > undefinedVariableRefusalReasonMaxLen {
		reason = reason[:undefinedVariableRefusalReasonMaxLen] + " ...(truncated)"
	}
	return reason
}

// envWithoutKey returns a COPY of env with every entry named key removed —
// never mutating env itself, and never touching process env: hostToolExecEnv
// already returns its own fresh copy of os.Environ(), and this function only
// ever builds a further new slice on top of it.
func envWithoutKey(env []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env))
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// envWithSetKey returns a COPY of env with every existing entry named key
// removed and exactly one `key=value` entry appended at the end —
// deterministic (the same env and key/value always yield byte-identical
// output) and, like envWithoutKey, never mutating its input or touching
// process env.
func envWithSetKey(env []string, key, value string) []string {
	out := envWithoutKey(env, key)
	return append(out, key+"="+value)
}

// observeDefinedDefaultInterpolation is AC-7's first pinned interpolation
// observation: create the defined/default fixture with
// interpolationDefinedHostVar explicitly set to a known value and
// interpolationMissingHostVar explicitly stripped, poll for a positively
// identified running row, then exec a terminating, non-TTY shell probe and
// assert its output carries the EXACT known defined value and the EXACT
// literal default — both are pinned, deterministic outcomes, so unlike
// observeUndefinedVariableBehavior below, any create/poll/exec failure here
// is a real check failure, never ambiguous evidence. Receipt-gated cleanup
// runs on every path via the deferred cleanupCreatedFixture call, exactly
// like every other fixture-creating check in this package.
func observeDefinedDefaultInterpolation(ctx context.Context, lw io.Writer, executor Executor, phaseDir string) (retErr error) {
	fixture := interpDefinedDefaultFixture()

	fixturePath, err := writeAuthoredFixture(phaseDir, "interp-defined-default.sbxenv.yaml", fixture)
	if err != nil {
		return err
	}
	fmt.Fprintf(lw, "interpolation (defined/default): authored fixture written to %s\n", fixturePath)

	env := envWithSetKey(envWithoutKey(hostToolExecEnv(), interpolationMissingHostVar), interpolationDefinedHostVar, interpolationDefinedHostValue)

	createArgs := []string{"env", "create", fixturePath}
	fmt.Fprintf(lw, "$ sbx %s\n", strings.Join(createArgs, " "))
	// createErr is deliberately its own never-reassigned identifier, exactly
	// like check_create_exec.go's own createErr: the deferred closure below
	// captures it by reference, so reusing it for a later call in this
	// function would let that later call's outcome silently overwrite the
	// create's own receipt by the time cleanup actually runs
	// (immutable_create_err_test.go statically guards this).
	createOut, createErrOut, createErr := executor.Run(ctx, "sbx", createArgs, env, phaseDir)
	fmt.Fprintf(lw, "stdout: %s\nstderr: %s\nerr: %v\n", createOut, createErrOut, createErr)
	defer func() {
		if cleanupErr := cleanupCreatedFixture(ctx, lw, executor, env, phaseDir, fixturePath, fixture.Name, createOut, createErr); cleanupErr != nil && retErr == nil {
			retErr = cleanupErr
		}
	}()
	if createErr != nil {
		return fmt.Errorf("interpolation (defined/default): sbx env create: %w", createErr)
	}
	if !strings.Contains(createOut, fixture.Name) {
		return fmt.Errorf("interpolation (defined/default): sbx env create did not report the expected positively-identified instance name %q (stdout=%q)", fixture.Name, createOut)
	}

	if err := pollForRunningInstance(ctx, lw, executor, env, phaseDir, fixture.Name, runningRowPollConfig); err != nil {
		return err
	}

	probeArgs := []string{"exec", "-i", fixture.Name, "--", "sh", "-c", interpolationDefinedDefaultProbeScript}
	fmt.Fprintf(lw, "$ sbx %s\n", strings.Join(probeArgs, " "))
	probeCtx, cancel := context.WithTimeout(ctx, echoProbeTimeout)
	defer cancel()
	probeOut, probeErrOut, probeErr := executor.Run(probeCtx, "sbx", probeArgs, env, phaseDir)
	fmt.Fprintf(lw, "stdout: %s\nstderr: %s\nerr: %v\n", probeOut, probeErrOut, probeErr)
	if probeErr != nil {
		if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("interpolation (defined/default): exec probe: timed out after %s: %w", echoProbeTimeout, probeErr)
		}
		return fmt.Errorf("interpolation (defined/default): exec probe: %w", probeErr)
	}
	want := expectedInterpolationDefinedDefaultProbeOutput()
	if probeOut != want {
		return fmt.Errorf("interpolation (defined/default): exec probe did not report the exact expected interpolated values: got %q, want %q", probeOut, want)
	}
	fmt.Fprintf(lw, "defined variable behavior: ${%s} resolved to the exact known value %q\n", interpolationDefinedHostVar, interpolationDefinedHostValue)
	fmt.Fprintf(lw, "missing-with-default behavior: ${%s:-%s} resolved to the exact literal default %q\n", interpolationMissingHostVar, interpolationDefaultFallbackValue, interpolationDefaultFallbackValue)
	return nil
}

// observeUndefinedVariableBehavior is AC-7's third required interpolation
// case: a bare `${VAR}` reference to interpolationMissingHostVar (explicitly
// stripped from the create call's own env) with no default at all. Section
// 6.2 says only that this behavior must be RECORDED, never what it must be
// — exactly the same admittedly-undocumented posture
// check_custom_agent_ollama.go already uses — so BOTH a loader/create
// refusal and a create success are legitimate observation evidence:
//
//   - refused before a positive receipt (createErr != nil): logs a
//     normalized "undefined variable behavior: refused: <bounded reason>"
//     line and issues NO removal at all — cleanupCreatedFixture's own
//     no-receipt branch already guarantees this (it never calls a real
//     command when createErr != nil), so this branch returns success
//     without ever attempting cleanup itself beyond the deferred call every
//     other fixture-creating check in this package already makes.
//   - create success: requires a positive instance-name receipt, polls for
//     a running row (reusing the SAME bounded poll every other check in
//     this package uses), execs the terminating classification probe
//     (reusing the SAME bounded echoProbeTimeout), classifies the result
//     into one of three legitimate shapes (classifyUndefinedVariableProbeOutput),
//     logs the normalized "undefined variable behavior: <classification>"
//     line, then runs the SAME receipt-gated cleanup every other
//     fixture-creating check in this package uses.
//
// Any OTHER outcome — a create that neither refuses nor positively
// identifies the fixture, a poll timeout, a probe that errors or times out,
// or probe output classifyUndefinedVariableProbeOutput cannot recognize —
// fails the check outright: an unrelated infrastructure failure must never
// be mislabeled as a legitimate "refused" or "resolved" observation.
func observeUndefinedVariableBehavior(ctx context.Context, lw io.Writer, executor Executor, phaseDir string) (retErr error) {
	fixture := interpMissingFixture()

	fixturePath, err := writeAuthoredFixture(phaseDir, "interp-missing.sbxenv.yaml", fixture)
	if err != nil {
		return err
	}
	fmt.Fprintf(lw, "interpolation (undefined variable): authored fixture written to %s\n", fixturePath)

	env := envWithoutKey(hostToolExecEnv(), interpolationMissingHostVar)

	createArgs := []string{"env", "create", fixturePath}
	fmt.Fprintf(lw, "$ sbx %s\n", strings.Join(createArgs, " "))
	// createErr, like observeDefinedDefaultInterpolation's own createErr, is
	// deliberately its own never-reassigned identifier —
	// immutable_create_err_test.go statically guards this.
	createOut, createErrOut, createErr := executor.Run(ctx, "sbx", createArgs, env, phaseDir)
	fmt.Fprintf(lw, "stdout: %s\nstderr: %s\nerr: %v\n", createOut, createErrOut, createErr)
	defer func() {
		if cleanupErr := cleanupCreatedFixture(ctx, lw, executor, env, phaseDir, fixturePath, fixture.Name, createOut, createErr); cleanupErr != nil && retErr == nil {
			retErr = cleanupErr
		}
	}()

	if createErr != nil {
		reason := boundedRefusalReason(createOut, createErrOut, createErr)
		fmt.Fprintf(lw, "undefined variable behavior: refused: %s\n", reason)
		return nil
	}
	if !strings.Contains(createOut, fixture.Name) {
		return fmt.Errorf("interpolation (undefined variable): sbx env create neither refused nor positively identified the expected instance name %q; unrelated/inconclusive outcome (stdout=%q)", fixture.Name, createOut)
	}

	if err := pollForRunningInstance(ctx, lw, executor, env, phaseDir, fixture.Name, runningRowPollConfig); err != nil {
		return err
	}

	probeArgs := []string{"exec", "-i", fixture.Name, "--", "sh", "-c", interpolationMissingProbeScript}
	fmt.Fprintf(lw, "$ sbx %s\n", strings.Join(probeArgs, " "))
	probeCtx, cancel := context.WithTimeout(ctx, echoProbeTimeout)
	defer cancel()
	probeOut, probeErrOut, probeErr := executor.Run(probeCtx, "sbx", probeArgs, env, phaseDir)
	fmt.Fprintf(lw, "stdout: %s\nstderr: %s\nerr: %v\n", probeOut, probeErrOut, probeErr)
	if probeErr != nil {
		if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("interpolation (undefined variable): exec probe: timed out after %s: %w", echoProbeTimeout, probeErr)
		}
		return fmt.Errorf("interpolation (undefined variable): exec probe: %w", probeErr)
	}

	classification, classifyErr := classifyUndefinedVariableProbeOutput(probeOut)
	if classifyErr != nil {
		return fmt.Errorf("interpolation (undefined variable): %w (stdout=%q stderr=%q)", classifyErr, probeOut, probeErrOut)
	}
	fmt.Fprintf(lw, "undefined variable behavior: %s\n", classification)
	return nil
}
