package uat_test

import (
	"os"

	"context"
	"io"
	"path/filepath"
	"testing"

	"pix/host/workflow/uat"
)

type capturedExec struct {
	args []string
	env  []string
	dir  string
}

type mockGit struct {
	resolveCommit func(ctx context.Context, commit string) (string, error)
	readTreeFile  func(ctx context.Context, commit, path string) ([]byte, error)
	clone         func(ctx context.Context, commit, dest string) error
}

func (m *mockGit) ResolveCommit(ctx context.Context, commit string) (string, error) {
	if m.resolveCommit != nil {
		return m.resolveCommit(ctx, commit)
	}
	return commit, nil
}

func (m *mockGit) ReadTreeFile(ctx context.Context, commit, path string) ([]byte, error) {
	if m.readTreeFile != nil {
		return m.readTreeFile(ctx, commit, path)
	}
	return []byte(""), nil
}

func (m *mockGit) Clone(ctx context.Context, commit, dest string) error {
	if m.clone != nil {
		return m.clone(ctx, commit, dest)
	}
	return nil
}

type mockExec struct{}

func (m *mockExec) CommandContext(ctx context.Context, name string, args ...string) uat.ExecCmd {
	return &mockCmd{}
}

type mockCmd struct{}

func (m *mockCmd) Run() error                         { return nil }
func (m *mockCmd) Start() error                       { return nil }
func (m *mockCmd) Wait() error                        { return nil }
func (m *mockCmd) Output() ([]byte, error)            { return nil, nil }
func (m *mockCmd) StdoutPipe() (io.ReadCloser, error) { return nil, nil }
func (m *mockCmd) StderrPipe() (io.ReadCloser, error) { return nil, nil }
func (m *mockCmd) SetEnv(env []string)                {}
func (m *mockCmd) SetDir(dir string)                  {}

type mockSandbox struct{}

func (m *mockSandbox) Create(ctx context.Context, runID string) error { return nil }
func (m *mockSandbox) Probe(ctx context.Context, runID string) error  { return nil }
func (m *mockSandbox) Remove(ctx context.Context, runID string) error { return nil }

type mockMCP struct{}

func (m *mockMCP) Add(ctx context.Context, name string, argv []string) error { return nil }
func (m *mockMCP) Auth(ctx context.Context, runID string, name string) error { return nil }
func (m *mockMCP) Status(ctx context.Context, name string) (string, error)   { return "ok", nil }
func (m *mockMCP) Remove(ctx context.Context, name string) error             { return nil }

type mockImage struct{}

func (m *mockImage) Load(ctx context.Context, tag, ws string) error { return nil }
func (m *mockImage) Probe(ctx context.Context, tag string) error    { return nil }

type mockLease struct {
	acquires []string
}

func (m *mockLease) Acquire(ctx context.Context, runID string, res string) error {
	m.acquires = append(m.acquires, res)
	return nil
}
func (m *mockLease) Release(ctx context.Context, runID string, res string) error { return nil }
func (m *mockLease) Cleanup(ctx context.Context, runID string) error             { return nil }

func TestSubmitDryRun(t *testing.T) {
	stateDir := t.TempDir()
	mg := &mockGit{
		readTreeFile: func(ctx context.Context, commit, path string) ([]byte, error) {
			return []byte(`schema: pix.uat/1
name: test-scenario
timeout: 1m
steps:
  - id: s1
    do: mcp_add
    with:
      name: testmcp`), nil
		},
	}

	pixHost := filepath.Join(stateDir, "pix-host")
	os.WriteFile(pixHost, []byte(""), 0755)
	runner, _ := uat.NewRunner(pixHost, "/repo", stateDir, mg, &mockExec{}, &mockSandbox{}, &mockMCP{}, &mockImage{}, &mockLease{}, 1)

	resp, err := runner.Submit(context.Background(), uat.SubmitRequest{
		Commit:       "main",
		ScenarioPath: "test.yaml",
		DryRun:       true,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.RunID != "" {
		t.Errorf("expected empty RunID for dry run, got %q", resp.RunID)
	}

	if resp.Plan != "test-scenario" {
		t.Errorf("expected plan 'test-scenario', got %q", resp.Plan)
	}
}

func TestSubmitAndStatus(t *testing.T) {
	stateDir := t.TempDir()
	mg := &mockGit{
		readTreeFile: func(ctx context.Context, commit, path string) ([]byte, error) {
			return []byte(`schema: pix.uat/1
name: test-scenario
timeout: 1m
steps:
  - id: s1
    do: mcp_add
    with:
      name: testmcp`), nil
		},
	}

	pixHost := filepath.Join(stateDir, "pix-host")
	os.WriteFile(pixHost, []byte(""), 0755)
	runner, _ := uat.NewRunner(pixHost, "/repo", stateDir, mg, &mockExec{}, &mockSandbox{}, &mockMCP{}, &mockImage{}, &mockLease{}, 1)

	resp, err := runner.Submit(context.Background(), uat.SubmitRequest{
		Commit:       "main",
		ScenarioPath: "test.yaml",
		DryRun:       false,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.RunID == "" {
		t.Fatalf("expected non-empty RunID")
	}

	// wait for run to finish (since our mocks are fast and async)
	statusReq := uat.StatusRequest{RunID: resp.RunID, Cursor: 0}
	var lastStatus *uat.StatusResponse

	var allEvents []uat.Event
	// simplistic wait loop
	for i := 0; i < 50; i++ {
		st, err := runner.Status(context.Background(), statusReq)
		if err != nil {
			t.Fatalf("unexpected status error: %v", err)
		}
		if len(st.Events) > 0 {
			allEvents = append(allEvents, st.Events...)
			statusReq.Cursor += int64(len(st.Events))
		}
		if st.State != "running" {
			lastStatus = st
			lastStatus.Events = allEvents
			break
		}
		statusReq.WaitMs = 100
	}

	if lastStatus == nil {
		t.Fatalf("run didn't finish")
	}

	if lastStatus.State != "pass" {
		t.Errorf("expected state 'pass', got %q", lastStatus.State)
	}

	// verify events
	if len(lastStatus.Events) == 0 {
		t.Errorf("expected events, got none")
	}

	hasRunDone := false
	for _, e := range lastStatus.Events {
		if e.Type == uat.EventRunDone {
			hasRunDone = true
			if e.State != "pass" {
				t.Errorf("run_done state: expected pass, got %s", e.State)
			}
		}
	}

	if !hasRunDone {
		t.Errorf("expected run_done event")
	}
}

type mockBlockingMCP struct {
	mockMCP
}

func (m *mockBlockingMCP) Add(ctx context.Context, name string, argv []string) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestSubmitAndAbort(t *testing.T) {
	stateDir := t.TempDir()
	mg := &mockGit{
		readTreeFile: func(ctx context.Context, commit, path string) ([]byte, error) {
			return []byte(`schema: pix.uat/1
name: test-abort
timeout: 1m
steps:
  - id: s1
    do: mcp_add
    with:
      name: testmcp`), nil
		},
	}

	blockingMCP := &mockBlockingMCP{}

	pixHost := filepath.Join(stateDir, "pix-host")
	os.WriteFile(pixHost, []byte(""), 0755)
	runner, _ := uat.NewRunner(pixHost, "/repo", stateDir, mg, &mockExec{}, &mockSandbox{}, blockingMCP, &mockImage{}, &mockLease{}, 1)

	resp, err := runner.Submit(context.Background(), uat.SubmitRequest{
		Commit:       "main",
		ScenarioPath: "test.yaml",
		DryRun:       false,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// cancel immediately
	err = runner.Abort(context.Background(), resp.RunID)
	if err != nil {
		t.Fatalf("unexpected error on abort: %v", err)
	}

	// wait for run to finish
	var lastStatus *uat.StatusResponse
	statusReq := uat.StatusRequest{RunID: resp.RunID, Cursor: 0}

	for i := 0; i < 100; i++ {
		st, _ := runner.Status(context.Background(), statusReq)
		if st.State != "running" {
			lastStatus = st
			break
		}
	}

	if lastStatus.State != "cancelled" {
		t.Errorf("expected cancelled, got %s", lastStatus.State)
	}
}

func TestCandidateSmoke(t *testing.T) {
	stateDir := t.TempDir()
	mg := &mockGit{
		readTreeFile: func(ctx context.Context, commit, path string) ([]byte, error) {
			return []byte(`schema: pix.uat/1
name: self-uat-runner
timeout: 1m
steps:
  - id: smoke
    do: candidate_smoke`), nil
		},
	}

	var execs []capturedExec

	me := &captureExecHelper{
		onCommand: func(name string, args ...string) uat.ExecCmd {
			return &mockCmdHelper{
				args: append([]string{name}, args...),
				record: func(ce capturedExec) {
					execs = append(execs, ce)
				},
			}
		},
	}

	ml := &mockLease{}
	pixHost := filepath.Join(stateDir, "pix-host")
	os.WriteFile(pixHost, []byte(""), 0755)
	runner, _ := uat.NewRunner(pixHost, "/repo", stateDir, mg, me, &mockSandbox{}, &mockMCP{}, &mockImage{}, ml, 1)

	resp, err := runner.Submit(context.Background(), uat.SubmitRequest{
		Commit:       "main",
		ScenarioPath: "test.yaml",
		DryRun:       false,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	statusReq := uat.StatusRequest{RunID: resp.RunID, Cursor: 0}
	var lastStatus *uat.StatusResponse
	var allEvents []uat.Event

	for i := 0; i < 50; i++ {
		st, err := runner.Status(context.Background(), statusReq)
		if err != nil {
			t.Fatalf("unexpected status error: %v", err)
		}
		if len(st.Events) > 0 {
			allEvents = append(allEvents, st.Events...)
			statusReq.Cursor += int64(len(st.Events))
		}
		if st.State != "running" {
			lastStatus = st
			lastStatus.Events = allEvents
			break
		}
		statusReq.WaitMs = 100
	}

	if lastStatus == nil || lastStatus.State != "pass" {
		t.Fatalf("expected pass, got %v", lastStatus)
	}

	if len(execs) < 6 {
		t.Errorf("expected at least 6 commands, got %d: %v", len(execs), execs)
	} else {
		if execs[0].args[0] != "docker" || execs[0].args[1] != "build" {
			t.Errorf("expected docker build, got %v", execs[0].args)
		}
		if execs[1].args[0] != "docker" || execs[1].args[1] != "run" {
			t.Errorf("expected docker run pix, got %v", execs[1].args)
		}
		if execs[2].args[0] != "docker" || execs[2].args[1] != "run" {
			t.Errorf("expected docker run pix-host, got %v", execs[2].args)
		}
		if execs[3].args[0] != "docker" || execs[3].args[1] != "save" {
			t.Errorf("expected docker save, got %v", execs[3].args)
		}

		if len(execs[4].args) != 4 || execs[4].args[0] != "sbx" || execs[4].args[1] != "template" || execs[4].args[2] != "load" || execs[4].args[3] != filepath.Join(stateDir, "runs", resp.RunID, "image.tar") {
			t.Errorf("expected sbx template load <tar>, got %v", execs[4].args)
		}

		lastCmd := execs[5]
		fixtureDir := filepath.Join(stateDir, "runs", resp.RunID, "fixture")
		expectedSandboxName := "pix-uat-" + resp.RunID
		expectedTemplate := "docker.io/mcavage/pix:uat-" + resp.RunID

		expectedArgs := []string{lastCmd.args[0], "run", fixtureDir, "--name", expectedSandboxName, "--template", expectedTemplate, "--dev"}
		for i, arg := range expectedArgs {
			if i >= len(lastCmd.args) || lastCmd.args[i] != arg {
				t.Errorf("expected arg %d to be %q, got %q", i, arg, lastCmd.args)
			}
		}

		expectedDir := filepath.Join(stateDir, "runs", resp.RunID, "source")
		if lastCmd.dir != expectedDir {
			t.Errorf("expected dir %s, got %s", expectedDir, lastCmd.dir)
		}
	}

	// Check leases
	expectedLeases := []string{
		"run",
		"sandbox_pix-uat-" + resp.RunID,
		"image_uat-" + resp.RunID,
		"template_docker.io/mcavage/pix:uat-" + resp.RunID,
	}
	for _, el := range expectedLeases {
		found := false
		for _, l := range ml.acquires {
			if l == el {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected lease %s, got %v", el, ml.acquires)
		}
	}
}

type captureExecHelper struct {
	onCommand func(name string, args ...string) uat.ExecCmd
}

func (c *captureExecHelper) CommandContext(ctx context.Context, name string, args ...string) uat.ExecCmd {
	return c.onCommand(name, args...)
}

type mockCmdHelper struct {
	args   []string
	env    []string
	dir    string
	err    error
	record func(ce capturedExec)
}

func (m *mockCmdHelper) Run() error {
	if m.err != nil {
		return m.err
	}
	m.record(capturedExec{args: m.args, env: m.env, dir: m.dir})
	return nil
}
func (m *mockCmdHelper) Start() error {
	if m.err != nil {
		return m.err
	}
	m.record(capturedExec{args: m.args, env: m.env, dir: m.dir})
	return nil
}
func (m *mockCmdHelper) Wait() error                        { return nil }
func (m *mockCmdHelper) Output() ([]byte, error)            { return nil, nil }
func (m *mockCmdHelper) StdoutPipe() (io.ReadCloser, error) { return nil, nil }
func (m *mockCmdHelper) StderrPipe() (io.ReadCloser, error) { return nil, nil }
func (m *mockCmdHelper) SetEnv(env []string)                { m.env = env }
func (m *mockCmdHelper) SetDir(dir string)                  { m.dir = dir }
