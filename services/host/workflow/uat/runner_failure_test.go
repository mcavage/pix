package uat_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"pix/host/workflow/uat"
)

type failInjector struct {
	failClone        bool
	failImageBuild   bool
	failPixBuild     bool
	failPixHostBuild bool
	failImageSave    bool
	failTemplateLoad bool
	failImageProbe   bool
	failCandidateRun bool
	failSandboxProbe bool
}

type trackLease struct {
	acquires []string
	cleanups []string
}

func (m *trackLease) Acquire(ctx context.Context, runID string, res string) error {
	m.acquires = append(m.acquires, res)
	return nil
}
func (m *trackLease) Release(ctx context.Context, runID string, res string) error { return nil }
func (m *trackLease) Cleanup(ctx context.Context, runID string) error {
	m.cleanups = append(m.cleanups, runID)
	return nil
}

type trackSandbox struct {
	failProbe bool
}

func (m *trackSandbox) Create(ctx context.Context, runID string) error { return nil }
func (m *trackSandbox) Probe(ctx context.Context, runID string) error {
	if m.failProbe {
		return errors.New("incomplete: sandbox probe failed")
	}
	return nil
}
func (m *trackSandbox) Remove(ctx context.Context, runID string) error { return nil }

type trackImage struct {
	failProbe bool
}

func (m *trackImage) Load(ctx context.Context, tag, ws string) error { return nil }
func (m *trackImage) Probe(ctx context.Context, tag string) error {
	if m.failProbe {
		return errors.New("incomplete: image probe failed")
	}
	return nil
}

func TestCandidateSmokeFailures(t *testing.T) {
	cases := []struct {
		name          string
		injector      failInjector
		expectedState string
	}{
		{"clone fail", failInjector{failClone: true}, "fail"},
		{"image build fail", failInjector{failImageBuild: true}, "fail"},
		{"pix build fail", failInjector{failPixBuild: true}, "fail"},
		{"pix-host build fail", failInjector{failPixHostBuild: true}, "fail"},
		{"save fail", failInjector{failImageSave: true}, "fail"},
		{"template load fail", failInjector{failTemplateLoad: true}, "fail"},
		{"image probe fail", failInjector{failImageProbe: true}, "incomplete"},
		{"candidate spawn fail", failInjector{failCandidateRun: true}, "fail"},
		{"sandbox probe fail", failInjector{failSandboxProbe: true}, "timed-out"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := t.TempDir()
			mg := &mockGit{
				readTreeFile: func(ctx context.Context, commit, path string) ([]byte, error) {
					return []byte("schema: pix.uat/1\nname: test\ntimeout: 100ms\nsteps:\n  - id: smoke\n    do: candidate_smoke"), nil
				},
				clone: func(ctx context.Context, commit, dest string) error {
					if tc.injector.failClone {
						return errors.New("clone fail")
					}
					return nil
				},
			}

			var execs []capturedExec
			me := &captureExecHelper{
				onCommand: func(name string, args ...string) uat.ExecCmd {
					cmdName := name
					if len(args) > 0 {
						cmdName = name + " " + args[0]
					}
					if len(args) > 1 {
						cmdName = cmdName + " " + args[1]
					}

					var err error
					if strings.HasPrefix(cmdName, "docker build") && tc.injector.failImageBuild {
						err = errors.New("err")
					}
					if strings.HasPrefix(cmdName, "docker run") {
						for _, arg := range args {
							if strings.Contains(arg, "cmd/pix") && tc.injector.failPixBuild {
								err = errors.New("err")
							}
							if arg == "." && tc.injector.failPixHostBuild {
								err = errors.New("err")
							}
						}
					}
					if strings.HasPrefix(cmdName, "docker save") && tc.injector.failImageSave {
						err = errors.New("err")
					}
					if strings.HasPrefix(cmdName, "sbx template load") && tc.injector.failTemplateLoad {
						err = errors.New("err")
					}
					if strings.Contains(name, "pix") && tc.injector.failCandidateRun {
						err = errors.New("err")
					}

					return &mockCmdHelper{
						args: append([]string{name}, args...),
						err:  err,
						record: func(ce capturedExec) {
							execs = append(execs, ce)
						},
					}
				},
			}

			ml := &trackLease{}
			runner := uat.NewRunner("/repo", stateDir, mg, me, &trackSandbox{failProbe: tc.injector.failSandboxProbe}, &mockMCP{}, &trackImage{failProbe: tc.injector.failImageProbe}, ml, 1)

			resp, err := runner.Submit(context.Background(), uat.SubmitRequest{
				Commit:       "main",
				ScenarioPath: "test.yaml",
				DryRun:       false,
			})
			if err != nil {
				t.Fatalf("submit fail: %v", err)
			}

			statusReq := uat.StatusRequest{RunID: resp.RunID, Cursor: 0}
			var lastStatus *uat.StatusResponse
			for i := 0; i < 50; i++ {
				st, _ := runner.Status(context.Background(), statusReq)
				if st != nil && st.State != "running" {
					lastStatus = st
					break
				}
				time.Sleep(10 * time.Millisecond)
			}

			if lastStatus == nil {
				t.Fatalf("never finished")
			}

			if lastStatus.State != tc.expectedState {
				t.Errorf("expected %s, got %s", tc.expectedState, lastStatus.State)
			}

			if len(ml.cleanups) == 0 {
				t.Errorf("expected lease cleanup to be called")
			}

			if len(ml.acquires) == 0 {
				t.Errorf("expected at least some acquires before failure")
			}
		})
	}
}
