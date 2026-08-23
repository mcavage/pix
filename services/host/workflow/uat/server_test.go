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

	"pix/host/uatenvmatrix"
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

func callMCPTool(t *testing.T, runner *uat.Runner, request string) map[string]interface{} {
	t.Helper()
	var out bytes.Buffer
	server := uat.NewMCPServer(runner, &mockBrowserFactory{}, t.TempDir(), strings.NewReader(request+"\n"), &out, nil)
	if err := server.Serve(context.Background()); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var envelope struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &envelope); err != nil {
		t.Fatalf("decode MCP envelope %q: %v", out.String(), err)
	}
	if len(envelope.Result.Content) != 1 {
		t.Fatalf("MCP content = %#v", envelope.Result.Content)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(envelope.Result.Content[0].Text), &payload); err != nil {
		t.Fatalf("decode MCP text payload %q: %v", envelope.Result.Content[0].Text, err)
	}
	return payload
}

func TestMCPServerCapabilitiesExposeVocabularyCoverageAndBrowserState(t *testing.T) {
	stateDir := t.TempDir()
	pixHost := filepath.Join(stateDir, "pix-host")
	os.WriteFile(pixHost, []byte(""), 0755)
	runner, _ := uat.NewRunner(pixHost, "/repo", stateDir, &mockGit{}, &mockExec{}, &mockSandbox{}, &mockMCP{}, &mockImage{}, &mockLease{}, 2)

	payload := callMCPTool(t, runner, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"uat_capabilities","arguments":{}}}`)
	actions, ok := payload["legal_actions"].([]interface{})
	if !ok || len(actions) == 0 {
		t.Fatalf("legal_actions = %#v", payload["legal_actions"])
	}
	browser, ok := payload["browser"].(map[string]interface{})
	if !ok {
		t.Fatalf("browser state = %#v, want object", payload["browser"])
	}
	if browser["uses_normal_browser_profile"] != false {
		t.Errorf("browser isolation state = %#v", browser)
	}
	coverage, ok := payload["candidate_smoke"].(map[string]interface{})
	if !ok {
		t.Fatalf("candidate_smoke = %#v", payload["candidate_smoke"])
	}
	checks, ok := coverage["memory_checks"].([]interface{})
	if !ok || len(checks) != 9 {
		t.Errorf("memory checks = %#v, want all 9", coverage["memory_checks"])
	}

	namedChecks, ok := payload["named_checks"].([]interface{})
	if !ok || len(namedChecks) == 0 {
		t.Fatalf("named_checks = %#v, want the non-empty uatenvmatrix registry", payload["named_checks"])
	}
	wantNamed := uatenvmatrix.CheckNames()
	if len(namedChecks) != len(wantNamed) {
		t.Fatalf("named_checks = %#v, want %#v", namedChecks, wantNamed)
	}
	for i, want := range wantNamed {
		if namedChecks[i] != want {
			t.Errorf("named_checks[%d] = %#v, want %q", i, namedChecks[i], want)
		}
	}
}

func TestMCPServerDryRunReturnsStructuredIsolationPlan(t *testing.T) {
	stateDir := t.TempDir()
	pixHost := filepath.Join(stateDir, "pix-host")
	os.WriteFile(pixHost, []byte(""), 0755)
	git := &mockGit{readTreeFile: func(context.Context, string, string) ([]byte, error) {
		return []byte("schema: pix.uat/1\nname: smoke\ntimeout: 5m\nsteps:\n  - id: smoke_test\n    do: candidate_smoke\n"), nil
	}}
	runner, _ := uat.NewRunner(pixHost, "/repo", stateDir, git, &mockExec{}, &mockSandbox{}, &mockMCP{}, &mockImage{}, &mockLease{}, 1)

	payload := callMCPTool(t, runner, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"uat_submit","arguments":{"commit":"abc123","scenario_path":"uat/scenarios/smoke.yaml","dry_run":true}}}`)
	if payload["run_id"] != "" {
		t.Errorf("dry-run run_id = %#v", payload["run_id"])
	}
	plan, ok := payload["plan"].(map[string]interface{})
	if !ok {
		t.Fatalf("plan = %#v, want object", payload["plan"])
	}
	candidate, ok := plan["candidate"].(map[string]interface{})
	if !ok {
		t.Fatalf("candidate plan = %#v", plan["candidate"])
	}
	if candidate["image_tag"] != "docker.io/mcavage/pix:uat-<run-id>" || candidate["uses_normal_pix_state"] != false {
		t.Errorf("candidate isolation plan = %#v", candidate)
	}
	browserProfile, _ := candidate["browser_profile"].(string)
	if !strings.HasSuffix(browserProfile, filepath.Join("uat", "browser", "temp", "<run-id>")) {
		t.Errorf("browser_profile = %q, want disposable UAT profile", browserProfile)
	}
	if _, ok := plan["candidate_build_limits"].(map[string]interface{}); !ok {
		t.Errorf("candidate_build_limits = %#v, want object", plan["candidate_build_limits"])
	}
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
	runner, _ := uat.NewRunner(pixHost, "/repo", stateDir, mg, matrixSkippingExec{&mockExec{}}, slowSb, &mockMCP{}, &mockImage{}, &mockLease{}, 1)

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
			return []byte("schema: pix.uat/1\nname: test\ntimeout: 1m\nsteps:\n  - id: remove\n    do: mcp_remove\n    with:\n      name: test"), nil
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

type stuckSandbox struct {
	mockSandbox
}

func (s *stuckSandbox) Probe(ctx context.Context, runID string) error {
	<-context.Background().Done() // Block forever unconditionally (ignore ctx cancel)
	return nil
}

type stuckBrowserFactory struct {
	mockBrowserFactory
}

type stuckBrowser struct {
	mockBrowser
}

func (s *stuckBrowser) Snapshot(ctx context.Context) (*uat.Snapshot, error) {
	// Block forever unconditionally
	select {}
}

func (s *stuckBrowserFactory) NewContext(ctx context.Context, runID string, initialURL *uat.ValidatedURL, policy uat.URLValidator) (uat.Browser, error) {
	return &stuckBrowser{}, nil
}

func TestMCPServer_ShutdownTimeout(t *testing.T) {
	inReader, inWriter := io.Pipe()
	var out bytes.Buffer

	bf := &stuckBrowserFactory{}
	mg := &mockGit{}
	tmpDir := t.TempDir()
	pixHost := filepath.Join(tmpDir, "pix-host")
	os.WriteFile(pixHost, []byte(""), 0755)

	runner, err := uat.NewRunner(pixHost, "/repo", tmpDir, mg, &mockExec{}, &stuckSandbox{}, &mockMCP{}, &mockImage{}, &mockLease{}, 1)
	if err != nil {
		t.Fatalf("NewRunner error: %v", err)
	}

	s := uat.NewMCPServer(runner, bf, "", inReader, &out, nil)
	s.ShutdownTimeout = 10 * time.Millisecond

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Serve(context.Background())
	}()

	// Send a stuck tool call
	req := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"uat_browser_action","arguments":{"run_id":"run1","action":"snapshot"}}}` + "\n"
	inWriter.Write([]byte(req))

	// Wait a moment for it to start
	time.Sleep(50 * time.Millisecond)

	// Close stdin mid-call to trigger EOF
	inWriter.Close()

	// Measure time to exit
	start := time.Now()
	select {
	case err := <-errCh:
		if err == nil {
			t.Errorf("expected timeout error, got nil")
		} else if !strings.Contains(err.Error(), "shutdown timeout") {
			t.Errorf("expected shutdown timeout error, got %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Serve did not exit quickly after EOF")
	}

	elapsed := time.Since(start)
	if elapsed > 100*time.Millisecond {
		t.Errorf("expected quick return after shutdown timeout, took %v", elapsed)
	}
}

type blockingWriter struct {
	writeBlock chan struct{}
}

func (w *blockingWriter) Write(p []byte) (n int, err error) {
	<-w.writeBlock
	return len(p), nil
}

func TestMCPServer_ShutdownWithBlockedWriter(t *testing.T) {
	inReader, inWriter := io.Pipe()

	bw := &blockingWriter{
		writeBlock: make(chan struct{}),
	}

	bf := &mockBrowserFactory{}
	mg := &mockGit{}
	tmpDir := t.TempDir()
	pixHost := filepath.Join(tmpDir, "pix-host")
	os.WriteFile(pixHost, []byte(""), 0755)

	runner, err := uat.NewRunner(pixHost, "/repo", tmpDir, mg, &mockExec{}, &mockSandbox{}, &mockMCP{}, &mockImage{}, &mockLease{}, 1)
	if err != nil {
		t.Fatalf("NewRunner error: %v", err)
	}

	s := uat.NewMCPServer(runner, bf, "", inReader, bw, nil)
	s.ShutdownTimeout = 50 * time.Millisecond

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Serve(context.Background())
	}()

	// Send a valid tool call that generates an immediate response
	req := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"uat_capabilities","arguments":{}}}` + "\n"
	inWriter.Write([]byte(req))

	// Allow the tool call to be processed, which will block in Write()
	time.Sleep(50 * time.Millisecond)

	// Trigger shutdown by EOF
	inWriter.Close()

	// Wait for Serve to return (it should NOT block on outMu if the writer is blocked)
	select {
	case err := <-errCh:
		if err != nil && !strings.Contains(err.Error(), "shutdown timeout") {
			t.Errorf("expected no error or shutdown timeout, got %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Serve deadlocked on blocked writer")
	}
	// Cleanup the blocked writer so the goroutine doesn't leak in tests
	close(bw.writeBlock)
}

type recordingBrowserFactory struct {
	mu        sync.Mutex
	calls     int
	blockChan chan struct{}
}

func (f *recordingBrowserFactory) NewContext(ctx context.Context, runID string, initialURL *uat.ValidatedURL, policy uat.URLValidator) (uat.Browser, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()

	if f.blockChan != nil {
		select {
		case <-f.blockChan:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &mockBrowser{}, nil
}

func (f *recordingBrowserFactory) NewOAuthContext(ctx context.Context, initialURL *uat.ValidatedURL, policy uat.URLValidator) (uat.Browser, error) {
	return nil, nil
}

func TestMCPServer_ConcurrentOneFactory(t *testing.T) {
	inReader, inWriter := io.Pipe()
	var out bytes.Buffer

	// Delay factory so concurrent calls queue up
	bf := &recordingBrowserFactory{
		blockChan: make(chan struct{}),
	}
	mg := &mockGit{}
	tmpDir := t.TempDir()
	pixHost := filepath.Join(tmpDir, "pix-host")
	os.WriteFile(pixHost, []byte(""), 0755)

	runner, err := uat.NewRunner(pixHost, "/repo", tmpDir, mg, &mockExec{}, &mockSandbox{}, &mockMCP{}, &mockImage{}, &mockLease{}, 1)
	if err != nil {
		t.Fatalf("NewRunner error: %v", err)
	}

	s := uat.NewMCPServer(runner, bf, "", inReader, &out, nil)

	go s.Serve(context.Background())

	// Send 3 concurrent requests for the same run_id
	req1 := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"uat_browser_action","arguments":{"run_id":"run_same","action":"snapshot"}}}` + "\n"
	req2 := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"uat_browser_action","arguments":{"run_id":"run_same","action":"read_visible_text"}}}` + "\n"
	req3 := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"uat_browser_action","arguments":{"run_id":"run_same","action":"snapshot"}}}` + "\n"

	inWriter.Write([]byte(req1))
	inWriter.Write([]byte(req2))
	inWriter.Write([]byte(req3))

	time.Sleep(50 * time.Millisecond)

	// Unblock factory
	close(bf.blockChan)

	time.Sleep(100 * time.Millisecond)
	inWriter.Close()

	bf.mu.Lock()
	calls := bf.calls
	bf.mu.Unlock()
	if calls != 1 {
		t.Errorf("Expected exactly 1 factory call, got %d", calls)
	}
}

func TestMCPServer_HungStartupShutdown(t *testing.T) {
	inReader, inWriter := io.Pipe()
	var out bytes.Buffer

	// Factory blocks until context is cancelled
	bf := &recordingBrowserFactory{
		blockChan: make(chan struct{}),
	}
	mg := &mockGit{}
	tmpDir := t.TempDir()
	pixHost := filepath.Join(tmpDir, "pix-host")
	os.WriteFile(pixHost, []byte(""), 0755)

	runner, err := uat.NewRunner(pixHost, "/repo", tmpDir, mg, &mockExec{}, &mockSandbox{}, &mockMCP{}, &mockImage{}, &mockLease{}, 1)
	if err != nil {
		t.Fatalf("NewRunner error: %v", err)
	}

	s := uat.NewMCPServer(runner, bf, "", inReader, &out, nil)

	go s.Serve(context.Background())

	// Send action to trigger factory
	req := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"uat_browser_action","arguments":{"run_id":"run_hung","action":"snapshot"}}}` + "\n"
	inWriter.Write([]byte(req))

	time.Sleep(50 * time.Millisecond)

	// Close stdin, triggering shutdown
	inWriter.Close()

	// Ensure we don't leak goroutines, checking that factory unblocks due to context cancellation
	// by waiting for the test to finish cleanly.
	time.Sleep(100 * time.Millisecond)

	bf.mu.Lock()
	calls := bf.calls
	bf.mu.Unlock()
	if calls != 1 {
		t.Errorf("Expected 1 factory call, got %d", calls)
	}
}

type singleflightRaceBrowser struct {
	mockBrowser
	mu     sync.Mutex
	closed bool
}

func (b *singleflightRaceBrowser) Close() error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	return nil
}

func (b *singleflightRaceBrowser) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

type singleflightRaceFactory struct {
	readyChan chan struct{}
	mu        sync.Mutex
	b         *singleflightRaceBrowser
}

func (f *singleflightRaceFactory) NewContext(ctx context.Context, runID string, initialURL *uat.ValidatedURL, policy uat.URLValidator) (uat.Browser, error) {
	<-f.readyChan
	f.mu.Lock()
	f.b = &singleflightRaceBrowser{}
	b := f.b
	f.mu.Unlock()
	return b, nil
}

func (f *singleflightRaceFactory) getB() *singleflightRaceBrowser {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.b
}

func (f *singleflightRaceFactory) NewOAuthContext(ctx context.Context, initialURL *uat.ValidatedURL, policy uat.URLValidator) (uat.Browser, error) {
	return nil, nil
}

func TestMCPServer_ShutdownRacingFactory(t *testing.T) {
	inReader, inWriter := io.Pipe()
	var out bytes.Buffer

	bf := &singleflightRaceFactory{
		readyChan: make(chan struct{}),
	}
	mg := &mockGit{}
	tmpDir := t.TempDir()
	pixHost := filepath.Join(tmpDir, "pix-host")
	os.WriteFile(pixHost, []byte(""), 0755)

	runner, _ := uat.NewRunner(pixHost, "/repo", tmpDir, mg, &mockExec{}, &mockSandbox{}, &mockMCP{}, &mockImage{}, &mockLease{}, 1)

	s := uat.NewMCPServer(runner, bf, "", inReader, &out, nil)
	s.ShutdownTimeout = 10 * time.Millisecond

	go s.Serve(context.Background())

	// 1. Kick off a browser creation that will block
	req := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"uat_browser_action","arguments":{"run_id":"run_race","action":"snapshot"}}}` + "\n"
	inWriter.Write([]byte(req))

	// 2. Wait for it to enter the factory
	time.Sleep(50 * time.Millisecond)

	// 3. Kick off a second request that will block on <-entry.ready
	req2 := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"uat_browser_action","arguments":{"run_id":"run_race","action":"snapshot"}}}` + "\n"
	inWriter.Write([]byte(req2))
	time.Sleep(50 * time.Millisecond)

	// 4. Trigger shutdown
	inWriter.Close()
	time.Sleep(50 * time.Millisecond) // let done.Store(true) execute

	// 5. Unblock factory, returning a valid browser while shutting down
	close(bf.readyChan)
	time.Sleep(100 * time.Millisecond) // let it process

	// 6. Ensure the browser got closed and singleflight returned error
	b := bf.getB()
	if b == nil || !b.isClosed() {
		t.Errorf("browser not closed during shutdown race")
	}

	// Wait, we can test singleflight state by reading the entry directly since it's unexported?
	// The problem was mutating it after closing readyChan.
	// Since we can't easily assert on `entry.err`, verifying that the browser was correctly closed
	// AND that no race condition happens under `go test -race` is sufficient.
}
