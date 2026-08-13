package uat_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sync"
	"testing"
	"time"

	"pix/host/workflow/uat"
)

type mockBrowser struct{}

func (m *mockBrowser) Snapshot(ctx context.Context) (*uat.Snapshot, error) { return nil, nil }
func (m *mockBrowser) Click(ctx context.Context, selector string) error    { return nil }
func (m *mockBrowser) WaitForURL(ctx context.Context, u *url.URL) error    { return nil }
func (m *mockBrowser) CurrentURL(ctx context.Context) (*url.URL, error)    { return nil, nil }
func (m *mockBrowser) Title(ctx context.Context) (string, error)           { return "", nil }
func (m *mockBrowser) VisibleText(ctx context.Context) (string, error)     { return "", nil }
func (m *mockBrowser) Close() error                                        { return nil }

type mockBrowserFactory struct {
	mu    sync.Mutex
	count int
}

func (m *mockBrowserFactory) NewContext(ctx context.Context, runID string, url *url.URL, policy *uat.URLPolicy) (uat.Browser, error) {
	m.mu.Lock()
	m.count++
	m.mu.Unlock()
	time.Sleep(50 * time.Millisecond) // artificially slow down browser creation
	return &mockBrowser{}, nil
}
func (m *mockBrowserFactory) NewOAuthContext(ctx context.Context, url *url.URL, policy *uat.URLPolicy) (uat.Browser, error) {
	return &mockBrowser{}, nil
}

type slowSandbox struct {
	mockSandbox
}

func (s *slowSandbox) Probe(ctx context.Context, runID string) error {
	time.Sleep(2 * time.Second)
	return nil
}

func TestMCPServerConcurrency(t *testing.T) {
	stateDir := t.TempDir()

	mg := &mockGit{
		readTreeFile: func(ctx context.Context, commit, path string) ([]byte, error) {
			return []byte("schema: pix.uat/1\nname: test\ntimeout: 1m\nsteps:\n  - id: smoke\n    do: candidate_smoke"), nil
		},
	}

	slowSb := &slowSandbox{}

	runner := uat.NewRunner("/repo", stateDir, mg, &mockExec{}, slowSb, &mockMCP{}, &mockImage{}, &mockLease{}, 1)

	resp, _ := runner.Submit(context.Background(), uat.SubmitRequest{
		Commit:       "main",
		ScenarioPath: "test.yaml",
		DryRun:       false,
	})
	runID := resp.RunID

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	bf := &mockBrowserFactory{}
	server := uat.NewMCPServer(runner, bf, stateDir, inR, outW, nil)

	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	go server.Serve(context.Background())

	// Scanner to read responses
	scanner := bufio.NewScanner(outR)
	var mu sync.Mutex
	responses := make(map[interface{}]string)

	go func() {
		for scanner.Scan() {
			var resp map[string]interface{}
			if err := json.Unmarshal(scanner.Bytes(), &resp); err == nil {
				if id, ok := resp["id"].(float64); ok {
					mu.Lock()
					responses[id] = string(scanner.Bytes())
					mu.Unlock()
				}
			}
		}
	}()

	// 1. Send long-poll status request (should block for 2 seconds)
	reqStatus := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"uat_status","arguments":{"run_id":%q,"cursor":1,"wait_ms":2000}}}`, runID)
	inW.Write([]byte(reqStatus + "\n"))

	// Ensure status request started processing
	time.Sleep(100 * time.Millisecond)

	// 2. Send concurrent abort request
	reqAbort := fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"uat_abort","arguments":{"run_id":%q}}}`, runID)

	// 3. Send concurrent capabilities (tools/list) request
	reqList := `{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}`

	// 4. Send concurrent browser creation requests
	reqBrowser1 := fmt.Sprintf(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"uat_browser_action","arguments":{"run_id":%q,"action":"click","ref":"btn"}}}`, runID)
	reqBrowser2 := fmt.Sprintf(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"uat_browser_action","arguments":{"run_id":%q,"action":"click","ref":"btn2"}}}`, runID)

	inW.Write([]byte(reqAbort + "\n"))
	inW.Write([]byte(reqList + "\n"))
	inW.Write([]byte(reqBrowser1 + "\n"))
	inW.Write([]byte(reqBrowser2 + "\n"))

	// Wait a bit, abort and list should return fast
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	_, hasAbort := responses[float64(2)]
	_, hasList := responses[float64(3)]
	statusResp, hasStatus := responses[float64(1)]
	mu.Unlock()

	if !hasAbort {
		t.Errorf("abort blocked by long poll")
	}
	if !hasList {
		t.Errorf("list blocked by long poll")
	}
	if hasStatus {
		t.Errorf("status returned too early: %s", statusResp)
	}

	// Wait for browser actions
	time.Sleep(500 * time.Millisecond)

	// The browser factory should have been called exactly once per run ID
	bf.mu.Lock()
	if bf.count != 1 {
		t.Errorf("expected 1 browser context creation, got %d", bf.count)
	}
	bf.mu.Unlock()
}
