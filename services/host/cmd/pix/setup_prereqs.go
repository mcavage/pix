package main

import (
	"fmt"
	"io"
	"runtime"
	"strings"
)

var setupHostOS = runtime.GOOS

// ensureSetupPrereqs installs only Pix's two core host tools on macOS. Optional
// capabilities such as Ollama, gh, and gog are intentionally absent here.
// Interactive setup asks once for the package category; unattended setup never
// installs and returns exact commands instead.
func ensureSetupPrereqs(env shellEnv, in io.Reader, out io.Writer, interactive bool) error {
	return ensureSetupPrereqsFor(env, in, out, interactive, true)
}

// ensureSetupPrereqsFor lets setup defer installing 1Password until after
// explicit packs have contributed inference. A pack using sbx-session auth
// must not make a keyless user install or sign into op.
func ensureSetupPrereqsFor(env shellEnv, in io.Reader, out io.Writer, interactive, requireOp bool) error {
	var missing []string
	names := []string{"sbx"}
	if requireOp {
		names = append(names, "op")
	}
	for _, name := range names {
		if env.lookPath == nil {
			missing = append(missing, name)
			continue
		}
		if _, err := env.lookPath(name); err != nil {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	commands := map[string]string{
		"sbx": sbxInstallHint,
		"op":  "brew install 1password-cli",
	}
	fix := func() string {
		var lines []string
		for _, name := range missing {
			lines = append(lines, "  "+commands[name])
		}
		return strings.Join(lines, "\n")
	}
	if setupHostOS != "darwin" || env.lookPath == nil {
		return fmt.Errorf("missing required host tool(s): %s\n%s", strings.Join(missing, ", "), fix())
	}
	if _, err := env.lookPath("brew"); err != nil {
		return fmt.Errorf("missing required host tool(s): %s; Homebrew is unavailable\n%s", strings.Join(missing, ", "), fix())
	}
	if !interactive || in == nil {
		return fmt.Errorf("missing required host tool(s): %s\n%s", strings.Join(missing, ", "), fix())
	}
	fmt.Fprintf(out, "Pix needs %s. Install with Homebrew now? [Y/n]: ", strings.Join(missing, " and "))
	if !confirmYN(in, out, "", true) {
		return fmt.Errorf("required host tools were not installed\n%s", fix())
	}
	if env.runInteractive == nil {
		return fmt.Errorf("cannot run Homebrew installer\n%s", fix())
	}
	var formulae []string
	for _, name := range missing {
		switch name {
		case "sbx":
			formulae = append(formulae, "docker/tap/sbx@nightly")
		case "op":
			formulae = append(formulae, "1password-cli")
		}
	}
	if err := env.runInteractive("brew", append([]string{"install"}, formulae...)...); err != nil {
		return fmt.Errorf("installing required host tools: %w", err)
	}
	for _, name := range missing {
		if _, err := env.lookPath(name); err != nil {
			return fmt.Errorf("Homebrew completed but %s is still not on PATH; restart the shell and re-run pix setup", name)
		}
	}
	return nil
}

// ensureSetupSbxSession drives Docker's own login flow when the sbx control
// plane is not usable. It creates no Pix account and stores no Docker token.
func ensureSetupSbxSession(env shellEnv, out io.Writer, interactive bool) error {
	if _, timedOut, err := probeRun(env, "sbx", "ls"); err == nil && !timedOut {
		return nil
	}
	if !interactive || env.runInteractive == nil {
		return fmt.Errorf("Docker Sandboxes is not signed in or reachable; run: sbx login")
	}
	fmt.Fprintln(out, "Docker Sandboxes needs authorization. Continuing with the official `sbx login` flow.")
	if err := env.runInteractive("sbx", "login"); err != nil {
		return fmt.Errorf("sbx login failed: %w", err)
	}
	if _, timedOut, err := probeRun(env, "sbx", "ls"); err != nil || timedOut {
		return fmt.Errorf("sbx login completed but Docker Sandboxes is still unreachable; run: sbx diagnose")
	}
	return nil
}
