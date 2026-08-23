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

// isolatedExecEnv builds the run-local env every check hands to Executor.Run:
// PATH/HOME/TMPDIR/LANG pass through (needed to exec `sbx` at all), and every
// XDG root plus PIX_CONFIG point inside phaseDir. It NEVER sets MEMORY_PORT,
// MEMORY_BIND, or any other memory-daemon variable — this matrix has no
// business anywhere near the memory port, unlike uatmatrix, which is the
// one place that legitimately does.
func isolatedExecEnv(phaseDir string) []string {
	var base []string
	for _, e := range os.Environ() {
		for _, allow := range []string{"PATH=", "TMPDIR=", "TMP=", "TEMP=", "LANG=", "LC_ALL="} {
			if strings.HasPrefix(e, allow) {
				base = append(base, e)
				break
			}
		}
	}
	homeDir := phaseDir + "/home"
	cfgDir := phaseDir + "/config"
	dataDir := phaseDir + "/data"
	stateDir := phaseDir + "/state"
	cacheDir := phaseDir + "/cache"
	for _, d := range []string{homeDir, cfgDir, dataDir, stateDir, cacheDir} {
		_ = os.MkdirAll(d, 0700)
	}
	base = setEnv(base, "HOME", homeDir)
	base = setEnv(base, "XDG_CONFIG_HOME", cfgDir)
	base = setEnv(base, "XDG_DATA_HOME", dataDir)
	base = setEnv(base, "XDG_STATE_HOME", stateDir)
	base = setEnv(base, "XDG_CACHE_HOME", cacheDir)
	base = setEnv(base, "PIX_CONFIG", cfgDir+"/config.toml")
	return base
}

// setEnv overrides key in env, filtering any existing entry first — the same
// caution uatmatrix's setEnv documents: os/exec does not define which of two
// duplicate-keyed entries a child observes.
func setEnv(env []string, key, val string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			continue
		}
		out = append(out, e)
	}
	return append(out, prefix+val)
}
