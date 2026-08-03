// Moved from workflow/doctor: the subject is cmd/pix's gog setup, which the
// doctor report only reads.
package main

import (
	"strings"
	"testing"

	"pix/host/hostenv/hostenvtest"
	"pix/host/workflow/doctor"
	"pix/host/workflow/gworkspace"
)

func TestRegisteredGogCommand_CurrentSbxPlainTable(t *testing.T) {
	regCmd := opWrappedGog(gogOpRefs, gogAcct)
	f := hostenvtest.Env{
		Present:  map[string]bool{"sbx": true, "op": true, "gog": true},
		EnvVars:  map[string]string{"PIX_CONFIG": gogCfgFile},
		StatFile: map[string]bool{gogOpRefs: true},
		Output: map[string]string{
			"sbx mcp ls": "NAME  TYPE   URL/COMMAND\n" +
				"google-workspace   local  " + regCmd + "\n",
		},
	}
	env := f.Build()
	argv, ok := doctor.RegisteredGogCommand(env)
	if !ok {
		t.Fatal("current sbx plain table carries the complete command and must be readable")
	}
	if got := strings.Join(argv, " "); got != regCmd {
		t.Fatalf("registered argv = %q, want %q", got, regCmd)
	}
	snap := gworkspace.SnapshotGogRegistration(env)
	if snap.State != gworkspace.GogRegPresent {
		t.Fatalf("gog setup snapshot state = %v, want present", snap.State)
	}
	if got := strings.Join(snap.Argv, " "); got != regCmd {
		t.Fatalf("snapshot argv = %q, want %q", got, regCmd)
	}
}

// TestDoctor_GogRegisteredCommandJSON: sbx exposes the registration only via
// `sbx mcp ls -o json`; doctor parses command+args and probes it.
