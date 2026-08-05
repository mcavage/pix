package provision

import (
	"fmt"
	"io"
	"os"

	"pix/host/config"
)

// MaybeFirstRun detects a fresh host (no config FILE yet — empty fields are
// legitimate defaults) and points the user at setup. `pix run` NEVER onboards on
// its own; the guided flow is the explicit `pix setup`. This only nudges from the
// bare `pix` status command so a fresh host is not left guessing, and it returns
// false — never handling the invocation — so the caller always shows status.
// It takes an injected writer rather than reaching for os.Stdout itself: L3
// returns/writes through what its L4 caller supplies, so only cmd/pix owns the
// process's actual stdout.
func MaybeFirstRun(out io.Writer) bool {
	if _, err := os.Stat(config.Path()); err == nil {
		return false
	}
	return FirstRunFlow(out)
}

// FirstRunFlow is the testable core: it PRINTS a nudge and nothing else. It
// takes no reader and no TTY flag because it asks nothing — a prompt seam here
// was an invitation to make first run interactive again.
func FirstRunFlow(out io.Writer) bool {
	fmt.Fprintln(out, "pix — first run (no config yet).")
	fmt.Fprintln(out, "Set up:  pix setup   — configures the host, then hands off to an agent to finish.")
	fmt.Fprintln(out, "Or just:  pix run    — skip setup and start working now.")
	return false
}
