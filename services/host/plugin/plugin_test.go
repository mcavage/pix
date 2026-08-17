package plugin

import (
	"errors"
	"net"
	"net/rpc"
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
func (noopMemory) Stats(string) (Stats, error)                      { return Stats{}, nil }
func (noopMemory) Health() (Health, error)                          { return Health{}, nil }

// Compile-time proof the trivial impl satisfies the interface.
var _ MemoryStore = noopMemory{}

func TestHandshakeProtocolVersion(t *testing.T) {
	if ProtocolVersion != 2 {
		t.Fatalf("ProtocolVersion = %d, want 2", ProtocolVersion)
	}
	if Handshake.ProtocolVersion != 2 {
		t.Fatalf("Handshake.ProtocolVersion = %d, want 2", Handshake.ProtocolVersion)
	}
	if Handshake.MagicCookieKey != "PIX_PLUGIN" {
		t.Fatalf("MagicCookieKey = %q, want PIX_PLUGIN", Handshake.MagicCookieKey)
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

// TestPluginMap asserts the client-side map covers the dispensable capability
// with the matching adapter type (nil Impl is correct for the client side).
// The set is CLOSED: memory is the only capability this binary dispenses now
// that the MCP plugin transport is retired (the bridge served zero built-in
// servers, and an external MCP server ships as a container the sbx gateway
// runs).
func TestPluginMap(t *testing.T) {
	wants := map[string]interface{}{
		"memory": &MemoryPlugin{},
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
	return SynthesizeResp{Merged: 3}, nil
}
func (echoMemory) Promotable(r PromotableReq) (PromotableResp, error) {
	return PromotableResp{Candidates: []Candidate{{ID: "c1", Frequency: r.MinFrequency}}}, nil
}
func (echoMemory) Observe(r ObserveReq) (ObserveResp, error) {
	return ObserveResp{Accepted: true, Reason: r.User}, nil
}
func (echoMemory) Stats(string) (Stats, error) { return Stats{Active: 11, Durable: 22}, nil }
func (echoMemory) Health() (Health, error)     { return Health{OK: true, WatcherModel: "m"}, nil }

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
	if got, err := c.Synthesize(SynthesizeReq{Threshold: 0.9}); err != nil || got.Merged != 3 {
		t.Fatalf("Synthesize round trip: got %+v err %v", got, err)
	}
	if got, err := c.Promotable(PromotableReq{MinFrequency: 5}); err != nil || len(got.Candidates) != 1 || got.Candidates[0].Frequency != 5 {
		t.Fatalf("Promotable round trip: got %+v err %v", got, err)
	}
	if got, err := c.Observe(ObserveReq{User: "alice"}); err != nil || !got.Accepted || got.Reason != "alice" {
		t.Fatalf("Observe round trip: got %+v err %v", got, err)
	}
	// Zero-arg method — the exact shape net/rpc would drop with an unexported arg.
	if got, err := c.Stats(""); err != nil || got.Active != 11 || got.Durable != 22 {
		t.Fatalf("Stats round trip: got %+v err %v", got, err)
	}
	// Zero-arg method.
	if got, err := c.Health(); err != nil || !got.OK || got.WatcherModel != "m" {
		t.Fatalf("Health round trip: got %+v err %v", got, err)
	}
}

// errMemory returns a distinctive error from a zero-arg method so we can assert
// the error string survives the rpc.ServerError round trip.
type errMemory struct{ echoMemory }

func (errMemory) Stats(string) (Stats, error) { return Stats{}, errors.New("boom: stats failed") }

func TestRPCRoundTripError(t *testing.T) {
	client := newRPCPair(t, &memoryRPCServer{Impl: errMemory{}})
	c := &memoryRPCClient{client: client}

	_, err := c.Stats("")
	if err == nil {
		t.Fatal("Stats: expected non-nil error from impl, got nil")
	}
	if !strings.Contains(err.Error(), "boom: stats failed") {
		t.Fatalf("Stats error message not preserved: got %q", err.Error())
	}
}
