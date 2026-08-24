package uat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// envMatrixMaxListedChecks bounds how many check names a candidate's
// `--list-checks` report may claim before this seam refuses it outright: a
// runaway or hostile candidate binary can never turn "list checks" into an
// unbounded read or an unbounded evidence write.
const envMatrixMaxListedChecks = 64

// runEnvMatrixStep executes candidate_smoke's env-matrix step through the
// Runner's wired envMatrix seam, failing closed when a Runner was built some
// way other than NewRunner. Extracted from executeCandidateSmoke so that
// guard is directly testable, the same fail-closed contract
// memoryMatrix's inline nil check gives the memory matrix.
func (r *Runner) runEnvMatrixStep(ctx context.Context, res RunResources, stepsDir string) error {
	if r.envMatrix == nil {
		return errors.New("candidate_smoke: no env matrix wired (Runner must be built by NewRunner)")
	}
	return r.envMatrix(ctx, res, stepsDir)
}

// runCandidateEnvMatrix is candidate_smoke's real, production env-matrix
// seam: NewRunner wires it as the default envMatrix unless exec supplies the
// RunCandidateEnvMatrix override used by orchestration tests. It executes the
// SUBMITTED candidate's own `pix-host uat-env-matrix` binary — built into
// res.OutDir earlier in this same run's build step — through the worker's
// own already-authenticated process context. That is the whole fix: the
// long-lived session worker must never execute its own statically linked
// pre-candidate uatenvmatrix code again (docs/design/self-development-uat.md;
// host run run-20260823-201941-8f7b648b proved it still did after commit
// 74f74103). uatenvmatrix.CheckNames() stays a legitimate, purely static
// import here (Runner.capabilities reports it); Run is never called from this
// package directly — TestCandidateSmokeNeverCallsLinkedEnvMatrix pins that.
//
// argv and cwd are fixed and worker-composed from res alone: cwd is
// res.OutDir, and no candidate-supplied flag or environment variable ever
// reaches this invocation. The child's env is deliberately left untouched —
// no SetEnv call — so it inherits the worker's full authenticated
// environment; uatenvmatrix's own hostToolExecEnv is what strips PIX_*
// before the candidate shells out to sbx/docker, exactly like every other
// named check in that package. Context cancellation reaches the child
// through the same Exec abstraction every other candidate_smoke command
// uses: execAdapter wraps exec.CommandContext, which kills the process on
// ctx.Done(), so an aborted run tears this child down the same way it tears
// down the candidate pix sandbox launch.
func runCandidateEnvMatrix(ctx context.Context, execer Exec, res RunResources, stepsDir string) error {
	candidateBin := filepath.Join(res.OutDir, "pix-host")
	imageTag := "docker.io/mcavage/pix:" + res.ImageTag

	logPath := filepath.Join(stepsDir, "env_matrix.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("create env matrix log: %w", err)
	}
	defer logFile.Close()
	logWriter := &cappedLogWriter{file: logFile, remaining: candidateLogMaxBytes}

	// List the candidate's own checks as evidence BEFORE executing anything,
	// bounded so a runaway candidate can never turn this into an unbounded
	// read.
	listArgs := []string{"uat-env-matrix", "--list-checks"}
	listOut, err := runEnvMatrixChild(ctx, execer, candidateBin, listArgs, res.OutDir, logWriter)
	if err != nil {
		return fmt.Errorf("candidate env matrix --list-checks failed: %w (log: steps/env_matrix.log)", err)
	}
	names := strings.Fields(listOut)
	if len(names) == 0 {
		return fmt.Errorf("candidate env matrix --list-checks reported no candidate checks (log: steps/env_matrix.log)")
	}
	if len(names) > envMatrixMaxListedChecks {
		return fmt.Errorf("candidate env matrix --list-checks reported an implausible check count (%d > %d) (log: steps/env_matrix.log)", len(names), envMatrixMaxListedChecks)
	}
	fmt.Fprintf(logWriter, "candidate checks (%d): %s\n", len(names), strings.Join(names, ", "))

	runArgs := []string{
		"uat-env-matrix",
		"--out-dir", res.OutDir,
		"--steps-dir", stepsDir,
		"--image-tag", imageTag,
	}
	if _, err := runEnvMatrixChild(ctx, execer, candidateBin, runArgs, res.OutDir, logWriter); err != nil {
		return fmt.Errorf("candidate env matrix failed: %w (log: steps/env_matrix.log)", err)
	}
	fmt.Fprintf(logWriter, "RESULT: PASS\n")
	return nil
}

// runEnvMatrixChild execs one candidate uat-env-matrix invocation, capturing
// its stdout and stderr into logWriter (bounded by cappedLogWriter) and
// returning stdout separately so a caller (the --list-checks step) can parse
// it. cwd is fixed to dir; env is never set on the returned command, so the
// child inherits whatever environment execer's underlying CommandContext
// itself runs with — the worker's authenticated environment in production.
func runEnvMatrixChild(ctx context.Context, execer Exec, bin string, args []string, dir string, logWriter io.Writer) (stdout string, err error) {
	fmt.Fprintf(logWriter, "$ %s %s\n", bin, strings.Join(args, " "))

	cmd := execer.CommandContext(ctx, bin, args...)
	cmd.SetDir(dir)
	// Deliberately no cmd.SetEnv call: see runCandidateEnvMatrix's doc comment.

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("capture candidate env-matrix stdout: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("capture candidate env-matrix stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start candidate env matrix: %w", err)
	}

	var stdoutBuf bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(io.MultiWriter(logWriter, &stdoutBuf), stdoutPipe)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(logWriter, stderrPipe)
	}()
	waitErr := cmd.Wait()
	wg.Wait()

	if waitErr != nil {
		fmt.Fprintf(logWriter, "error: %v\n", waitErr)
		return stdoutBuf.String(), waitErr
	}
	return stdoutBuf.String(), nil
}
