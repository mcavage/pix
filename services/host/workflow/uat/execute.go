package uat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
			} else if failErr == ErrNotFound || failErr == ErrQuotaExceeded || strings.Contains(failErr.Error(), "incomplete") { // mapping host unknown absence as incomplete
				state = "incomplete"
			} else {
				state = "fail"
			}
		}

		cleanupErr := r.lease.Cleanup(context.Background(), runID)

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

		if cleanupErr != nil {
			_ = evLog.Append(Event{Type: EventStatus, State: "cleanup_fail", Message: cleanupErr.Error()})
		}

		r.mu.Lock()
		rc := r.activeRuns[runID]
		delete(r.activeRuns, runID)
		r.mu.Unlock()
		if rc != nil {
			rc.cancel()
			rc.wg.Wait()
		}
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

	if err := r.lease.Acquire(ctx, runID, "sandbox_"+res.SandboxName); err != nil {
		return err
	}
	if err := r.lease.Acquire(ctx, runID, "image_uat-"+runID); err != nil {
		return err
	}
	if err := r.lease.Acquire(ctx, runID, "template_docker.io/mcavage/pix:"+res.ImageTag); err != nil {
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

	goVersion := "1.22"
	goModPath := filepath.Join(res.SourceDir, "services", "host", "go.mod")
	if b, err := os.ReadFile(goModPath); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "go ") {
				goVersion = strings.TrimSpace(strings.TrimPrefix(line, "go "))
				break
			}
		}
	}
	golangImage := fmt.Sprintf("golang:%s", goVersion)

	buildCandidatePixCmd := r.exec.CommandContext(ctx, "docker", "run", "--rm",
		"-v", res.SourceDir+":/src",
		"-v", res.OutDir+":/out",
		"-w", "/src/services/host",
		"-e", "CGO_ENABLED=0",
		"-e", "GOOS=darwin",
		golangImage,
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
		golangImage,
		"go", "build", "-o", "/out/pix-host", ".",
	)
	if err := buildCandidateHostCmd.Run(); err != nil {
		return fmt.Errorf("build candidate pix-host: %w", err)
	}

	saveCmd := r.exec.CommandContext(ctx, "docker", "save", "docker.io/mcavage/pix:"+res.ImageTag, "-o", res.ImageTar)
	if err := saveCmd.Run(); err != nil {
		return fmt.Errorf("docker save: %w", err)
	}

	loadCmd := r.exec.CommandContext(ctx, "sbx", "template", "load", res.ImageTar)
	if err := loadCmd.Run(); err != nil {
		return fmt.Errorf("template load: %w", err)
	}

	if err := r.image.Probe(ctx, "docker.io/mcavage/pix:"+res.ImageTag); err != nil {
		return fmt.Errorf("image probe: %w", err)
	}

	if err := os.MkdirAll(res.FixtureDir, 0755); err != nil {
		return err
	}

	args := []string{"run", res.FixtureDir, "--name", res.SandboxName, "--template", "docker.io/mcavage/pix:" + res.ImageTag}
	if scenario.Name == "self-uat-runner" || scenario.Name == "self-development-uat" {
		args = append(args, "--dev")
	}

	pixCmd := r.exec.CommandContext(ctx, filepath.Join(res.OutDir, "pix"), args...)
	// 1) Candidate pix environment: isolate from host pix paths
	fakeHome := filepath.Join(r.stateDir, "runs", runID, "home")
	fakeConfig := filepath.Join(r.stateDir, "runs", runID, "config")
	fakeData := filepath.Join(r.stateDir, "runs", runID, "data")
	fakeState := filepath.Join(r.stateDir, "runs", runID, "state")
	fakeCache := filepath.Join(r.stateDir, "runs", runID, "cache")

	for _, d := range []string{fakeHome, fakeConfig, fakeData, fakeState, fakeCache} {
		if err := os.MkdirAll(d, 0700); err != nil {
			return err
		}
	}
	pixConfigFile := filepath.Join(fakeConfig, "config.toml")
	if err := os.WriteFile(pixConfigFile, []byte(""), 0600); err != nil {
		return err
	}

	envVars := []string{
		"PATH", "TMPDIR", "TMP", "TEMP", "LANG", "LC_ALL", "TERM",
		"DOCKER_HOST", "DOCKER_CONFIG", // for sbx auth
	}
	hasDockerConfig := false
	var newEnv []string
	for _, ev := range os.Environ() {
		for _, allow := range envVars {
			if strings.HasPrefix(ev, allow+"=") {
				newEnv = append(newEnv, ev)
				if allow == "DOCKER_CONFIG" {
					hasDockerConfig = true
				}
				break
			}
		}
	}
	if !hasDockerConfig {
		// Record host sbx auth compatibility as bootstrap proof
		hostHome, _ := os.UserHomeDir()
		if hostHome != "" {
			newEnv = append(newEnv, "DOCKER_CONFIG="+filepath.Join(hostHome, ".docker"))
		}
	}
	newEnv = append(newEnv, 
		"HOME="+fakeHome,
		"XDG_CONFIG_HOME="+fakeConfig,
		"XDG_DATA_HOME="+fakeData,
		"XDG_STATE_HOME="+fakeState,
		"XDG_CACHE_HOME="+fakeCache,
		"PIX_CONFIG="+pixConfigFile,
		"PIX_UAT_RECURSION=1",
	)

	pixCmd.SetEnv(newEnv)
	pixCmd.SetDir(res.SourceDir)

	if err := pixCmd.Start(); err != nil {
		return fmt.Errorf("start candidate pix: %w", err)
	}

	r.mu.Lock()
	if rc, ok := r.activeRuns[runID]; ok {
		rc.wg.Add(1)
		go func() {
			defer rc.wg.Done()
			_ = pixCmd.Wait()
		}()
	} else {
		// shouldn't happen, but just in case
		go func() { _ = pixCmd.Wait() }()
	}
	r.mu.Unlock()

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
		uatName := "uat-" + runID + "-" + name
		if err := r.lease.Acquire(ctx, runID, "mcp:"+uatName); err != nil {
			return err
		}
		planner, err := uattypes.NewMCPPlanner(r.pixHost, r.repoPath, r.stateDir, runID)
		if err != nil {
			return err
		}
		argv := planner.PlanRegistrationAdd(uatName)
		return r.mcp.Add(ctx, uatName, argv)
	case "mcp_auth":
		name := extractStringMap(step.With, "name")
		uatName := "uat-" + runID + "-" + name
		return r.mcp.Auth(ctx, uatName)
	case "mcp_status": // maybe check?
		// check expect
		name := extractStringMap(step.With, "name")
		uatName := "uat-" + runID + "-" + name
		status, err := r.mcp.Status(ctx, uatName)
		if err != nil {
			return err
		}
		expected := extractStringMap(step.Expect, "mcp_status")
		if status != expected {
			return fmt.Errorf("expected mcp_status %s, got %s", expected, status)
		}
	case "mcp_remove":
		name := extractStringMap(step.With, "name")
		uatName := "uat-" + runID + "-" + name
		return r.mcp.Remove(ctx, uatName)
	case "candidate_smoke":
		return r.executeCandidateSmoke(ctx, runID, commit, scenario)

	case "browser_check":
		urlStr := extractStringMap(step.With, "url")
		if urlStr == "" {
			return fmt.Errorf("missing url for browser_check")
		}

		cfg := CheckLinkConfig{
			RunID:  runID,
			Policy: &PublicLinkPolicy{Resolver: &realResolver{}},
		}

		factory := NewRealBrowserFactory()
		_, err := CheckLink(ctx, factory, cfg, urlStr)
		if err != nil {
			return fmt.Errorf("browser_check failed: %w", err)
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
