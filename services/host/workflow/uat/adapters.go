package uat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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
	pixHost  string
	stateDir string
	exec     Exec
	factory  BrowserFactory
}

func NewRealMCP(pixHost, stateDir string, e Exec, factory BrowserFactory) MCP {
	if e == nil {
		e = NewRealExec()
	}
	return &realMCP{pixHost: pixHost, stateDir: stateDir, exec: e, factory: factory}
}

func (m *realMCP) Add(ctx context.Context, name string, argv []string) error {
	cmd := m.exec.CommandContext(ctx, "sbx", argv...)
	return cmd.Run()
}

var readCaptureFileFunc = readCaptureFileNoFollow

func (m *realMCP) Auth(ctx context.Context, runID string, name string) (err error) {
	// Task B: create run-owned bin dir, symlink shim, capture OAuth.
	binDir := filepath.Join(m.stateDir, "runs", runID, "bin")
	if err := os.MkdirAll(binDir, 0700); err != nil {
		return err
	}

	shimPath := filepath.Join(binDir, "pix-uat-browser")
	_ = os.Remove(shimPath)
	if err := os.Symlink(m.pixHost, shimPath); err != nil {
		if err := os.Link(m.pixHost, shimPath); err != nil {
			return fmt.Errorf("failed to create browser shim: %w", err)
		}
	}

	capturePath := filepath.Join(m.stateDir, "runs", runID, "browser_capture")
	_ = os.Remove(capturePath)

	cmdCtx, cancelCmd := context.WithCancel(ctx)
	cmd := m.exec.CommandContext(cmdCtx, "sbx", "mcp", "auth", "--", name)

	// Inherit PATH but set BROWSER and PIX_UAT_BROWSER_CAPTURE
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "BROWSER=") && !strings.HasPrefix(e, "PIX_UAT_BROWSER_CAPTURE=") {
			env = append(env, e)
		}
	}
	env = append(env, "BROWSER="+shimPath)
	env = append(env, "PIX_UAT_BROWSER_CAPTURE="+capturePath)
	cmd.SetEnv(env)

	// Suppress inherited stdio by giving it pipes that we close, or simply wait on it?
	// The task says "asynchronously with inherited stdio suppressed/capped, waits bounded for the capture"
	if errStart := cmd.Start(); errStart != nil {
		cancelCmd()
		return fmt.Errorf("sbx mcp auth start: %w", errStart)
	}

	waitErrCh := make(chan error, 1)
	go func() {
		waitErrCh <- cmd.Wait()
	}()

	var waitErr error
	var waitDone bool
	var returnedFromWait bool

	defer func() {
		if !waitDone {
			cancelCmd()
			waitErr = <-waitErrCh
			waitDone = true
		}
		if returnedFromWait {
			return
		}
		if err != nil {
			if waitErr != nil {
				err = fmt.Errorf("%w (reap status: %v)", err, waitErr)
			}
		} else {
			if waitErr != nil {
				err = fmt.Errorf("sbx mcp auth failed: %w", waitErr)
			}
		}
	}()

	// Wait bounded for the capture
	// We need to wait for `capturePath` to be written.
	captureCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var rawURL string
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case wErr := <-waitErrCh:
			waitErr = wErr
			waitDone = true
			if wErr != nil {
				returnedFromWait = true
				return fmt.Errorf("sbx mcp auth failed before capture: %w", wErr)
			}
			urlStr, err := readCaptureFileFunc(capturePath)
			if err != nil && !os.IsNotExist(err) {
				returnedFromWait = true
				return fmt.Errorf("%w: security or platform error reading capture: %v", ErrIncomplete, err)
			}
			if urlStr != "" {
				rawURL = urlStr
				os.Remove(capturePath)
				break
			}
			returnedFromWait = true
			return fmt.Errorf("%w: sbx exited without writing capture URL", ErrIncomplete)
		case <-captureCtx.Done():
			return fmt.Errorf("%w: browser capture timeout (sbx ignored BROWSER shim - check host bootstrap)", ErrIncomplete)
		case <-ticker.C:
			urlStr, err := readCaptureFileFunc(capturePath)
			if err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("%w: security or platform error reading capture: %v", ErrIncomplete, err)
			}
			if urlStr != "" {
				rawURL = urlStr
				os.Remove(capturePath)
				break
			}
		}
		if rawURL != "" {
			break
		}
	}

	// Parse auth URL and its percent-decoded redirect_uri
	authURL, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid auth URL: %w", err)
	}

	redirectURI := authURL.Query().Get("redirect_uri")
	if redirectURI == "" {
		return fmt.Errorf("no redirect_uri in auth URL")
	}

	callbackURL, err := url.Parse(redirectURI)
	if err != nil {
		return fmt.Errorf("invalid redirect_uri: %w", err)
	}

	// Identify provider through closed registry
	var provider OAuthProvider
	providerFound := false
	for p, hosts := range ProviderAuthHosts {
		for _, host := range hosts {
			if authURL.Hostname() == host {
				provider = p
				providerFound = true
				break
			}
		}
		if providerFound {
			break
		}
	}
	if !providerFound {
		return fmt.Errorf("unknown provider for host: %s", authURL.Host)
	}

	portStr := callbackURL.Port()
	port, _ := strconv.Atoi(portStr)

	// Validates exact OAuth origin and loopback callback/port
	policy := &OAuthPolicy{
		Provider:    provider,
		LeasedPorts: []int{port},
	}
	_, parseErr := policy.Validate(authURL.String())
	if parseErr != nil {
		return fmt.Errorf("auth URL validation failed: %w", parseErr)
	}
	_, parseErr = policy.Validate(callbackURL.String())
	if parseErr != nil {
		return fmt.Errorf("callback URL validation failed: %w", parseErr)
	}

	// Drive NewOAuthContext
	b, parseErr := m.factory.NewOAuthContext(ctx, &ValidatedURL{URL: authURL}, policy)
	if parseErr != nil {
		return fmt.Errorf("new oauth context: %w", parseErr)
	}
	defer b.Close()

	if waitErr := b.WaitForURL(ctx, callbackURL); waitErr != nil {
		return fmt.Errorf("wait for callback: %w", waitErr)
	}

	// Validate returned callback through OAuthPolicy before success
	finalURL, finalErr := b.CurrentURL(ctx)
	if finalErr != nil {
		return fmt.Errorf("get final URL: %w", finalErr)
	}
	if _, valErr := policy.Validate(finalURL.String()); valErr != nil {
		return fmt.Errorf("returned callback policy validation failed: %w", valErr)
	}

	if waitDone {
		return nil
	}

	settleCtx, cancelSettle := context.WithTimeout(ctx, 5*time.Second)
	defer cancelSettle()

	select {
	case wErr := <-waitErrCh:
		waitDone = true
		waitErr = wErr
		if wErr != nil {
			return fmt.Errorf("sbx mcp auth failed during settle: %w", wErr)
		}
		return nil
	case <-settleCtx.Done():
		return fmt.Errorf("%w: sbx mcp auth did not settle in time", ErrIncomplete)
	}
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

type leaseRecord struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

func parseResource(res string) (string, string) {
	if res == "run" {
		return "run", ""
	}
	if strings.HasPrefix(res, "sandbox_") {
		return "sandbox", strings.TrimPrefix(res, "sandbox_")
	}
	if strings.HasPrefix(res, "image_") {
		return "image", strings.TrimPrefix(res, "image_")
	}
	if strings.HasPrefix(res, "template_") {
		return "template", strings.TrimPrefix(res, "template_")
	}
	if strings.HasPrefix(res, "mcp:") {
		return "mcp", strings.TrimPrefix(res, "mcp:")
	}
	return "", ""
}

func hashResource(res string) string {
	h := sha256.Sum256([]byte(res))
	return hex.EncodeToString(h[:])
}

func (l *realLease) Acquire(ctx context.Context, runID string, resource string) error {
	dir := filepath.Join(l.stateDir, "leases", runID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	kind, name := parseResource(resource)
	if kind == "" {
		return fmt.Errorf("unknown lease resource format: %s", resource)
	}

	record := leaseRecord{Kind: kind, Name: name}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}

	path := filepath.Join(dir, hashResource(resource))

	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		if os.IsExist(err) {
			return errors.New("resource already leased")
		}
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

func (l *realLease) Release(ctx context.Context, runID string, resource string) error {
	dir := filepath.Join(l.stateDir, "leases", runID)
	path := filepath.Join(dir, hashResource(resource))
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
		path := filepath.Join(dir, ent.Name())
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			errs = append(errs, fmt.Errorf("read lease %s: %w", ent.Name(), readErr))
			continue
		}
		var record leaseRecord
		if unmarshalErr := json.Unmarshal(data, &record); unmarshalErr != nil {
			errs = append(errs, fmt.Errorf("malformed lease record %s", ent.Name()))
			continue
		}

		var cleanupErr error
		switch record.Kind {
		case "run":
			// bookkeeping-only run marker
		case "mcp":
			cleanupErr = l.exec.CommandContext(ctx, "sbx", "mcp", "rm", "--", record.Name).Run()
		case "sandbox":
			if argv, planErr := sandbox.PlanForceRemove(record.Name); planErr == nil {
				cleanupErr = l.exec.CommandContext(ctx, "sbx", argv...).Run()
			} else {
				cleanupErr = planErr
			}
		case "image":
			tag := record.Name
			if strings.HasPrefix(tag, "uat-") {
				tag = "docker.io/mcavage/pix:" + tag
			}
			cleanupErr = l.exec.CommandContext(ctx, "docker", "image", "rm", "-f", "--", tag).Run()
		case "template":
			cleanupErr = l.exec.CommandContext(ctx, "sbx", "template", "rm", "--", record.Name).Run()
		default:
			errs = append(errs, fmt.Errorf("refusing foreign lease kind: %s", record.Kind))
			continue
		}

		if cleanupErr != nil {
			errs = append(errs, fmt.Errorf("cleanup %s %s: %w", record.Kind, record.Name, cleanupErr))
			continue
		}
		if removeErr := os.Remove(path); removeErr != nil {
			errs = append(errs, removeErr)
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
