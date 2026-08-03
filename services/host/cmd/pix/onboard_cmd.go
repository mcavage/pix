// onboard_cmd.go — the composition onboarding cannot do for itself: the paired
// pix-host binary, MCP registration, and the shipped-catalog readiness gate.
// Two of the three reach capabilities onboarding may not import, which is why
// they are supplied rather than called.
package main

import (
	"pix/host/workflow/onboard"
	"pix/host/workflow/setup"
)

// onboardDeps is the real wiring, in one place so the two launch paths that
// reconcile onboarding (pix run, pix setup) cannot drift apart.
func onboardDeps() onboard.Deps {
	return onboard.Deps{
		HostBinary:    hostBinaryResolver,
		Register:      registerServers,
		VerifyCatalog: setup.VerifyCatalogMCPReady,
	}
}
