package uat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"pix/host/sandbox"
)

type realGit struct {
	repoPath string
	exec     Exec
}

func NewRealGit(repoPath string, e Exec) Git {
	if e == nil {
		e = NewRealExec()
	}
	return &realGit{repoPath: repoPath, exec: e}
}

func (g *realGit) ResolveCommit(ctx context.Context, commit string) (string, error) {
	cmd := g.exec.CommandContext(ctx, "git", "-C", g.repoPath, "rev-parse", "--verify", commit+"^{commit}")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolve commit: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (g *realGit) ReadTreeFile(ctx context.Context, commit, path string) ([]byte, error) {
	if filepath.IsAbs(path) {
		return nil, errors.New("absolute path not allowed")
	}
	clean := filepath.Clean(path)
	if strings.HasPrefix(clean, "..") || clean == ".." {
		return nil, errors.New("path escapes root")
	}
	target := fmt.Sprintf("%s:%s", commit, clean)
	cmd := g.exec.CommandContext(ctx, "git", "-C", g.repoPath, "show", target)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("read tree file: %w", err)
	}
	return out, nil
}

func (g *realGit) Clone(ctx context.Context, commit, destPath string) error {
	// local clone
	cmd := g.exec.CommandContext(ctx, "git", "clone", "--no-checkout", "--", g.repoPath, destPath)
	if err := cmd.Run(); err != nil {
		return err
	}
	cmd2 := g.exec.CommandContext(ctx, "git", "-C", destPath, "checkout", commit)
	return cmd2.Run()
}

type realSandbox struct{}

func NewRealSandbox() Sandbox { return &realSandbox{} }

func (s *realSandbox) Create(ctx context.Context, runID string) error {
	cmd := exec.CommandContext(ctx, "sbx", "run", "--name", "pix-uat-"+runID, "-d")
	return cmd.Run()
}

func (s *realSandbox) Probe(ctx context.Context, runID string) error {
	cmd := exec.CommandContext(ctx, "sbx", "ls", "--format", "json")
	out, err := cmd.Output()
	if err != nil {
		return err
	}
	if !strings.Contains(string(out), "pix-uat-"+runID) {
		return errors.New("incomplete")
	}
	return nil
}

func (s *realSandbox) Remove(ctx context.Context, runID string) error {
	argv, err := sandbox.PlanForceRemove("pix-uat-" + runID)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "sbx", argv...)
	return cmd.Run()
}

type realMCP struct {
	exec Exec
}

func NewRealMCP(e Exec) MCP {
	if e == nil {
		e = NewRealExec()
	}
	return &realMCP{exec: e}
}

func (m *realMCP) Add(ctx context.Context, name string, argv []string) error {
	cmd := m.exec.CommandContext(ctx, "sbx", argv...)
	return cmd.Run()
}

func (m *realMCP) Auth(ctx context.Context, name string) error {
	cmd := m.exec.CommandContext(ctx, "sbx", "mcp", "auth", name)
	return cmd.Run()
}

func (m *realMCP) Status(ctx context.Context, name string) (string, error) {
	cmd := m.exec.CommandContext(ctx, "sbx", "mcp", "status", name)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (m *realMCP) Remove(ctx context.Context, name string) error {
	cmd := m.exec.CommandContext(ctx, "sbx", "mcp", "rm", name)
	return cmd.Run()
}

type realImage struct{}

func NewRealImage() Image { return &realImage{} }

func (i *realImage) Load(ctx context.Context, tag, workspacePath string) error {
	cmd := exec.CommandContext(ctx, "sbx", "template", "load", tag, workspacePath)
	return cmd.Run()
}

func (i *realImage) Probe(ctx context.Context, tag string) error {
	cmd := exec.CommandContext(ctx, "docker", "image", "inspect", tag)
	return cmd.Run()
}

type realLease struct {
	stateDir string
}

func NewRealLease(stateDir string) Lease { return &realLease{stateDir: stateDir} }

func (l *realLease) Acquire(ctx context.Context, runID string, resource string) error {
	dir := filepath.Join(l.stateDir, "leases", runID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	// Sanitize resource name
	safeRes := strings.ReplaceAll(resource, ":", "_")
	safeRes = strings.ReplaceAll(safeRes, "/", "_")
	path := filepath.Join(dir, safeRes)

	// Create the file with 0600
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		if os.IsExist(err) {
			return errors.New("resource already leased")
		}
		return err
	}
	f.Close()
	return nil
}

func (l *realLease) Release(ctx context.Context, runID string, resource string) error {
	dir := filepath.Join(l.stateDir, "leases", runID)
	safeRes := strings.ReplaceAll(resource, ":", "_")
	safeRes = strings.ReplaceAll(safeRes, "/", "_")
	path := filepath.Join(dir, safeRes)
	return os.Remove(path)
}

func (l *realLease) Cleanup(ctx context.Context, runID string) error {
	dir := filepath.Join(l.stateDir, "leases", runID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var errs []error
	for _, ent := range entries {
		res := ent.Name()
		if !strings.HasPrefix(res, "mcp_") && !strings.HasPrefix(res, "sandbox_") {
			errs = append(errs, fmt.Errorf("refusing foreign lease name: %s", res))
			continue
		}

		var cleanupErr error
		if strings.HasPrefix(res, "mcp_") {
			name := strings.TrimPrefix(res, "mcp_")
			cleanupErr = exec.CommandContext(ctx, "sbx", "mcp", "rm", name).Run()
		} else if strings.HasPrefix(res, "sandbox_") {
			name := strings.TrimPrefix(res, "sandbox_")
			if argv, planErr := sandbox.PlanForceRemove(name); planErr == nil {
				cleanupErr = exec.CommandContext(ctx, "sbx", argv...).Run()
			} else {
				cleanupErr = planErr
			}
		}

		if cleanupErr != nil {
			errs = append(errs, fmt.Errorf("failed to cleanup %s: %w", res, cleanupErr))
			continue
		}
		// Confirm absence could be checked but here we just remove the lease file if cleanup succeeded
		if err := os.Remove(filepath.Join(dir, res)); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("cleanup errors: %v", errs)
	}

	return os.Remove(dir)
}

type realExec struct{}

func NewRealExec() Exec { return &realExec{} }

func (e *realExec) CommandContext(ctx context.Context, name string, args ...string) ExecCmd {
	return exec.CommandContext(ctx, name, args...)
}
