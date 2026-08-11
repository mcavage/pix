package pack

import "testing"

// TestSetupInteractivityIsMeasured is the regression for a hardcoded constant.
//
// adoptForSetup passed `false` for interactive, always. The cost fell entirely on
// NEW users, which is why it survived every review: a step whose remediation
// needs a terminal — `slack-mcp auth login`, `gog auth setup --login`, any
// browser authorization — was refused on a run that had JUST prompted the user
// for a y/N and two 1Password references. The refusal then told them to "re-run
// without --yes/--non-interactive", flags they had never passed, so there was no
// next step to take.
//
// A new user has no Slack grant by definition. So that step could never pass, its
// fix could never run, and `pix setup --pack` could never complete. Every peer
// onboarded would have hit it.
func TestSetupInteractivityIsMeasured(t *testing.T) {
	for _, c := range []struct {
		name           string
		assumeYes, tty bool
		want           bool
		why            string
	}{
		{"a terminal, no --yes", false, true, true,
			"the case that was broken: a real user at a real prompt must be able to authorize"},
		{"--yes in a terminal", true, true, false,
			"--yes means ask me nothing, and a browser authorization is a question"},
		{"no terminal, no --yes", false, false, false,
			"nobody is there to answer, so prompting would hang a CI job"},
		{"--yes and no terminal", true, false, false, "both reasons apply"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := setupInteractivity(c.assumeYes, c.tty); got != c.want {
				t.Errorf("setupInteractivity(assumeYes=%v, tty=%v) = %v, want %v — %s",
					c.assumeYes, c.tty, got, c.want, c.why)
			}
		})
	}
}
