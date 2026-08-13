package uat

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	hostuat "pix/host/uat"
)

type browserEntry struct {
	ready chan struct{}
	b     Browser
	err   error
}

type toolDescriptor struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

func getTools() []toolDescriptor {
	return []toolDescriptor{
		{
			Name:        "uat_capabilities",
			Description: "List UAT capabilities",
			InputSchema: map[string]interface{}{"type": "object"},
		},
		{
			Name:        "uat_submit",
			Description: "Submit a UAT scenario",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"commit":        map[string]interface{}{"type": "string"},
					"scenario_path": map[string]interface{}{"type": "string"},
					"dry_run":       map[string]interface{}{"type": "boolean"},
				},
				"required": []string{"commit", "scenario_path"},
			},
		},
		{
			Name:        "uat_status",
			Description: "Check UAT run status",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"run_id":  map[string]interface{}{"type": "string"},
					"cursor":  map[string]interface{}{"type": "number"},
					"wait_ms": map[string]interface{}{"type": "number"},
				},
				"required": []string{"run_id", "cursor"},
			},
		},
		{
			Name:        "uat_artifact",
			Description: "Read UAT artifact",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"run_id": map[string]interface{}{"type": "string"},
					"path":   map[string]interface{}{"type": "string"},
					"cursor": map[string]interface{}{"type": "number"},
					"tail":   map[string]interface{}{"type": "number"},
				},
				"required": []string{"run_id", "path"},
			},
		},
		{
			Name:        "uat_browser_action",
			Description: "Interact with browser",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"run_id": map[string]interface{}{"type": "string"},
					"action": map[string]interface{}{"type": "string"},
					"ref":    map[string]interface{}{"type": "string"},
					"value":  map[string]interface{}{"type": "string"},
				},
				"required": []string{"run_id", "action"},
			},
		},
		{
			Name:        "uat_abort",
			Description: "Abort a UAT run",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"run_id": map[string]interface{}{"type": "string"},
					"reason": map[string]interface{}{"type": "string"},
				},
				"required": []string{"run_id"},
			},
		},
	}
}

type MCPServer struct {
	runner         *Runner
	browserFactory BrowserFactory
	stateDir       string
	in             io.Reader
	out            io.Writer
	outMu          sync.Mutex
	done           atomic.Bool

	browsersMu sync.Mutex
	browsers   map[string]*browserEntry

	retryReport map[string]string

	ShutdownTimeout time.Duration
}

func NewMCPServer(runner *Runner, bf BrowserFactory, stateDir string, in io.Reader, out io.Writer, retryReport map[string]string) *MCPServer {
	return &MCPServer{
		runner:         runner,
		browserFactory: bf,
		stateDir:       stateDir,
		in:             in,
		out:            out,
		browsers:       make(map[string]*browserEntry),
		retryReport:    retryReport,
		ShutdownTimeout: 5 * time.Second,
	}
}

func (s *MCPServer) getOrCreateBrowser(ctx context.Context, runID string) (Browser, error) {
	s.browsersMu.Lock()
	if s.done.Load() {
		s.browsersMu.Unlock()
		return nil, fmt.Errorf("server shutting down")
	}

	entry, exists := s.browsers[runID]
	if !exists {
		entry = &browserEntry{ready: make(chan struct{})}
		s.browsers[runID] = entry
	}
	s.browsersMu.Unlock()

	if exists {
		select {
		case <-entry.ready:
			return entry.b, entry.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	u, _ := url.Parse("about:blank")
	b, err := s.browserFactory.NewContext(ctx, runID, &ValidatedURL{URL: u}, &OAuthPolicy{Resolver: &realResolver{}})
	
	s.browsersMu.Lock()
	if s.done.Load() && b != nil {
		b.Close()
		b = nil
		err = fmt.Errorf("server shutting down")
	}
	entry.b = b
	entry.err = err
	close(entry.ready)
	s.browsersMu.Unlock()

	
	return b, err
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type mcpResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   *mcpError   `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *MCPServer) Serve(ctx context.Context) error {
	scanner := bufio.NewScanner(s.in)
	// Max frame size: e.g., 10MB
	buf := make([]byte, 1024*1024*10)
	scanner.Buffer(buf, 1024*1024*10)

	var wg sync.WaitGroup
	serveCtx, cancelServe := context.WithCancel(ctx)
	defer cancelServe()

	for scanner.Scan() {
		line := scanner.Bytes()
		var req mcpRequest
		if err := json.Unmarshal(line, &req); err != nil {
			s.sendError(nil, -32700, "Parse error")
			continue
		}

		if req.JSONRPC != "2.0" {
			s.sendError(req.ID, -32600, "Invalid Request")
			continue
		}

		if req.ID == nil {
			// notification
			continue
		}

		switch req.Method {
		case "initialize":
			s.sendResponse(req.ID, map[string]interface{}{
				"protocolVersion": "2024-11-05",
				"capabilities": map[string]interface{}{
					"tools": map[string]interface{}{},
				},
				"serverInfo": map[string]interface{}{
					"name":    "pix-host-uat",
					"version": "1.0",
				},
			})
		case "tools/list":
			s.sendResponse(req.ID, map[string]interface{}{
				"tools": getTools(),
			})
		case "tools/call":
			wg.Add(1)
			go func(req mcpRequest) {
				defer wg.Done()
				s.handleToolCall(serveCtx, req.ID, req.Params)
			}(req)
		default:
			s.sendError(req.ID, -32601, "Method not found")
		}
	}

	var scanErr error
	if err := scanner.Err(); err != nil && err != io.EOF {
		scanErr = err
	}

	cancelServe()

	waitCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(waitCh)
	}()

	select {
	case <-waitCh:
	case <-time.After(s.ShutdownTimeout):
		scanErr = fmt.Errorf("shutdown timeout: pending tool calls did not finish")
	}

	s.done.Store(true)

	s.browsersMu.Lock()
	for _, entry := range s.browsers {
		select {
		case <-entry.ready:
			if entry.b != nil {
				entry.b.Close()
			}
		default:
		}
	}
	s.browsersMu.Unlock()

	return scanErr
}

func (s *MCPServer) sendResponse(id interface{}, result interface{}) {
	resp := mcpResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	b, _ := json.Marshal(resp)
	if !s.done.Load() {
		s.outMu.Lock()
		if !s.done.Load() {
			fmt.Fprintf(s.out, "%s\n", b)
		}
		s.outMu.Unlock()
	}
}

func (s *MCPServer) sendError(id interface{}, code int, message string) {
	resp := mcpResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &mcpError{
			Code:    code,
			Message: message,
		},
	}
	b, _ := json.Marshal(resp)
	if !s.done.Load() {
		s.outMu.Lock()
		if !s.done.Load() {
			fmt.Fprintf(s.out, "%s\n", b)
		}
		s.outMu.Unlock()
	}
}

func (s *MCPServer) handleToolCall(ctx context.Context, id interface{}, params json.RawMessage) {
	var p struct {
		Name      string                 `json:"name"`
		Arguments map[string]interface{} `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		s.sendError(id, -32602, "Invalid params")
		return
	}

	var textResult string
	var err error

	getString := func(k string) string {
		v, ok := p.Arguments[k].(string)
		if !ok {
			return ""
		}
		return v
	}
	getBool := func(k string) bool {
		v, ok := p.Arguments[k].(bool)
		if !ok {
			return false
		}
		return v
	}
	getInt64 := func(k string) int64 {
		v, ok := p.Arguments[k].(float64)
		if !ok {
			return 0
		}
		return int64(v)
	}

	switch p.Name {
	case "uat_capabilities":
		respMap := map[string]interface{}{
			"runner":       true,
			"browser":      true,
			"sandbox":      true,
			"retry_report": s.retryReport,
		}
		respBytes, _ := json.Marshal(respMap)
		textResult = string(respBytes)
	case "uat_submit":
		commit := getString("commit")
		scenarioPath := getString("scenario_path")
		dryRun := getBool("dry_run")

		if commit == "" || scenarioPath == "" {
			err = fmt.Errorf("missing required args")
			break
		}

		req := SubmitRequest{
			Commit:       commit,
			ScenarioPath: scenarioPath,
			DryRun:       dryRun,
		}
		var resp *SubmitResponse
		resp, err = s.runner.Submit(ctx, req)
		if err == nil {
			b, _ := json.Marshal(resp)
			textResult = string(b)
		}
	case "uat_status":
		runID := getString("run_id")
		cursor := getInt64("cursor")
		waitMs := getInt64("wait_ms")

		if runID == "" {
			err = fmt.Errorf("missing run_id")
			break
		}
		if err = hostuat.ValidateID(runID); err != nil {
			err = fmt.Errorf("invalid run_id: %w", err)
			break
		}
		if waitMs < 0 {
			waitMs = 0
		}
		if waitMs > 30000 {
			waitMs = 30000
		}

		req := StatusRequest{
			RunID:  runID,
			Cursor: cursor,
			WaitMs: waitMs,
		}
		var resp *StatusResponse
		resp, err = s.runner.Status(ctx, req)
		if err == nil {
			b, _ := json.Marshal(resp)
			textResult = string(b)
		}
	case "uat_artifact":
		runID := getString("run_id")
		artifactPath := getString("path")
		cursor := getInt64("cursor")
		tail := getInt64("tail")

		if runID == "" || artifactPath == "" {
			err = fmt.Errorf("missing run_id or path")
			break
		}

		if err = hostuat.ValidateID(runID); err != nil {
			err = fmt.Errorf("invalid run_id: %w", err)
			break
		}

		// Use safe ReadArtifact
		runDir := filepath.Join(s.stateDir, "runs", runID)
		b, nextCursor, readErr := hostuat.ReadArtifact(runDir, artifactPath, 1<<20, int(tail), cursor)
		if readErr != nil {
			err = fmt.Errorf("read artifact: %w", readErr)
			break
		}

		// Return structured JSON
		respMap := map[string]interface{}{
			"content":     string(b),
			"next_cursor": nextCursor,
		}
		respBytes, _ := json.Marshal(respMap)
		textResult = string(respBytes)
	case "uat_browser_action":
		runID := getString("run_id")
		action := getString("action")
		ref := getString("ref")
		// value := getString("value") // unused right now

		if runID == "" || action == "" {
			err = fmt.Errorf("missing run_id or action")
			break
		}

		// Get or create browser context
		// Assuming we just instantiate for simplicity, but state says "using active run state"
		// The prompt says "never accept URL in browser_action"
		// How to associate Browser with RunID? A simple map in MCPServer.

		bCtx, createErr := s.getOrCreateBrowser(ctx, runID)
		if createErr != nil {
			err = fmt.Errorf("browser: %w", createErr)
			break
		}

		switch action {
		case "snapshot":
			snap, e := bCtx.Snapshot(ctx)
			if e != nil {
				err = e
			} else {
				respMap := map[string]interface{}{
					"dom_length":      len(snap.DOM),
					"screenshot_size": len(snap.Screenshot),
				}
				respBytes, _ := json.Marshal(respMap)
				textResult = string(respBytes)
			}
		case "click":
			if ref == "" {
				err = fmt.Errorf("missing ref")
				break
			}
			err = bCtx.Click(ctx, ref)
			if err == nil {
				respMap := map[string]interface{}{"status": "clicked"}
				respBytes, _ := json.Marshal(respMap)
				textResult = string(respBytes)
			}
		case "read_visible_text":
			textResult, err = bCtx.VisibleText(ctx)
			if err == nil {
				respMap := map[string]interface{}{"text": textResult}
				respBytes, _ := json.Marshal(respMap)
				textResult = string(respBytes)
			}
		default:
			err = fmt.Errorf("unknown browser action: %s", action)
		}

	case "uat_abort":
		runID := getString("run_id")

		if runID == "" {
			err = fmt.Errorf("missing run_id")
			break
		}
		if err = hostuat.ValidateID(runID); err != nil {
			err = fmt.Errorf("invalid run_id: %w", err)
			break
		}
		err = s.runner.Abort(ctx, runID)
		if err == nil {
			respMap := map[string]interface{}{"status": "aborted"}
			respBytes, _ := json.Marshal(respMap)
			textResult = string(respBytes)
		}
	default:
		s.sendError(id, -32601, "Tool not found")
		return
	}

	if err != nil {
		s.sendResponse(id, map[string]interface{}{
			"content": []interface{}{
				map[string]interface{}{
					"type": "text",
					"text": err.Error(),
				},
			},
			"isError": true,
		})
	} else {
		s.sendResponse(id, map[string]interface{}{
			"content": []interface{}{
				map[string]interface{}{
					"type": "text",
					"text": textResult,
				},
			},
		})
	}
}
