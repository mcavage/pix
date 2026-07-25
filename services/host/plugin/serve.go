package plugin

import goplugin "github.com/hashicorp/go-plugin"

// Serve runs a plugin binary, serving the given implementations under the shared
// Handshake. A built-in self-exec (`pix plugin memory`) or a third-party
// binary calls this in its main() with a single-entry map, e.g.
//
//	plugin.Serve(map[string]plugin.Plugin{"memory": &plugin.MemoryPlugin{Impl: store}})
//
// Serve blocks until the host closes the connection. If PIX_PLUGIN is not
// set to the expected magic-cookie value in the environment, go-plugin prints
// the "this is a plugin, not meant to be run directly" notice and exits — that
// is the intended behaviour for a plugin binary run by hand.
//
// TODO(gRPC/AutoMTLS): once the transport moves to gRPC, add
// GRPCServer + a plugin.ServeConfig{ GRPCServer: ... } here and enable AutoMTLS.
func Serve(impls map[string]goplugin.Plugin) {
	goplugin.Serve(&goplugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         impls,
	})
}
