package uatenvmatrix

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// Executor runs one host command and reports exactly what it received. Every
// check in this package talks to `sbx` (and anything else it needs to shell
// out to) ONLY through this seam, so a unit test can inject a fake that
// records the exact argv/env/dir without ever invoking a real `sbx` binary —
// the acceptance requirement this whole package exists to satisfy: named
// environment checks must be provable without real sbx in CI.
type Executor interface {
	Run(ctx context.Context, name string, args, env []string, dir string) (stdout, stderr string, err error)
}

// execExecutor is the real, production implementation: a plain os/exec
// subprocess. It is the only thing in this package that ever touches a real
// `sbx` binary.
type execExecutor struct{}

func (execExecutor) Run(ctx context.Context, name string, args, env []string, dir string) (stdout, stderr string, err error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	cmd.Dir = dir
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	runErr := cmd.Run()
	return outBuf.String(), errBuf.String(), runErr
}

// cappedLogWriter is uatmatrix's cappedLogWriter, duplicated rather than
// imported (see matrix.go's package doc for why an L1 sibling never imports
// another). Every check gets exactly one bounded artifact, capped at
// envMatrixLogMaxBytes.
type cappedLogWriter struct {
	mu        sync.Mutex
	file      *os.File
	remaining int
	truncated bool
}

func (w *cappedLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	originalLen := len(p)
	if w.remaining <= 0 {
		if !w.truncated {
			_, _ = w.file.WriteString("\n[output truncated at 1 MiB]\n")
			w.truncated = true
		}
		return originalLen, nil
	}
	toWrite := p
	if len(toWrite) > w.remaining {
		toWrite = toWrite[:w.remaining]
	}
	written, err := w.file.Write(toWrite)
	w.remaining -= written
	if err != nil {
		return written, err
	}
	if written < len(toWrite) {
		return written, io.ErrShortWrite
	}
	return originalLen, nil
}

// runEnvCheck writes ONE bounded artifact per check (steps/env_<name>.log,
// capped at envMatrixLogMaxBytes exactly like memory_<name>.log), so a
// failure in the env matrix is diagnosable from the run's steps directory
// alone, the same way the memory matrix's artifacts are.
func runEnvCheck(stepsDir, name string, fn func(io.Writer) error) error {
	logPath := stepsDir + "/env_" + name + ".log"
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("create env check log %s: %w", name, err)
	}
	lw := &cappedLogWriter{file: f, remaining: envMatrixLogMaxBytes}
	fmt.Fprintf(lw, "=== env check: %s ===\n", name)
	checkErr := fn(lw)
	if checkErr != nil {
		fmt.Fprintf(lw, "RESULT: FAIL: %v\n", checkErr)
	} else {
		fmt.Fprintf(lw, "RESULT: PASS\n")
	}
	_ = f.Close()
	if checkErr != nil {
		return fmt.Errorf("%s: %w (log: steps/env_%s.log)", name, checkErr, name)
	}
	return nil
}

// hostToolExecEnv builds the env every check hands to Executor.Run. Every
// check in this package shells out ONLY to real host tools — `sbx` and
// `docker` — and NEVER to pix or pix-host, so unlike uatmatrix (which runs
// the candidate pix binary and must rehome its Pix roots to avoid touching
// the operator's real config), this matrix has nothing of Pix's to protect
// on the sbx/Docker side: sbx and Docker Desktop discover their runtime
// socket and login/auth/session state beneath whatever host channels the
// daemon process itself was started with (HOME, XDG roots, or something
// this package's author never anticipated), so the daemon's FULL
// environment passes through unchanged.
//
// This was previously `isolatedExecEnv`, which curated an allowlist (PATH,
// HOME, TMPDIR, LANG, DOCKER_HOST, DOCKER_CONFIG) and rehomed every XDG root
// plus PIX_CONFIG under phaseDir, believing that protected Pix config from
// this package's checks. It did not: nothing in this package ever invokes
// pix/pix-host, so nothing here ever reads a Pix-rooted XDG path in the
// first place — the rehoming only broke sbx's own config/session discovery,
// which lives under those same real XDG roots. Preserving HOME/DOCKER_HOST/
// DOCKER_CONFIG alone (run run-20260823-155824-4d96352e) was not enough: the
// escalated failure (run run-20260823-160820-41a5b981) persisted with those
// three fixed, because the allowlist was still dropping whatever other host
// channel sbx's own auth actually depends on. Passing the full environment
// through is the smallest correction that cannot keep dropping channels this
// package's author has not enumerated.
//
// The one thing still stripped is Pix's own runtime variables (the PIX_
// prefix: PIX_CONFIG, PIX_UAT_SMOKE, ...). sbx/docker never consult one, and
// the daemon process (pix-host serve) may itself be running with one set, so
// dropping the prefix is what actually keeps the promise that a check can
// never leak normal Pix config into a host-tool subprocess. It never sets
// MEMORY_PORT, MEMORY_BIND, or any other memory-daemon variable either —
// this matrix has no business anywhere near the memory port, unlike
// uatmatrix, which is the one place that legitimately does — but if the
// daemon's own environment happens to carry one, it passes through inert:
// sbx/docker do not read it.
func hostToolExecEnv() []string {
	environ := os.Environ()
	out := make([]string, 0, len(environ))
	for _, e := range environ {
		if strings.HasPrefix(e, "PIX_") {
			continue
		}
		out = append(out, e)
	}
	return out
}
