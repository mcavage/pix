package plugin

import (
	"encoding/json"
	"errors"
	"net"
	"net/rpc"
	"reflect"
	"strings"
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

// ============================ RPC round-trip tests ===========================
//
// These exercise the FULL net/rpc path in-process: they register the *RPCServer
// wrapper under the "Plugin" name (exactly as go-plugin does), connect a real
// rpc.Client over an in-memory net.Pipe with go's default gob codec, and drive
// every method through the client stub. This is the coverage that was missing:
// net/rpc silently skips any method whose arg/reply type is unexported, so a
// zero-arg method keyed on an unexported placeholder would register as absent
// and fail here with "can't find method". These tests fail pre-fix (unexported
// `empty`) and pass after the rename to the exported `Empty`.

// newRPCPair registers rcvr as "Plugin" on a fresh rpc.Server, serves it over
// one end of a net.Pipe (gob codec), and returns a connected rpc.Client. Both
// ends are closed on test cleanup.
func newRPCPair(t *testing.T, rcvr interface{}) *rpc.Client {
	t.Helper()
	srv := rpc.NewServer()
	if err := srv.RegisterName("Plugin", rcvr); err != nil {
		t.Fatalf("RegisterName: %v", err)
	}
	serverEnd, clientEnd := net.Pipe()
	go srv.ServeConn(serverEnd)
	client := rpc.NewClient(clientEnd)
	t.Cleanup(func() {
		_ = client.Close()
		_ = serverEnd.Close()
	})
	return client
}

// echoMemory returns distinctive, argument-derived values so a round trip can be
// asserted end to end (not just non-error).
type echoMemory struct{}

func (echoMemory) Remember(r RememberReq) (RememberResp, error) {
	return RememberResp{ID: r.Content, Reaffirmed: true}, nil
}
func (echoMemory) Recall(r RecallReq) (RecallResp, error) {
	return RecallResp{Hits: []Hit{{ID: "h1", Content: r.Query, Score: 0.5}}}, nil
}
func (echoMemory) Forget(r ForgetReq) (ForgetResp, error) { return ForgetResp{OK: r.ID != ""}, nil }
func (echoMemory) Synthesize(r SynthesizeReq) (SynthesizeResp, error) {
	return SynthesizeResp{Merged: 3, Expired: 7}, nil
}
func (echoMemory) Promotable(r PromotableReq) (PromotableResp, error) {
	return PromotableResp{Candidates: []Candidate{{ID: "c1", Frequency: r.MinFrequency}}}, nil
}
func (echoMemory) Observe(r ObserveReq) (ObserveResp, error) {
	return ObserveResp{Accepted: true, Reason: r.User}, nil
}
func (echoMemory) Stats() (Stats, error)   { return Stats{Active: 11, Durable: 22}, nil }
func (echoMemory) Health() (Health, error) { return Health{OK: true, WatcherModel: "m"}, nil }

func TestRPCRoundTripMemory(t *testing.T) {
	client := newRPCPair(t, &memoryRPCServer{Impl: echoMemory{}})
	c := &memoryRPCClient{client: client}

	if got, err := c.Remember(RememberReq{Content: "hello"}); err != nil || got.ID != "hello" || !got.Reaffirmed {
		t.Fatalf("Remember round trip: got %+v err %v", got, err)
	}
	if got, err := c.Recall(RecallReq{Query: "q"}); err != nil || len(got.Hits) != 1 || got.Hits[0].Content != "q" {
		t.Fatalf("Recall round trip: got %+v err %v", got, err)
	}
	if got, err := c.Forget(ForgetReq{ID: "x"}); err != nil || !got.OK {
		t.Fatalf("Forget round trip: got %+v err %v", got, err)
	}
	if got, err := c.Synthesize(SynthesizeReq{Threshold: 0.9}); err != nil || got.Merged != 3 || got.Expired != 7 {
		t.Fatalf("Synthesize round trip: got %+v err %v", got, err)
	}
	if got, err := c.Promotable(PromotableReq{MinFrequency: 5}); err != nil || len(got.Candidates) != 1 || got.Candidates[0].Frequency != 5 {
		t.Fatalf("Promotable round trip: got %+v err %v", got, err)
	}
	if got, err := c.Observe(ObserveReq{User: "alice"}); err != nil || !got.Accepted || got.Reason != "alice" {
		t.Fatalf("Observe round trip: got %+v err %v", got, err)
	}
	// Zero-arg method — the exact shape net/rpc would drop with an unexported arg.
	if got, err := c.Stats(); err != nil || got.Active != 11 || got.Durable != 22 {
		t.Fatalf("Stats round trip: got %+v err %v", got, err)
	}
	// Zero-arg method.
	if got, err := c.Health(); err != nil || !got.OK || got.WatcherModel != "m" {
		t.Fatalf("Health round trip: got %+v err %v", got, err)
	}
}

// echoBroker mirrors args into its results.
type echoBroker struct{}

func (echoBroker) Mint(audience string, scopes []string) (Token, error) {
	return Token{AccessToken: audience, TokenType: "Bearer", ExpiresIn: len(scopes)}, nil
}
func (echoBroker) Check() error { return nil }
func (echoBroker) Describe() (BrokerInfo, error) {
	return BrokerInfo{Name: "gws", DefaultPort: 11441, RequiresHostCLI: true}, nil
}

func TestRPCRoundTripBroker(t *testing.T) {
	client := newRPCPair(t, &brokerRPCServer{Impl: echoBroker{}})
	c := &brokerRPCClient{client: client}

	if got, err := c.Mint("aud", []string{"a", "b"}); err != nil || got.AccessToken != "aud" || got.ExpiresIn != 2 {
		t.Fatalf("Mint round trip: got %+v err %v", got, err)
	}
	// Zero-arg method (empty reply too).
	if err := c.Check(); err != nil {
		t.Fatalf("Check round trip: err %v", err)
	}
	// Zero-arg method.
	if got, err := c.Describe(); err != nil || got.Name != "gws" || got.DefaultPort != 11441 || !got.RequiresHostCLI {
		t.Fatalf("Describe round trip: got %+v err %v", got, err)
	}
}

// echoMcp mirrors args into its results.
type echoMcp struct{}

func (echoMcp) Info() (ServerInfo, error) {
	return ServerInfo{Name: "slack", Version: "1", ProtocolVersion: "2024"}, nil
}
func (echoMcp) ListTools() ([]ToolSpec, error) {
	return []ToolSpec{{Name: "post", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}}, nil
}
func (echoMcp) CallTool(name string, args json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{"tool":"` + name + `"}`), nil
}

func TestRPCRoundTripMcp(t *testing.T) {
	client := newRPCPair(t, &mcpRPCServer{Impl: echoMcp{}})
	c := &mcpRPCClient{client: client}

	// Zero-arg method.
	if got, err := c.Info(); err != nil || got.Name != "slack" || got.ProtocolVersion != "2024" {
		t.Fatalf("Info round trip: got %+v err %v", got, err)
	}
	// Zero-arg method.
	got, err := c.ListTools()
	if err != nil || len(got) != 1 || got[0].Name != "post" {
		t.Fatalf("ListTools round trip: got %+v err %v", got, err)
	}
	if !reflect.DeepEqual([]byte(got[0].InputSchema), []byte(`{"type":"object"}`)) {
		t.Fatalf("ListTools schema round trip: got %s", got[0].InputSchema)
	}
	out, err := c.CallTool("post", json.RawMessage(`{"a":1}`))
	if err != nil || string(out) != `{"tool":"post"}` {
		t.Fatalf("CallTool round trip: got %s err %v", out, err)
	}
}

// errMemory returns a distinctive error from a zero-arg method so we can assert
// the error string survives the rpc.ServerError round trip.
type errMemory struct{ echoMemory }

func (errMemory) Stats() (Stats, error) { return Stats{}, errors.New("boom: stats failed") }

func TestRPCRoundTripError(t *testing.T) {
	client := newRPCPair(t, &memoryRPCServer{Impl: errMemory{}})
	c := &memoryRPCClient{client: client}

	_, err := c.Stats()
	if err == nil {
		t.Fatal("Stats: expected non-nil error from impl, got nil")
	}
	if !strings.Contains(err.Error(), "boom: stats failed") {
		t.Fatalf("Stats error message not preserved: got %q", err.Error())
	}
}
