// Package plugin is the go-plugin FOUNDATION for pix's host binary.
//
// The host (`pix-host`, module pix/host) is one statically linked
// binary. Its extension seam runs at process-launch time, not link time,
// using hashicorp/go-plugin: a third party can OVERRIDE a host capability with
// their own out-of-process plugin binary (SHA-pinned via config's [plugins.*])
// without ever linking into the public tree.
//
// This package is intentionally ADDITIVE: it defines the shared handshake, the
// three capability interfaces (MemoryStore, CredentialBroker, McpServer) derived
// from the real services (../memory.go, ../slack.go), and the
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
// interface change; go-plugin then refuses a skewed plugin at launch.
// v2 (U07): Stats takes a profile, Hit carries CreatedAt, Health carries
// CaptureReason — fields :11435 has always returned, now that memory is always
// plugin-backed.
const ProtocolVersion = 2

// Handshake is the single shared handshake for every pix plugin kind.
//
// The MagicCookie is a cheap "is this actually one of our plugins" guard: a
// binary launched as a plugin must have PIX_PLUGIN set to the expected
// value in its environment, or go-plugin refuses to treat it as a plugin (this
// stops a random exec being mistaken for one). The ProtocolVersion is the real
// compatibility gate.
var Handshake = goplugin.HandshakeConfig{
	ProtocolVersion:  ProtocolVersion,
	MagicCookieKey:   "PIX_PLUGIN",
	MagicCookieValue: "pix.v1",
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
