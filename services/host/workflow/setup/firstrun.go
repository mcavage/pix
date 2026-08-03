package setup

import (
	"fmt"
	"io"
	"os"

	"pix/host/cli"
	"pix/host/config"
)

// MaybeFirstRun detects a fresh host (no config file yet) and points the user at
// setup. `pix run` NEVER onboards on its own; the guided flow is the
// explicit `pix setup`. This only nudges from the bare `pix` status
// command so a fresh host is not left guessing. It returns false (never handles
// the invocation) so the caller always continues to show status.
//
// Rules: trigger on config-FILE absence (not empty fields — defaults are
// legitimate). This never launches a sandbox and never blocks: it prints one
// line and returns.
func MaybeFirstRun() bool {
	if configExists() {
		return false
	}
	return FirstRunFlow(os.Stdin, os.Stdout, cli.IsTTY(os.Stdin))
}

// configExists reports whether the config file is present on disk.
func configExists() bool {
	_, err := os.Stat(config.Path())
	return err == nil
}

// FirstRunFlow is the testable core. It only PRINTS a nudge (setup is explicit),
// never runs anything, and always returns false so the caller continues. in/tty
// are accepted for signature stability but no prompt is read.
func FirstRunFlow(in io.Reader, out io.Writer, tty bool) bool {
	_ = in
	_ = tty
	fmt.Fprintln(out, "pix — first run (no config yet).")
	fmt.Fprintln(out, "Set up:  pix setup   — configures the host, then hands off to an agent to finish.")
	fmt.Fprintln(out, "Or just:  pix run    — skip setup and start working now.")
	return false
}
