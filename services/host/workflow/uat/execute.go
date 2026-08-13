package uat

import (
	"context"
	"fmt"
	"path/filepath"

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

		// Best effort idempotent cleanup of leased resources
		_ = r.lease.Cleanup(context.Background(), runID)

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

		err := r.executeStep(ctx, runID, commit, step)
		if err != nil {
			failErr = err
			_ = evLog.Append(Event{Type: EventStepDone, State: "fail", Message: err.Error()})
			return
		}

		_ = evLog.Append(Event{Type: EventStepDone, State: "pass", Message: step.ID})
	}
}

func (r *Runner) executeStep(ctx context.Context, runID, commit string, step uattypes.Step) error {
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
	case "sandbox_create":
		if err := r.lease.Acquire(ctx, runID, "sandbox:"+runID); err != nil {
			return err
		}
		return r.sandbox.Create(ctx, runID)
	case "sandbox_probe":
		return r.sandbox.Probe(ctx, runID)
	case "sandbox_remove":
		return r.sandbox.Remove(ctx, runID)
	case "image_load":
		tag := extractStringMap(step.With, "tag")
		return r.image.Load(ctx, tag, r.repoPath)
	case "image_probe":
		tag := extractStringMap(step.With, "tag")
		return r.image.Probe(ctx, tag)
	case "clone":
		dest := extractStringMap(step.With, "dest")
		return r.git.Clone(ctx, commit, dest)
	case "build":
		select {
		case r.buildSem <- struct{}{}:
			defer func() { <-r.buildSem }()
		case <-ctx.Done():
			return ctx.Err()
		}
		cmd := r.exec.CommandContext(ctx, "docker", "build", "-t", "pix:uat-"+runID, r.repoPath)
		return cmd.Run()
	case "check":
		// named check
		return nil
	default:
		return fmt.Errorf("unknown action: %s", step.Do)
	}
	return nil
}
