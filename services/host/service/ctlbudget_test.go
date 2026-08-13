package service

import (
	"strings"
	"testing"
	"time"
)

// TestRealCmdRunner_BoundsAWedgedChild is the regression for the second half of
// the `pix pack use` hang. `launchctl kickstart -k` kills the running daemon and
// WAITS for it to die, so a `pix-host serve` wedged in shutdown blocked the CLI
// forever — inside propagateConfig, whose contract is to warn and move on.
//
// PropagateConfig already had the right answer for a restart that fails ("could
// not restart the managed pix service … restart it manually"); it just never
// got to give it, because the failure never arrived. Now it does.
func TestRealCmdRunner_BoundsAWedgedChild(t *testing.T) {
	prev := ctlTimeout
	ctlTimeout = 200 * time.Millisecond
	defer func() { ctlTimeout = prev }()

	start := time.Now()
	_, err := realCmdRunner("sleep", "30")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a child that outruns its budget must be reported as a failure")
	}
	if !strings.Contains(err.Error(), "did not finish within") {
		t.Errorf("the error must say the budget was exceeded, got: %v", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("took %s — the child was not actually bounded", elapsed)
	}
}

// TestRealCmdRunner_PassesThroughANormalRun: the bound must not change what a
// command that finishes reports, or every service-control caller's tests would
// be measuring the shim instead of the argv.
func TestRealCmdRunner_PassesThroughANormalRun(t *testing.T) {
	out, err := realCmdRunner("echo", "kickstarted")
	if err != nil {
		t.Fatalf("a bounded runner must still run: %v", err)
	}
	if !strings.Contains(out, "kickstarted") {
		t.Errorf("output not passed through, got %q", out)
	}
}
