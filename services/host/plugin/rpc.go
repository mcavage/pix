package plugin

import (
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

// Compile-time guarantees that the client stubs satisfy the public interfaces
// and the plugin adapters satisfy plugin.Plugin.
var (
	_ MemoryStore = (*memoryRPCClient)(nil)

	_ goplugin.Plugin = (*MemoryPlugin)(nil)
)
