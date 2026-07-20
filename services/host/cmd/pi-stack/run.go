package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"pi-stack/host/config"
)

// runRun implements bare `pi-stack [DIR]` and `pi-stack run ...`. It reads the
// config, resolves the run options (including a repo checkout for --dev),
// composes the sbx argv, and execs it with stdio inherited.
//
// The default path forwards NO credential bearer into the sandbox: Google
// Workspace now rides the sbx gateway as the host-side `gog` MCP server (the
// `slack` pattern), authed entirely on the host — there is nothing to inject.
func runRun(argv []string) {
	o, err := parseRunArgs(argv)
	if err != nil {
		if err == errHelpRequested {
			// -h/--help: usage to STDOUT, exit 0 (a help request, not an error).
			fmt.Print(runUsage)
			return
		}
		fmt.Fprintf(os.Stderr, "pi-stack run: %v\n\n", err)
		fmt.Fprint(os.Stderr, runUsage)
		os.Exit(2)
	}

	// Resolve --intent to a concrete session model via the router (unless --model
	// already pinned one, which wins). This makes the INTERACTIVE session use the
	// same cost/latency/accuracy routing the subagent crew uses.
	if o.Intent != "" && o.Model == "" {
		m, rerr := resolveSessionModel(o.Intent)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "pi-stack run: --intent %q: %v\n", o.Intent, rerr)
			os.Exit(2)
		}
		o.Model = m
		fmt.Fprintf(os.Stderr, "pi-stack: intent %q -> model %s\n", o.Intent, m)
	}

	// Preflight: refuse to launch a sandbox that has no model to talk to. A pi
	// session needs at least one model provider key (anthropic/openai/google); a
	// github token authorizes git, not the model, so it does NOT count. We can
	// only check when sbx is on PATH (the keys live proxy-side); when it is
	// absent we cannot verify and proceed as before.
	//
	// First, the no-ritual 1Password path: if a provider key is MISSING from sbx
	// but you own an op:// ref for it, resolve it into sbx now (op prompts at most
	// once per key, then never again). Present keys are skipped, so this is a
	// no-op on every launch after the first.
	ensureProviderKeysFromRefs(defaultShellEnv(), os.Stderr)
	if msg, block := modelProviderPreflight(defaultShellEnv()); block {
		fmt.Fprint(os.Stderr, msg)
		os.Exit(1)
	}

	// `pi-stack run` NEVER onboards on its own (owner decision): it just launches
	// the agent. If the host was never set up, print a one-line, non-blocking
	// heads-up of what is missing and continue straight into the session — no
	// prompt, no delay. The guided flow is the explicit `pi-stack setup`, which
	// does the host phase then hands off by launching a run whose first pi message
	// kicks off onboarding.
	warnUnconfigured(defaultShellEnv(), os.Stderr)

	// Reconcile any control-plane proposal a prior in-session onboarding wrote
	// (<workspace>/.pi-stack/onboarding.json): validate it, show the diff, apply
	// under a [Y/n] gate, register newly-enabled MCP servers, delete the file. This
	// runs BEFORE loadResolvedConfig so a fresh create picks up the applied config.
	// Best-effort and non-blocking on a non-TTY (it just leaves the file).
	reconcileOnboarding(o.Workspace, defaultShellEnv(), os.Stdin, os.Stdout, false, isTTY(os.Stdin))

	// Resolve the active profile into a flat config so the rest of run (kits, mcp,
	// gog) sees the profile's overrides. loadResolvedConfig errors on a typo'd /
	// unknown profile name rather than silently running the base config. The
	// profile also namespaces the sandbox name so contexts never collide.
	cfg, profile, err := loadResolvedConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack run: %v\n", err)
		os.Exit(1)
	}
	if profile != config.DefaultProfile {
		fmt.Fprintf(os.Stderr, "pi-stack: profile %q\n", profile)
	}

	// Own the sandbox name so we can manage its lifecycle. sbx would otherwise
	// auto-derive `pi-stack-<dir>`. This needs only Workspace + profile, so it is
	// resolved (and the sandbox state probed) BEFORE any create-only input
	// resolution below — a plain re-attach must never fail on a --dev/checkout or
	// --kit problem it doesn't even need.
	if o.Name == "" {
		o.Name = deriveSandboxName(o.Workspace)
		if profile != config.DefaultProfile {
			o.Name += "-" + sanitizeProfileName(profile)
		}
	}
	state := probeTaskSandbox(defaultShellEnv(), o.Name)

	// Mirror sbx's own model: an existing sandbox (running OR stopped) RE-ATTACHES
	// instead of refusing/recreating — the create-only flags (--kit/--template/
	// --mcp/overlay-kit/--dev/dev-skills) only apply to a fresh create, so they are
	// simply not sent (and, per willCreate below, not even RESOLVED) on re-attach.
	// --replace forces the old implicit-recreate behavior (rm -f then create) for
	// either state, so changed kit/mcp/create-only flags take effect.
	if willCreate(state, o.Replace) {
		// Kit selection. A CLEAN released version (e.g. "0.0.16") pins the matching
		// git tag; anything else — an unstamped "dev" build, a "0.0.16+local" local
		// build, or non-semver — is UNRELEASED, its tag does not exist, so we never
		// pin v<version>. --dev forces the local checkout kit; an unreleased build
		// uses it too when a checkout is resolvable, else falls back to #ref=main.
		released := isReleased(version)
		kitOverride := len(o.Kits) > 0

		if o.Dev {
			// --dev needs a resolvable repo checkout; fail loud otherwise. --dev is
			// create/replace-only (this branch), so it is a no-op on a plain re-attach.
			root, err := resolveRepoRoot()
			if err != nil {
				fmt.Fprintf(os.Stderr, "pi-stack run --dev: %v\n", err)
				os.Exit(1)
			}
			o.DevRoot = root
			o.LocalKit = filepath.Join(root, "pi-kit")
			o.LocalImageTag = readLocalImageTag(root)
		} else if !released && !kitOverride {
			if root, err := resolveRepoRoot(); err == nil {
				o.LocalKit = filepath.Join(root, "pi-kit")
				o.LocalImageTag = readLocalImageTag(root)
				note := ""
				if o.LocalImageTag != "" {
					note = " (local image :" + o.LocalImageTag + ")"
				}
				fmt.Fprintf(os.Stderr, "pi-stack: unreleased build %q — using local checkout kit %s%s\n", version, o.LocalKit, note)
			} else {
				fmt.Fprintf(os.Stderr, "pi-stack: unreleased build %q and no pi-stack checkout found — "+
					"kit tracks #ref=main (may not match this binary). Use `pi-stack run --dev` from a "+
					"checkout or `pi-stack run --kit <path-or-git-url>` to override.\n", version)
			}
		}
	} else if o.Dev {
		fmt.Fprintln(os.Stderr, "pi-stack: --dev is create/replace-only; re-attaching to the existing sandbox as-is (use --replace to recreate with --dev)")
	}

	// --mcp is only a valid sbx flag when the gateway is enabled (SBX_MCP_URL set,
	// like the Makefile gates it). Set MCPEnabled from the env so buildSbxArgs stays
	// pure, and warn (once) when MCP servers are configured but the gateway is off,
	// rather than letting sbx bail with `unknown flag: --mcp`.
	o.MCPEnabled = strings.TrimSpace(os.Getenv("SBX_MCP_URL")) != ""
	if !o.MCPEnabled {
		configured := append(append([]string(nil), cfg.MCP...), o.MCP...)
		if msg := mcpGatewayOffWarning(configured); msg != "" {
			fmt.Fprintln(os.Stderr, msg)
		}
	}

	// Active pack: mount its skills/ + knowledge/ so the pack's context loads in
	// this sandbox. --pack overrides config.Pack; with neither set, the personal
	// pack (config.PackDir()) loads if it exists. Create-time only (skills +
	// knowledge are create-time mounts; a re-attach keeps what it was made with).
	if willCreate(state, o.Replace) {
		packRoot := activePackRoot(cfg.Pack, o.Pack)
		if packRoot == "" {
			packRoot = config.PackDir() // personal pack, if it is one
		}
		if p, err := loadPack(packRoot); err == nil {
			if p.SkillsDir != "" && !containsStr(o.Skills, p.SkillsDir) {
				o.Skills = append(o.Skills, p.SkillsDir)
			}
			if p.KnowledgeDir != "" && !containsStr(cfg.KnowledgeBundles, p.KnowledgeDir) {
				cfg.KnowledgeBundles = append(cfg.KnowledgeBundles, p.KnowledgeDir)
			}
			// Reference-only integrations: attach the pack's MCP servers (host-
			// provided) and warn about any credential the pack needs but the user
			// hasn't wired as an op:// ref yet. No pack code executes here.
			penv := defaultShellEnv()
			for _, ig := range p.Manifest.Integrations {
				if ig.MCP != "" && !containsStr(cfg.MCP, ig.MCP) {
					cfg.MCP = append(cfg.MCP, ig.MCP)
				}
				if ig.Env != "" && !opRefFilled(penv, ig.Env) {
					fmt.Fprintf(os.Stderr, "pi-stack: pack integration %q needs a credential — set it: pi-stack secret set %s op://vault/item/field\n", ig.Name, ig.Env)
				}
			}
		}
	}

	plan := planSandboxLaunch(state, o.Replace, cfg, o, version)
	switch {
	case o.Replace:
		fmt.Fprintf(os.Stderr, "pi-stack run: replacing sandbox %q\n", o.Name)
	case plan.Reattach && state == sbxRunning:
		fmt.Fprintf(os.Stderr, "pi-stack run: re-attaching to running sandbox %q\n", o.Name)
	case plan.Reattach:
		fmt.Fprintf(os.Stderr, "pi-stack run: starting + attaching existing sandbox %q (use --replace to recreate with current kit/mcp/flags)\n", o.Name)
	}
	if plan.RmFirst {
		if err := applyReplaceRm(defaultShellEnv(), plan, o.Name); err != nil {
			fmt.Fprintf(os.Stderr, "pi-stack run: %v\n", err)
			os.Exit(1)
		}
	}

	// Lazy auto-start: make the configured host services (memory/knowledge)
	// reachable before the sandbox tries them, with a SHORT budget — the launch
	// waits AT MOST ensureServeRunTimeout (8s), covering spawn-lock acquisition
	// AND the health poll under one deadline (M2), then proceeds regardless
	// (recall/knowledge degrade in-VM exactly as before). ensureServe prints its
	// own progress/failure lines.
	ensureServeUp(nil, ensureServeRunTimeout)

	// Knowledge scope: resolve this workspace's bundle set (global config bundles
	// + the project's .pi-stack/knowledge pointer), lazily reindex the project
	// bundle when the daemon is up and doesn't know it yet, and write the scope
	// file the in-VM recall extension reads. Entirely best-effort: it never blocks
	// or fails the launch (recall just misses a bundle this run).
	wireKnowledgeScope(cfg, o.Workspace, defaultKnowledgeRPC())

	// Memory scope: hand the active profile name to the in-VM memory extensions
	// (recall/capture) via a per-run workspace file, mirroring the knowledge scope
	// file. They stamp captures with it and filter recall to {profile}∪{default}.
	// Best-effort: a failure just leaves memory un-scoped (all default) this run.
	writeProfileFile(o.Workspace, profile)

	// Local model: hand the configured ollama_bridge_model to the in-VM
	// ollama-bridge via a per-run workspace file, so `pi-stack config set
	// ollama_bridge_model <tag>` is all you need (no sandbox env editing). Mirrors
	// the profile/knowledge-scope seam. Best-effort.
	writeOllamaBridgeFile(o.Workspace, cfg.OllamaBridgeModel)

	// Host-state truth file: the host-visible facts the fenced agent can't see
	// (keys/services/knowledge/gog/mcp/models/overlay). The onboarding skill reads
	// it instead of guessing. Best-effort.
	writeHostStateFile(o.Workspace, cfg, defaultShellEnv(), o.MCPEnabled)

	args := plan.Args

	if os.Getenv("PI_STACK_DEBUG") != "" {
		fmt.Fprintln(os.Stderr, "+ sbx "+strings.Join(args, " "))
	}

	cmd := exec.Command("sbx", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// The default path injects no credential bearer: gog authenticates on the host
	// inside the gateway-spawned MCP server, so the sandbox never sees a Google
	// token. A future overlay credential broker would set its own bearer through
	// the retained generic seam and own that plumbing itself.
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			// If we pinned a git #ref kit and sbx bailed (classically git exit 128
			// "Remote branch not found"), the raw error is opaque — replace it with
			// an actionable note instead of leaking the git 128.
			if msg := kitResolveFailureMsg(pinnedGitKit(args)); msg != "" {
				fmt.Fprintln(os.Stderr, msg)
			}
			// A re-attach exec can fail on an sbx version that won't reattach a
			// kit-created sandbox; don't leave the user stuck without a next step.
			if plan.Reattach {
				fmt.Fprintln(os.Stderr, "pi-stack run: re-attach failed; recreate it with: pi-stack run --replace")
			}
			os.Exit(exit.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "pi-stack run: exec sbx: %v\n", err)
		if plan.Reattach {
			fmt.Fprintln(os.Stderr, "pi-stack run: re-attach failed; recreate it with: pi-stack run --replace")
		}
		os.Exit(1)
	}
}

// applyReplaceRm runs the plan's RmFirst step (`sbx rm -f <name>`) via env when
// required, and MUST be checked by the caller: a failed rm means the old
// sandbox may still exist under that name, and proceeding to create against it
// anyway is undefined (sbx may error, or silently reattach to a sandbox with
// stale kit/mcp/create-only flags — exactly what --replace was trying to avoid).
// A no-op (nil) when the plan doesn't call for it.
func applyReplaceRm(env shellEnv, plan runLaunchPlan, name string) error {
	if !plan.RmFirst {
		return nil
	}
	if _, err := env.run("sbx", "rm", "-f", name); err != nil {
		return fmt.Errorf("could not remove existing sandbox %q to replace it: %w", name, err)
	}
	return nil
}

// modelProviders are the model-provider secret keys a pi session needs at least
// one of to run. github is deliberately excluded: it authorizes git operations,
// not the model.
var modelProviders = []string{"anthropic", "openai", "google"}

// modelProviderPreflight verifies at least one model provider key is set before
// a launch. It reads `sbx secret ls` (the keys live proxy-side, never in the VM).
// It returns a guidance message + block=true ONLY when sbx is on PATH, its
// secret listing is readable, and NONE of the model providers are present. When
// sbx is absent (e.g. inside a sandbox) or the listing can't be read we cannot
// verify, so block=false and the launch proceeds unchanged.
func modelProviderPreflight(env shellEnv) (msg string, block bool) {
	if env.lookPath == nil {
		return "", false
	}
	if _, err := env.lookPath("sbx"); err != nil {
		return "", false // sbx not on PATH: cannot verify
	}
	if env.run == nil {
		return "", false
	}
	out, err := env.run("sbx", "secret", "ls")
	if err != nil {
		return "", false // couldn't read secrets: don't block
	}
	var missing []string
	for _, k := range modelProviders {
		if grepWord(out, k) {
			return "", false // at least one model key present
		}
		missing = append(missing, k)
	}
	msg = fmt.Sprintf("pi-stack run: no model provider key is set (need one of %s).\n",
		strings.Join(missing, ", "))
	if providerKeyRefsPresent(env) {
		// The user owns 1Password op:// refs for keys but they haven't been pushed
		// into sbx yet — point at the sync, not a manual paste.
		msg += "You have 1Password key refs; resolve them into sbx with:\n  pi-stack secret sync\n"
	} else {
		msg += "Set one on the host, then re-run. Either:\n" +
			"  pi-stack secret set ANTHROPIC_API_KEY op://vault/item/field && pi-stack secret sync   (1Password)\n" +
			"  sbx secret set -g anthropic -t \"sk-...\"                                            (direct)\n"
	}
	return msg, true
}

// parseRunArgs is a small hand-rolled parser (no cobra, no third-party flags) so
// DIR can appear before or after the flags, matching the flexibility of the old
// bin/pi-stack shell launcher. Everything after `--` is pi passthrough.
func parseRunArgs(argv []string) (runOpts, error) {
	// -h/--help anywhere before `--` is a help request, not a parse error.
	if wantsHelp(argv) {
		return runOpts{}, errHelpRequested
	}
	o := runOpts{Workspace: "."}
	wsSet := false

	// Split off the `--` passthrough first.
	pre := argv
	for i, a := range argv {
		if a == "--" {
			pre = argv[:i]
			o.Passthrough = append([]string(nil), argv[i+1:]...)
			break
		}
	}

	valueOf := func(a string, i *int) (string, error) {
		if eq := strings.IndexByte(a, '='); eq >= 0 {
			return a[eq+1:], nil
		}
		if *i+1 >= len(pre) {
			return "", fmt.Errorf("flag %s needs a value", a)
		}
		*i++
		return pre[*i], nil
	}

	for i := 0; i < len(pre); i++ {
		a := pre[i]
		name := a
		if eq := strings.IndexByte(a, '='); eq >= 0 {
			name = a[:eq]
		}
		switch {
		case a == "--dev":
			o.Dev = true
		case a == "--replace":
			o.Replace = true
		case name == "--name":
			v, err := valueOf(a, &i)
			if err != nil {
				return o, err
			}
			o.Name = v
		case name == "--model":
			v, err := valueOf(a, &i)
			if err != nil {
				return o, err
			}
			o.Model = v
		case name == "--intent":
			v, err := valueOf(a, &i)
			if err != nil {
				return o, err
			}
			o.Intent = v
		case name == "--skills":
			v, err := valueOf(a, &i)
			if err != nil {
				return o, err
			}
			o.Skills = append(o.Skills, v)
		case name == "--kit":
			v, err := valueOf(a, &i)
			if err != nil {
				return o, err
			}
			o.Kits = append(o.Kits, v)
		case name == "--pack":
			v, err := valueOf(a, &i)
			if err != nil {
				return o, err
			}
			o.Pack = v
		case name == "--mcp":
			v, err := valueOf(a, &i)
			if err != nil {
				return o, err
			}
			o.MCP = append(o.MCP, v)
		case strings.HasPrefix(a, "-"):
			return o, fmt.Errorf("unknown flag %q", a)
		default:
			if wsSet {
				return o, fmt.Errorf("unexpected extra argument %q (only one DIR allowed; use -- for pi args)", a)
			}
			o.Workspace = a
			wsSet = true
		}
	}
	// A non-"." workspace MUST be an existing directory. Otherwise a mistyped verb
	// (`pi-stack run help`, `run doctro`) would silently boot a junk sandbox named
	// after the typo. Reject it, suggesting the verb when the token matches one.
	if err := validateRunWorkspace(o.Workspace); err != nil {
		return o, err
	}
	return o, nil
}

// validateRunWorkspace verifies a resolved run workspace is launchable: the cwd
// default (".") always is; any other value must name an existing directory. A
// non-directory token that matches a known verb gets a "did you mean" hint.
func validateRunWorkspace(ws string) error {
	if ws == "." {
		return nil
	}
	if fi, err := os.Stat(ws); err == nil && fi.IsDir() {
		return nil
	}
	if knownVerbs[ws] {
		return fmt.Errorf("%q is not a directory. Did you mean `pi-stack %s`?", ws, ws)
	}
	return fmt.Errorf("%q is not a directory", ws)
}

// resolveRepoRoot finds a pi-stack repo checkout for the local kit path, in
// order: $PI_STACK_DEV_ROOT if set, else walking up from the current working
// directory, else the launcher binary's own location (make install symlinks
// ~/.local/bin/pi-stack -> <repo>/out/pi-stack, so the repo is two levels up
// from the resolved binary). Fails when none resolves.
func resolveRepoRoot() (string, error) {
	if r := strings.TrimSpace(os.Getenv("PI_STACK_DEV_ROOT")); r != "" {
		if isRepoRoot(r) {
			return r, nil
		}
		return "", fmt.Errorf("$PI_STACK_DEV_ROOT=%q is not a pi-stack checkout (no pi-kit/spec.yaml)", r)
	}
	// Walk up from cwd looking for a repo root.
	if cwd, err := os.Getwd(); err == nil {
		dir := cwd
		for {
			if isRepoRoot(dir) {
				return dir, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	// The launcher binary's own location (symlink-resolved).
	if repo, ok := repoFromBinary(); ok {
		return repo, nil
	}
	return "", fmt.Errorf("no pi-stack checkout found (set $PI_STACK_DEV_ROOT or run from inside a checkout)")
}

// repoFromBinary resolves the launcher binary (following symlinks) and reports
// the repo root two levels up (<repo>/out/pi-stack -> <repo>) when it looks
// like a checkout.
func repoFromBinary() (string, bool) {
	self, err := os.Executable()
	if err != nil {
		return "", false
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	repo := filepath.Dir(filepath.Dir(self))
	if isRepoRoot(repo) {
		return repo, true
	}
	return "", false
}

// readLocalImageTag returns the trimmed contents of <root>/out/.local-image-tag
// (written by `make load`), or "" when absent — in which case the caller skips
// the --template pin.
func readLocalImageTag(root string) string {
	b, err := os.ReadFile(filepath.Join(root, "out", ".local-image-tag"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// isRepoRoot reports whether dir looks like a pi-stack repo checkout.
func isRepoRoot(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "pi-kit", "spec.yaml"))
	return err == nil
}

// knowledgeRPC is the tiny seam the run wiring uses to talk to the knowledge
// daemon (:11436). It is injected so tests stay hermetic — no real dial, no real
// POST. defaultKnowledgeRPC() wires the real HTTP JSON-RPC client.
type knowledgeRPC struct {
	up      func() bool               // is the daemon reachable? (short-timeout dial)
	health  func() ([]string, error)  // health.bundles: the ids already indexed
	reindex func(bundle string) error // reindex one bundle path (lazy add)
}

// wireKnowledgeScope resolves the workspace's knowledge scope and writes it out.
// The scope is the ordered, de-duplicated set of canonical bundle ids: the
// global bundles from config, then the project bundle declared by
// <workspace>/.pi-stack/knowledge (if any). All ids are canonicalized the SAME
// way the store keys its `bundle` column so `WHERE bundle IN (…)` matches.
//
//   - Lazy reindex: when the daemon is up and the project bundle is NOT already
//     in health.bundles, fire one reindex for it. Skipped entirely when the
//     daemon is down (serve not running) or the bundle is already indexed.
//   - Scope file: write <workspace>/.pi-stack/knowledge.scope, one canonical id
//     per line, so the in-VM recall (U6) forwards it as the `bundles` filter.
//     With no bundles at all, any stale scope file is removed (recall = all/none).
func wireKnowledgeScope(cfg *config.Config, workspace string, rpc knowledgeRPC) {
	project := projectBundle(workspace) // canonical id, or ""

	var ids []string
	seen := map[string]bool{}
	add := func(p string) {
		c := canonicalizeKnowledgeBundle(p)
		if c == "" || seen[c] {
			return
		}
		seen[c] = true
		ids = append(ids, c)
	}
	for _, b := range cfg.KnowledgeBundles {
		add(b)
	}
	if project != "" {
		add(project)
	}

	// Lazy reindex the project bundle when the daemon is up and doesn't have it.
	if project != "" && rpc.up != nil && rpc.up() {
		known := map[string]bool{}
		if rpc.health != nil {
			if hb, err := rpc.health(); err == nil {
				for _, b := range hb {
					known[b] = true
				}
			}
		}
		if !known[project] && rpc.reindex != nil {
			_ = rpc.reindex(project) // best-effort; a cold first turn is acceptable
		}
	}

	// No bundles at all → leave the workspace un-scoped (recall queries all/none).
	// Remove any stale scope file from a previous run (when bundles were wired)
	// so the in-VM recall stops forwarding dead bundle ids. Best-effort.
	if len(ids) == 0 {
		_ = os.Remove(filepath.Join(workspace, ".pi-stack", "knowledge.scope"))
		return
	}
	_ = writeKnowledgeScope(workspace, ids)
}

// projectBundle reads <workspace>/.pi-stack/knowledge and resolves it to a
// canonical bundle id: a git URL is cloned/pulled into the cache (same resolver
// as `use`), an absolute path is used as-is, and a relative path is taken
// relative to the workspace. Returns "" when there is no pointer or it can't be
// resolved (non-fatal: the workspace just has no project bundle this run).
func projectBundle(workspace string) string {
	line := readProjectPointer(workspace)
	if line == "" {
		return ""
	}
	var local string
	switch {
	case isGitURL(line):
		r, err := resolveBundleRef(line, knowledgeCacheDir(), io.Discard)
		if err != nil {
			return ""
		}
		local = r
	case filepath.IsAbs(line):
		local = line
	default:
		local = filepath.Join(workspace, line)
	}
	return canonicalizeKnowledgeBundle(local)
}

// writeProfileFile writes <workspace>/.pi-stack/profile: the active profile name
// on one line. The in-VM memory extensions read it to scope recall/capture,
// mirroring how writeKnowledgeScope communicates the knowledge bundle set. It is
// launcher-generated, per-run, and gitignored. Always written (even "default")
// so a stale name from a previous run can never linger. Best-effort.
// writeOllamaBridgeFile writes <workspace>/.pi-stack/ollama-bridge.model: the
// local model tag the in-VM ollama-bridge should expose (interactive cycle + the
// router's local option). Configured on the host with `pi-stack config set
// ollama_bridge_model`; the bridge reads it (env var still overrides). Per-run,
// gitignored, best-effort — an absent file just means the bridge uses its default.
func writeOllamaBridgeFile(workspace, model string) {
	model = strings.TrimSpace(model)
	if model == "" {
		model = config.DefaultOllamaBridgeModel
	}
	dir := filepath.Join(workspace, ".pi-stack")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "ollama-bridge.model"), []byte(model+"\n"), 0o644)
}

func writeProfileFile(workspace, profile string) error {
	if strings.TrimSpace(profile) == "" {
		profile = config.DefaultProfile
	}
	// For a NAMED (non-default) profile, a write failure means recall/capture in
	// the VM will silently fall back to the default bucket — wrong scope, not a
	// no-op. Warn to stderr (still best-effort; don't abort the launch). The
	// default profile stays silent (its absence IS the default behavior).
	named := profile != config.DefaultProfile
	warn := func(err error) error {
		if named {
			fmt.Fprintf(os.Stderr, "pi-stack: warning: could not write .pi-stack/profile for profile %q (recall/capture will fall back to the default bucket): %v\n", profile, err)
		}
		return err
	}
	dir := filepath.Join(workspace, ".pi-stack")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return warn(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "profile"), []byte(profile+"\n"), 0o644); err != nil {
		return warn(err)
	}
	return nil
}

// writeKnowledgeScope writes <workspace>/.pi-stack/knowledge.scope: one canonical
// bundle id per line, trailing newline. This is the launcher-generated,
// per-run, gitignored file the recall extension reads (the committed pointer is
// .pi-stack/knowledge).
func writeKnowledgeScope(workspace string, ids []string) error {
	dir := filepath.Join(workspace, ".pi-stack")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	content := strings.Join(ids, "\n") + "\n"
	return os.WriteFile(filepath.Join(dir, "knowledge.scope"), []byte(content), 0o644)
}

// defaultKnowledgeRPC wires the real, short-timeout HTTP JSON-RPC client for the
// knowledge daemon on 127.0.0.1:11436.
func defaultKnowledgeRPC() knowledgeRPC {
	const port = 11436
	return knowledgeRPC{
		up:      func() bool { return dialLocalPort(port) },
		health:  func() ([]string, error) { return knowledgeHealthBundles(port) },
		reindex: func(bundle string) error { return knowledgeReindex(port, bundle) },
	}
}

// dialLocalPort reports whether something is listening on 127.0.0.1:port within
// a short timeout (so a down daemon costs the launch almost nothing).
func dialLocalPort(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// knowledgeRPCCall POSTs a JSON-RPC 2.0 request to the daemon and returns the
// decoded envelope, mapping a JSON-RPC error object to a Go error. Short
// timeout: launch must never hang on a slow daemon.
func knowledgeRPCCall(port int, method string, params map[string]any) (map[string]any, error) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Post(fmt.Sprintf("http://127.0.0.1:%d/", port), "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var parsed map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	if e, ok := parsed["error"].(map[string]any); ok {
		return nil, fmt.Errorf("rpc %s: %v", method, e["message"])
	}
	return parsed, nil
}

// knowledgeHealthBundles returns the bundle ids the daemon reports as indexed.
func knowledgeHealthBundles(port int) ([]string, error) {
	r, err := knowledgeRPCCall(port, "health", nil)
	if err != nil {
		return nil, err
	}
	result, _ := r["result"].(map[string]any)
	return toStringSlice(result["bundles"]), nil
}

// knowledgeReindex fires a reindex for a single bundle path (idempotent add).
func knowledgeReindex(port int, bundle string) error {
	_, err := knowledgeRPCCall(port, "reindex", map[string]any{"bundle_paths": []string{bundle}})
	return err
}

// toStringSlice coerces a decoded JSON array (any of []any / []string) to
// []string, dropping non-strings.
func toStringSlice(v any) []string {
	switch xs := v.(type) {
	case []string:
		return xs
	case []any:
		out := make([]string, 0, len(xs))
		for _, x := range xs {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// deriveSandboxName mirrors sbx's default `pi-stack-<workspace-basename>` so the
// launcher owns (and can recreate) the same sandbox sbx would auto-name.
func deriveSandboxName(ws string) string {
	abs, err := filepath.Abs(ws)
	if err != nil {
		abs = ws
	}
	base := filepath.Base(abs)
	if base == "" || base == "." || base == string(filepath.Separator) {
		base = "workspace"
	}
	return "pi-stack-" + base
}

// The tri-state sandbox probe (running/stopped/absent/unknown) that drives the
// create-vs-reattach-vs-replace decision lives in task.go as probeTaskSandbox +
// sbxState — run.go reuses it rather than duplicating the `sbx ls` parse.
