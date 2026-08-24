package uatenvmatrix

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// uatenvFixturePrefix is the ONLY namespace cleanupCreatedFixture may ever
// act on. Every fixture this package authors (fixtures.go, check_local_image.go,
// check_failed_create_cleanup.go) uses a literal pix-uatenv-* name — a
// namespace dedicated to this matrix's own throwaway UAT sandboxes, never
// shared with a real pix-* sandbox a user or another Pix workflow created.
// cleanupCreatedFixture refuses outright, before touching the injected
// Executor at all, the moment a caller hands it a name outside this
// namespace: a bug that widened scope to bare pix-* (or further) is exactly
// the class of mistake this guard exists to make impossible to reach a real
// `sbx` binary.
const uatenvFixturePrefix = "pix-uatenv-"

// cleanupCreatedFixture is the ONE shared receipt-gated teardown every check
// in this package that creates a real fixture sandbox must call — on both
// its success path and its downstream-failure path (a caller wires this via
// `defer` so cleanup always runs, never only on the happy path) — so a
// create call this package makes never strands a pix-uatenv-* sandbox on the
// host. It mirrors docs/design/environments.md section 9.3's production
// policy for `sbx env rm -f` verbatim, narrowed here to this matrix's own
// fixtures:
//
//   - No positive create receipt (createErr != nil, or createOut never
//     positively identified expectedName): no removal authority. Fails
//     closed, reports possible residue in lw, and never calls a real
//     command.
//   - A positive receipt, but a FRESH `sbx ls --json` probe — issued here,
//     after create, never reused from the create call itself — does not
//     still report expectedName, or the probe itself errors: fails closed,
//     reports residue, and never removes. An identity this package cannot
//     re-confirm right now is exactly the identity it must never touch —
//     another process could already have reused or removed it.
//   - A positive receipt AND a fresh probe confirming the same identity:
//     removes via the environment-scoped `sbx env rm -f <fixturePath>` —
//     never a bare `sbx rm`, never `--prune-bindings` (docs/design/
//     environments.md section 10.3).
//
// Every step is written to lw, so cleanup evidence always lives in the
// calling check's own bounded artifact, regardless of outcome.
// cleanupCreatedFixture returns a non-nil error only when it could not
// positively confirm-and-remove a receipted instance (an out-of-scope name,
// a fresh-probe mismatch/error, or the removal command itself failing) — a
// real residue condition worth surfacing — never merely because no receipt
// existed in the first place, which is the normal, already-reported outcome
// of a create that failed for its own reason.
//
// expectedName MUST be scoped to uatenvFixturePrefix; this is the first
// thing checked, so a caller bug can never reach a real (or even fake, in a
// test) removal call for an out-of-scope name.
func cleanupCreatedFixture(ctx context.Context, lw io.Writer, executor Executor, env []string, dir, fixturePath, expectedName, createOut string, createErr error) error {
	fmt.Fprintf(lw, "cleanup: evaluating %s\n", expectedName)

	if !strings.HasPrefix(expectedName, uatenvFixturePrefix) {
		err := fmt.Errorf("cleanup: refusing out-of-scope name %q (must start with %q)", expectedName, uatenvFixturePrefix)
		fmt.Fprintf(lw, "cleanup: %v\n", err)
		return err
	}

	if createErr != nil || !strings.Contains(createOut, expectedName) {
		fmt.Fprintf(lw, "cleanup: no positive create receipt for %s; residue possible, no removal attempted (docs/design/environments.md section 9.3)\n", expectedName)
		return nil
	}
	fmt.Fprintf(lw, "cleanup: positive create receipt observed for %s\n", expectedName)

	probeArgs := []string{"ls", "--json"}
	fmt.Fprintf(lw, "cleanup: $ sbx %s\n", strings.Join(probeArgs, " "))
	probeOut, probeErrOut, probeErr := executor.Run(ctx, "sbx", probeArgs, env, dir)
	fmt.Fprintf(lw, "cleanup: stdout: %s\ncleanup: stderr: %s\ncleanup: err: %v\n", probeOut, probeErrOut, probeErr)
	if probeErr != nil || !strings.Contains(probeOut, expectedName) {
		fmt.Fprintf(lw, "cleanup: fresh probe did not reconfirm %s; residue possible, no removal attempted\n", expectedName)
		return fmt.Errorf("cleanup: fresh probe failed to reconfirm receipted instance %s (probe err=%v)", expectedName, probeErr)
	}
	fmt.Fprintf(lw, "cleanup: fresh probe reconfirmed %s\n", expectedName)

	removeArgs := []string{"env", "rm", "-f", fixturePath}
	fmt.Fprintf(lw, "cleanup: $ sbx %s\n", strings.Join(removeArgs, " "))
	removeOut, removeErrOut, removeErr := executor.Run(ctx, "sbx", removeArgs, env, dir)
	fmt.Fprintf(lw, "cleanup: stdout: %s\ncleanup: stderr: %s\ncleanup: err: %v\n", removeOut, removeErrOut, removeErr)
	if removeErr != nil {
		fmt.Fprintf(lw, "cleanup: removal command failed for %s; residue possible\n", expectedName)
		return fmt.Errorf("cleanup: sbx env rm -f %s: %w", fixturePath, removeErr)
	}

	fmt.Fprintf(lw, "cleanup: removed %s\n", expectedName)
	return nil
}
