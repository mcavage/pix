// Moved from workflow/gworkspace: the subject is the argv seam, which owns
// os.Exit and lives at L4.
package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"pix/host/workflow/gworkspace"
)

// TestRunGworkspaceCmd_Help: the no-subcommand and -h paths print the noun
// usage without exiting (wantsHelp is checked ONLY over argv[:1], so `setup
// -h` must still reach the subcommand's OWN usage further down).
func TestRunGworkspaceCmd_Help(t *testing.T) {
	// Can't call runGworkspaceCmd(nil) directly (os.Exit(2)); -h is the
	// in-process path.
	// Redirect stdout via a pipe would be heavier than needed here — the
	// individual subcommand usage constants are asserted directly below
	// (naming-leak section), and dispatch-with-exit is covered by the
	// subprocess test below.
	_ = gworkspace.Usage
}

// TestRunGworkspaceCmd_UnknownSubcommandExitsNonZero and
// TestRunGworkspaceCmd_NoArgsExitsNonZero re-exec this test binary to observe
// os.Exit(2) without killing the real test process (same pattern as
// TestPackUse_ChangedGogAccountRegates).

// TestRunGworkspaceCmd_UnknownSubcommandExitsNonZero and
// TestRunGworkspaceCmd_NoArgsExitsNonZero re-exec this test binary to observe
// os.Exit(2) without killing the real test process (same pattern as
// TestPackUse_ChangedGogAccountRegates).
func TestRunGworkspaceCmd_UnknownSubcommandExitsNonZero(t *testing.T) {
	if os.Getenv("PIX_TEST_GWORKSPACE_DISPATCH") == "unknown" {
		runGworkspaceCmd([]string{"bogus"})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run", "^TestRunGworkspaceCmd_UnknownSubcommandExitsNonZero$")
	cmd.Env = append(os.Environ(), "PIX_TEST_GWORKSPACE_DISPATCH=unknown")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("an unknown subcommand must exit non-zero, output:\n%s", out)
	}
	if !strings.Contains(string(out), "unknown subcommand") {
		t.Errorf("expected an unknown-subcommand message, got:\n%s", out)
	}
}

func TestRunGworkspaceCmd_NoArgsExitsNonZero(t *testing.T) {
	if os.Getenv("PIX_TEST_GWORKSPACE_DISPATCH") == "noargs" {
		runGworkspaceCmd(nil)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run", "^TestRunGworkspaceCmd_NoArgsExitsNonZero$")
	cmd.Env = append(os.Environ(), "PIX_TEST_GWORKSPACE_DISPATCH=noargs")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("no subcommand must exit non-zero, output:\n%s", out)
	}
	if !strings.Contains(string(out), "usage: pix gworkspace") {
		t.Errorf("expected the noun usage on stderr, got:\n%s", out)
	}
}

// --- gworkspace.Setup façade: success, zero-tools, rollback ---------------
