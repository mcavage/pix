package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"pi-stack/host/config"
)

// maybeFirstRun detects a fresh host (no config file yet) and steers the user
// into onboarding instead of doing the requested action blind. It returns true
// when it HANDLED the invocation (ran setup), so the caller returns early.
//
// Rules (crew consensus): trigger on config-FILE absence (not empty fields —
// defaults are legitimate). On a TTY, offer setup [Y/n]; declining continues to
// the original command. Non-interactive: print the one-liner and continue
// (never block automation).
func maybeFirstRun() bool {
	if configExists() {
		return false
	}
	return firstRunFlow(os.Stdin, os.Stdout, isTTY(os.Stdin), runSetupCmd)
}

// configExists reports whether the config file is present on disk.
func configExists() bool {
	_, err := os.Stat(config.Path())
	return err == nil
}

// firstRunFlow is the testable core. setup is injected so tests don't shell out.
func firstRunFlow(in io.Reader, out io.Writer, tty bool, setup func([]string)) bool {
	fmt.Fprintf(out, "pi-stack — first run. No config at %s yet.\n", config.Path())
	if !tty {
		fmt.Fprintln(out, "Run `pi-stack setup` to configure the stack (keys, memory, knowledge, gog).")
		return false
	}
	fmt.Fprint(out, "Set you up now? [Y/n]: ")
	reader := bufio.NewReader(in)
	line, _ := reader.ReadString('\n')
	ans := strings.ToLower(strings.TrimSpace(line))
	if ans == "n" || ans == "no" {
		fmt.Fprintln(out, "Skipped — run `pi-stack setup` anytime.")
		return false
	}
	setup(nil)
	return true
}
