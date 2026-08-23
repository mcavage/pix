package uatenvmatrix

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// rmScopeRefusalFixtureName is the literal `pix-*` sandbox name this check
// treats as the recorded instance for both refusal cases — owned directly
// here, exactly like every other fixture name in this package (fixtures.go's
// package doc).
const rmScopeRefusalFixtureName = "pix-uatenv-fixture-rm-scope"

// rmScopeRefusalNonPixEffectiveName is case 1's effective name: outside
// `pix-*` scope entirely, so docs/design/environments.md section 10.3's
// "refuses anything outside pix-*" clause must refuse it regardless of
// which instance is recorded.
const rmScopeRefusalNonPixEffectiveName = "not-pix-scoped-env"

// rmScopeRefusalMismatchedEffectiveName is case 2's effective name: itself
// `pix-*` scoped, but a different sandbox than rmScopeRefusalFixtureName, so
// section 10.3's "or unequal to the recorded [instance name]" clause must
// refuse it even though the scope check alone would pass.
const rmScopeRefusalMismatchedEffectiveName = "pix-uatenv-fixture-rm-scope-mismatch"

// planEnvRemoveRefusal is this package's own, literal, typed encoding of the
// removal-scope safety policy docs/design/environments.md section 10.3
// describes for the future production `sandbox.PlanEnvRemove` — which Story
// 0 does not build (Story 2 wires launch and removal through it). This is
// deliberately NOT a claim that any real `sbx` binary enforces Pix scope
// itself: section 10.3 is explicit that composing the effective path,
// recomputing the effective name, and refusing anything outside `pix-*` or
// unequal to the recorded instance name are PIX'S OWN proof obligations,
// applied before Pix ever appends `-f` inside its existing proof-gated
// removal seam. checkEnvironmentRmScopeRefusal exercises exactly these two
// typed safety-policy cases against this function; it never asks a real or
// fake `sbx` to enforce them.
//
// It returns a non-nil, case-naming error on refusal and nil once both
// proofs hold (effectiveName is `pix-*` scoped AND equals
// recordedInstanceName) — the recreate-only success case Story 2's launch
// path will require before ever appending `-f`.
func planEnvRemoveRefusal(effectiveName, recordedInstanceName string) error {
	if !strings.HasPrefix(effectiveName, "pix-") {
		return fmt.Errorf("refused: effective name %q is outside pix-* scope", effectiveName)
	}
	if effectiveName != recordedInstanceName {
		return fmt.Errorf("refused: effective name %q does not match recorded instance %q", effectiveName, recordedInstanceName)
	}
	return nil
}

// isRmScopeRefusalCommand reports whether args is a removal invocation this
// check must never issue, reusing the same shape isRemovalCommand already
// defines in check_failed_create_cleanup.go for `sbx env rm ...` and bare
// `sbx rm ...`.
//
// rmScopeRefusalGuardExecutor wraps the injected Executor and physically
// refuses to forward a removal command to it — this check's own
// belt-and-braces enforcement, exactly like check_failed_create_cleanup.go's
// noCleanupExecutor: checkEnvironmentRmScopeRefusal has no code path that
// legitimately calls Run at all (it proves a pure safety-policy contract
// via planEnvRemoveRefusal, never a native invocation), so in normal
// operation this wrapper never intercepts anything. It exists so a future
// edit that mistakenly wires a removal call into this check can never reach
// a real `sbx` binary, and is reported as a check failure rather than
// silently executed.
type rmScopeRefusalGuardExecutor struct {
	inner            Executor
	attemptedRemoval bool
}

func (g *rmScopeRefusalGuardExecutor) Run(ctx context.Context, name string, args, env []string, dir string) (string, string, error) {
	if name == "sbx" && isRemovalCommand(args) {
		g.attemptedRemoval = true
		return "", "", fmt.Errorf("policy violation: environment_rm_scope_refusal refused to forward a removal command (sbx %s)", strings.Join(args, " "))
	}
	return g.inner.Run(ctx, name, args, env, dir)
}

// checkEnvironmentRmScopeRefusal is Story 0's fifth named check
// (docs/design/environments.md section 11, item 7 / section 10.3): prove
// that the Pix removal contract refuses BOTH a non-`pix-*` effective name
// and an effective-name-vs-recorded-instance mismatch, with zero real
// removal ever attempted for either case.
//
// Story 0 has no production `sandbox.PlanEnvRemove` yet, so this check
// exercises its own typed safety-policy function, planEnvRemoveRefusal,
// against two literal fixtures this file owns outright — never a claim that
// upstream `sbx` itself enforces Pix scope. The injected Executor is wrapped
// in rmScopeRefusalGuardExecutor purely as this check's own enforcement
// seam: no legitimate code path here ever calls Run, and any command that
// did would have to be a removal command reaching a real `sbx` binary,
// which the guard refuses before it can happen.
func checkEnvironmentRmScopeRefusal(ctx context.Context, lw io.Writer, executor Executor, phaseDir string) error {
	guarded := &rmScopeRefusalGuardExecutor{inner: executor}
	_ = guarded // referenced only as the enforcement seam; see doc comment.

	fmt.Fprintf(lw, "policy (docs/design/environments.md section 10.3): sandbox.PlanEnvRemove recomputes the effective name and refuses anything outside pix-* or unequal to the recorded instance name before ever appending -f inside the proof-gated removal seam\n")
	fmt.Fprintf(lw, "recorded instance: %s\n", rmScopeRefusalFixtureName)

	fmt.Fprintf(lw, "\ncase: scope\n")
	fmt.Fprintf(lw, "effective name: %s (not pix-* scoped)\n", rmScopeRefusalNonPixEffectiveName)
	if err := planEnvRemoveRefusal(rmScopeRefusalNonPixEffectiveName, rmScopeRefusalFixtureName); err == nil {
		return fmt.Errorf("policy violation: environment_rm_scope_refusal did not refuse a non-pix-* effective name %q", rmScopeRefusalNonPixEffectiveName)
	} else {
		fmt.Fprintf(lw, "refused: %v\n", err)
	}

	fmt.Fprintf(lw, "\ncase: mismatch\n")
	fmt.Fprintf(lw, "effective name: %s (pix-* scoped, but not the recorded instance)\n", rmScopeRefusalMismatchedEffectiveName)
	if err := planEnvRemoveRefusal(rmScopeRefusalMismatchedEffectiveName, rmScopeRefusalFixtureName); err == nil {
		return fmt.Errorf("policy violation: environment_rm_scope_refusal did not refuse a mismatched effective name %q against recorded instance %q", rmScopeRefusalMismatchedEffectiveName, rmScopeRefusalFixtureName)
	} else {
		fmt.Fprintf(lw, "refused: %v\n", err)
	}

	fmt.Fprintf(lw, "\nno removal argv was ever issued for either case: no environment-scoped or bare removal command was called\n")

	if guarded.attemptedRemoval {
		return fmt.Errorf("policy violation: environment_rm_scope_refusal attempted a removal call despite both cases being refused")
	}

	return nil
}
