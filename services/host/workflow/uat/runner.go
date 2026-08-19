package uat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	uattypes "pix/host/uat"
	"pix/host/uatmatrix"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrQuotaExceeded = errors.New("quota exceeded")
)

type Git interface {
	ResolveCommit(ctx context.Context, commit string) (string, error)
	ReadTreeFile(ctx context.Context, commit, path string) ([]byte, error)
	Clone(ctx context.Context, commit, destPath string) error
}

type Runner struct {
	repoPath string
	stateDir string
	pixHost  string

	git     Git
	exec    Exec
	sandbox Sandbox
	mcp     MCP
	image   Image
	lease   Lease

	mu         sync.Mutex
	activeRuns map[string]*runContext

	buildSem         chan struct{}
	buildConcurrency int

	// memoryMatrix is the isolated, host-backed memory UAT coverage run against
	// the just-built candidate binaries before sandbox launch. NewRunner wires
	// the real matrix unless the injected Exec supplies the narrow test-only
	// override method described there.
	memoryMatrix func(context.Context, RunResources, string) error
}

func (r *Runner) RetryCleanups() map[string]string {
	report := make(map[string]string)

	// 1. Clean leases
	entries, err := os.ReadDir(filepath.Join(r.stateDir, "leases"))
	if err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				runID := entry.Name()
				if err := r.lease.Cleanup(context.Background(), runID); err != nil {
					report["lease_"+runID] = err.Error()
				} else {
					report["lease_"+runID] = "success"
				}
			}
		}
	}

	// 2. Janitor run dirs
	runsDir := filepath.Join(r.stateDir, "runs")
	runEntries, err := os.ReadDir(runsDir)
	if err == nil {
		type runInfo struct {
			id      string
			modTime time.Time
			path    string
		}
		var completedRuns []runInfo
		now := time.Now()

		for _, entry := range runEntries {
			if !entry.IsDir() {
				continue
			}
			runID := entry.Name()
			runPath := filepath.Join(runsDir, runID)

			// Confinement check: no symlinks escaping root
			info, err := os.Lstat(runPath)
			if err != nil || info.Mode()&os.ModeSymlink != 0 {
				report["janitor_"+runID] = "skip: symlink or stat error"
				continue
			}

			// Active run check: if a lease dir still exists, skip
			leaseDir := filepath.Join(r.stateDir, "leases", runID)
			if _, err := os.Stat(leaseDir); err == nil {
				continue // active
			}

			completedRuns = append(completedRuns, runInfo{
				id:      runID,
				modTime: info.ModTime(),
				path:    runPath,
			})
		}

		// Sort by newest first
		sort.Slice(completedRuns, func(i, j int) bool {
			return completedRuns[i].modTime.After(completedRuns[j].modTime)
		})

		for i, run := range completedRuns {
			if i >= 8 || now.Sub(run.modTime) > 24*time.Hour {
				if err := os.RemoveAll(run.path); err != nil {
					report["janitor_"+run.id] = err.Error()
				} else {
					report["janitor_"+run.id] = "removed"
				}
			}
		}
	}

	return report
}

type runContext struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func NewRunner(pixHost, repoPath, stateDir string, git Git, exec Exec, sandbox Sandbox, mcp MCP, image Image, lease Lease, buildConcurrency int) (*Runner, error) {
	if buildConcurrency <= 0 {
		buildConcurrency = 1
	}

	if !filepath.IsAbs(pixHost) {
		return nil, fmt.Errorf("pixHost must be absolute: %s", pixHost)
	}
	info, err := os.Stat(pixHost)
	if err != nil {
		return nil, fmt.Errorf("pixHost not found: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("pixHost must be a regular file")
	}

	memoryMatrix := func(ctx context.Context, res RunResources, stepsDir string) error {
		return uatmatrix.Run(ctx, uatmatrix.Inputs{OutDir: res.OutDir, StepsDir: stepsDir})
	}
	// Mock command executors do not produce runnable candidate binaries. Tests
	// that exercise candidate_smoke's orchestration may implement this optional
	// method to replace only the host-backed matrix; production's realExec does
	// not implement it, so production cannot silently skip the checks.
	if override, ok := exec.(interface {
		RunCandidateMemoryMatrix(context.Context, RunResources, string) error
	}); ok {
		memoryMatrix = override.RunCandidateMemoryMatrix
	}

	return &Runner{
		pixHost:          pixHost,
		repoPath:         repoPath,
		stateDir:         stateDir,
		git:              git,
		exec:             exec,
		sandbox:          sandbox,
		mcp:              mcp,
		image:            image,
		lease:            lease,
		activeRuns:       make(map[string]*runContext),
		buildSem:         make(chan struct{}, buildConcurrency),
		buildConcurrency: buildConcurrency,
		memoryMatrix:     memoryMatrix,
	}, nil
}

type SubmitRequest struct {
	Commit       string
	ScenarioPath string
	DryRun       bool
}

func maxRunTimeout() time.Duration { return 60 * time.Minute }

type executionLimits struct {
	BuildConcurrency int    `json:"build_concurrency"`
	MaxRunTimeout    string `json:"max_run_timeout"`
	MaxLogBytes      int    `json:"max_log_bytes"`
	MaxArtifactBytes int    `json:"max_artifact_bytes"`
}

type candidateSmokeCoverage struct {
	Builds       []string `json:"builds"`
	MemoryChecks []string `json:"memory_checks"`
	HostServices []string `json:"host_services"`
}

type runnerCapabilities struct {
	Runner          bool                   `json:"runner"`
	ScenarioSchema  string                 `json:"scenario_schema"`
	LegalNeeds      []string               `json:"legal_needs"`
	LegalActions    []string               `json:"legal_actions"`
	LegalAssertions []string               `json:"legal_assertions"`
	NamedChecks     []string               `json:"named_checks"`
	Limits          executionLimits        `json:"candidate_build_limits"`
	CandidateSmoke  candidateSmokeCoverage `json:"candidate_smoke"`
}

func (r *Runner) limits() executionLimits {
	return executionLimits{
		BuildConcurrency: r.buildConcurrency,
		MaxRunTimeout:    maxRunTimeout().String(),
		MaxLogBytes:      candidateLogMaxBytes,
		MaxArtifactBytes: candidateLogMaxBytes,
	}
}

// Capabilities reports the runner's actual closed vocabulary and limits. The
// lists come from the scenario validator and memory matrix, not a parallel
// documentation-only registry.
func (r *Runner) capabilities() runnerCapabilities {
	legalNeeds, legalActions, legalAssertions := uattypes.LegalVocabulary()
	return runnerCapabilities{
		Runner:          true,
		ScenarioSchema:  "pix.uat/1",
		LegalNeeds:      legalNeeds,
		LegalActions:    legalActions,
		LegalAssertions: legalAssertions,
		NamedChecks:     []string{},
		Limits:          r.limits(),
		CandidateSmoke: candidateSmokeCoverage{
			Builds:       []string{"sandbox_image", "darwin_pix", "darwin_pix_host"},
			MemoryChecks: uatmatrix.CheckNames(),
			HostServices: []string{"memory"},
		},
	}
}

type planStep struct {
	ID     string `json:"id"`
	Action string `json:"action"`
}

type candidatePlan struct {
	ImageTag                 string   `json:"image_tag"`
	SandboxName              string   `json:"sandbox_name"`
	RunRoot                  string   `json:"run_root"`
	SourceDir                string   `json:"source_dir"`
	OutDir                   string   `json:"out_dir"`
	FixtureDir               string   `json:"fixture_dir"`
	ConfigDir                string   `json:"config_dir"`
	DataDir                  string   `json:"data_dir"`
	StateDir                 string   `json:"state_dir"`
	CacheDir                 string   `json:"cache_dir"`
	BrowserProfile           string   `json:"browser_profile"`
	UsesNormalPixState       bool     `json:"uses_normal_pix_state"`
	UsesNormalBrowserProfile bool     `json:"uses_normal_browser_profile"`
	Builds                   []string `json:"builds"`
	MemoryChecks             []string `json:"memory_checks"`
	Cleanup                  []string `json:"cleanup"`
}

type submitPlan struct {
	Scenario     string          `json:"scenario"`
	Commit       string          `json:"commit"`
	ScenarioPath string          `json:"scenario_path"`
	Timeout      string          `json:"timeout"`
	Needs        []string        `json:"needs"`
	Steps        []planStep      `json:"steps"`
	MutatesHost  bool            `json:"mutates_host"`
	Limits       executionLimits `json:"candidate_build_limits"`
	Candidate    *candidatePlan  `json:"candidate,omitempty"`
}

type SubmitResponse struct {
	RunID string     `json:"run_id"`
	Plan  submitPlan `json:"plan"`
}

func (r *Runner) plan(resolvedCommit, scenarioPath, runID string, scenario *uattypes.Scenario, mutatesHost bool) (submitPlan, error) {
	plan := submitPlan{
		Scenario:     scenario.Name,
		Commit:       resolvedCommit,
		ScenarioPath: scenarioPath,
		Timeout:      scenario.Timeout,
		Needs:        append([]string(nil), scenario.Needs...),
		MutatesHost:  mutatesHost,
		Limits:       r.limits(),
	}
	for _, step := range scenario.Steps {
		plan.Steps = append(plan.Steps, planStep{ID: step.ID, Action: step.Do})
		if step.Do == "candidate_smoke" && plan.Candidate == nil {
			resources := candidateRunResources(r.stateDir, runID)
			browserProfile, err := tempProfilePath(runID)
			if err != nil {
				return submitPlan{}, fmt.Errorf("resolve fresh UAT browser profile: %w", err)
			}
			plan.Candidate = &candidatePlan{
				ImageTag:                 "docker.io/mcavage/pix:" + resources.ImageTag,
				SandboxName:              resources.SandboxName,
				RunRoot:                  filepath.Join(r.stateDir, "runs", runID),
				SourceDir:                resources.SourceDir,
				OutDir:                   resources.OutDir,
				FixtureDir:               resources.FixtureDir,
				ConfigDir:                filepath.Join(r.stateDir, "runs", runID, "config"),
				DataDir:                  filepath.Join(r.stateDir, "runs", runID, "data"),
				StateDir:                 filepath.Join(r.stateDir, "runs", runID, "state"),
				CacheDir:                 filepath.Join(r.stateDir, "runs", runID, "cache"),
				BrowserProfile:           browserProfile,
				UsesNormalPixState:       false,
				UsesNormalBrowserProfile: false,
				Builds:                   []string{"sandbox_image", "darwin_pix", "darwin_pix_host"},
				MemoryChecks:             uatmatrix.CheckNames(),
				Cleanup:                  []string{"sandbox", "image", "sbx_template", "candidate_process_group", "fresh_browser_profile"},
			}
		}
	}
	return plan, nil
}

func (r *Runner) Submit(ctx context.Context, req SubmitRequest) (*SubmitResponse, error) {
	resolvedCommit, err := r.git.ResolveCommit(ctx, req.Commit)
	if err != nil {
		return nil, fmt.Errorf("resolve commit: %w", err)
	}

	scenarioData, err := r.git.ReadTreeFile(ctx, resolvedCommit, req.ScenarioPath)
	if err != nil {
		return nil, fmt.Errorf("read scenario: %w", err)
	}

	scenario, err := uattypes.UnmarshalScenario(scenarioData)
	if err != nil {
		return nil, fmt.Errorf("validate scenario: %w", err)
	}

	if req.DryRun {
		plan, planErr := r.plan(resolvedCommit, req.ScenarioPath, "<run-id>", scenario, false)
		if planErr != nil {
			return nil, planErr
		}
		return &SubmitResponse{RunID: "", Plan: plan}, nil
	}

	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("generate id: %w", err)
	}
	runID := fmt.Sprintf("run-%s-%s", time.Now().Format("20060102-150405"), hex.EncodeToString(b))

	plan, err := r.plan(resolvedCommit, req.ScenarioPath, runID, scenario, true)
	if err != nil {
		return nil, err
	}

	runDir := filepath.Join(r.stateDir, "runs", runID)
	if err := os.MkdirAll(runDir, 0700); err != nil {
		return nil, fmt.Errorf("create run dir: %w", err)
	}

	metadataPath := filepath.Join(runDir, "metadata.yaml")
	if err := os.WriteFile(metadataPath, []byte(fmt.Sprintf("commit: %s\nscenario: %s\n", resolvedCommit, scenario.Name)), 0600); err != nil {
		return nil, fmt.Errorf("write metadata: %w", err)
	}

	eventsPath := filepath.Join(runDir, "events.log")
	if err := os.WriteFile(eventsPath, []byte(""), 0600); err != nil {
		return nil, fmt.Errorf("write events: %w", err)
	}

	// host wall budget
	timeoutDur, _ := time.ParseDuration(scenario.Timeout)
	if timeoutDur == 0 {
		timeoutDur = 10 * time.Minute
	}
	if timeoutDur > maxRunTimeout() {
		timeoutDur = maxRunTimeout()
	}
	runCtx, cancel := context.WithTimeout(context.Background(), timeoutDur)

	r.mu.Lock()
	r.activeRuns[runID] = &runContext{cancel: cancel}
	r.mu.Unlock()

	go r.executeAsync(runCtx, runID, resolvedCommit, scenario)

	return &SubmitResponse{RunID: runID, Plan: plan}, nil
}

func (r *Runner) Abort(ctx context.Context, runID string) error {
	r.mu.Lock()
	rc, exists := r.activeRuns[runID]
	r.mu.Unlock()
	if !exists {
		return ErrNotFound
	}
	rc.cancel()
	return nil
}

type StatusRequest struct {
	RunID  string
	Cursor int64
	WaitMs int64
}

type StatusResponse struct {
	Events []Event
	State  string
}

func (r *Runner) Status(ctx context.Context, req StatusRequest) (*StatusResponse, error) {
	runDir := filepath.Join(r.stateDir, "runs", req.RunID)
	if _, err := os.Stat(runDir); os.IsNotExist(err) {
		return nil, ErrNotFound
	}

	eventsPath := filepath.Join(runDir, "events.log")
	evLog := NewEventLog(eventsPath)

	var events []Event
	var err error
	deadline := time.Now().Add(time.Duration(req.WaitMs) * time.Millisecond)

	for {
		events, err = evLog.ReadSince(req.Cursor)
		if err != nil {
			return nil, fmt.Errorf("read events: %w", err)
		}

		r.mu.Lock()
		_, active := r.activeRuns[req.RunID]
		r.mu.Unlock()

		if len(events) > 0 || !active || req.WaitMs <= 0 || time.Now().After(deadline) {
			state := "running"
			if !active {
				state = "incomplete"
				// Re-read all to find EventRunDone if not active, or just look at all we have?
				// Wait, the cursor might mean we don't see EventRunDone if it happened before the cursor.
				// Better read all to find final state.
				allEvents, _ := evLog.ReadSince(0)
				for _, e := range allEvents {
					if e.Type == EventRunDone {
						state = e.State
					}
				}
			}
			return &StatusResponse{
				Events: events,
				State:  state,
			}, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
			// retry
		}
	}
}
