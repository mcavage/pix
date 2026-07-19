package main

import (
	"fmt"
	"io"
	"os"

	"pi-stack/host/config"
)

// maybeFirstRun detects a fresh host (no config file yet) and points the user at
// the real onboarding path. Onboarding itself is IN-SESSION (the agent offers it
// on the first `pi-stack run`); this only nudges from the bare `pi-stack` status
// command so a fresh host is not left guessing. It returns false (never handles
// the invocation) so the caller always continues to show status.
//
// Rules: trigger on config-FILE absence (not empty fields — defaults are
// legitimate). This never launches a sandbox and never blocks: it prints one
// line and returns.
func maybeFirstRun() bool {
	if configExists() {
		return false
	}
	return firstRunFlow(os.Stdin, os.Stdout, isTTY(os.Stdin))
}

// configExists reports whether the config file is present on disk.
func configExists() bool {
	_, err := os.Stat(config.Path())
	return err == nil
}

// firstRunFlow is the testable core. It only PRINTS a nudge (onboarding is
// in-session), never runs anything, and always returns false so the caller
// continues. in/tty are accepted for signature stability but no prompt is read.
func firstRunFlow(in io.Reader, out io.Writer, tty bool) bool {
	_ = in
	_ = tty
	fmt.Fprintf(out, "pi-stack — first run. No config at %s yet.\n", config.Path())
	fmt.Fprintln(out, "Run `pi-stack run` to start; the agent offers to onboard you (opt-in).")
	fmt.Fprintln(out, "Or `pi-stack onboard` for host-side/CI config.")
	return false
}
