package uat

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// envMatrixFakeCmd is a minimal ExecCmd double with REAL stdout/stderr pipes
// (unlike runner_test.go's mockCmdHelper, which returns nil, nil and would
// panic under runEnvMatrixChild's io.Copy): these tests need to prove what
// actually got written to the bounded log, and whether SetEnv was ever
// called at all.
type envMatrixFakeCmd struct {
	exec    *envMatrixFakeExec
	name    string
	args    []string
	dir     string
	stdout  string
	stderr  string
	waitErr error
}

func (c *envMatrixFakeCmd) Run() error               { return c.waitErr }
func (c *envMatrixFakeCmd) Start() error              { return nil }
func (c *envMatrixFakeCmd) Wait() error               { return c.waitErr }
func (c *envMatrixFakeCmd) Output() ([]byte, error)   { return []byte(c.stdout), c.waitErr }
func (c *envMatrixFakeCmd) StdoutPipe() (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(c.stdout)), nil
}
func (c *envMatrixFakeCmd) StderrPipe() (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(c.stderr)), nil
}
func (c *envMatrixFakeCmd) SetDir(dir string) { c.dir = dir }
func (c *envMatrixFakeCmd) SetEnv(env []string) {
	c.exec.sawSetEnvAny = true
}

// envMatrixFakeExec builds one envMatrixFakeCmd per CommandContext call,
// answering the list-checks call and the run call differently (keyed on argv
// shape, the same convention uatenvmatrix's own test fakes use) and
// recording every call for exact-binary/argv/cwd assertions.
type envMatrixFakeExec struct {
	calls        []*envMatrixFakeCmd
	listStdout   string
	listErr      error
	runStdout    string
	runStderr    string
	runErr       error
	sawSetEnvAny bool
}

func (f *envMatrixFakeExec) CommandContext(ctx context.Context, name string, args ...string) ExecCmd {
	cmd := &envMatrixFakeCmd{exec: f, name: name, args: append([]string(nil), args...)}
	if len(args) > 1 && args[1] == "--list-checks" {
		cmd.stdout = f.listStdout
		cmd.waitErr = f.listErr
	} else {
		cmd.stdout = f.runStdout
		cmd.stderr = f.runStderr
		cmd.waitErr = f.runErr
	}
	f.calls = append(f.calls, cmd)
	return cmd
}

func newRunResourcesForEnvMatrixTest(t *testing.T) RunResources {
	t.Helper()
	outDir := t.TempDir()
	return RunResources{OutDir: outDir, ImageTag: "uat-run-abc123"}
}

func TestRunCandidateEnvMatrix_ExactBinaryArgvAndCwd(t *testing.T) {
	res := newRunResourcesForEnvMatrixTest(t)
	stepsDir := t.TempDir()
	fe := &envMatrixFakeExec{listStdout: "check_one check_two\n"}

	if err := runCandidateEnvMatrix(context.Background(), fe, res, stepsDir); err != nil {
		t.Fatalf("runCandidateEnvMatrix: %v", err)
	}

	if len(fe.calls) != 2 {
		t.Fatalf("expected exactly 2 candidate calls, got %d", len(fe.calls))
	}

	wantBin := filepath.Join(res.OutDir, "pix-host")
	listCall := fe.calls[0]
	if listCall.name != wantBin {
		t.Errorf("list call binary = %q, want %q", listCall.name, wantBin)
	}
	if want := []string{"uat-env-matrix", "--list-checks"}; !equalArgs(listCall.args, want) {
		t.Errorf("list call args = %v, want %v", listCall.args, want)
	}
	if listCall.dir != res.OutDir {
		t.Errorf("list call cwd = %q, want %q", listCall.dir, res.OutDir)
	}

	runCall := fe.calls[1]
	if runCall.name != wantBin {
		t.Errorf("run call binary = %q, want %q", runCall.name, wantBin)
	}
	wantImageTag := "docker.io/mcavage/pix:" + res.ImageTag
	wantRunArgs := []string{"uat-env-matrix", "--out-dir", res.OutDir, "--steps-dir", stepsDir, "--image-tag", wantImageTag}
	if !equalArgs(runCall.args, wantRunArgs) {
		t.Errorf("run call args = %v, want %v", runCall.args, wantRunArgs)
	}
	if runCall.dir != res.OutDir {
		t.Errorf("run call cwd = %q, want %q", runCall.dir, res.OutDir)
	}
}

func TestRunCandidateEnvMatrix_NeverCallsSetEnv(t *testing.T) {
	res := newRunResourcesForEnvMatrixTest(t)
	stepsDir := t.TempDir()
	fe := &envMatrixFakeExec{listStdout: "check_one\n"}

	if err := runCandidateEnvMatrix(context.Background(), fe, res, stepsDir); err != nil {
		t.Fatalf("runCandidateEnvMatrix: %v", err)
	}
	if fe.sawSetEnvAny {
		t.Error("runCandidateEnvMatrix must never call SetEnv: the child must inherit the worker's authenticated environment")
	}
}

func TestRunCandidateEnvMatrix_ListsCandidateChecksAsEvidenceBeforeExecution(t *testing.T) {
	res := newRunResourcesForEnvMatrixTest(t)
	stepsDir := t.TempDir()
	fe := &envMatrixFakeExec{listStdout: "alpha_check beta_check\n"}

	if err := runCandidateEnvMatrix(context.Background(), fe, res, stepsDir); err != nil {
		t.Fatalf("runCandidateEnvMatrix: %v", err)
	}
	logBytes, err := os.ReadFile(filepath.Join(stepsDir, "env_matrix.log"))
	if err != nil {
		t.Fatalf("read env_matrix.log: %v", err)
	}
	log := string(logBytes)
	if !strings.Contains(log, "alpha_check") || !strings.Contains(log, "beta_check") {
		t.Errorf("evidence log does not list candidate checks: %s", log)
	}
	// The evidence line must appear before the actual run invocation.
	evidenceIdx := strings.Index(log, "candidate checks")
	runIdx := strings.Index(log, "--out-dir")
	if evidenceIdx == -1 || runIdx == -1 || evidenceIdx > runIdx {
		t.Errorf("candidate checks were not listed as evidence before execution: %s", log)
	}
}

func TestRunCandidateEnvMatrix_ListChecksBoundRejectsImplausibleCount(t *testing.T) {
	res := newRunResourcesForEnvMatrixTest(t)
	stepsDir := t.TempDir()
	huge := strings.Repeat("check ", envMatrixMaxListedChecks+1)
	fe := &envMatrixFakeExec{listStdout: huge}

	err := runCandidateEnvMatrix(context.Background(), fe, res, stepsDir)
	if err == nil {
		t.Fatal("expected an error for an implausible check count")
	}
	if len(fe.calls) != 1 {
		t.Errorf("expected the run step to never execute after a bad list, got %d calls", len(fe.calls))
	}
}

func TestRunCandidateEnvMatrix_ListChecksBoundRejectsEmpty(t *testing.T) {
	res := newRunResourcesForEnvMatrixTest(t)
	stepsDir := t.TempDir()
	fe := &envMatrixFakeExec{listStdout: "   \n"}

	if err := runCandidateEnvMatrix(context.Background(), fe, res, stepsDir); err == nil {
		t.Fatal("expected an error when the candidate reports zero checks")
	}
}

func TestRunCandidateEnvMatrix_BoundedLogOnNonzeroFailure(t *testing.T) {
	res := newRunResourcesForEnvMatrixTest(t)
	stepsDir := t.TempDir()
	huge := strings.Repeat("x", 2*1024*1024)
	fe := &envMatrixFakeExec{
		listStdout: "check_one\n",
		runStdout:  huge,
		runErr:     errors.New("exit status 1"),
	}

	err := runCandidateEnvMatrix(context.Background(), fe, res, stepsDir)
	if err == nil {
		t.Fatal("expected an error when the candidate run step fails")
	}
	if !strings.Contains(err.Error(), "steps/env_matrix.log") {
		t.Errorf("error does not report the artifact: %v", err)
	}

	fi, statErr := os.Stat(filepath.Join(stepsDir, "env_matrix.log"))
	if statErr != nil {
		t.Fatalf("stat env_matrix.log: %v", statErr)
	}
	const margin = 4096
	if fi.Size() > candidateLogMaxBytes+margin {
		t.Errorf("log size %d exceeds the bounded cap plus margin", fi.Size())
	}
}

func TestRunEnvMatrixStep_NilSeamFailsClosed(t *testing.T) {
	r := &Runner{} // deliberately built without NewRunner: envMatrix is nil
	err := r.runEnvMatrixStep(context.Background(), RunResources{}, t.TempDir())
	if err == nil {
		t.Fatal("expected an error when envMatrix is not wired")
	}
	if !strings.Contains(err.Error(), "no env matrix wired") {
		t.Errorf("error = %v, want it to name the missing seam", err)
	}
}

func TestRunEnvMatrixStep_DelegatesToWiredSeam(t *testing.T) {
	var gotRes RunResources
	var gotStepsDir string
	r := &Runner{envMatrix: func(ctx context.Context, res RunResources, stepsDir string) error {
		gotRes = res
		gotStepsDir = stepsDir
		return nil
	}}
	res := RunResources{OutDir: "/out", ImageTag: "uat-x"}
	if err := r.runEnvMatrixStep(context.Background(), res, "/steps"); err != nil {
		t.Fatalf("runEnvMatrixStep: %v", err)
	}
	if gotRes != res || gotStepsDir != "/steps" {
		t.Errorf("seam received (%#v, %q), want (%#v, %q)", gotRes, gotStepsDir, res, "/steps")
	}
}

func equalArgs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
