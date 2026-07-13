// Package plugin is the go-plugin FOUNDATION for pi-stack's host binary.
//
// Today the host (`pi-stack-host`, module pi-stack/host) is one statically
// linked binary whose only extension seam is compile-time init() registration
// (see ../main.go's extraCommands / extraServiceFactories). We are moving that
// seam from link time to process-launch time using hashicorp/go-plugin, so a
// third party can OVERRIDE a host capability with their own out-of-process
// plugin binary without ever linking into the public tree.
//
// This package is intentionally ADDITIVE: it defines the shared handshake, the
// three capability interfaces (MemoryStore, CredentialBroker, McpServer) derived
// from the real services (../memory.go, ../gwstoken.go, ../slack.go), and the
// net/rpc go-plugin wiring for each. A later unit wires the existing services
// onto these interfaces; nothing here imports or mutates package main.
//
// Transport note: this foundation uses go-plugin's net/rpc transport (no codegen
// needed, unlike gRPC which requires protoc). The arch doc's target is gRPC +
// AutoMTLS over a unix socket; that upgrade is deferred and flagged with
// TODO(gRPC/AutoMTLS) at the relevant seams.
package plugin

import goplugin "github.com/hashicorp/go-plugin"

// ProtocolVersion gates host <-> plugin compatibility. Bump it on ANY breaking
// change to an interface below; go-plugin then refuses a skewed plugin at launch
// with a clear error instead of failing mysteriously at call time.
const ProtocolVersion = 1

// Handshake is the single shared handshake for every pi-stack plugin kind.
//
// The MagicCookie is a cheap "is this actually one of our plugins" guard: a
// binary launched as a plugin must have PI_STACK_PLUGIN set to the expected
// value in its environment, or go-plugin refuses to treat it as a plugin (this
// stops a random exec being mistaken for one). The ProtocolVersion is the real
// compatibility gate.
var Handshake = goplugin.HandshakeConfig{
	ProtocolVersion:  ProtocolVersion,
	MagicCookieKey:   "PI_STACK_PLUGIN",
	MagicCookieValue: "pi-stack.v1",
}

// PluginMap maps a capability name to the plugin.Plugin used on the CLIENT side
// (the kernel/supervisor). The zero-value plugin types carry a nil Impl, which
// is correct for a client: Impl is only consulted by Server(), which the client
// never calls. A plugin binary serving an implementation builds its own map with
// a populated Impl (see Serve).
var PluginMap = map[string]goplugin.Plugin{
	"memory": &MemoryPlugin{},
	"broker": &BrokerPlugin{},
	"mcp":    &McpPlugin{},
}
