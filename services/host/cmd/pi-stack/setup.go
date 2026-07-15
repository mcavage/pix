package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"pi-stack/host/config"
)

// setup is a real, IDEMPOTENT onboarding wizard that WRITES config and REGISTERS
// MCP servers — it never tells you to hand-edit the toml. It is the last piece
// of the repo-less-host goal: `pi-stack setup` configures everything.
//
// It runs interactively by default (prompting for the one thing it can't infer,
// the Google Workspace account) but is fully flag-driven for automation:
//
//	--account <email>        set the gog account non-interactively
//	--knowledge <path|url>   set up the global knowledge base non-interactively
//	                          (local path scaffolded/wired, git URL cloned+used)
//	--yes | --non-interactive  never prompt; do what it can, print the rest as
//	                          `pi-stack config set …` commands
//
// Every step is idempotent: adding an already-present value is a no-op, and
// re-running never clobbers. Secret entry is inherently external (op/1Password,
// sbx), so setup guides the exact `sbx secret set -g …` commands rather than
// pretending to automate them.

// setupOpts is the parsed setup flag set.
type setupOpts struct {
	account   string // --account <email>
	knowledge string // --knowledge <path|url>: the global KB source
	assumeYes bool   // --yes / --non-interactive: never prompt
}

// setupIO carries the streams + a TTY flag so tests can exercise the non-TTY
// path hermetically.
type setupIO struct {
	in    io.Reader
	out   io.Writer
	isTTY bool
}

// runSetup performs the wizard against an injected shellEnv + save func (so
// tests stay hermetic): it detects provider secrets, resolves + writes the gog
// account, enables + registers gog, ensures the memory service, and prints a
// tight next-steps summary. It returns the outstanding steps (as commands) so
// callers/tests can assert on them.
func runSetup(cfg *config.Config, env shellEnv, sio setupIO, opts setupOpts,
	save func(*config.Config) error) []string {

	fmt.Fprintln(sio.out, "pi-stack setup — configures the stack for you. Idempotent; safe to re-run.")
	fmt.Fprintln(sio.out)

	var steps []string
	todo := func(cmd string) { steps = append(steps, cmd) }

	// 1. Provider secrets. Proxy-injected, never in the VM; entry is external
	// (interactive `sbx secret set`), so we guide the exact command per missing
	// key rather than pretend to automate it.
	fmt.Fprintln(sio.out, "Provider keys (proxy-injected, never in the VM):")
	sbxOut, sbxOK := "", false
	if _, err := env.lookPath("sbx"); err == nil {
		if out, err := env.run("sbx", "secret", "ls"); err == nil {
			sbxOut, sbxOK = out, true
		}
	}
	for _, key := range []string{"anthropic", "openai", "google", "github"} {
		c := secretCheck(key, key, sbxOut, sbxOK)
		if c.state == stateOK {
			fmt.Fprintf(sio.out, "  ✓ %s set\n", key)
		} else {
			fmt.Fprintf(sio.out, "  ✗ %s — run: %s\n", key, c.todo)
			todo(c.todo)
		}
	}
	fmt.Fprintln(sio.out)

	// 2. Google Workspace account. From --account, else the already-configured
	// value, else an interactive prompt (only when we have a TTY and weren't told
	// to assume-yes). Non-interactive with nothing configured: guide the command.
	fmt.Fprintln(sio.out, "Google Workspace (gog MCP server, optional):")
	account := strings.TrimSpace(opts.account)
	if account == "" {
		account = strings.TrimSpace(cfg.GogAccount)
	}
	if account == "" && sio.isTTY && !opts.assumeYes {
		account = promptLine(sio, "  Google Workspace account (email, blank to skip): ")
	}

	if account != "" {
		// 2a. Write the account. 2b. Enable gog in the MCP set. Both idempotent.
		cfg.SetGogAccount(account)
		cfg.AddMCP("gog")
		fmt.Fprintf(sio.out, "  ✓ gog_account = %s\n", account)
		fmt.Fprintln(sio.out, "  ✓ gog enabled (added to mcp)")
	} else {
		fmt.Fprintln(sio.out, "  ✗ no account set — enable gog later with:")
		fmt.Fprintln(sio.out, "      pi-stack config set gog_account <you@example.com>")
		fmt.Fprintln(sio.out, "      pi-stack config set mcp gog")
		todo("pi-stack config set gog_account <you@example.com>")
		todo("pi-stack config set mcp gog")
	}
	fmt.Fprintln(sio.out)

	// 3. Knowledge base (global-first). Set up the GLOBAL OKF bundle the first
	// time — scaffold a new one at <config-dir>/knowledge, or point at an
	// existing/shared bundle (local path or git URL). Idempotent: an
	// already-configured bundle is reported and left untouched, never clobbered.
	// knowledgeInit/knowledgeUse (the `knowledge` verb's logic) do the scaffold +
	// config wiring (knowledge_bundles += dir, services += knowledge) — reused
	// here so setup never re-implements the OKF skeleton.
	fmt.Fprintln(sio.out, "Knowledge base (OKF bundle the knowledge service indexes):")
	switch {
	case len(cfg.KnowledgeBundles) > 0:
		// Already wired — report + skip. Never clobber a configured bundle.
		fmt.Fprintf(sio.out, "  ✓ already configured: %s (leaving as-is)\n",
			strings.Join(cfg.KnowledgeBundles, ", "))
	case strings.TrimSpace(opts.knowledge) != "":
		// Explicit source from --knowledge: a local path is scaffolded-if-new +
		// wired; a git URL is cloned/pulled + used (override to a shared bundle).
		if err := setupKnowledge(cfg, opts.knowledge, sio.out); err != nil {
			fmt.Fprintf(sio.out, "  ✗ could not set up %s — %v\n", opts.knowledge, err)
			todo("pi-stack knowledge init")
		}
	case sio.isTTY && !opts.assumeYes:
		// Interactive: default (Enter) scaffolds the global KB; a path/url points
		// at an existing/shared bundle instead; "skip" defers it.
		def := defaultKnowledgeDir()
		ans := promptLine(sio, fmt.Sprintf(
			"  Set up a knowledge base? [Enter = scaffold at %s; a path/git-url uses that; 'skip']: ", def))
		switch {
		case strings.EqualFold(ans, "skip"):
			fmt.Fprintln(sio.out, "  ✗ skipped — set one up later with: pi-stack knowledge init")
			todo("pi-stack knowledge init")
		case ans == "":
			if err := knowledgeInit(cfg, def, sio.out); err != nil {
				fmt.Fprintf(sio.out, "  ✗ scaffold failed — %v\n", err)
				todo("pi-stack knowledge init")
			}
		default:
			if err := setupKnowledge(cfg, ans, sio.out); err != nil {
				fmt.Fprintf(sio.out, "  ✗ could not set up %s — %v\n", ans, err)
				todo("pi-stack knowledge init")
			}
		}
	default:
		// Non-interactive with no --knowledge: scaffold the default global KB so
		// the stack ships with a working knowledge base out of the box.
		if err := knowledgeInit(cfg, defaultKnowledgeDir(), sio.out); err != nil {
			fmt.Fprintf(sio.out, "  ✗ scaffold failed — %v\n", err)
			todo("pi-stack knowledge init")
		}
	}
	fmt.Fprintln(sio.out)

	// 4. Services. Ensure the memory service is enabled (it is the default; this
	// is a no-op on a fresh config but makes setup self-contained).
	cfg.AddService("memory")

	// 5. Persist everything we just decided. This is the write that replaces
	// hand-editing config.toml. (knowledgeInit/knowledgeUse also Save() as they
	// wire; this re-save is idempotent and keeps setup self-contained.)
	if err := save(cfg); err != nil {
		fmt.Fprintf(sio.out, "Config: ✗ could not save %s — %v\n", config.Path(), err)
	} else {
		fmt.Fprintf(sio.out, "Config: ✓ saved %s (services=%v, mcp=%v)\n",
			config.Path(), cfg.Services, cfg.MCP)
	}
	fmt.Fprintln(sio.out)

	// 6. Register the local stdio MCP servers now configured (best-effort: guards
	// degrade cleanly with an actionable message, and setup never aborts on them).
	if len(localMCPTargets(cfg)) > 0 {
		fmt.Fprintln(sio.out, "Registering MCP servers with the sbx gateway:")
		if err := registerServers(cfg, env, sio.out, nil, findHostBinary); err != nil {
			fmt.Fprintf(sio.out, "  skipped: %v\n", err)
			fmt.Fprintln(sio.out, "  finish later with: pi-stack mcp register")
			todo("pi-stack mcp register")
		}
		fmt.Fprintln(sio.out)
	}

	// Summary: the commands that matter.
	if len(steps) == 0 {
		fmt.Fprintln(sio.out, "Done — the stack is configured. Next:")
	} else {
		fmt.Fprintf(sio.out, "Done — %s still to finish (above). Next:\n", plural(len(steps), "item"))
	}
	fmt.Fprintln(sio.out, "  start services:  pi-stack serve  (indexes your knowledge base)")
	fmt.Fprintln(sio.out, "  launch sandbox:  pi-stack run")
	fmt.Fprintln(sio.out, "  verify anytime:  pi-stack doctor")

	return steps
}

// setupKnowledge sets up the global knowledge base from a user-supplied source,
// reusing the `knowledge` verb's logic (no duplicated OKF scaffold or config
// wiring). A git URL is cloned/pulled and used in place (knowledgeUse); a local
// path is scaffolded-if-new and wired (knowledgeInit, which never clobbers an
// existing bundle). Both add the bundle to knowledge_bundles + enable the
// knowledge service and Save().
func setupKnowledge(cfg *config.Config, ref string, out io.Writer) error {
	ref = strings.TrimSpace(ref)
	if isGitURL(ref) {
		return knowledgeUse(cfg, ref, out)
	}
	abs, err := filepath.Abs(ref)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", ref, err)
	}
	return knowledgeInit(cfg, abs, out)
}

// localMCPTargets returns the local stdio servers in cfg.MCP (the ones
// registerServers would act on). Every configured mcp name is a local stdio
// server (gog + everything else; see mcp.go's header), so this is just cfg.MCP.
func localMCPTargets(cfg *config.Config) []string {
	return append([]string(nil), cfg.MCP...)
}

// promptLine reads a single trimmed line from sio.in after writing prompt.
func promptLine(sio setupIO, prompt string) string {
	fmt.Fprint(sio.out, prompt)
	line, _ := bufio.NewReader(sio.in).ReadString('\n')
	return strings.TrimSpace(line)
}

// parseSetupArgs parses the setup flag set.
func parseSetupArgs(argv []string) (setupOpts, error) {
	var o setupOpts
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "--yes" || a == "-y" || a == "--non-interactive":
			o.assumeYes = true
		case a == "--account":
			if i+1 >= len(argv) {
				return o, fmt.Errorf("--account needs a value")
			}
			i++
			o.account = argv[i]
		case strings.HasPrefix(a, "--account="):
			o.account = strings.TrimPrefix(a, "--account=")
		case a == "--knowledge":
			if i+1 >= len(argv) {
				return o, fmt.Errorf("--knowledge needs a value")
			}
			i++
			o.knowledge = argv[i]
		case strings.HasPrefix(a, "--knowledge="):
			o.knowledge = strings.TrimPrefix(a, "--knowledge=")
		default:
			return o, fmt.Errorf("unknown flag %q (want: --account <email>, --knowledge <path|url>, --yes/--non-interactive)", a)
		}
	}
	return o, nil
}

// runSetupCmd is the CLI entry point wired into main's dispatch.
func runSetupCmd(argv []string) {
	opts, err := parseSetupArgs(argv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack setup: %v\n", err)
		os.Exit(2)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack setup: loading config: %v\n", err)
		os.Exit(1)
	}
	sio := setupIO{in: os.Stdin, out: os.Stdout, isTTY: isTTY(os.Stdin)}
	runSetup(cfg, defaultShellEnv(), sio, opts, func(c *config.Config) error { return c.Save() })
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
