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

func validateRevision(rev string) error {
	if rev == "" {
		return errors.New("empty revision")
	}
	if strings.HasPrefix(rev, "-") {
		return errors.New("revision cannot start with '-'")
	}
	if strings.Contains(rev, "..") {
		return errors.New("revision cannot contain '..'")
	}
	if strings.Contains(rev, "@{") {
		return errors.New("revision cannot contain reflog syntax '@{'")
	}
	for _, c := range rev {
		if c <= 32 || c == 127 {
			return errors.New("revision cannot contain whitespace or control characters")
		}
	}
	return nil
}

func (g *realGit) ResolveCommit(ctx context.Context, commit string) (string, error) {
	if err := validateRevision(commit); err != nil {
		return "", err
	}
	cmd := g.exec.CommandContext(ctx, "git", "-C", g.repoPath, "rev-parse", "--verify", "--end-of-options", commit+"^{commit}")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolve commit: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (g *realGit) ReadTreeFile(ctx context.Context, commit, path string) ([]byte, error) {
	if err := validateRevision(commit); err != nil {
		return nil, err
	}
	if filepath.IsAbs(path) {
		return nil, errors.New("absolute path not allowed")
	}
	clean := filepath.Clean(path)
	if strings.HasPrefix(clean, "..") || clean == ".." {
		return nil, errors.New("path escapes root")
	}
	target := fmt.Sprintf("%s:%s", commit, clean)
	cmd := g.exec.CommandContext(ctx, "git", "-C", g.repoPath, "show", "--end-of-options", target)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("read tree file: %w", err)
	}
	return out, nil
}

func (g *realGit) Clone(ctx context.Context, commit, destPath string) error {
	if err := validateRevision(commit); err != nil {
		return err
	}
	// local clone
	cmd := g.exec.CommandContext(ctx, "git", "clone", "--no-checkout", "--", g.repoPath, destPath)
	if err := cmd.Run(); err != nil {
		return err
	}
	cmd2 := g.exec.CommandContext(ctx, "git", "-C", destPath, "switch", "--detach", "--end-of-options", commit)
	return cmd2.Run()
}

type realSandbox struct {
	exec Exec
}

func NewRealSandbox(e Exec) Sandbox {
	if e == nil {
		e = NewRealExec()
	}
	return &realSandbox{exec: e}
}

func (s *realSandbox) Create(ctx context.Context, runID string) error {
	cmd := s.exec.CommandContext(ctx, "sbx", "run", "--name", "pix-uat-"+runID, "-d")
	return cmd.Run()
}

func (s *realSandbox) Probe(ctx context.Context, runID string) error {
	cmd := s.exec.CommandContext(ctx, "sbx", "ls", "--format", "json")
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
	cmd := s.exec.CommandContext(ctx, "sbx", argv...)
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
	cmd := m.exec.CommandContext(ctx, "sbx", "mcp", "auth", "--", name)
	return cmd.Run()
}

func (m *realMCP) Status(ctx context.Context, name string) (string, error) {
	cmd := m.exec.CommandContext(ctx, "sbx", "mcp", "status", "--", name)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (m *realMCP) Remove(ctx context.Context, name string) error {
	cmd := m.exec.CommandContext(ctx, "sbx", "mcp", "rm", "--", name)
	return cmd.Run()
}

type realImage struct {
	exec Exec
}

func NewRealImage(e Exec) Image {
	if e == nil {
		e = NewRealExec()
	}
	return &realImage{exec: e}
}

func (i *realImage) Load(ctx context.Context, tag, workspacePath string) error {
	// If workspacePath is a tar file, we load it. (We changed executeCandidateSmoke to pass ImageTar as second arg).
	cmd := i.exec.CommandContext(ctx, "sbx", "template", "load", "--", workspacePath)
	return cmd.Run()
}

func (i *realImage) Probe(ctx context.Context, tag string) error {
	cmd := i.exec.CommandContext(ctx, "docker", "image", "inspect", "--", tag)
	return cmd.Run()
}

type realLease struct {
	stateDir string
	exec     Exec
}

func NewRealLease(stateDir string, e Exec) Lease {
	if e == nil {
		e = NewRealExec()
	}
	return &realLease{stateDir: stateDir, exec: e}
}

func (l *realLease) Acquire(ctx context.Context, runID string, resource string) error {
	dir := filepath.Join(l.stateDir, "leases", runID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	safeRes := strings.ReplaceAll(resource, ":", "_")
	safeRes = strings.ReplaceAll(safeRes, "/", "_")
	path := filepath.Join(dir, safeRes)

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
		if !strings.HasPrefix(res, "mcp_") && !strings.HasPrefix(res, "sandbox_") && !strings.HasPrefix(res, "image_") {
			errs = append(errs, fmt.Errorf("refusing foreign lease name: %s", res))
			continue
		}

		var cleanupErr error
		if strings.HasPrefix(res, "mcp_") {
			name := strings.TrimPrefix(res, "mcp_")
			cleanupErr = l.exec.CommandContext(ctx, "sbx", "mcp", "rm", "--", name).Run()
		} else if strings.HasPrefix(res, "sandbox_") {
			name := strings.TrimPrefix(res, "sandbox_")
			if argv, planErr := sandbox.PlanForceRemove(name); planErr == nil {
				cleanupErr = l.exec.CommandContext(ctx, "sbx", argv...).Run()
			} else {
				cleanupErr = planErr
			}
		} else if strings.HasPrefix(res, "image_") {
			tag := strings.TrimPrefix(res, "image_")
			if strings.HasPrefix(tag, "uat-") {
				tag = "docker.io/mcavage/pix:" + tag
			} else {
				// Revert standard replacing if needed, but for now we just handle uat- prefix
				tag = strings.ReplaceAll(tag, "_", "/") // Best effort for others
			}
			cleanupErr = l.exec.CommandContext(ctx, "docker", "image", "rm", "-f", "--", tag).Run()
		}

		if cleanupErr != nil {
			errs = append(errs, fmt.Errorf("failed to cleanup %s: %w", res, cleanupErr))
			continue
		}
		if err := os.Remove(filepath.Join(dir, res)); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("cleanup errors: %v", errs)
	}

	tempProfileDir := filepath.Join(l.stateDir, "uat", "browser", "temp", runID)
	_ = os.RemoveAll(tempProfileDir)

	return os.Remove(dir)
}

type realExec struct{}

func NewRealExec() Exec { return &realExec{} }

func (e *realExec) CommandContext(ctx context.Context, name string, args ...string) ExecCmd {
	return &realExecCmd{Cmd: exec.CommandContext(ctx, name, args...)}
}

type realExecCmd struct {
	*exec.Cmd
}

func (c *realExecCmd) SetEnv(env []string) {
	c.Cmd.Env = env
}

func (c *realExecCmd) SetDir(dir string) {
	c.Cmd.Dir = dir
}
