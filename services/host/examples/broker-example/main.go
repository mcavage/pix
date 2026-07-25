// Command broker-example is a REFERENCE example of an external credential
// broker: how an operator ships their OWN out-of-process broker binary that
// provides a credential broker without ever linking into the public tree. The
// public tree ships NO built-in broker (the seam is dormant); this is the
// artifact that fills it.
//
// It is NOT wired into `serve` by default. To use it, a user points
// ~/.config/pix/config.toml's [plugins.broker] at this binary:
//
//	[plugins.broker]
//	impl = "example"
//	path = "/path/to/broker-example"
//	sha  = "<sha256 of the binary>"
//
// The supervisor (services/host/serve_plugin.go) then sha-verifies and launches
// it as a go-plugin subprocess, dispenses the "broker" capability, and backs the
// stable /token shim (brokerProxyMux) with it. This example mints a FAKE
// short-lived token so it can be proven end-to-end — it NEVER returns a real secret.
package main

import (
	goplugin "github.com/hashicorp/go-plugin"

	"pix/host/plugin"
)

// exampleBroker is a trivial CredentialBroker: Mint returns a fake short-lived
// token derived from the audience, Check always succeeds, and Describe reports
// the broker's shape to the supervisor.
type exampleBroker struct{}

func (exampleBroker) Mint(audience string, scopes []string) (plugin.Token, error) {
	return plugin.Token{AccessToken: "example-token-" + audience, TokenType: "Bearer", ExpiresIn: 300}, nil
}

func (exampleBroker) Check() error { return nil }

func (exampleBroker) Describe() (plugin.BrokerInfo, error) {
	return plugin.BrokerInfo{Name: "example", DefaultPort: 0, AuthHeader: "Authorization", RequiresHostCLI: false}, nil
}

func main() {
	plugin.Serve(map[string]goplugin.Plugin{"broker": &plugin.BrokerPlugin{Impl: exampleBroker{}}})
}
