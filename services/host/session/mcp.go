// mcp.go is the session-control MCP tool: a minimal stdio JSON-RPC server
// (architecture §7.2's "narrowly scoped Gateway-launched MCP command
// implemented by pix"), NOT a general MCP framework and not a shell. It
// exposes exactly one tool, pix_session_delegate, whose input schema is
// ChildRequest's four fields and nothing else, and whose only action is to
// record a child node and spawn one bounded child-runner (Spawner). Anything
// this tool cannot express — an arbitrary command, a plugin call, a second
// tool — is out of scope on purpose.
package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// ServerContext is the identity this sandbox's session-control server acts
// with: the parent node a delegated child's Parent field names, the tree it
// joins, and the instance its reference must bind to. It is supplied by the
// hidden invocation's caller (cmd/pix/sessionctl.go), which reads it from
// the environment the launch-time Gateway registration sets — wiring that
// env at `pix run` is a separate, launch-boundary change; this type only
// says what the server needs once it has it, so it is directly testable
// without that wiring existing yet.
type ServerContext struct {
	StoreRoot  string
	SandboxDir string
	TreeID     string
	ParentID   string
	Sandbox    string
	InstanceID string
}

// Spawner starts one detached child-runner for a validated request and
// returns the node id it was assigned. It never blocks on the child's
// completion — RunChild's own reference hold, not this call, is what keeps
// the sandbox alive while the child runs (see childrunner.go).
type Spawner func(ctx ServerContext, treeID, nodeID string, req ChildRequest) error

// Server is the stdio MCP server. NewID is overridable so a test can pin a
// deterministic node id instead of reading crypto/rand. Limits is L2's
// (security re-review) delegate cap, enforced atomically per tree
// (Store.WithTreeLock/CheckDelegateCaps) — NewServer sets it to
// DefaultLimits so a caller that never touches this field still gets a
// bounded server, never an accidentally-unbounded one.
type Server struct {
	Ctx    ServerContext
	Spawn  Spawner
	NewID  func() (string, error)
	Limits Limits
	in     io.Reader
	out    io.Writer
	outMu  sync.Mutex
	closed atomic.Bool
}

func NewServer(ctx ServerContext, spawn Spawner, in io.Reader, out io.Writer) *Server {
	return &Server{Ctx: ctx, Spawn: spawn, NewID: NewID, Limits: DefaultLimits(), in: in, out: out}
}

const toolName = "pix_session_delegate"

// ReservedMCPName is the ONE static-mcp server name this feature ever
// registers as: a pix-owned local command (`pix __pix-session-mcp`), never
// a pack-contributed one. A Tier-1 trust-bill renderer (architecture §11)
// must recognize this exact name as reserved rather than treating it as an
// arbitrary pack's local MCP command declaration.
const ReservedMCPName = "pix-session"

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

func sessionDelegateTool() map[string]interface{} {
	return map[string]interface{}{
		"name":        toolName,
		"description": "Delegate one bounded child agent invocation that can outlive this session.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"agent":  map[string]interface{}{"type": "string"},
				"task":   map[string]interface{}{"type": "string"},
				"model":  map[string]interface{}{"type": "string"},
				"target": map[string]interface{}{"type": "string", "enum": []string{"local-process", "local-sandbox", "cloud-sandbox"}},
			},
			"required":             []string{"agent", "task", "target"},
			"additionalProperties": false,
		},
	}
}

// Serve reads newline-delimited JSON-RPC requests until EOF or a read error.
func (s *Server) Serve() error {
	scanner := bufio.NewScanner(s.in)
	buf := make([]byte, 1024*1024)
	scanner.Buffer(buf, 1024*1024)

	var wg sync.WaitGroup
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
			continue // notification
		}
		switch req.Method {
		case "initialize":
			s.sendResponse(req.ID, map[string]interface{}{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
				"serverInfo":      map[string]interface{}{"name": "pix-session-control", "version": "1.0"},
			})
		case "tools/list":
			s.sendResponse(req.ID, map[string]interface{}{"tools": []interface{}{sessionDelegateTool()}})
		case "tools/call":
			wg.Add(1)
			go func(req mcpRequest) {
				defer wg.Done()
				s.handleToolCall(req.ID, req.Params)
			}(req)
		default:
			s.sendError(req.ID, -32601, "Method not found")
		}
	}
	wg.Wait()
	s.closed.Store(true)
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

func (s *Server) handleToolCall(id interface{}, params json.RawMessage) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		s.sendError(id, -32602, "Invalid params")
		return
	}
	if p.Name != toolName {
		s.sendError(id, -32601, "Tool not found")
		return
	}
	result, err := s.Delegate(p.Arguments)
	if err != nil {
		s.sendToolError(id, err)
		return
	}
	b, _ := json.Marshal(result)
	s.sendToolText(id, string(b))
}

// DelegateResult is what a successful pix_session_delegate call returns:
// enough for the caller to name what it started, never a promise about
// completion (the caller never blocks on the child; the child's own
// reference is what keeps the sandbox alive).
type DelegateResult struct {
	Tree string `json:"tree"`
	Node string `json:"node"`
}

// Delegate is the tool's whole behavior, factored out of the JSON-RPC
// envelope so it is directly unit-testable: decode strictly (refuses a
// general command/shell field), validate, refuse an unknown or unsupported
// target, record the child node under its parent, and spawn one detached
// child-runner.
func (s *Server) Delegate(raw json.RawMessage) (*DelegateResult, error) {
	req, err := DecodeChildRequest(raw)
	if err != nil {
		return nil, err
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if err := CheckTarget(Target(req.Target)); err != nil {
		return nil, err
	}
	store := Store{Root: s.Ctx.StoreRoot}
	limits := s.Limits
	if limits == (Limits{}) {
		limits = DefaultLimits()
	}
	// L2 (security re-review): the cap check and the node write that
	// reserves the slot it approves run under ONE per-tree lock, so a
	// concurrent delegate call against the SAME tree is refused before
	// EITHER spawns or holds anything, never a race where both pass the
	// same check.
	var nodeID string
	if lerr := store.WithTreeLock(s.Ctx.TreeID, func() error {
		if cerr := store.CheckDelegateCaps(s.Ctx.TreeID, s.Ctx.ParentID, limits); cerr != nil {
			return cerr
		}
		id, nerr := s.NewID()
		if nerr != nil {
			return fmt.Errorf("pix: session delegate could not allocate a node id: %w", nerr)
		}
		node := Node{
			ID:         id,
			Parent:     s.Ctx.ParentID,
			Model:      req.Model,
			Target:     Target(req.Target),
			Sandbox:    s.Ctx.Sandbox,
			InstanceID: s.Ctx.InstanceID,
			State:      StateStarting,
		}
		if perr := store.PutNode(s.Ctx.TreeID, node); perr != nil {
			return fmt.Errorf("pix: session delegate could not record the child node: %w", perr)
		}
		nodeID = id
		return nil
	}); lerr != nil {
		return nil, lerr
	}
	if s.Spawn != nil {
		if err := s.Spawn(s.Ctx, s.Ctx.TreeID, nodeID, req); err != nil {
			// Marking the reserving node Failed (a terminal state) is what
			// frees its live-child slot immediately — LiveNodeCount only
			// counts non-terminal nodes — rather than holding it until some
			// later cleanup notices the spawn never happened.
			failed := Node{
				ID: nodeID, Parent: s.Ctx.ParentID, Model: req.Model, Target: Target(req.Target),
				Sandbox: s.Ctx.Sandbox, InstanceID: s.Ctx.InstanceID, State: StateFailed,
			}
			_ = store.PutNode(s.Ctx.TreeID, failed)
			return nil, fmt.Errorf("pix: session delegate could not start the child runner: %w", err)
		}
	}
	return &DelegateResult{Tree: s.Ctx.TreeID, Node: nodeID}, nil
}

func (s *Server) sendToolText(id interface{}, text string) {
	s.sendResponse(id, map[string]interface{}{
		"content": []interface{}{map[string]interface{}{"type": "text", "text": text}},
	})
}

func (s *Server) sendToolError(id interface{}, err error) {
	s.sendResponse(id, map[string]interface{}{
		"content": []interface{}{map[string]interface{}{"type": "text", "text": err.Error()}},
		"isError": true,
	})
}

func (s *Server) sendResponse(id interface{}, result interface{}) {
	s.write(mcpResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *Server) sendError(id interface{}, code int, message string) {
	s.write(mcpResponse{JSONRPC: "2.0", ID: id, Error: &mcpError{Code: code, Message: message}})
}

func (s *Server) write(resp mcpResponse) {
	if s.closed.Load() {
		return
	}
	b, _ := json.Marshal(resp)
	s.outMu.Lock()
	defer s.outMu.Unlock()
	if s.closed.Load() {
		return
	}
	fmt.Fprintf(s.out, "%s\n", b)
}
