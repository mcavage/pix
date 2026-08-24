package uatenvmatrix

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// recreateCommand renders the exact Pix recreate instruction this check's
// artifact names once drift refusal is observed: `pix rm <name> && pix run
// --env <env>`, the recreate path docs/design/environments.md section 8's
// example error text and section 10.2's recreate-only policy both name.
func recreateCommand(name, envName string) string {
	return fmt.Sprintf("pix rm %s && pix run --env %s", name, envName)
}

// checkEnvironmentRecreateBoundary is Story 0's third named check
// (docs/design/environments.md section 11, item 4 / section 10.2): prove
// that reusing the SAME declared environment identity across a mutated
// effective facet is never silently accepted at the native layer.
//
// This is upstream-contract evidence, not production fingerprint logic:
// Pix's own recreate-only drift refusal (section 10.2) does not exist yet
// (Story 2 wires it). What this check CAN observe today, with only the
// injected Executor and its own literal fixtures, is the native `sbx env
// create` contract itself: per docs/design/environments.md section 5,
// "`sbx env create [PATH...]` creates without attaching" (unlike `sbx env
// run`, which "creates if needed and attaches"). Create never attaching is
// exactly the property this check pins as an explicit ASSUMPTION for
// docs/upstream/sbx-0.39-environments.md and Story 1's E0.7 host-assumption
// review to independently verify against a real sbx binary: a second `sbx
// env create` issued at the SAME declared environment identity, after that
// identity's effective declaration has drifted, must be REFUSED by sbx
// itself (a non-nil error) rather than silently accepted as a reuse/attach
// of the first instance. A second create that succeeds is exactly the
// silent-reuse hazard Pix P0's own recreate-only policy exists to close, and
// this check fails loudly the moment it is observed rather than assuming it
// away.
//
// Every host command goes through the injected Executor, exactly like the
// two checks above: no real `sbx` binary is required under `go test`, and
// no user-authored `--name` is ever passed — the reported instance name
// comes from sbx's own positive identification, the same contract
// checkEnvironmentCreateThenExecInvocation relies on.
func checkEnvironmentRecreateBoundary(ctx context.Context, lw io.Writer, executor Executor, phaseDir string) (retErr error) {
	env := hostToolExecEnv()
	fixturePath := filepath.Join(phaseDir, "recreate-boundary.sbxenv.yaml")

	if err := os.WriteFile(fixturePath, recreateBoundaryFixtureYAML(), 0600); err != nil {
		return fmt.Errorf("write baseline recreate-boundary fixture: %w", err)
	}
	fmt.Fprintf(lw, "baseline fixture written to %s\n", fixturePath)

	baselineArgs := []string{"env", "create", fixturePath}
	fmt.Fprintf(lw, "$ sbx %s\n", strings.Join(baselineArgs, " "))
	baselineOut, baselineErrOut, err := executor.Run(ctx, "sbx", baselineArgs, env, phaseDir)
	fmt.Fprintf(lw, "stdout: %s\nstderr: %s\nerr: %v\n", baselineOut, baselineErrOut, err)
	// cleanupCreatedFixture is deferred here, immediately after the baseline
	// create, and keyed on the BASELINE's own receipt: the baseline is the
	// one call that ever creates a real instance under this declared
	// identity (docs/design/environments.md section 10.2's create-never-
	// attaches contract means the drifted call below must never succeed in
	// creating a second one), so it is the baseline's receipt — not the
	// drifted call's outcome — that gates removal, regardless of which
	// branch this check's own assertion below takes.
	defer func() {
		if cleanupErr := cleanupCreatedFixture(ctx, lw, executor, env, phaseDir, fixturePath, recreateBoundaryFixtureName, baselineOut, err); cleanupErr != nil && retErr == nil {
			retErr = cleanupErr
		}
	}()
	if err != nil {
		return fmt.Errorf("baseline sbx env create: %w", err)
	}
	if !strings.Contains(baselineOut, recreateBoundaryFixtureName) {
		return fmt.Errorf("baseline sbx env create did not report the expected positively-identified instance name %q (stdout=%q)", recreateBoundaryFixtureName, baselineOut)
	}

	if err := os.WriteFile(fixturePath, recreateBoundaryMutatedFixtureYAML(), 0600); err != nil {
		return fmt.Errorf("write mutated recreate-boundary fixture: %w", err)
	}
	fmt.Fprintf(lw, "mutated facet %s; drifted fixture written to %s\n", recreateBoundaryMutatedFacet, fixturePath)

	driftedArgs := []string{"env", "create", fixturePath}
	fmt.Fprintf(lw, "$ sbx %s\n", strings.Join(driftedArgs, " "))
	driftedOut, driftedErrOut, driftErr := executor.Run(ctx, "sbx", driftedArgs, env, phaseDir)
	fmt.Fprintf(lw, "stdout: %s\nstderr: %s\nerr: %v\n", driftedOut, driftedErrOut, driftErr)

	recreate := recreateCommand(recreateBoundaryFixtureName, recreateBoundaryEnvName)
	if driftErr == nil {
		return fmt.Errorf("sbx env create silently accepted the mutated facet %s at an existing environment identity (stdout=%q); reuse/attach after an effective declaration change must be refused, not silently accepted \u2014 recreate explicitly: %s", recreateBoundaryMutatedFacet, driftedOut, recreate)
	}

	fmt.Fprintf(lw, "mutated facet %s refused reuse as expected; recreate explicitly: %s\n", recreateBoundaryMutatedFacet, recreate)
	return nil
}
