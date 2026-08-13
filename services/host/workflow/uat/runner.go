package uat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	uattypes "pix/host/uat"
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

	buildSem chan struct{}
}

func (r *Runner) RetryCleanups() map[string]string {
	report := make(map[string]string)
	entries, err := os.ReadDir(filepath.Join(r.stateDir, "leases"))
	if err != nil {
		return report
	}
	for _, entry := range entries {
		if entry.IsDir() {
			runID := entry.Name()
			if err := r.lease.Cleanup(context.Background(), runID); err != nil {
				report[runID] = err.Error()
			} else {
				report[runID] = "success"
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

	return &Runner{
		pixHost:    pixHost,
		repoPath:   repoPath,
		stateDir:   stateDir,
		git:        git,
		exec:       exec,
		sandbox:    sandbox,
		mcp:        mcp,
		image:      image,
		lease:      lease,
		activeRuns: make(map[string]*runContext),
		buildSem:   make(chan struct{}, buildConcurrency),
	}, nil
}

type SubmitRequest struct {
	Commit       string
	ScenarioPath string
	DryRun       bool
}

type SubmitResponse struct {
	RunID string
	Plan  string
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
		return &SubmitResponse{RunID: "", Plan: scenario.Name}, nil
	}

	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("generate id: %w", err)
	}
	runID := fmt.Sprintf("run-%s-%s", time.Now().Format("20060102-150405"), hex.EncodeToString(b))

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
	if timeoutDur > 60*time.Minute {
		timeoutDur = 60 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(context.Background(), timeoutDur)

	r.mu.Lock()
	r.activeRuns[runID] = &runContext{cancel: cancel}
	r.mu.Unlock()

	go r.executeAsync(runCtx, runID, resolvedCommit, scenario)

	return &SubmitResponse{RunID: runID, Plan: scenario.Name}, nil
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
