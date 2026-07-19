package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"pi-stack/host/config"
)

// maybeFirstRun detects a fresh host (no config file yet) and points the user at
// setup. `pi-stack run` NEVER onboards on its own; the guided flow is the
// explicit `pi-stack setup`. This only nudges from the bare `pi-stack` status
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

// firstRunFlow is the testable core. It only PRINTS a nudge (setup is explicit),
// never runs anything, and always returns false so the caller continues. in/tty
// are accepted for signature stability but no prompt is read.
func firstRunFlow(in io.Reader, out io.Writer, tty bool) bool {
	_ = in
	_ = tty
	fmt.Fprintln(out, "pi-stack — first run (no config yet).")
	fmt.Fprintln(out, "Set up:  pi-stack setup   — configures the host, then hands off to an agent to finish.")
	fmt.Fprintln(out, "Or just:  pi-stack run    — skip setup and start working now.")
	return false
}

// warnUnconfigured prints a ONE-LINE, non-blocking heads-up before `pi-stack
// run` launches on a host that was never set up (no config file). It never
// prompts and never delays — `run` goes straight into the session. The guided
// path is the explicit `pi-stack setup`. Silent once the host has a config.
func warnUnconfigured(env shellEnv, out io.Writer) {
	if configExists() {
		return
	}
	var missing []string
	if env.dial == nil || !env.dial(memoryPortDefault) {
		missing = append(missing, "memory service down")
	}
	detail := ""
	if len(missing) > 0 {
		detail = " (" + strings.Join(missing, ", ") + ")"
	}
	fmt.Fprintf(out, "pi-stack: no host setup yet%s — run `pi-stack setup` for the guided flow; continuing anyway.\n", detail)
}
