package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"pix/host/workflow/uat"
)

type fakeGit struct{}

func (f *fakeGit) ResolveCommit(ctx context.Context, commit string) (string, error) {
	return commit, nil
}
func (f *fakeGit) ReadTreeFile(ctx context.Context, commit, path string) ([]byte, error) {
	return []byte("test"), nil
}
func (f *fakeGit) Clone(ctx context.Context, commit, destPath string) error { return nil }

type fakeSandbox struct{}

func (f *fakeSandbox) Create(ctx context.Context, runID string) error { return nil }
func (f *fakeSandbox) Probe(ctx context.Context, runID string) error  { return nil }
func (f *fakeSandbox) Remove(ctx context.Context, runID string) error { return nil }

type fakeMCP struct{}

func (f *fakeMCP) Add(ctx context.Context, name string, argv []string) error { return nil }
func (f *fakeMCP) Auth(ctx context.Context, runID string, name string) error { return nil }
func (f *fakeMCP) Status(ctx context.Context, name string) (string, error)   { return "ready", nil }
func (f *fakeMCP) Remove(ctx context.Context, name string) error             { return nil }

type fakeImage struct{}

func (f *fakeImage) Load(ctx context.Context, tag, workspacePath string) error { return nil }
func (f *fakeImage) Probe(ctx context.Context, tag string) error               { return nil }

type fakeLease struct{}

func (f *fakeLease) Acquire(ctx context.Context, runID, resource string) error { return nil }
func (f *fakeLease) Release(ctx context.Context, runID, resource string) error { return nil }
func (f *fakeLease) Cleanup(ctx context.Context, runID string) error           { return nil }

type fakeExec struct{}

func (f *fakeExec) CommandContext(ctx context.Context, name string, args ...string) uat.ExecCmd {
	return nil
} // Might panic if ExecCmd is called

type fakeBrowser struct{}

func (f *fakeBrowser) Snapshot(ctx context.Context) (*uat.Snapshot, error) {
	return &uat.Snapshot{DOM: "<html></html>"}, nil
}
func (f *fakeBrowser) Click(ctx context.Context, refID string) error    { return nil }
func (f *fakeBrowser) WaitForURL(ctx context.Context, u *url.URL) error { return nil }
func (f *fakeBrowser) CurrentURL(ctx context.Context) (*url.URL, error) {
	return url.Parse("about:blank")
}
func (f *fakeBrowser) Title(ctx context.Context) (string, error)       { return "Title", nil }
func (f *fakeBrowser) VisibleText(ctx context.Context) (string, error) { return "text", nil }
func (f *fakeBrowser) Close() error                                    { return nil }

type fakeFactory struct{}

func (f *fakeFactory) NewContext(ctx context.Context, runID string, initialURL *uat.ValidatedURL, policy uat.URLValidator) (uat.Browser, error) {
	return &fakeBrowser{}, nil
}
func (f *fakeFactory) NewOAuthContext(ctx context.Context, initialURL *uat.ValidatedURL, policy uat.URLValidator) (uat.Browser, error) {
	return &fakeBrowser{}, nil
}

func TestMCPServer_EndToEnd(t *testing.T) {
	stateDir := t.TempDir()
	repoDir := t.TempDir()

	pixHost := filepath.Join(stateDir, "pix-host")
	os.WriteFile(pixHost, []byte(""), 0755)
	runner, _ := uat.NewRunner(pixHost, repoDir, stateDir, &fakeGit{}, &fakeExec{}, &fakeSandbox{}, &fakeMCP{}, &fakeImage{}, &fakeLease{}, 1)
	bf := &fakeFactory{}

	runID := "run-12345"
	runDir := filepath.Join(stateDir, "runs", runID)
	os.MkdirAll(runDir, 0755)
	os.WriteFile(filepath.Join(runDir, "artifact.txt"), []byte("hello world"), 0644)

	tests := []struct {
		name         string
		req          string
		expectSubstr string
	}{
		{
			"initialize",
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
			`"protocolVersion"`,
		},
		{
			"notification_silence",
			`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
			``, // no output expected
		},
		{
			"tools_list",
			`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
			`"uat_capabilities"`,
		},
		{
			"tool_capabilities",
			`{"jsonrpc":"2.0","id":"abc","method":"tools/call","params":{"name":"uat_capabilities","arguments":{}}}`,
			`\"scenario_schema\":\"pix.uat/1\"`,
		},
		{
			"tool_artifact",
			`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"uat_artifact","arguments":{"run_id":"run-12345","path":"artifact.txt"}}}`,
			`hello world`,
		},
		{
			"unknown_method",
			`{"jsonrpc":"2.0","id":5,"method":"unknown_method"}`,
			`"error":{"code":-32601`,
		},
		{
			"unknown_tool",
			`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"unknown_tool","arguments":{}}}`,
			`"error":{"code":-32601`,
		},
		{
			"malformed",
			`{invalid json`,
			`"error":{"code":-32700`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := bytes.NewBufferString(tt.req + "\n")
			out := &safeBuffer{}
			server := uat.NewMCPServer(runner, bf, stateDir, in, out, nil)

			err := server.Serve(context.Background())
			if err != nil {
				t.Fatalf("Serve error: %v", err)
			}

			var res string
			for i := 0; i < 50; i++ {
				res = out.String()
				if tt.expectSubstr == "" || strings.Contains(res, tt.expectSubstr) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if tt.expectSubstr == "" {
				if res != "" {
					t.Errorf("expected no output, got %q", res)
				}
			} else {
				if !strings.Contains(res, tt.expectSubstr) {
					t.Errorf("expected %q in output, got %q", tt.expectSubstr, res)
				}

				// Protocol purity: should be valid JSON
				if res != "" {
					for _, line := range strings.Split(strings.TrimSpace(res), "\n") {
						var m map[string]interface{}
						if err := json.Unmarshal([]byte(line), &m); err != nil {
							t.Errorf("stdout not valid JSON: %v, line: %s", err, line)
						}
					}
				}
			}
		})
	}
}

type safeBuffer struct {
	bytes.Buffer
	sync.Mutex
}

func (s *safeBuffer) Write(p []byte) (n int, err error) {
	s.Lock()
	defer s.Unlock()
	return s.Buffer.Write(p)
}
func (s *safeBuffer) String() string {
	s.Lock()
	defer s.Unlock()
	return s.Buffer.String()
}
