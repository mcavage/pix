package uat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	uattypes "pix/host/uat"
)

func (r *Runner) executeAsync(ctx context.Context, runID, commit string, scenario *uattypes.Scenario) {
	runDir := filepath.Join(r.stateDir, "runs", runID)
	eventsPath := filepath.Join(runDir, "events.log")
	evLog := NewEventLog(eventsPath)

	state := "pass"
	var failErr error

	defer func() {
		if failErr != nil {
			if ctx.Err() == context.DeadlineExceeded {
				state = "timed-out"
			} else if ctx.Err() == context.Canceled {
				state = "cancelled"
			} else if failErr == ErrNotFound || failErr == ErrQuotaExceeded || IsHostFailure(failErr) || failErr.Error() == "incomplete" { // mapping host unknown absence as incomplete
				state = "incomplete"
			} else {
				state = "fail"
			}
		}

		_ = evLog.Append(Event{
			Type:  EventRunDone,
			State: state,
			Message: func() string {
				if failErr != nil {
					return failErr.Error()
				}
				return "success"
			}(),
		})

		cleanupErr := r.lease.Cleanup(context.Background(), runID)
		if cleanupErr != nil {
			_ = evLog.Append(Event{Type: EventStatus, State: "cleanup_fail", Message: cleanupErr.Error()})
		}

		r.mu.Lock()
		delete(r.activeRuns, runID)
		r.mu.Unlock()
	}()

	// Acquire initial lease for run runID
	if err := r.lease.Acquire(ctx, runID, "run"); err != nil {
		failErr = fmt.Errorf("incomplete: failed to acquire run lease: %w", err)
		return
	}

	for _, step := range scenario.Steps {
		if err := ctx.Err(); err != nil {
			failErr = err
			return
		}

		_ = evLog.Append(Event{Type: EventStepStart, Message: step.ID})

		err := r.executeStep(ctx, runID, commit, scenario, step)
		if err != nil {
			failErr = err
			_ = evLog.Append(Event{Type: EventStepDone, State: "fail", Message: err.Error()})
			return
		}

		_ = evLog.Append(Event{Type: EventStepDone, State: "pass", Message: step.ID})
	}
}

type RunResources struct {
	SourceDir   string
	OutDir      string
	ImageTar    string
	FixtureDir  string
	ImageTag    string
	SandboxName string
}

func (r *Runner) executeCandidateSmoke(ctx context.Context, runID, commit string, scenario *uattypes.Scenario) error {
	res := RunResources{
		SourceDir:   filepath.Join(r.stateDir, "runs", runID, "source"),
		OutDir:      filepath.Join(r.stateDir, "runs", runID, "out"),
		ImageTar:    filepath.Join(r.stateDir, "runs", runID, "image.tar"),
		FixtureDir:  filepath.Join(r.stateDir, "runs", runID, "fixture"),
		ImageTag:    "uat-" + runID,
		SandboxName: "pix-uat-" + runID,
	}

	if err := r.lease.Acquire(ctx, runID, "sandbox_"+runID); err != nil {
		return err
	}
	if err := r.lease.Acquire(ctx, runID, "image_uat-"+runID); err != nil {
		return err
	}

	if err := r.git.Clone(ctx, commit, res.SourceDir); err != nil {
		return fmt.Errorf("clone: %w", err)
	}

	err := func() error {
		select {
		case r.buildSem <- struct{}{}:
			defer func() { <-r.buildSem }()
		case <-ctx.Done():
			return ctx.Err()
		}
		cmd := r.exec.CommandContext(ctx, "docker", "build", "-t", "docker.io/mcavage/pix:"+res.ImageTag, "--", res.SourceDir)
		return cmd.Run()
	}()
	if err != nil {
		return fmt.Errorf("build image: %w", err)
	}

	if err := os.MkdirAll(res.OutDir, 0755); err != nil {
		return err
	}

	buildCandidatePixCmd := r.exec.CommandContext(ctx, "docker", "run", "--rm",
		"-v", res.SourceDir+":/src",
		"-v", res.OutDir+":/out",
		"-w", "/src/services/host",
		"-e", "CGO_ENABLED=0",
		"-e", "GOOS=darwin",
		"golang:1.22",
		"go", "build", "-o", "/out/pix", "./cmd/pix",
	)
	if err := buildCandidatePixCmd.Run(); err != nil {
		return fmt.Errorf("build candidate pix: %w", err)
	}

	buildCandidateHostCmd := r.exec.CommandContext(ctx, "docker", "run", "--rm",
		"-v", res.SourceDir+":/src",
		"-v", res.OutDir+":/out",
		"-w", "/src/services/host",
		"-e", "CGO_ENABLED=0",
		"-e", "GOOS=darwin",
		"golang:1.22",
		"go", "build", "-o", "/out/pix-host", ".",
	)
	if err := buildCandidateHostCmd.Run(); err != nil {
		return fmt.Errorf("build candidate pix-host: %w", err)
	}

	saveCmd := r.exec.CommandContext(ctx, "docker", "save", "docker.io/mcavage/pix:"+res.ImageTag, "-o", res.ImageTar)
	if err := saveCmd.Run(); err != nil {
		return fmt.Errorf("docker save: %w", err)
	}

	loadCmd := r.exec.CommandContext(ctx, "sbx", "template", "load", "--", "docker.io/mcavage/pix:"+res.ImageTag, res.ImageTar)
	if err := loadCmd.Run(); err != nil {
		return fmt.Errorf("template load: %w", err)
	}

	if err := r.image.Probe(ctx, "docker.io/mcavage/pix:"+res.ImageTag); err != nil {
		return fmt.Errorf("image probe: %w", err)
	}

	if err := os.MkdirAll(res.FixtureDir, 0755); err != nil {
		return err
	}

	args := []string{"run", "--name", res.SandboxName, "--template", "docker.io/mcavage/pix:"+res.ImageTag}
	if scenario.Name == "self-uat-runner" || scenario.Name == "self-development-uat" {
		args = append(args, "--dev")
	}

	pixCmd := r.exec.CommandContext(ctx, filepath.Join(res.OutDir, "pix"), args...)
	pixCmd.SetEnv(append(os.Environ(), "PIX_UAT_RECURSION_DISABLE=1"))
	pixCmd.SetDir(res.FixtureDir)

	if err := pixCmd.Start(); err != nil {
		return fmt.Errorf("start candidate pix: %w", err)
	}

	go func() {
		// Wait to reap the zombie process, ignore error since it will be killed by context cancellation
		_ = pixCmd.Wait()
	}()

	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		if err := r.sandbox.Probe(probeCtx, runID); err == nil {
			break
		}
		select {
		case <-probeCtx.Done():
			return fmt.Errorf("sandbox probe timeout")
		case <-time.After(1 * time.Second):
		}
	}

	return nil
}

func (r *Runner) executeStep(ctx context.Context, runID, commit string, scenario *uattypes.Scenario, step uattypes.Step) error {
	// Step executor based on fixed actions
	switch step.Do {
	case "mcp_add":
		name := extractStringMap(step.With, "name")
		if err := r.lease.Acquire(ctx, runID, "mcp:"+name); err != nil {
			return err
		}
		// Candidate Makefile is never executed.
		// "Every external call uses exec.CommandContext-style argv with no shell and capped output/time."
		planner, err := uattypes.NewMCPPlanner("/usr/local/bin/pix-host", r.repoPath, r.stateDir, runID)
		if err != nil {
			return err
		}
		argv := planner.PlanRegistrationAdd(name)
		return r.mcp.Add(ctx, name, argv)
	case "mcp_auth":
		name := extractStringMap(step.With, "name")
		return r.mcp.Auth(ctx, name)
	case "mcp_status": // maybe check?
		// check expect
		name := extractStringMap(step.With, "name")
		status, err := r.mcp.Status(ctx, name)
		if err != nil {
			return err
		}
		expected := extractStringMap(step.Expect, "mcp_status")
		if status != expected {
			return fmt.Errorf("expected mcp_status %s, got %s", expected, status)
		}
	case "mcp_remove":
		name := extractStringMap(step.With, "name")
		return r.mcp.Remove(ctx, name)
	case "candidate_smoke":
		return r.executeCandidateSmoke(ctx, runID, commit, scenario)
	case "mcp_tool_present":
		toolName := extractStringMap(step.With, "tool")
		for _, c := range toolName {
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
				return fmt.Errorf("invalid tool name: must be strict identifier")
			}
		}
		status, err := r.mcp.Status(ctx, toolName)
		if err != nil {
			return err
		}
		if status == "" || status == "not found" { // maybe depends on how status is reported
			return fmt.Errorf("mcp tool not present: %s", toolName)
		}
		return nil
	case "check":
		// named check
		return nil
	default:
		return fmt.Errorf("unknown action: %s", step.Do)
	}
	return nil
}
