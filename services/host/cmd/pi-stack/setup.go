package main

import (
	"fmt"
	"io"
	"os"

	"pi-stack/host/config"
)

// setup is a RESUMABLE, IDEMPOTENT onboarding wizard. It reuses doctor's probes
// to detect what's already done and only surfaces what's missing, in dependency
// order, reusing doctor's `TODO: <command>` grammar. Running it twice is safe:
// nothing is clobbered (config.Seed never overwrites, config.EnsureToken is
// idempotent) and it picks up wherever you left off.
//
// It MUST be non-interactive-safe: with no TTY on stdin it prints the ordered
// checklist and exits rather than blocking on input.

// setupIO carries the streams + a TTY flag so tests can exercise the non-TTY
// path hermetically.
type setupIO struct {
	in    io.Reader
	out   io.Writer
	isTTY bool
}

// runSetup performs the (side-effect-light) parts of setup that are safe and
// idempotent — seeding the config and minting the broker token — and prints an
// ordered checklist of everything the user still has to do on the host. The two
// write actions go through the injected seed/ensureToken funcs so tests stay
// hermetic. Returns the checklist steps it emitted (for assertions).
func runSetup(cfg *config.Config, env shellEnv, sio setupIO,
	seed func(string) (bool, error), ensureToken func() (string, error)) []string {

	fmt.Fprintln(sio.out, "pi-stack setup — dependency-ordered onboarding. Idempotent; safe to re-run.")
	fmt.Fprintln(sio.out)

	var steps []string
	step := func(done bool, label, todo string) {
		mark := "✗"
		if done {
			mark = "✓"
		}
		if done {
			fmt.Fprintf(sio.out, "  %s %s\n", mark, label)
			return
		}
		fmt.Fprintf(sio.out, "  %s %s\n      TODO: %s\n", mark, label, todo)
		steps = append(steps, todo)
	}

	// 0. Prerequisites: sbx + docker on the host.
	fmt.Fprintln(sio.out, "Prerequisites:")
	_, sbxErr := env.lookPath("sbx")
	step(sbxErr == nil, "sbx CLI", "install Docker Sandboxes (sbx) — https://docs.docker.com/ai/sandboxes")
	_, dockerErr := env.lookPath("docker")
	step(dockerErr == nil, "docker", "install Docker — https://docs.docker.com/get-docker")
	fmt.Fprintln(sio.out)

	// 1. Provider secrets (needs sbx). Reuse doctor's probe.
	fmt.Fprintln(sio.out, "Provider keys:")
	sbxOut, sbxOK := "", false
	if sbxErr == nil {
		if out, err := env.run("sbx", "secret", "ls"); err == nil {
			sbxOut, sbxOK = out, true
		}
	}
	for _, key := range []string{"anthropic", "openai", "google", "github"} {
		c := secretCheck(key, key, sbxOut, sbxOK)
		step(c.state == stateOK, key, c.todo)
	}
	fmt.Fprintln(sio.out)

	// 2. Ollama + the configured models.
	fmt.Fprintln(sio.out, "Local models (optional):")
	_, ollamaErr := env.lookPath("ollama")
	step(ollamaErr == nil, "ollama installed", "install ollama — https://ollama.com")
	modelOut, modelOK := "", false
	if ollamaErr == nil {
		if out, err := env.run("ollama", "list"); err == nil {
			modelOut, modelOK = out, true
		}
	}
	for _, m := range []string{cfg.MemoryWatcherModel, cfg.MemoryEmbedModel} {
		done := ollamaErr == nil && modelOK && modelPulled(modelOut, m)
		step(done, "model "+m, "ollama pull "+m)
	}
	fmt.Fprintln(sio.out)

	// 3. gws auth.
	fmt.Fprintln(sio.out, "Google Workspace (optional):")
	_, gwsErr := env.lookPath("gws")
	step(gwsErr == nil, "gws CLI", "install gws, then `gws auth login`")
	fmt.Fprintln(sio.out)

	// 4. 1Password + op-refs for MCP creds (only relevant if MCP is configured).
	if len(cfg.MCP) > 0 {
		fmt.Fprintln(sio.out, "MCP credentials:")
		_, opErr := env.lookPath("op")
		step(opErr == nil, "1Password CLI (op)", "install the 1Password CLI — https://developer.1password.com/docs/cli")
		step(false, "op-refs.env",
			"cp config/op-refs.env.example config/op-refs.env && fill in your refs, then `pi-stack mcp register`")
		fmt.Fprintln(sio.out)
	}

	// 5. Config file — seed only if absent (never clobber).
	fmt.Fprintln(sio.out, "Local config:")
	path := config.Path()
	if wrote, err := seed(path); err != nil {
		fmt.Fprintf(sio.out, "  ✗ config %s — error: %v\n", path, err)
	} else if wrote {
		fmt.Fprintf(sio.out, "  ✓ wrote default config %s\n", path)
	} else {
		fmt.Fprintf(sio.out, "  ✓ config already present %s (left as-is)\n", path)
	}

	// 6. Broker token — idempotent mint.
	if _, err := ensureToken(); err != nil {
		fmt.Fprintf(sio.out, "  ✗ broker token — error: %v\n", err)
	} else {
		fmt.Fprintf(sio.out, "  ✓ broker token ready %s\n", config.TokenPath())
	}
	fmt.Fprintln(sio.out)

	// Closing: the two commands that matter + the verifier.
	if len(steps) == 0 {
		fmt.Fprintln(sio.out, "All set. Start the stack:")
	} else {
		fmt.Fprintf(sio.out, "%s still to do (above). Once done, start the stack:\n", plural(len(steps), "step"))
	}
	fmt.Fprintln(sio.out, "  1) in one terminal:  pi-stack serve")
	fmt.Fprintln(sio.out, "  2) in another:       pi-stack")
	fmt.Fprintln(sio.out, "  verify anytime:      pi-stack doctor")

	if !sio.isTTY {
		fmt.Fprintln(sio.out)
		fmt.Fprintln(sio.out, "(non-interactive: printed the checklist above — nothing is waiting on input)")
	}

	return steps
}

// runSetupCmd is the CLI entry point wired into main's dispatch.
func runSetupCmd(argv []string) {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack setup: loading config: %v\n", err)
		os.Exit(1)
	}
	sio := setupIO{in: os.Stdin, out: os.Stdout, isTTY: isTTY(os.Stdin)}
	runSetup(cfg, defaultShellEnv(), sio, config.Seed, config.EnsureToken)
}

// isTTY reports whether r is an interactive terminal. Any non-*os.File (e.g. a
// test buffer) or a redirected/piped stdin is treated as non-interactive.
func isTTY(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
