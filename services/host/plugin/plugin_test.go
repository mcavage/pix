package plugin

import (
	"encoding/json"
	"testing"

	goplugin "github.com/hashicorp/go-plugin"
)

// Trivial in-memory implementations used to prove the interfaces are satisfiable
// and to populate a plugin's Impl in the table test below. They are compile-time
// checked against the public interfaces.
type noopMemory struct{}

func (noopMemory) Remember(RememberReq) (RememberResp, error)       { return RememberResp{}, nil }
func (noopMemory) Recall(RecallReq) (RecallResp, error)             { return RecallResp{}, nil }
func (noopMemory) Forget(ForgetReq) (ForgetResp, error)             { return ForgetResp{}, nil }
func (noopMemory) Synthesize(SynthesizeReq) (SynthesizeResp, error) { return SynthesizeResp{}, nil }
func (noopMemory) Promotable(PromotableReq) (PromotableResp, error) { return PromotableResp{}, nil }
func (noopMemory) Observe(ObserveReq) (ObserveResp, error)          { return ObserveResp{}, nil }
func (noopMemory) Stats() (Stats, error)                            { return Stats{}, nil }
func (noopMemory) Health() (Health, error)                          { return Health{}, nil }

type noopBroker struct{}

func (noopBroker) Mint(string, []string) (Token, error) { return Token{}, nil }
func (noopBroker) Check() error                         { return nil }
func (noopBroker) Describe() (BrokerInfo, error)        { return BrokerInfo{}, nil }

type noopMcp struct{}

func (noopMcp) Info() (ServerInfo, error)      { return ServerInfo{}, nil }
func (noopMcp) ListTools() ([]ToolSpec, error) { return nil, nil }
func (noopMcp) CallTool(string, json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage("null"), nil
}

// Compile-time proof the trivial impls satisfy the interfaces.
var (
	_ MemoryStore      = noopMemory{}
	_ CredentialBroker = noopBroker{}
	_ McpServer        = noopMcp{}
)

func TestHandshakeProtocolVersion(t *testing.T) {
	if ProtocolVersion != 1 {
		t.Fatalf("ProtocolVersion = %d, want 1", ProtocolVersion)
	}
	if Handshake.ProtocolVersion != 1 {
		t.Fatalf("Handshake.ProtocolVersion = %d, want 1", Handshake.ProtocolVersion)
	}
	if Handshake.MagicCookieKey != "PI_STACK_PLUGIN" {
		t.Fatalf("MagicCookieKey = %q, want PI_STACK_PLUGIN", Handshake.MagicCookieKey)
	}
	if Handshake.MagicCookieValue == "" {
		t.Fatal("MagicCookieValue must not be empty (skew/random-exec guard)")
	}
}

// TestRPCPlugins constructs each RPCPlugin with a trivial Impl and asserts it
// satisfies plugin.Plugin. It does NOT spawn a subprocess; it exercises the
// adapter construction and the Server side of the net/rpc wiring.
func TestRPCPlugins(t *testing.T) {
	cases := []struct {
		name string
		p    goplugin.Plugin
	}{
		{"memory", &MemoryPlugin{Impl: noopMemory{}}},
		{"broker", &BrokerPlugin{Impl: noopBroker{}}},
		{"mcp", &McpPlugin{Impl: noopMcp{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, err := tc.p.Server(nil)
			if err != nil {
				t.Fatalf("Server() error: %v", err)
			}
			if srv == nil {
				t.Fatal("Server() returned nil object")
			}
		})
	}
}

// TestPluginMap asserts the client-side map covers the three capabilities with
// the matching adapter types (nil Impl is correct for the client side).
func TestPluginMap(t *testing.T) {
	wants := map[string]interface{}{
		"memory": &MemoryPlugin{},
		"broker": &BrokerPlugin{},
		"mcp":    &McpPlugin{},
	}
	if len(PluginMap) != len(wants) {
		t.Fatalf("PluginMap has %d entries, want %d", len(PluginMap), len(wants))
	}
	for k, want := range wants {
		got, ok := PluginMap[k]
		if !ok {
			t.Fatalf("PluginMap missing capability %q", k)
		}
		if got == nil {
			t.Fatalf("PluginMap[%q] is nil", k)
		}
		switch want.(type) {
		case *MemoryPlugin:
			if _, ok := got.(*MemoryPlugin); !ok {
				t.Fatalf("PluginMap[%q] is %T, want *MemoryPlugin", k, got)
			}
		case *BrokerPlugin:
			if _, ok := got.(*BrokerPlugin); !ok {
				t.Fatalf("PluginMap[%q] is %T, want *BrokerPlugin", k, got)
			}
		case *McpPlugin:
			if _, ok := got.(*McpPlugin); !ok {
				t.Fatalf("PluginMap[%q] is %T, want *McpPlugin", k, got)
			}
		}
	}
}
