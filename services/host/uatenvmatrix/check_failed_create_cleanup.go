package uatenvmatrix

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// failedCreateCleanupFixtureName is the literal pix-* sandbox name Story 0
// authors for environment_failed_create_cleanup's declaration — owned
// directly here, exactly like every other fixture name in this package
// (fixtures.go's package doc).
const failedCreateCleanupFixtureName = "pix-uatenv-fixture-failed-create"

// failedCreateCleanupFixtureYAML renders the one declaration this check
// creates, a package-owned literal against the upstream schema, never
// derived from envinfo (matrix.go's package doc).
func failedCreateCleanupFixtureYAML() []byte {
	return []byte(`schemaVersion: "1"
agent: pix

sandboxOptions:
  memory: 4g
`)
}

// isRemovalCommand reports whether args is a removal invocation this
// package must never issue against a create that failed before a positive
// receipt: `sbx env rm ...` or bare `sbx rm ...` (docs/design/environments.md
// section 9.3 and section 10.3).
func isRemovalCommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if args[0] == "rm" {
		return true
	}
	if len(args) >= 2 && args[0] == "env" && args[1] == "rm" {
		return true
	}
	return false
}

// noCleanupExecutor wraps the injected Executor and physically refuses to
// forward a removal command to it. docs/design/environments.md section 9.3
// is explicit: "Pix calls `sbx env rm -f` after failure only when it first
// obtained a positive create receipt and a fresh probe still reports that
// exact instance id... If create failed before a receipt, Pix fails closed
// and reports possible residue instead of risking another sandbox" — another
// creator can race the same identity, so a create failure with no receipt is
// never removal authority.
//
// checkEnvironmentFailedCreateCleanup only ever exercises the before-receipt
// branch (it has no "receipt observed -> now clean up" code path at all), so
// in normal operation this wrapper never intercepts anything. It exists as
// this check's own belt-and-braces enforcement of the policy: a future edit
// that mistakenly adds a cleanup call here can never reach a real `sbx`
// binary, and is reported as a check failure rather than silently executed.
type noCleanupExecutor struct {
	inner            Executor
	attemptedRemoval bool
}

func (g *noCleanupExecutor) Run(ctx context.Context, name string, args, env []string, dir string) (string, string, error) {
	if name == "sbx" && isRemovalCommand(args) {
		g.attemptedRemoval = true
		return "", "", fmt.Errorf("policy violation: environment_failed_create_cleanup refused to forward a removal command (sbx %s) with no positive create receipt", strings.Join(args, " "))
	}
	return g.inner.Run(ctx, name, args, env, dir)
}

// checkEnvironmentFailedCreateCleanup is Story 0's fourth named check
// (docs/design/environments.md section 9.3 / section 11, item 5): prove that
// a native create failing BEFORE Pix ever observes a positive receipt (a
// positively-identified instance id in the create output) results in ZERO
// removal calls, and that the artifact instead names the possible
// scoped-secret/binding/MCP residue and states plainly that no cleanup was
// attempted — fail closed, never guess.
//
// This check is deliberately narrow: it only proves the before-receipt
// branch. The positive-receipt / successful-create-then-fresh-probe branch
// (where Pix DOES have removal authority) is a different scenario this unit
// does not cover — a create that positively identifies its fixture's
// instance is treated as a check failure here, not silently accepted as
// "also fine".
//
// Every host command goes through the injected Executor, wrapped in
// noCleanupExecutor so no removal command this check's own code could ever
// mistakenly issue reaches it: no real `sbx` binary is required under `go
// test`, and none is ever asked to remove anything.
func checkEnvironmentFailedCreateCleanup(ctx context.Context, lw io.Writer, executor Executor, phaseDir string) error {
	guarded := &noCleanupExecutor{inner: executor}

	fixturePath := filepath.Join(phaseDir, "failed-create-cleanup.sbxenv.yaml")
	if err := os.WriteFile(fixturePath, failedCreateCleanupFixtureYAML(), 0600); err != nil {
		return fmt.Errorf("write failed-create-cleanup fixture: %w", err)
	}
	fmt.Fprintf(lw, "fixture written to %s\n", fixturePath)

	env := isolatedExecEnv(phaseDir)
	createArgs := []string{"env", "create", fixturePath}
	fmt.Fprintf(lw, "$ sbx %s\n", strings.Join(createArgs, " "))
	createOut, createErrOut, err := guarded.Run(ctx, "sbx", createArgs, env, phaseDir)
	fmt.Fprintf(lw, "stdout: %s\nstderr: %s\nerr: %v\n", createOut, createErrOut, err)

	receipted := err == nil && strings.Contains(createOut, failedCreateCleanupFixtureName)
	if receipted {
		return fmt.Errorf("sbx env create positively identified %s (stdout=%q); environment_failed_create_cleanup only exercises the before-receipt path (docs/design/environments.md section 9.3) \u2014 a positive receipt is a different scenario, not covered by this check", failedCreateCleanupFixtureName, createOut)
	}

	fmt.Fprintf(lw, "create failed before a positive receipt for %s: no instance id was ever positively identified\n", failedCreateCleanupFixtureName)
	fmt.Fprintf(lw, "policy (docs/design/environments.md section 9.3): without a positive create receipt AND a fresh probe confirming that exact instance id, Pix has no removal authority \u2014 another creator could race the same identity\n")
	fmt.Fprintf(lw, "possible residue: %s may have left scoped secrets, bindings, and/or MCP registrations that sbx resolves before creating the sandbox; Pix cannot safely distinguish real residue from another creator's in-flight identity\n", failedCreateCleanupFixtureName)
	fmt.Fprintf(lw, "no cleanup was attempted: neither `sbx env rm` nor `sbx rm` was called\n")

	if guarded.attemptedRemoval {
		return fmt.Errorf("policy violation: environment_failed_create_cleanup attempted a removal call with no positive create receipt")
	}

	return nil
}
