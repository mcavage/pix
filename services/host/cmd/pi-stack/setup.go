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
	help      bool   // -h / --help: print usage + exit 0
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
	fmt.Fprintln(sio.out, "I will walk through 4 steps. Only the first is required. You can stop after any")
	fmt.Fprintln(sio.out, "step and re-run `pi-stack setup` later.")
	fmt.Fprintln(sio.out)

	var steps []string
	todo := func(cmd string) { steps = append(steps, cmd) }

	// Step 1 of 4 — Provider API keys (required). Proxy-injected, never in the VM;
	// entry is external (interactive `sbx secret set`), so we guide the exact
	// command per missing key rather than pretend to automate it.
	fmt.Fprintln(sio.out, "Step 1 of 4 — Provider API keys        (required)")
	fmt.Fprintln(sio.out, "Provider keys (proxy-injected, never in the VM):")
	sbxOut, sbxOK := "", false
	sbxOnPath := false
	if _, err := env.lookPath("sbx"); err == nil {
		sbxOnPath = true
		if out, err := env.run("sbx", "secret", "ls"); err == nil {
			sbxOut, sbxOK = out, true
		}
	}
	// Only a MODEL provider key (anthropic/openai/google) satisfies readiness — a
	// github token authorizes git, not the model, so it is shown but never sets
	// anyKey.
	modelKey := map[string]bool{"anthropic": true, "openai": true, "google": true}
	anyKey := false
	if sbxOnPath && !sbxOK {
		// sbx is installed but `sbx secret ls` failed: we cannot assert the keys are
		// missing, only that we could not verify them. Printing per-key ✗ + `sbx
		// secret set` TODOs here would falsely claim absence, so say so honestly and
		// leave anyKey false (the readiness verdict handles this case distinctly).
		fmt.Fprintln(sio.out, "  · could not verify provider keys (sbx secret ls failed); check sbx")
	} else {
		for _, key := range []string{"anthropic", "openai", "google", "github"} {
			c := secretCheck(key, key, sbxOut, sbxOK)
			if c.state == stateOK {
				fmt.Fprintf(sio.out, "  ✓ %s set\n", key)
				if modelKey[key] {
					anyKey = true
				}
			} else {
				fmt.Fprintf(sio.out, "  ✗ %s — run: %s\n", key, c.todo)
				todo(c.todo)
			}
		}
	}
	fmt.Fprintln(sio.out)

	// Step 2 of 4 — Memory & knowledge (recommended). Ensure the memory service is
	// enabled (it is the default; a no-op on a fresh config but keeps setup
	// self-contained), then set up the knowledge base.
	fmt.Fprintln(sio.out, "Step 2 of 4 — Memory & knowledge       (recommended)")
	cfg.AddService("memory")
	fmt.Fprintln(sio.out, "  ✓ memory enabled")

	// Knowledge base (global-first). Set up the GLOBAL OKF bundle the first
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

	// Step 3 of 4 — Integrations (optional; skip if unsure). Google Workspace
	// account from --account, else the already-configured value, else an
	// interactive prompt (only when we have a TTY and weren't told to assume-yes).
	// Non-interactive with nothing configured: guide the command.
	fmt.Fprintln(sio.out, "Step 3 of 4 — Integrations             (optional; skip if unsure)")
	fmt.Fprintln(sio.out, "Google Workspace (gog MCP server, optional):")
	account := strings.TrimSpace(opts.account)
	if account == "" {
		account = strings.TrimSpace(cfg.GogAccount)
	}
	if account == "" && sio.isTTY && !opts.assumeYes {
		account = promptLine(sio, "  Google Workspace account (email, blank to skip): ")
	}

	gogNeedsAuth := false
	if account != "" {
		// 2a. Write the account. 2b. Enable gog in the MCP set. Both idempotent.
		cfg.SetGogAccount(account)
		cfg.AddMCP("gog")
		fmt.Fprintf(sio.out, "  ✓ gog_account = %s\n", account)
		// Setting an email is not the same as completing OAuth. Label gog "needs
		// auth" until a real account-scoped `gog auth status` probe passes.
		if gogAuthed(env, account) {
			fmt.Fprintln(sio.out, "  ✓ gog enabled (added to mcp)")
		} else {
			gogNeedsAuth = true
			fmt.Fprintln(sio.out, "  · gog (Google Workspace) — account set, needs auth (run gog auth login)")
			todo("gog auth login")
		}
	} else {
		fmt.Fprintln(sio.out, "  ✗ no account set — enable gog later with:")
		fmt.Fprintln(sio.out, "      pi-stack config set gog_account <you@example.com>")
		fmt.Fprintln(sio.out, "      pi-stack config set mcp gog")
		todo("pi-stack config set gog_account <you@example.com>")
		todo("pi-stack config set mcp gog")
	}
	fmt.Fprintln(sio.out)

	// Step 4 of 4 — Integration credentials. MUST come last: needsCreds depends on
	// what step 3 added (an integration that needs a host credential, cfg.MCP minus
	// gog, which uses OAuth/keychain). Host MCP servers get their creds from
	// 1Password at gateway spawn; nothing is prompted here (secret VALUES live in
	// 1Password, never on disk). Wholly non-blocking: with no credential
	// integration it prints a short skip note and creates NO file.
	// needsCreds is a BEST-EFFORT signal, not an authoritative one: it is driven by
	// LOCAL-non-gog membership (a local stdio server per `pi-stack-host mcp --list`
	// that isn't gog). There is NO per-server credential metadata — `pi-stack-host
	// mcp --list` returns names only (see mcp_bridge.go builtinMcpNames) — so a
	// credential-FREE local server like `pio` will over-trigger this: it shows the
	// Step 4 explanation and seeds a harmless empty op-refs template. That is
	// deliberately tolerable (the seed is a no-op no-clobber template; see mcp.go).
	// The proper fix is a per-server NeedsCreds capability on the McpServer plugin
	// interface, which is out of scope here.
	//
	// The determination is THREE-state, not a boolean: the local-set probe can
	// fail (pi-stack-host missing or `mcp --list` errored), and collapsing that to
	// "no server needs creds" would print a confident skip when the truth is
	// unknown. So credNeeds / credNone / credUnknown are kept distinct.
	cstate, _ := credentialDetermination(cfg, env, hostBinaryResolver)
	hasNonGogMCP := false
	for _, m := range cfg.MCP {
		if m != "gog" {
			hasNonGogMCP = true
			break
		}
	}
	setupSecretsSection(env, sio.out, cstate, hasNonGogMCP, todo)

	// Persist everything we just decided. This is the write that replaces
	// hand-editing config.toml. (knowledgeInit/knowledgeUse also Save() as they
	// wire; this re-save is idempotent and keeps setup self-contained.)
	if err := save(cfg); err != nil {
		fmt.Fprintf(sio.out, "Config: ✗ could not save %s — %v\n", config.Path(), err)
	} else {
		fmt.Fprintf(sio.out, "Config: ✓ saved %s (services=%v, mcp=%v)\n",
			config.Path(), cfg.Services, cfg.MCP)
	}
	fmt.Fprintln(sio.out)

	// Readiness verdict: whether you can run right now.
	ready := anyKey && sbxOnPath
	switch {
	case ready:
		fmt.Fprintln(sio.out, "You are ready. Run:  pi-stack run")
	case !sbxOnPath:
		// The real blocker is the launcher itself, not a key: sbx is required to
		// launch a sandbox at all. Name that first, and mention a missing key too.
		fmt.Fprintln(sio.out, "You are NOT fully ready yet: Docker Sandboxes (sbx) is not installed; it is required to launch.")
		fmt.Fprintln(sio.out, "  Install the Docker Sandboxes CLI (sbx); see https://docs.docker.com/sandboxes/")
		if !anyKey {
			fmt.Fprintln(sio.out, `  Also set a model provider key, then run pi-stack setup again:  sbx secret set -g anthropic -t "sk-..."`)
		}
	case !sbxOK:
		// sbx is present but `sbx secret ls` failed, so we could NOT verify any
		// provider key. This is distinct from a confirmed no-key host: we assert
		// nothing about which keys are set.
		fmt.Fprintln(sio.out, "You are NOT fully ready yet: provider keys could not be verified (sbx secret ls failed). Check sbx, then run pi-stack setup again.")
	default:
		// sbx is present and the probe worked; the only gap is a model provider key.
		fmt.Fprintln(sio.out, "You are NOT fully ready yet: no model provider key is set. Set one, then run:")
		fmt.Fprintln(sio.out, `  sbx secret set -g anthropic -t "sk-..."`)
	}
	fmt.Fprintln(sio.out)

	// Summary: register the configured MCP servers now (best-effort: guards
	// degrade cleanly with an actionable message, and setup never aborts on them).
	// registerServers is the tolerant path — it partitions cfg.MCP into gog, local
	// stdio servers, and remote gateway-catalog names (which it skips), and wraps a
	// server in `op run` only when op-refs resolves, else registers it bare
	// (documented-harmless, recoverable via `pi-stack mcp register`). We do NOT
	// gate individual servers on their creds.
	if len(cfg.MCP) > 0 {
		fmt.Fprintln(sio.out, "Registering MCP servers with the sbx gateway:")
		if err := registerServers(cfg, env, sio.out, nil, hostBinaryResolver); err != nil {
			fmt.Fprintf(sio.out, "  skipped: %v\n", err)
			fmt.Fprintln(sio.out, "  finish later with: pi-stack mcp register")
			todo("pi-stack mcp register")
		}
		fmt.Fprintln(sio.out)
	}
	// When a local server is configured but op-refs isn't resolvable yet, it was
	// registered bare (no token). Filling op-refs.env later does not re-register
	// it, so leave a single informational recovery step. We cannot tell whether any
	// of those local servers actually needs a credential (no per-server metadata),
	// so the note stays conditional rather than asserting a requirement. This is a
	// coarse, non-per-server gate.
	if cstate == credNeeds && !opRefsResolvable(env) {
		fmt.Fprintln(sio.out, "Note: one or more local integrations MIGHT need a password, and op-refs isn't resolvable yet.")
		fmt.Fprintln(sio.out, "  If any of yours do, fill op-refs.env (pi-stack secret edit), verify (pi-stack secret check),")
		fmt.Fprintln(sio.out, "  then re-register:  pi-stack mcp register")
		todo("pi-stack mcp register")
		fmt.Fprintln(sio.out)
	}
	if gogNeedsAuth {
		fmt.Fprintln(sio.out, "Needs auth:  gog (Google Workspace) - run `gog auth login`")
		fmt.Fprintln(sio.out)
	}
	fmt.Fprintln(sio.out, "Next:")
	fmt.Fprintln(sio.out, "  pi-stack serve     start the services you set up")
	fmt.Fprintln(sio.out, "  pi-stack run       launch a sandbox and start working")
	fmt.Fprintln(sio.out, "  pi-stack doctor    re-check everything, anytime")

	return steps
}

// credentialMCPs returns the configured integrations that need a host credential
// (op-refs.env) at spawn time: LOCAL stdio servers (in the `pi-stack-host mcp
// --list` set) minus gog. gog authenticates via OAuth/keychain (`gog auth
// login`), not an op-refs token. Remote gateway-catalog servers (notion,
// atlassian, ...) are attached a different way and never use op-refs, so they
// are excluded. When the local set can't be established the result is empty
// (fail closed). needsCreds/step 4 gate on this.
//
// This is a BEST-EFFORT signal (local non-gog), NOT proof a server needs a
// credential: there is no per-server credential metadata (`pi-stack-host mcp
// --list` returns names only), so a credential-free local server like `pio`
// will over-trigger the Step 4 explanation and a harmless empty op-refs
// template. The proper fix is a per-server NeedsCreds capability on the
// McpServer plugin interface (out of scope here).
func credentialMCPs(cfg *config.Config, env shellEnv, hostResolver func() (string, error)) []string {
	_, servers := credentialDetermination(cfg, env, hostResolver)
	return servers
}

// credState is the three-state credential determination for Step 4. A boolean
// would conflate two different "no" answers: the local-set probe
// (`pi-stack-host mcp --list`) can itself fail, and there is no per-server
// credential metadata, so we distinguish credNeeds (>=1 local non-gog server),
// credNone (probe succeeded, found no such server), and credUnknown (the local
// set could not be determined: pi-stack-host missing or the probe errored). This
// mirrors the fail-closed distinction registerServers already draws, so the
// setup copy stays honest instead of claiming "none" when the answer is unknown.
type credState int

const (
	credNone credState = iota
	credNeeds
	credUnknown
)

// credentialDetermination runs the local-set probe once and classifies the
// configured integrations into a credState plus the local non-gog servers found.
// A failed probe is credUnknown (never silently credNone), so Step 4 does not
// print a confident skip when it could not actually tell.
func credentialDetermination(cfg *config.Config, env shellEnv, hostResolver func() (string, error)) (credState, []string) {
	localSet, known := localMCPNames(env, hostResolver)
	if !known {
		return credUnknown, nil
	}
	var out []string
	for _, m := range cfg.MCP {
		if m != "gog" && localSet[m] {
			out = append(out, m)
		}
	}
	if len(out) > 0 {
		return credNeeds, out
	}
	return credNone, nil
}

// opRefsResolvable reports whether host MCP creds can be injected yet: op is
// installed + has an account configured AND an op-refs.env file exists. It is a
// COARSE gate (not per-server) behind the single `pi-stack mcp register`
// recovery step — there is no authoritative server->ref map, and registration is
// tolerant, so we never try to resolve individual servers.
func opRefsResolvable(env shellEnv) bool {
	if !opInstalled(env) || !opSignedIn(env) {
		return false
	}
	_, _, exists := opRefsContent(env)
	return exists
}

// gogAuthed reports whether gog has usable auth for a specific account: it is on
// PATH and an account-scoped `gog --account <account> auth status` exits 0.
// Setting an account email does NOT imply completed OAuth, so setup/status pass
// the CONFIGURED account and probe THAT account (mirroring doctor's
// account-scoped `gog --account <acct> ...` form) before claiming gog is ready.
// The probe is BOUNDED (gogAuthTimeout) so a network round-trip can never hang a
// fast command like `status` (mirrors doctor's bounded probes): real callers
// wire env.probe, and when present we run our own short-timeout exec; tests
// leave probe nil and use the hermetic env.run. Best-effort: any gap (gog
// absent, a timeout, status errors) is "not authed", never a crash.
func gogAuthed(env shellEnv, account string) bool {
	if env.lookPath == nil {
		return false
	}
	if _, err := env.lookPath("gog"); err != nil {
		return false
	}
	if env.probe != nil {
		_, timedOut, err := runWithTimeoutD(gogAuthTimeout, "gog", "--account", account, "auth", "status")
		return !timedOut && err == nil
	}
	if env.run == nil {
		return false
	}
	_, err := env.run("gog", "--account", account, "auth", "status")
	return err == nil
}

// setupSecretsSection is the "Secrets (1Password)" step of setup. It is
// THREE-state (see credState):
//   - credUnknown with non-gog integrations configured: the local-set probe
//     failed, so it does NOT claim "nothing needs a password"; it says so plainly
//     and adds a recovery TODO.
//   - credNone (or credUnknown with nothing that could need creds): a short
//     "skipped" note, and NO file is created.
//   - credNeeds: seed the refs template (idempotent, no-clobber), give the
//     3-step plain-language 1Password explanation (kept conditional, since we
//     cannot prove any local server actually needs a credential), and report
//     whether op is installed + signed in.
//
// Prompts nothing (secret values live in 1Password) and is wholly non-blocking.
// It adds outstanding steps as TODOs where relevant.
func setupSecretsSection(env shellEnv, out io.Writer, state credState, hasNonGogMCP bool, todo func(string)) {
	if state == credUnknown && hasNonGogMCP {
		fmt.Fprintln(out, "Step 4 of 4 — Integration credentials   (undetermined)")
		fmt.Fprintln(out, "  Could not determine whether your integrations need credentials")
		fmt.Fprintln(out, "  (pi-stack-host mcp --list failed). Build pi-stack-host, then run")
		fmt.Fprintln(out, "  pi-stack mcp register and pi-stack secret check to sort it out.")
		fmt.Fprintln(out)
		todo("pi-stack mcp register")
		return
	}
	if state != credNeeds {
		fmt.Fprintln(out, "Step 4 of 4 — Integration credentials   (skipped)")
		fmt.Fprintln(out, "  You added no integrations that need a password. Skipping.")
		fmt.Fprintln(out, "  No file is created until you actually add one.")
		fmt.Fprintln(out, "  When you do:  pi-stack secret edit, then pi-stack secret check,")
		fmt.Fprintln(out, "  then pi-stack mcp register.")
		fmt.Fprintln(out)
		return
	}
	fmt.Fprintln(out, "Step 4 of 4 — Integration credentials   (only if a local integration needs a password)")
	fmt.Fprintln(out, "Secrets (1Password, optional):")
	fmt.Fprintln(out, "  You added a local integration. We cannot tell which local servers need a")
	fmt.Fprintln(out, "  password, so if any of yours do, here is how credentials work: host MCP")
	fmt.Fprintln(out, "  servers get their creds from 1Password at spawn time; pi-stack never stores")
	fmt.Fprintln(out, "  them. Many local servers need nothing here, so this may not apply to you.")
	fmt.Fprintln(out, indent(config.OpRefsMentalModel))

	// ALWAYS seed (idempotent, no-clobber). This is the ONE seeder.
	path, created, err := config.SeedOpRefs()
	switch {
	case err != nil:
		fmt.Fprintf(out, "  ✗ could not seed op-refs.env at %s: %v\n", path, err)
	case created:
		fmt.Fprintf(out, "  ✓ seeded a template op-refs.env at %s (fill in your op:// refs)\n", path)
	default:
		fmt.Fprintf(out, "  ✓ op-refs.env already present at %s (left as-is)\n", path)
	}

	// op state: installed / missing / installed-not-signed-in. All non-blocking.
	switch {
	case !opInstalled(env):
		fmt.Fprintln(out, "  · op (1Password CLI) not installed — optional, install it to resolve refs:")
		fmt.Fprintln(out, "      https://developer.1password.com/docs/cli")
		todo("install the 1Password CLI (op) if a host MCP server needs creds")
	case !opSignedIn(env):
		fmt.Fprintln(out, "  · op installed, no account configured — run: op signin  (when you add creds)")
		todo("op signin")
	default:
		fmt.Fprintln(out, "  ✓ op installed, account configured")
	}
	fmt.Fprintln(out, "  edit refs anytime:  pi-stack secret edit   (check them: pi-stack secret check)")
	fmt.Fprintln(out)
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

// promptLine reads a single trimmed line from sio.in after writing prompt.
func promptLine(sio setupIO, prompt string) string {
	fmt.Fprint(sio.out, prompt)
	line, _ := bufio.NewReader(sio.in).ReadString('\n')
	return strings.TrimSpace(line)
}

// parseSetupArgs parses the setup flag set.
// setupHelp is a sentinel setupOpts flag: parseSetupArgs sets it on -h/--help so
// runSetupCmd prints usage + exits 0 rather than running the wizard.
func parseSetupArgs(argv []string) (setupOpts, error) {
	var o setupOpts
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "-h" || a == "--help":
			o.help = true
			return o, nil
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
		fmt.Fprintf(os.Stderr, "pi-stack setup: %v\n\n%s", err, setupUsage)
		os.Exit(2)
	}
	if opts.help {
		fmt.Print(setupUsage)
		return
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
