package uatenvmatrix

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// buildExecArgv composes the exact `sbx exec` argv a positively-identified,
// name-based re-attach must receive for f: `exec -it <name> -- pi <kit>
// <skills> <model> <resume>`. It is intentionally independent of both
// sandbox.ExecArgv (an L1 sibling this package must not import) and
// workflow/launch's real argv builder: this package proves the upstream
// contract from first principles, so an accidental agreement with the real
// builder is evidence, not a foregone conclusion.
func buildExecArgv(f EnvironmentFixture) []string {
	piArgs := []string{"--kit", f.Kit}
	for _, skill := range f.LiveSkills {
		piArgs = append(piArgs, "--skill", skill)
	}
	if f.Model != "" {
		piArgs = append(piArgs, "--model", f.Model)
	}
	if f.Resume != "" {
		piArgs = append(piArgs, "--resume", f.Resume)
	}
	args := []string{"exec", "-it", f.Name, "--", "pi"}
	return append(args, piArgs...)
}

// checkEnvironmentCreateThenExecInvocation is the first Story 0 named check:
// create a native environment fixture with the candidate Pix custom agent
// (`agent: pix`), then prove name-based `sbx exec` receives the exact
// invocation the fixture's typed facts demand — never a command sbx derived
// itself from the environment path.
//
// Every host command goes through the injected Executor, so this check needs
// no real `sbx` binary to run under `go test`: production wires the real
// execExecutor, tests inject a fake that records and answers deterministically.
func checkEnvironmentCreateThenExecInvocation(ctx context.Context, lw io.Writer, executor Executor, phaseDir string) (retErr error) {
	fixture := customAgentFixture()

	fixturePath := filepath.Join(phaseDir, "authored.sbxenv.yaml")
	if err := os.WriteFile(fixturePath, fixture.YAML, 0600); err != nil {
		return fmt.Errorf("write authored fixture: %w", err)
	}
	fmt.Fprintf(lw, "authored fixture written to %s\n", fixturePath)

	env := hostToolExecEnv()

	createArgs := []string{"env", "create", fixturePath}
	fmt.Fprintf(lw, "$ sbx %s\n", strings.Join(createArgs, " "))
	// createErr is deliberately its own named variable, never reused for the
	// exec call below: cleanupCreatedFixture is gated on the CREATE's own
	// receipt, and the deferred closure captures createOut/createErr by
	// reference, so reusing the same variable name for a later call (as this
	// package once did) would have the exec call's outcome silently
	// overwrite the create's by the time the deferred cleanup actually runs.
	createOut, createErrOut, createErr := executor.Run(ctx, "sbx", createArgs, env, phaseDir)
	fmt.Fprintf(lw, "stdout: %s\nstderr: %s\nerr: %v\n", createOut, createErrOut, createErr)
	defer func() {
		if cleanupErr := cleanupCreatedFixture(ctx, lw, executor, env, phaseDir, fixturePath, fixture.Name, createOut, createErr); cleanupErr != nil && retErr == nil {
			retErr = cleanupErr
		}
	}()
	if createErr != nil {
		return fmt.Errorf("sbx env create: %w", createErr)
	}
	if !strings.Contains(createOut, fixture.Name) {
		return fmt.Errorf("sbx env create did not report the expected positively-identified instance name %q (stdout=%q)", fixture.Name, createOut)
	}

	execArgs := buildExecArgv(fixture)
	fmt.Fprintf(lw, "$ sbx %s\n", strings.Join(execArgs, " "))
	execOut, execErrOut, execErr := executor.Run(ctx, "sbx", execArgs, env, phaseDir)
	fmt.Fprintf(lw, "stdout: %s\nstderr: %s\nerr: %v\n", execOut, execErrOut, execErr)
	if execErr != nil {
		return fmt.Errorf("name-based sbx exec: %w", execErr)
	}

	return nil
}
