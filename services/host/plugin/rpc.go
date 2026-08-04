package plugin

import (
	"encoding/json"
	"net/rpc"

	goplugin "github.com/hashicorp/go-plugin"
)

// This file implements the go-plugin net/rpc transport for each capability.
//
// go-plugin registers the served object under the RPC name "Plugin", so every
// method is invoked as "Plugin.<Method>". Errors returned by an Impl method
// propagate to the client as an rpc.ServerError (the error string is preserved),
// so the client stubs return them directly without an embedded error field.
//
// net/rpc requires each server method to have the shape
// func(args T1, reply *T2) error with exported, gob-encodable T1/T2. Methods
// that take no arguments use an Empty struct placeholder.
//
// TODO(gRPC/AutoMTLS): upgrade transport per arch doc — replace the net/rpc
// Server/Client pairs below with gRPC stubs (protoc-generated) and enable
// AutoMTLS over the unix socket. The Handshake, interfaces, and PluginMap stay;
// only the wiring in this file changes.

// Empty is the placeholder args/reply for zero-argument RPC methods.
type Empty struct{}

// =============================== MemoryStore =================================

// MemoryPlugin adapts a MemoryStore to plugin.Plugin over net/rpc. On the client
// side Impl is nil (only Server consults it).
type MemoryPlugin struct{ Impl MemoryStore }

func (p *MemoryPlugin) Server(*goplugin.MuxBroker) (interface{}, error) {
	return &memoryRPCServer{Impl: p.Impl}, nil
}

func (p *MemoryPlugin) Client(_ *goplugin.MuxBroker, c *rpc.Client) (interface{}, error) {
	return &memoryRPCClient{client: c}, nil
}

type memoryRPCServer struct{ Impl MemoryStore }

func (s *memoryRPCServer) Remember(req RememberReq, resp *RememberResp) error {
	r, err := s.Impl.Remember(req)
	if err != nil {
		return err
	}
	*resp = r
	return nil
}

func (s *memoryRPCServer) Recall(req RecallReq, resp *RecallResp) error {
	r, err := s.Impl.Recall(req)
	if err != nil {
		return err
	}
	*resp = r
	return nil
}

func (s *memoryRPCServer) Forget(req ForgetReq, resp *ForgetResp) error {
	r, err := s.Impl.Forget(req)
	if err != nil {
		return err
	}
	*resp = r
	return nil
}

func (s *memoryRPCServer) Synthesize(req SynthesizeReq, resp *SynthesizeResp) error {
	r, err := s.Impl.Synthesize(req)
	if err != nil {
		return err
	}
	*resp = r
	return nil
}

func (s *memoryRPCServer) Promotable(req PromotableReq, resp *PromotableResp) error {
	r, err := s.Impl.Promotable(req)
	if err != nil {
		return err
	}
	*resp = r
	return nil
}

func (s *memoryRPCServer) Observe(req ObserveReq, resp *ObserveResp) error {
	r, err := s.Impl.Observe(req)
	if err != nil {
		return err
	}
	*resp = r
	return nil
}

func (s *memoryRPCServer) Stats(profile string, resp *Stats) error {
	r, err := s.Impl.Stats(profile)
	if err != nil {
		return err
	}
	*resp = r
	return nil
}

func (s *memoryRPCServer) Health(_ Empty, resp *Health) error {
	r, err := s.Impl.Health()
	if err != nil {
		return err
	}
	*resp = r
	return nil
}

type memoryRPCClient struct{ client *rpc.Client }

func (c *memoryRPCClient) Remember(req RememberReq) (RememberResp, error) {
	var resp RememberResp
	err := c.client.Call("Plugin.Remember", req, &resp)
	return resp, err
}

func (c *memoryRPCClient) Recall(req RecallReq) (RecallResp, error) {
	var resp RecallResp
	err := c.client.Call("Plugin.Recall", req, &resp)
	return resp, err
}

func (c *memoryRPCClient) Forget(req ForgetReq) (ForgetResp, error) {
	var resp ForgetResp
	err := c.client.Call("Plugin.Forget", req, &resp)
	return resp, err
}

func (c *memoryRPCClient) Synthesize(req SynthesizeReq) (SynthesizeResp, error) {
	var resp SynthesizeResp
	err := c.client.Call("Plugin.Synthesize", req, &resp)
	return resp, err
}

func (c *memoryRPCClient) Promotable(req PromotableReq) (PromotableResp, error) {
	var resp PromotableResp
	err := c.client.Call("Plugin.Promotable", req, &resp)
	return resp, err
}

func (c *memoryRPCClient) Observe(req ObserveReq) (ObserveResp, error) {
	var resp ObserveResp
	err := c.client.Call("Plugin.Observe", req, &resp)
	return resp, err
}

func (c *memoryRPCClient) Stats(profile string) (Stats, error) {
	var resp Stats
	err := c.client.Call("Plugin.Stats", profile, &resp)
	return resp, err
}

func (c *memoryRPCClient) Health() (Health, error) {
	var resp Health
	err := c.client.Call("Plugin.Health", Empty{}, &resp)
	return resp, err
}

// ============================== KnowledgeStore ===============================

// KnowledgePlugin adapts a KnowledgeStore to plugin.Plugin over net/rpc. On the
// client side Impl is nil (only Server consults it).
type KnowledgePlugin struct{ Impl KnowledgeStore }

func (p *KnowledgePlugin) Server(*goplugin.MuxBroker) (interface{}, error) {
	return &knowledgeRPCServer{Impl: p.Impl}, nil
}

func (p *KnowledgePlugin) Client(_ *goplugin.MuxBroker, c *rpc.Client) (interface{}, error) {
	return &knowledgeRPCClient{client: c}, nil
}

type knowledgeRPCServer struct{ Impl KnowledgeStore }

func (s *knowledgeRPCServer) Query(req QueryArgs, resp *QueryResult) error {
	r, err := s.Impl.Query(req)
	if err != nil {
		return err
	}
	*resp = r
	return nil
}

func (s *knowledgeRPCServer) Reindex(req ReindexArgs, resp *ReindexResult) error {
	r, err := s.Impl.Reindex(req)
	if err != nil {
		return err
	}
	*resp = r
	return nil
}

func (s *knowledgeRPCServer) Health(_ Empty, resp *KnowledgeHealth) error {
	r, err := s.Impl.Health()
	if err != nil {
		return err
	}
	*resp = r
	return nil
}

type knowledgeRPCClient struct{ client *rpc.Client }

func (c *knowledgeRPCClient) Query(req QueryArgs) (QueryResult, error) {
	var resp QueryResult
	err := c.client.Call("Plugin.Query", req, &resp)
	return resp, err
}

func (c *knowledgeRPCClient) Reindex(req ReindexArgs) (ReindexResult, error) {
	var resp ReindexResult
	err := c.client.Call("Plugin.Reindex", req, &resp)
	return resp, err
}

func (c *knowledgeRPCClient) Health() (KnowledgeHealth, error) {
	var resp KnowledgeHealth
	err := c.client.Call("Plugin.Health", Empty{}, &resp)
	return resp, err
}

// ================================= McpServer =================================

// McpPlugin adapts an McpServer to plugin.Plugin over net/rpc.
type McpPlugin struct{ Impl McpServer }

func (p *McpPlugin) Server(*goplugin.MuxBroker) (interface{}, error) {
	return &mcpRPCServer{Impl: p.Impl}, nil
}

func (p *McpPlugin) Client(_ *goplugin.MuxBroker, c *rpc.Client) (interface{}, error) {
	return &mcpRPCClient{client: c}, nil
}

// CallToolArgs packs CallTool's two parameters into one gob-encodable struct.
type CallToolArgs struct {
	Name string
	Args json.RawMessage
}

type mcpRPCServer struct{ Impl McpServer }

func (s *mcpRPCServer) Info(_ Empty, resp *ServerInfo) error {
	info, err := s.Impl.Info()
	if err != nil {
		return err
	}
	*resp = info
	return nil
}

func (s *mcpRPCServer) ListTools(_ Empty, resp *[]ToolSpec) error {
	tools, err := s.Impl.ListTools()
	if err != nil {
		return err
	}
	*resp = tools
	return nil
}

func (s *mcpRPCServer) CallTool(args CallToolArgs, resp *json.RawMessage) error {
	out, err := s.Impl.CallTool(args.Name, args.Args)
	if err != nil {
		return err
	}
	*resp = out
	return nil
}

type mcpRPCClient struct{ client *rpc.Client }

func (c *mcpRPCClient) Info() (ServerInfo, error) {
	var resp ServerInfo
	err := c.client.Call("Plugin.Info", Empty{}, &resp)
	return resp, err
}

func (c *mcpRPCClient) ListTools() ([]ToolSpec, error) {
	var resp []ToolSpec
	err := c.client.Call("Plugin.ListTools", Empty{}, &resp)
	return resp, err
}

func (c *mcpRPCClient) CallTool(name string, args json.RawMessage) (json.RawMessage, error) {
	var resp json.RawMessage
	err := c.client.Call("Plugin.CallTool", CallToolArgs{Name: name, Args: args}, &resp)
	return resp, err
}

// Compile-time guarantees that the client stubs satisfy the public interfaces
// and the plugin adapters satisfy plugin.Plugin.
var (
	_ MemoryStore    = (*memoryRPCClient)(nil)
	_ KnowledgeStore = (*knowledgeRPCClient)(nil)
	_ McpServer      = (*mcpRPCClient)(nil)

	_ goplugin.Plugin = (*MemoryPlugin)(nil)
	_ goplugin.Plugin = (*KnowledgePlugin)(nil)
	_ goplugin.Plugin = (*McpPlugin)(nil)
)
