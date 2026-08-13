package uat

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type captureExec struct {
	lastArgs []string
}

func (c *captureExec) CommandContext(ctx context.Context, name string, args ...string) ExecCmd {
	c.lastArgs = append([]string{name}, args...)
	return &fakeCmd{}
}

type fakeCmd struct{}

func (f *fakeCmd) Run() error                         { return nil }
func (f *fakeCmd) Start() error                       { return nil }
func (f *fakeCmd) Wait() error                        { return nil }
func (f *fakeCmd) Output() ([]byte, error)            { return nil, nil }
func (f *fakeCmd) StdoutPipe() (io.ReadCloser, error) { return nil, nil }
func (f *fakeCmd) StderrPipe() (io.ReadCloser, error) { return nil, nil }
func (f *fakeCmd) SetEnv(env []string)                {}
func (f *fakeCmd) SetDir(dir string)                  {}

func TestAdapters_LeaseCleanupSandboxRemoveArgv(t *testing.T) {
	ce := &captureExec{}
	stateDir := t.TempDir()

	// Create a run and a sandbox lease
	runID := "testrun123"
	leaseDir := filepath.Join(stateDir, "leases", runID)
	os.MkdirAll(leaseDir, 0700)
	// lease name must be sandbox_pix-uat-<runID>
	os.WriteFile(filepath.Join(leaseDir, "sandbox_pix-uat-"+runID), []byte(""), 0600)

	l := NewRealLease(stateDir, ce)
	err := l.Cleanup(context.Background(), runID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	joined := strings.Join(ce.lastArgs, " ")
	expected := "sbx rm -f pix-uat-" + runID
	if joined != expected {
		t.Errorf("expected %q, got %q", expected, joined)
	}
}

func TestAdapters_GitShowArgv(t *testing.T) {
	ce := &captureExec{}
	g := NewRealGit("/repo", ce)
	_, err := g.ReadTreeFile(context.Background(), "abc1234", "dir/file.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	joined := strings.Join(ce.lastArgs, " ")
	expected := "git -C /repo show --end-of-options abc1234:dir/file.txt"
	if joined != expected {
		t.Errorf("expected %q, got %q", expected, joined)
	}
}

func TestAdapters_MCPAddArgv(t *testing.T) {
	ce := &captureExec{}
	m := NewRealMCP(ce)
	err := m.Add(context.Background(), "my-mcp", []string{"mcp", "add", "my-mcp", "--command", "host"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	joined := strings.Join(ce.lastArgs, " ")
	expected := "sbx mcp add my-mcp --command host"
	if joined != expected {
		t.Errorf("expected %q, got %q", expected, joined)
	}
}

func TestAdapters_Integration(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}

	repoPath := t.TempDir()

	runGit := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v, output: %s", args, err, out)
		}
	}

	runGit(repoPath, "init")
	runGit(repoPath, "config", "user.name", "test")
	runGit(repoPath, "config", "user.email", "test@test.com")

	err := os.WriteFile(filepath.Join(repoPath, "hello.txt"), []byte("world\n"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	runGit(repoPath, "add", "hello.txt")
	runGit(repoPath, "commit", "-m", "init")

	g := NewRealGit(repoPath, nil)

	// Test ResolveCommit(HEAD)
	headSHA, err := g.ResolveCommit(context.Background(), "HEAD")
	if err != nil {
		t.Fatalf("ResolveCommit failed: %v", err)
	}
	if len(headSHA) != 40 {
		t.Fatalf("Expected full SHA, got: %q", headSHA)
	}

	// Test ReadTreeFile
	content, err := g.ReadTreeFile(context.Background(), headSHA, "hello.txt")
	if err != nil {
		t.Fatalf("ReadTreeFile failed: %v", err)
	}
	if string(content) != "world\n" {
		t.Fatalf("Expected 'world\\n', got %q", content)
	}

	// Test Clone
	destPath := t.TempDir()
	os.RemoveAll(destPath)
	err = g.Clone(context.Background(), headSHA, destPath)
	if err != nil {
		t.Fatalf("Clone failed: %v", err)
	}

	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = destPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to get HEAD in cloned repo: %v", err)
	}
	clonedSHA := strings.TrimSpace(string(out))
	if clonedSHA != headSHA {
		t.Fatalf("Expected cloned HEAD to be %q, got %q", headSHA, clonedSHA)
	}

	badRevs := []string{
		"-HEAD",
		"HEAD..main",
		"HEAD@{1}",
		"HEAD ",
		"HEAD\t",
	}
	for _, bad := range badRevs {
		_, err := g.ResolveCommit(context.Background(), bad)
		if err == nil {
			t.Errorf("expected error for bad revision %q", bad)
		}
	}
}
