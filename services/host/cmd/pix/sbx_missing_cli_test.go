package main

// sbx_missing_cli_test.go — DX finding 5: `pix ls` and `pix rm` must exit the
// SAME rpc.ExitServiceDown (3) `pix mcp` already exits when sbx is not on
// PATH, not the generic 1 a plain error falls through to. See
// workflow/launch/sbx_missing_test.go for the error-shape half of this
// contract (mcp.ErrSbxUnavailable + the exact health.SbxInstallFix message).

import (
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/health"
	"pix/host/rpc"
	"pix/host/sys"
)

func TestRunLs_AbsentSbxExitsServiceDown(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // nothing on PATH, including no sbx
	d, out, errb := rootDeps()
	d.Sys = sys.Real{}
	err := runRootParse([]string{"ls"}, d)
	if got, want := cli.ExitCode(err), rpc.ExitServiceDown; got != want {
		t.Errorf("exit = %d, want %d; stdout=%q stderr=%q", got, want, out.String(), errb.String())
	}
	if !strings.Contains(errb.String(), health.SbxInstallFix) {
		t.Errorf("ls must print the exact install fix on stderr, got stderr=%q", errb.String())
	}
}

func TestRunRm_AbsentSbxExitsServiceDown(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	d, out, errb := rootDeps()
	d.Sys = sys.Real{}
	err := runRootParse([]string{"rm", "pix-x"}, d)
	if got, want := cli.ExitCode(err), rpc.ExitServiceDown; got != want {
		t.Errorf("exit = %d, want %d; stdout=%q stderr=%q", got, want, out.String(), errb.String())
	}
	if !strings.Contains(errb.String(), health.SbxInstallFix) {
		t.Errorf("rm must print the exact install fix on stderr, got stderr=%q", errb.String())
	}
}
