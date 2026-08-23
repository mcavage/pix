// Package uatenvmatrix is the host-backed native-sandbox-environment UAT
// coverage that Story 0 of docs/design/environments.md asked for: prove the
// upstream `sbx env create` / name-based `sbx exec` contract against literal,
// hand-authored fixtures before any Pix code depends on it.
//
// It deliberately MIRRORS uatmatrix (the memory matrix) rather than importing
// it: this package is a sibling L1 capability, and arch_test.go forbids one L1
// capability importing another. Small helpers (the bounded log writer, the
// run-local isolated-env builder) are duplicated on purpose, the same way
// uatmatrix duplicates memWatcherDailyBudgetUAT instead of importing the
// `main` package that owns the real constant — a future change to either
// twin's behavior makes its OWN check fail loudly instead of silently
// drifting.
//
// This package also never imports the future `envinfo` capability (Story 1's
// `.sbxenv.yaml` parser/renderer). Its fixtures are literal bytes it owns
// outright (fixtures.go): if envinfo's renderer later produces bytes that
// happen to agree with this package's fixtures, that agreement is evidence
// the renderer is right, not a tautology — which it would be if this package
// asked envinfo to build its own fixture. arch_test.go's
// TestArchitecture_UatenvmatrixNeverImportsEnvinfo is the explicit guard.
//
// executeCandidateSmoke calls Run through workflow/uat.Runner.envMatrix AFTER
// the existing memory matrix and before the sandbox launches. Every check
// here must be testable through an injected Executor — unit tests in this
// package never need a real `sbx` binary on PATH.
package uatenvmatrix

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// envMatrixLogMaxBytes caps every check's bounded artifact, mirroring
// uatmatrix's own cap (candidateLogMaxBytes / matrixLogMaxBytes): a runaway
// check can never grow the run's steps directory without bound.
const envMatrixLogMaxBytes = 1024 * 1024

// Inputs identifies the run-local paths and (optionally) the injected
// Executor this matrix uses. Executor is nil in production, which selects
// the real os/exec-backed implementation; tests inject a fake to prove
// success/failure without a real `sbx` binary.
type Inputs struct {
	OutDir   string
	StepsDir string
	Executor Executor
	// ImageTag is the exact candidate image reference this run built,
	// saved, and `sbx template load`ed (docker.io/mcavage/pix:<tag>).
	// environment_uses_local_candidate_image (E0.2) requires it to prove a
	// created environment's image digest matches the locally loaded
	// candidate rather than a registry pull; production always sets it,
	// the same caller-bug contract the candidate-binary check above
	// enforces for OutDir.
	ImageTag string
}

type envMatrixCheck struct {
	name string
	fn   func(ctx context.Context, lw io.Writer, executor Executor, phaseDir string) error
}

// checks builds the ordered named-check registry. imageTag is threaded only
// to the checks that need it (currently environment_uses_local_candidate_image);
// CheckNames calls this with an empty imageTag since it only reads names,
// never executes a check.
func checks(imageTag string) []envMatrixCheck {
	return []envMatrixCheck{
		{"environment_create_then_exec_invocation", checkEnvironmentCreateThenExecInvocation},
		{"environment_uses_local_candidate_image", func(ctx context.Context, lw io.Writer, executor Executor, phaseDir string) error {
			return checkEnvironmentUsesLocalCandidateImage(ctx, lw, executor, phaseDir, imageTag)
		}},
		{"environment_recreate_boundary", checkEnvironmentRecreateBoundary},
	}
}

// CheckNames returns the exact named environment checks candidate_smoke
// runs. Capability reporting consumes this list so advertised coverage
// cannot drift from execution — the same contract uatmatrix.CheckNames
// gives the memory matrix.
func CheckNames() []string {
	matrixChecks := checks("")
	names := make([]string, 0, len(matrixChecks))
	for _, check := range matrixChecks {
		names = append(names, check.name)
	}
	return names
}

// Run executes the named environment checks, in isolation, against the
// candidate binaries built for this run — after the memory matrix, before
// the sandbox launches. It fails closed if the candidate binaries a real
// candidate_smoke run always builds first are missing: a caller constructing
// Inputs any other way is a caller bug, not a supported way to skip this
// coverage.
func Run(ctx context.Context, in Inputs) error {
	pixBin := filepath.Join(in.OutDir, "pix")
	pixHostBin := filepath.Join(in.OutDir, "pix-host")
	for _, p := range []string{pixBin, pixHostBin} {
		fi, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("env matrix: candidate binary missing (%s): %w", p, err)
		}
		if fi.IsDir() {
			return fmt.Errorf("env matrix: candidate binary path is a directory: %s", p)
		}
	}

	if in.ImageTag == "" {
		return fmt.Errorf("env matrix: no candidate image tag supplied (caller bug: Inputs.ImageTag must always be set)")
	}

	executor := in.Executor
	if executor == nil {
		executor = execExecutor{}
	}

	matrixRoot := filepath.Join(filepath.Dir(in.StepsDir), "env-matrix")
	if err := os.MkdirAll(matrixRoot, 0700); err != nil {
		return fmt.Errorf("env matrix: create scratch root: %w", err)
	}

	for _, c := range checks(in.ImageTag) {
		phaseDir := filepath.Join(matrixRoot, c.name)
		if err := os.MkdirAll(phaseDir, 0700); err != nil {
			return fmt.Errorf("env matrix: create phase dir %s: %w", c.name, err)
		}
		fn := c.fn
		if err := runEnvCheck(in.StepsDir, c.name, func(lw io.Writer) error {
			return fn(ctx, lw, executor, phaseDir)
		}); err != nil {
			return err
		}
	}
	return nil
}
