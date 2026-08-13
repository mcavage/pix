package uat

import (
	"context"
	"io"
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
func (f *fakeCmd) Output() ([]byte, error)            { return nil, nil }
func (f *fakeCmd) StdoutPipe() (io.ReadCloser, error) { return nil, nil }
func (f *fakeCmd) StderrPipe() (io.ReadCloser, error) { return nil, nil }

func TestAdapters_GitShowArgv(t *testing.T) {
	ce := &captureExec{}
	g := NewRealGit("/repo", ce)
	_, err := g.ReadTreeFile(context.Background(), "abc1234", "dir/file.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	joined := strings.Join(ce.lastArgs, " ")
	expected := "git -C /repo show -- abc1234:dir/file.txt"
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
