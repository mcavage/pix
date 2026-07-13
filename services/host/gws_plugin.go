// gws credential-broker adapter.
//
// Wraps the existing gws-token service (gwstoken.go) in the go-plugin
// CredentialBroker interface (plugin/interfaces.go), so the same "keep the
// long-lived Google OAuth refresh credential on the host, mint a short-lived
// bearer, run the real `gws` in the VM" pattern is reachable over the plugin
// transport as well as the HTTP :11441 front-end.
//
// This file adds NO new behaviour: Mint delegates to gwsTokenSvc.mint() and
// Check to gwsTokenCheck(). The raw long-lived credential (client_id /
// client_secret / refresh_token) NEVER crosses the interface — Mint returns
// only a short-lived plugin.Token, exactly like /token returns only a bearer.

package main

import (
	goplugin "github.com/hashicorp/go-plugin"

	"pi-stack/host/plugin"
)

// gwsBrokerAdapter implements plugin.CredentialBroker on top of gwsTokenSvc.
type gwsBrokerAdapter struct {
	svc *gwsTokenSvc
}

// newGwsBrokerAdapter builds an adapter backed by a fresh token service (its
// mint cache is per-instance, matching gwsTokenMux()'s single-svc lifetime).
func newGwsBrokerAdapter() *gwsBrokerAdapter {
	return &gwsBrokerAdapter{svc: &gwsTokenSvc{}}
}

// Mint returns a short-lived bearer via the existing mint path. gws ignores
// audience and scopes today (the credential's scopes are fixed at
// `gws auth login` time); a future warehouse/CRM broker would honour them.
func (a *gwsBrokerAdapter) Mint(audience string, scopes []string) (plugin.Token, error) {
	bearer, err := a.svc.mint()
	if err != nil {
		return plugin.Token{}, err
	}
	return plugin.Token{
		AccessToken: bearer.AccessToken,
		TokenType:   bearer.TokenType,
		ExpiresIn:   bearer.ExpiresIn,
	}, nil
}

// Check is the serve preflight: confirm the host `gws` is authenticated.
func (a *gwsBrokerAdapter) Check() error {
	return gwsTokenCheck()
}

// Describe reports the broker's shape to the supervisor.
func (a *gwsBrokerAdapter) Describe() (plugin.BrokerInfo, error) {
	return plugin.BrokerInfo{
		Name:            "gws",
		DefaultPort:     11441,
		AuthHeader:      "Authorization",
		RequiresHostCLI: true,
	}, nil
}

// servePluginBroker runs the gws broker as a go-plugin self-exec entry. name is
// the plugin map key the supervisor dispenses under (e.g. "broker").
func servePluginBroker(name string) {
	plugin.Serve(map[string]goplugin.Plugin{
		name: &plugin.BrokerPlugin{Impl: newGwsBrokerAdapter()},
	})
}
