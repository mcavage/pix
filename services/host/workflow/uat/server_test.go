package uat_test

import (
	"os"
	"path/filepath"

	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"pix/host/workflow/uat"
)

type mockBrowser struct{}

func (m *mockBrowser) Snapshot(ctx context.Context) (*uat.Snapshot, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (m *mockBrowser) Click(ctx context.Context, selector string) error { return nil }
func (m *mockBrowser) WaitForURL(ctx context.Context, u *url.URL) error { return nil }
func (m *mockBrowser) CurrentURL(ctx context.Context) (*url.URL, error) { return nil, nil }
func (m *mockBrowser) Title(ctx context.Context) (string, error)        { return "", nil }
func (m *mockBrowser) VisibleText(ctx context.Context) (string, error)  { return "", nil }
func (m *mockBrowser) Close() error                                     { return nil }

type mockBrowserFactory struct {
	mu    sync.Mutex
	count int
}

func (m *mockBrowserFactory) NewContext(ctx context.Context, runID string, initialURL *uat.ValidatedURL, policy uat.URLValidator) (uat.Browser, error) {
	m.mu.Lock()
	m.count++
	m.mu.Unlock()
	time.Sleep(50 * time.Millisecond) // artificially slow down browser creation
	return &mockBrowser{}, nil
}
func (m *mockBrowserFactory) NewOAuthContext(ctx context.Context, initialURL *uat.ValidatedURL, policy uat.URLValidator) (uat.Browser, error) {
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

	pixHost := filepath.Join(stateDir, "pix-host")
	os.WriteFile(pixHost, []byte(""), 0755)
	runner, _ := uat.NewRunner(pixHost, "/repo", stateDir, mg, &mockExec{}, slowSb, &mockMCP{}, &mockImage{}, &mockLease{}, 1)

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

func TestMCPServerIsolation(t *testing.T) {
	state1 := t.TempDir()
	state2 := t.TempDir()

	mg := &mockGit{
		readTreeFile: func(ctx context.Context, commit, path string) ([]byte, error) {
			return []byte("schema: pix.uat/1\nname: test\ntimeout: 1m\nsteps:\n  - id: smoke\n    do: check"), nil
		},
	}

	pixHost1 := filepath.Join(state1, "pix-host")
	os.WriteFile(pixHost1, []byte(""), 0755)
	r1, _ := uat.NewRunner(pixHost1, "/repo", state1, mg, &mockExec{}, &mockSandbox{}, &mockMCP{}, &mockImage{}, &mockLease{}, 1)
	pixHost2 := filepath.Join(state2, "pix-host")
	os.WriteFile(pixHost2, []byte(""), 0755)
	r2, _ := uat.NewRunner(pixHost2, "/repo", state2, mg, &mockExec{}, &mockSandbox{}, &mockMCP{}, &mockImage{}, &mockLease{}, 1)

	resp1, _ := r1.Submit(context.Background(), uat.SubmitRequest{Commit: "main", ScenarioPath: "test.yaml", DryRun: false})
	resp2, _ := r2.Submit(context.Background(), uat.SubmitRequest{Commit: "main", ScenarioPath: "test.yaml", DryRun: false})

	inR1, inW1 := io.Pipe()
	outR1, outW1 := io.Pipe()

	bf := &mockBrowserFactory{}
	s1 := uat.NewMCPServer(r1, bf, state1, inR1, outW1, nil)
	go s1.Serve(context.Background())

	scanner1 := bufio.NewScanner(outR1)

	// Try to abort run2 from server1
	reqAbort := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"uat_abort","arguments":{"run_id":%q}}}`, resp2.RunID)
	inW1.Write([]byte(reqAbort + "\n"))

	if !scanner1.Scan() {
		t.Fatalf("expected response")
	}
	resp := scanner1.Text()
	if !strings.Contains(resp, "not found") {
		t.Errorf("expected not found error for abort %q in r1, got %s", resp2.RunID, resp)
	}

	// ensure r1.Submit didn't fail (ignoring resp1 warning otherwise)
	_ = resp1

	// Try to read artifact from run2 via server1
	reqArtifact := fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"uat_artifact","arguments":{"run_id":%q,"path":"events.log"}}}`, resp2.RunID)
	inW1.Write([]byte(reqArtifact + "\n"))

	if !scanner1.Scan() {
		t.Fatalf("expected response")
	}
	resp = scanner1.Text()
	if !strings.Contains(resp, "no such file") && !strings.Contains(resp, "not found") {
		t.Errorf("expected file not found error for artifact %q in r1, got %s", resp2.RunID, resp)
	}
}

func TestMCPServer_EOF_CancelsInFlight(t *testing.T) {
	inReader, inWriter := io.Pipe()
	var out bytes.Buffer

	bf := &mockBrowserFactory{}
	mg := &mockGit{}
	tmpDir := t.TempDir()
	pixHost := filepath.Join(tmpDir, "pix-host")
	os.WriteFile(pixHost, []byte(""), 0755)

	runner, err := uat.NewRunner(pixHost, "/repo", tmpDir, mg, &mockExec{}, &slowSandbox{}, &mockMCP{}, &mockImage{}, &mockLease{}, 1)
	if err != nil {
		t.Fatalf("NewRunner error: %v", err)
	}

	s := uat.NewMCPServer(runner, bf, "", inReader, &out, nil)

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Serve(context.Background())
	}()

	// Send a slow tool call (uat_execute with empty plan should just do the acquire, which takes time in slowSandbox)
	req := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"uat_browser_action","arguments":{"run_id":"run1","action":"snapshot"}}}` + "\n"
	inWriter.Write([]byte(req))

	// Wait a moment for it to start
	time.Sleep(100 * time.Millisecond)

	// Close stdin mid-call
	inWriter.Close()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("expected nil error, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not exit quickly after EOF")
	}

	// The response should be an error because it got canceled
	outStr := out.String()
	if !strings.Contains(strings.ToLower(outStr), "error") || !strings.Contains(outStr, "canceled") {
		t.Errorf("expected cancellation error in response, got: %s", outStr)
	}
}
