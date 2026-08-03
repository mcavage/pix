// setup_gwflags_test.go — setup.CheckGoogleWorkspaceFlags is setup's flag
// validation, so its test lives with it.
package main

import (
	"testing"

	"pix/host/workflow/onboard"
	"pix/host/workflow/setup"
)

// TestCheckGoogleWorkspaceFlags_RequireOptIn covers AC-P0-312: --account and
// --credentials are Google Workspace inputs and are REJECTED (before any
// probe or mutation) unless --google-workspace is also set.
func TestCheckGoogleWorkspaceFlags_RequireOptIn(t *testing.T) {
	cases := []struct {
		name    string
		opts    onboard.Opts
		wantErr bool
	}{
		{"account alone", onboard.Opts{Account: "a@b.com"}, true},
		{"credentials alone", onboard.Opts{Credentials: "/x.json"}, true},
		{"both alone", onboard.Opts{Account: "a@b.com", Credentials: "/x.json"}, true},
		{"account with opt-in", onboard.Opts{Account: "a@b.com", GoogleWorkspace: true}, false},
		{"credentials with opt-in", onboard.Opts{Credentials: "/x.json", GoogleWorkspace: true}, false},
		{"opt-in alone", onboard.Opts{GoogleWorkspace: true}, false},
		{"neither", onboard.Opts{}, false},
	}
	for _, c := range cases {
		err := setup.CheckGoogleWorkspaceFlags(c.opts)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: setup.CheckGoogleWorkspaceFlags(%+v) = %v, wantErr=%v", c.name, c.opts, err, c.wantErr)
		}
	}
}

// --- naming-leak: the façade's own strings hold the public contract -------

// TestGworkspaceUsage_NamingContract pins gworkspace.go's own naming rule: the
// CLI noun is `gworkspace`, the MCP name is config.GWServerName ("google-workspace"),
// and the external binary's name ("gog") appears ONLY in the dependency
// install/upgrade lines — never as a verb, a flag, or a registered/display
// name in any of the four usage blocks.
