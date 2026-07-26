package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"pix/host/config"
)

// runRun implements bare `pix [DIR]` and `pix run ...`. It reads the
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
		fmt.Fprintf(os.Stderr, "pix run: %v\n\n", err)
		fmt.Fprint(os.Stderr, runUsage)
		os.Exit(2)
	}

	// Default the session intent from config (run_intent, the "overlord") when the
	// user pinned neither --model nor --intent. This is what flips the top-level
	// interactive orchestrator to its configured vendor (the stack ships
	// run_intent=overlord -> GPT-5.6 Sol). Track that it came from config, not a
	// flag: a bad config-sourced intent must NOT brick the launch the way an
	// explicit --intent typo does.
	intentFromConfig := false
	if o.Intent == "" && o.Model == "" {
		if cfg, cerr := config.Load(); cerr == nil && strings.TrimSpace(cfg.RunIntent) != "" {
			o.Intent = strings.TrimSpace(cfg.RunIntent)
			intentFromConfig = true
		}
	}

	// "none"/"off" is the explicit opt-out: use pi's own default model, no router
	// (honored for both --intent and run_intent). Kept here so a user who does not
	// want the overlord default has a first-class, documented escape.
	if o.Model == "" && (strings.EqualFold(o.Intent, "none") || strings.EqualFold(o.Intent, "off")) {
		o.Intent = ""
	}

	// Resolve --intent to a concrete session model via the router (unless --model
	// already pinned one, which wins). This makes the INTERACTIVE session use the
	// same cost/latency/accuracy routing the subagent crew uses.
	if o.Intent != "" && o.Model == "" {
		m, rerr := resolveSessionModel(o.Intent)
		if rerr != nil {
			if intentFromConfig {
				// Degrade to pi's own default model rather than block a launch on a
				// misconfigured run_intent. Loud, non-fatal.
				fmt.Fprintf(os.Stderr, "pix: run_intent %q did not resolve (%v); using pi's default model. Fix with `pix config set run_intent <intent>`.\n", o.Intent, rerr)
				o.Intent = ""
			} else {
				fmt.Fprintf(os.Stderr, "pix run: --intent %q: %v\n", o.Intent, rerr)
				os.Exit(2)
			}
		} else {
			o.Model = m
			fmt.Fprintf(os.Stderr, "pix: intent %q -> model %s\n", o.Intent, m)
		}
	}

	// Bare-minimum key bootstrap: a pi session needs at least one model provider
	// key. `run` otherwise stays out of the way (no onboarding, no nags), but with
	// NO usable key it can't launch — so auto-run the key flow (resolve your
	// 1Password refs into sbx; on a TTY, steer you to 1Password). When a key is
	// already present this is a cheap no-op. Then refuse ONLY when we can POSITIVELY
	// confirm no key (tri-state): sbx absent OR its control plane unprobeable =
	// can't verify = proceed, never a false refusal on a transient failure.
	//
	// The probe result is KEPT (keyEvidence) and handed to the readiness
	// snapshot further down, so rendering readiness costs `run` no second
	// `sbx secret ls`.
	var keyEvidence sbxKeyEvidence
	if _, err := defaultShellEnv().lookPath("sbx"); err == nil {
		env := defaultShellEnv()
		bootstrapProviderKeys(env, os.Stdin, os.Stderr, isTTY(os.Stdin))
		keyEvidence = probeSbxKeyEvidence(env)
		if keyEvidence.ok() && !anyModelKeyInOutput(keyEvidence.out) {
			fmt.Fprint(os.Stderr, modelKeyMissingMessage(env))
			os.Exit(1)
		}
	}

	// Reconcile any control-plane proposal a prior in-session onboarding wrote
	// (<workspace>/.pix/onboarding.json): validate it, show the diff, apply
	// under a [Y/n] gate, register newly-enabled MCP servers, delete the file. This
	// runs BEFORE loadResolvedConfig so a fresh create picks up the applied config.
	// Best-effort and non-blocking on a non-TTY (it just leaves the file).
	reconcileOnboarding(o.Workspace, defaultShellEnv(), os.Stdin, os.Stdout, false, isTTY(os.Stdin))

	// Load the config for the rest of run (kits, mcp, gog, pack). The
	cfg, _, err := loadResolvedConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix run: %v\n", err)
		os.Exit(1)
	}

	// Own the sandbox name so we can manage its lifecycle. sbx would otherwise
	// auto-derive `pix-<dir>`. Resolved (and the sandbox state probed) BEFORE
	// any create-only input resolution below — a plain re-attach must never fail on
	// a --dev/checkout or --kit problem it doesn't even need.
	if o.Name == "" {
		o.Name = deriveSandboxName(o.Workspace)
	}
	state := probeTaskSandbox(defaultShellEnv(), o.Name)

	// Mirror sbx's own model: an existing sandbox (running OR stopped) RE-ATTACHES
	// instead of refusing/recreating — the create-only flags (--kit/--template/
	// --mcp/config-stacked-kits/--dev/dev-skills) only apply to a fresh create, so they are
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

		// Which RELEASE does the auto pin resolve to? By default the newest
		// published one rather than this binary's own, so an installed launcher
		// picks up kit/image fixes instead of being frozen at the version it was
		// installed at. Every failure (offline, GitHub down, junk response) yields
		// "" and falls back to the stamped pin. See kitref.go.
		if !o.Dev && !kitOverride {
			latest := ""
			if released && o.KitRef == "" && strings.TrimSpace(cfg.VersionPin) == "" {
				latest = resolveLatestRelease(&http.Client{Timeout: latestReleaseTimeout}, time.Now())
			}
			ref, src := resolveKitRef(version, o.KitRef, cfg.VersionPin, latest)
			o.KitRef = ref
			if msg := kitRefNotice(version, ref, src); msg != "" {
				fmt.Fprintln(os.Stderr, msg)
			}
		}

		if o.Dev {
			// --dev needs a resolvable repo checkout; fail loud otherwise. --dev is
			// create/replace-only (this branch), so it is a no-op on a plain re-attach.
			root, err := resolveRepoRoot()
			if err != nil {
				fmt.Fprintf(os.Stderr, "pix run --dev: %v\n", err)
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
				fmt.Fprintf(os.Stderr, "pix: unreleased build %q — using local checkout kit %s%s\n", version, o.LocalKit, note)
			} else {
				fmt.Fprintf(os.Stderr, "pix: unreleased build %q and no pix checkout found — "+
					"kit tracks #ref=main (may not match this binary). Use `pix run --dev` from a "+
					"checkout or `pix run --kit <path-or-git-url>` to override.\n", version)
			}
		}
	} else if o.Dev {
		fmt.Fprintln(os.Stderr, "pix: --dev is create/replace-only; re-attaching to the existing sandbox as-is (use --replace to recreate with --dev)")
	}

	// Active pack: mount its skills/ + knowledge/ so the pack's context loads in
	// this sandbox. --pack overrides config.Pack; with neither set, no pack is
	// active. Create-time only (skills + knowledge are create-time mounts; a
	// re-attach keeps what it was made with).
	// effectivePack is the pack root that ACTUALLY loaded and applied, as
	// opposed to activePackRoot(cfg.Pack, o.Pack) (the merely CONFIGURED one).
	// It defaults to the configured root for a re-attach (no applyPackToLaunch
	// call — writePackContextFiles below still attempts its own load and
	// degrades to unscoped on failure), and is overwritten with
	// applyPackToLaunch's honest return on a create, where it becomes "" if
	// the pack degraded via errNotAPack. This is what keeps the sandbox.pack
	// marker and memory scope from disagreeing about what actually loaded.
	effectivePack := activePackRoot(cfg.Pack, o.Pack)
	if willCreate(state, o.Replace) {
		// Fatal on error (explicit --pack that doesn't load, or a declared
		// sandbox proxy whose kit can't be built — round-4 F2 fail-closed):
		// never create a sandbox missing context the pack declared.
		root, err := applyPackToLaunch(cfg, &o, defaultShellEnv())
		if err != nil {
			fmt.Fprintf(os.Stderr, "pix: %v\n", err)
			os.Exit(1)
		}
		effectivePack = root
	}

	// Local-image preflight: when we're about to pin --template to a locally loaded
	// tag (a --dev / unreleased build) and that tag is NOT in sbx's image store, sbx
	// would try to PULL it from the registry — but local-* tags are never published,
	// so the user gets a confusing interactive "pull? use cached?" prompt and a slow
	// hang. Refuse fast with the real fix instead. (Only on create; a re-attach
	// reads the sandbox's own spec and doesn't re-pin --template.)
	if willCreate(state, o.Replace) && o.LocalImageTag != "" && len(o.Kits) == 0 && o.LocalKit != "" && o.Template == "" {
		if !localImageLoaded(defaultShellEnv(), o.LocalImageTag) {
			fmt.Fprintf(os.Stderr, "pix: local image %s:%s is not loaded in sbx.\n", dockerImageRepo, o.LocalImageTag)
			fmt.Fprintln(os.Stderr, "It's a local build (never published), so sbx would try to pull it and stall on a prompt.")
			fmt.Fprintln(os.Stderr, "Load this build into sbx first, from your pix checkout:")
			fmt.Fprintln(os.Stderr, "  make load")
			os.Exit(1)
		}
	}

	// Same preflight for an explicit --template that names a local-* build: it's
	// never published, so an unloaded ref would make sbx stall on a pull prompt.
	// Only local-* tags get this guard — a published ref is legitimately pullable.
	if willCreate(state, o.Replace) && o.Template != "" {
		if tag := templateTag(o.Template); strings.HasPrefix(tag, "local-") && !localImageLoaded(defaultShellEnv(), tag) {
			fmt.Fprintf(os.Stderr, "pix: --template %s is not loaded in sbx.\n", o.Template)
			fmt.Fprintln(os.Stderr, "It's a local build (never published), so sbx would try to pull it and stall on a prompt.")
			fmt.Fprintln(os.Stderr, "Load it first, from the checkout that built it:  make load")
			os.Exit(1)
		}
	}

	// Resolve every configured MCP server to attach at create (--static-mcp).
	// S01: all of them preload — no more eager/lazy split. Only needed on a
	// create — a re-attach never sends --static-mcp.
	if willCreate(state, o.Replace) {
		o.StaticMCP = allPreloadedMCP(append(append([]string(nil), cfg.MCP...), o.MCP...))
	}

	plan := planSandboxLaunch(state, o.Replace, cfg, o, version)
	if plan.Err != nil {
		// Fail closed BEFORE any output claims a replace/create/reattach is
		// happening, and before RmFirst or exec — see planSandboxLaunch's
		// sbxUnknown+replace case.
		fmt.Fprintf(os.Stderr, "pix run: %v\n", plan.Err)
		os.Exit(1)
	}
	switch {
	case o.Replace:
		fmt.Fprintf(os.Stderr, "pix run: replacing sandbox %q\n", o.Name)
	case plan.Reattach && state == sbxRunning:
		fmt.Fprintf(os.Stderr, "pix run: re-attaching to running sandbox %q\n", o.Name)
	case plan.Reattach:
		fmt.Fprintf(os.Stderr, "pix run: starting + attaching existing sandbox %q (use --replace to recreate with current kit/mcp/flags)\n", o.Name)
	}
	// finding #8: a reattach is honest about live-vs-recreate for MOST facets
	// above, but says nothing about a pack switched since this sandbox was
	// created — its mcp/bin/skills are create-only and won't attach without
	// --replace. Surface that explicitly rather than silently reattaching to a
	// stale facet set.
	if msg := stalePackReattachWarning(cfg, o, plan.Reattach); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
	}
	// Product gap #2: reattach honesty. Separate from stalePackReattachWarning
	// (which only speaks to skills/bin drift from a pack switch). This checks
	// MCP attachment PRECISELY, via the launcher's own receipt, regardless of
	// WHY a desired server might not be attached (config change, pack change,
	// explicit --mcp, or no receipt at all). Never auto-loads, only reports.
	if msg := mcpReattachWarning(cfg, o, plan.Reattach); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
	}
	if plan.RmFirst {
		if err := applyReplaceRm(defaultShellEnv(), plan, o.Name); err != nil {
			fmt.Fprintf(os.Stderr, "pix run: %v\n", err)
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

	// Readiness, rendered from the SHARED lazy snapshot (readiness_launch.go)
	// and reusing the key evidence the launch gate above already paid for. Two
	// rules, both deliberate:
	//   - AT MOST launchWarningLimit rows, then a single count pointing at
	//     doctor. `run` is the daily command; a wall of readiness text here is
	//     how a user learns to skip readiness output entirely.
	//   - It NEVER blocks. The only launch-stopping condition is the missing
	//     provider key handled above, because that is the only gap that makes
	//     the session useless rather than degraded.
	renderReadinessWarnings(os.Stderr, fastReadinessSnapshot(cfg, defaultShellEnv(), keyEvidence), launchWarningLimit)

	// Knowledge scope: resolve this workspace's bundle set (global config bundles
	// + the project's .pix/knowledge pointer), lazily reindex the project
	// bundle when the daemon is up and doesn't know it yet, and write the scope
	// file the in-VM recall extension reads. Entirely best-effort: it never blocks
	// or fails the launch (recall just misses a bundle this run).
	wireKnowledgeScope(cfg, o.Workspace, defaultKnowledgeRPC())

	// Local model + memory scope: hand the configured ollama_bridge_model to the
	// in-VM ollama-bridge, and the active pack's memory_scope (default: the pack
	// name; "default" is the shared/unscoped tag) to the in-VM recall/capture
	// extensions, via per-run workspace files. No active pack -> writeMemoryScope
	// removes any stale file (unscoped recall). Best-effort throughout: an
	// unloadable pack degrades to unscoped rather than failing run. Shared with
	// `task new` via writePackContextFiles (pack.go) so both launch paths write
	// the SAME pack context.
	writePackContextFiles(cfg, o, effectivePack)

	// finding G + round-3 R3: record the pack this sandbox is being CREATED
	// with (workspace marker), so a later re-attach can warn precisely when the
	// create-time pack differs from the then-active pack — and stay silent when
	// they match. Written ONLY on a DEFINITE create (--replace, or a positive
	// "absent" probe) — never on sbxUnknown: willCreate optimistically prepares
	// create args for a FAILED probe, but sbx may well re-attach the OLD sandbox
	// then, and overwriting the marker with the active pack would silence the
	// stale-pack warning for a sandbox still carrying its create-time pack. On
	// a re-attach/unknown path any existing marker stays untouched. Written
	// from effectivePack (what applyPackToLaunch actually applied), NOT
	// activePackRoot(cfg.Pack, o.Pack) — a degraded (errNotAPack) launch must
	// record NO pack, or a later reattach's stalePackReattachWarning would
	// wrongly stay silent comparing marker == active while the sandbox never got
	// the pack's facets.
	if definitelyCreating(state, o.Replace) {
		writeSandboxPackMarker(o.Workspace, effectivePack)
	}

	// Trusted host state: the host-visible facts the fenced agent can't see for
	// itself (keys/services/knowledge/gog/mcp/models/pack/identity). This
	// travels ONLY inside the launcher-generated initial prompt (the pi
	// passthrough arg carrying generatedInputMarker, e.g. onboardingKickoff) —
	// never as a workspace file, which a cloned repo could plant or leave stale.
	// injectTrustedHostState is a no-op (and probes nothing) when no such arg is
	// present, so a normal run never pays for or produces onboarding truth.
	//
	// The --pack override only takes effect on a CREATE (packs mount at creation);
	// on a re-attach the sandbox keeps whatever pack it was made with, so don't
	// claim the override in the payload — fall back to the persisted active pack.
	packForState := ""
	if willCreate(state, o.Replace) {
		packForState = o.Pack
	}
	// This is a HARD contract, not best-effort: when a generated prompt IS
	// present, it is the fenced in-VM agent's ONLY source of trusted host-visible
	// truth, so a launch that can't build/encode it must ABORT before exec'ing
	// sbx rather than hand the agent a generated prompt with no trusted payload.
	args, err := injectTrustedHostState(plan.Args, cfg, defaultShellEnv(), packForState)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix run: could not build trusted host state: %v\n", err)
		os.Exit(1)
	}

	if os.Getenv("PIX_DEBUG") != "" {
		fmt.Fprintln(os.Stderr, "+ sbx "+strings.Join(args, " "))
	}

	cmd := exec.Command("sbx", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// The default path injects no credential bearer: gog authenticates on the host
	// inside the gateway-spawned MCP server, so the sandbox never sees a Google
	// token. A future external credential broker plugin would set its own bearer through
	// the retained generic seam and own that plumbing itself.
	cmd.Env = os.Environ()
	// S03: on a DEFINITE create (definitelyCreating — the same predicate that
	// gates the sandbox.pack marker above, for the identical reason: an
	// sbxUnknown probe may still have sbx reattach the OLD sandbox, and a plain
	// re-attach must never write a fresh create receipt over one), record the
	// create receipt ONLY after this exact `sbx run` exec has itself succeeded
	// — never before, never on failure, never on reattach.
	if err := execSbxRunAndRecordCreate(cmd, definitelyCreating(state, o.Replace), o.Name, canonicalWorkspacePath(o.Workspace), o.StaticMCP); err != nil {
		var rerr *receiptRecordError
		if errors.As(err, &rerr) {
			// The sandbox itself WAS created successfully — only the local
			// receipt failed. Say so honestly rather than implying the launch
			// failed, but still exit non-zero: doctor/status must not be told
			// this sandbox's MCP set is recorded when it isn't.
			fmt.Fprintf(os.Stderr, "pix run: %v\n", rerr)
			fmt.Fprintln(os.Stderr, "the sandbox itself launched fine; only pix's local record of its preloaded MCP set failed to write. Check state-dir permissions and re-run `pix doctor`.")
			os.Exit(1)
		}
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
				fmt.Fprintf(os.Stderr, "pix run: re-attach failed; recreate it with: %s\n", runReplaceCommand(o.Workspace))
			}
			os.Exit(exit.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "pix run: exec sbx: %v\n", err)
		if plan.Reattach {
			fmt.Fprintf(os.Stderr, "pix run: re-attach failed; recreate it with: %s\n", runReplaceCommand(o.Workspace))
		}
		os.Exit(1)
	}
}

// Creation-evidence poll seams. After `sbx run` is STARTED (not waited), the
// create path polls for the named sandbox to become visible through
// sandboxAppearProbeFn, records the create receipt the moment it is, and only
// then settles into Wait — so status/doctor can render preload provenance
// WHILE the interactive session is alive, not hours later when it exits.
// Injectable so tests never shell out or sleep for real; production polls
// `sbx ls` via probeTaskSandbox. The timeout is deliberately generous: a
// first create may pull the image for minutes before the sandbox exists, and
// the poll only runs while `sbx run` itself is still alive, so a large bound
// costs the happy path nothing.
var (
	sandboxAppearProbeFn = func(name string) sbxState {
		return probeTaskSandbox(defaultShellEnv(), name)
	}
	sandboxAppearPollInterval = 500 * time.Millisecond
	sandboxAppearPollTimeout  = 15 * time.Minute
)

// sandboxAppeared reports whether st is POSITIVE existence evidence: the name
// is present in `sbx ls`, running or not. Absent keeps polling; unknown (a
// failed probe) proves nothing and also keeps polling — never record a create
// receipt on an indeterminate read.
func sandboxAppeared(st sbxState) bool { return st == sbxRunning || st == sbxStopped }

// recordCreateReceipt commits the create receipt for sandbox — called ONLY by
// execSbxRunAndRecordCreate, once its creation-evidence poll has positively
// seen run.go's OWN `sbx run` create appear. preloaded is the EXACT
// --static-mcp set that launch emitted (o.StaticMCP:
// allPreloadedMCP(cfg.MCP+o.MCP), which already folds in every
// active/transient pack integration's MCP server — applyPackToLaunch runs
// before this set is computed), so a receipt read later never disagrees with
// what create actually requested. merge=true (the normal path: the
// pre-create clear succeeded) preserves loads a concurrent `pix mcp
// load` appended during the create window; merge=false (the clear could not
// be proven) replaces outright so a prior lifetime's loads can never survive.
// workspace is the CANONICAL workspace path the create was for
// (canonicalWorkspacePath) — the receipt's workspace->sandbox identity that
// resolveWorkspaceSandbox reads back for custom-named sandboxes.
func recordCreateReceipt(sandbox, workspace string, preloaded []string, merge bool) error {
	dir, err := sandboxMCPStateDirFn()
	if err != nil {
		return &receiptRecordError{op: "create", sandbox: sandbox, err: fmt.Errorf("resolving pix state dir: %w", err)}
	}
	var werr error
	if merge {
		werr = commitCreateReceipt(dir, sandbox, workspace, preloaded, nil)
	} else {
		werr = writeCreateReceipt(dir, sandbox, workspace, preloaded, nil)
	}
	if werr != nil {
		return &receiptRecordError{op: "create", sandbox: sandbox, err: werr}
	}
	return nil
}

// execSbxRunAndRecordCreate runs cmd (the already-composed `sbx run ...`
// invocation, stdio already wired by the caller — Start/Wait preserve it) and
// owns the create-receipt lifecycle around it:
//
//   - writeReceipt=false (a plain re-attach, or an inconclusive sbxUnknown
//     probe — see definitelyCreating): cmd.Run() and nothing else. A re-attach
//     writes nothing, clears nothing.
//   - writeReceipt=true (a definite create/replace): any stale receipt from a
//     prior same-name lifetime is CLEARED under the per-sandbox lock BEFORE
//     the create starts; then cmd is STARTED and the sandbox's appearance is
//     polled (sandboxAppearProbeFn, bounded by sandboxAppearPollTimeout).
//     The moment it appears the receipt is committed — while the interactive
//     session is still alive — merging any loads recorded since the clear;
//     then we Wait for the session.
//
// Outcome contract: if the process exits BEFORE creation evidence, its error
// is returned and no receipt is written (a final probe on a CLEAN exit still
// records — evidence found at exit is evidence). If the receipt cannot be
// recorded after the sandbox positively appeared (or the poll timed out with
// the session still running), the session is still waited to completion and
// the failure surfaces as *receiptRecordError — the caller reports
// "launched/attached, but state unrecorded" and exits non-zero, never a
// silent success and never confused with a launch failure. The Wait goroutine
// always terminates when the process exits and its result is always drained
// — no goroutine leaks on any path.
func execSbxRunAndRecordCreate(cmd *exec.Cmd, writeReceipt bool, sandbox, workspace string, preloaded []string) error {
	if !writeReceipt {
		return cmd.Run()
	}

	// Pre-create clear (B): under the same per-sandbox lock the writers use,
	// drop any receipt from a previous incarnation of this name so its load
	// history can never leak into the new lifetime. merge stays false unless
	// the clear POSITIVELY succeeded — the commit then merges only loads
	// appended after this point; an unproven clear degrades to a plain
	// replace, which cannot resurrect old loads.
	merge := false
	if stateDir, err := sandboxMCPStateDirFn(); err == nil {
		if err := clearSandboxMCPReceipt(stateDir, sandbox); err == nil {
			merge = true
		}
	}

	if err := cmd.Start(); err != nil {
		return err
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var recErr error
	deadline := time.Now().Add(sandboxAppearPollTimeout)
	ticker := time.NewTicker(sandboxAppearPollInterval)
	defer ticker.Stop()
poll:
	for {
		if sandboxAppeared(sandboxAppearProbeFn(sandbox)) {
			recErr = recordCreateReceipt(sandbox, workspace, preloaded, merge)
			break poll
		}
		if time.Now().After(deadline) {
			recErr = &receiptRecordError{op: "create", sandbox: sandbox,
				err: fmt.Errorf("timed out after %s waiting for the sandbox to appear in `sbx ls`; its preloaded MCP set was not recorded", sandboxAppearPollTimeout)}
			break poll
		}
		select {
		case werr := <-waitCh:
			// The process exited before creation evidence. A failed exec
			// surfaces its OWN error, receiptless. A clean exit gets ONE final
			// probe (the sandbox may have appeared exactly as it exited, e.g. a
			// detached create); still no evidence means honestly no receipt.
			if werr != nil {
				return werr
			}
			if sandboxAppeared(sandboxAppearProbeFn(sandbox)) {
				return recordCreateReceipt(sandbox, workspace, preloaded, merge)
			}
			return nil
		case <-ticker.C:
		}
	}
	// Receipt outcome decided (recorded, failed, or timed out) — now hand the
	// terminal back to the session and wait it out. Its own failure dominates
	// the report; a receipt failure surfaces only on a clean session exit.
	if werr := <-waitCh; werr != nil {
		return werr
	}
	return recErr
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
	// The launcher itself removed this sandbox, so its MCP receipt describes a
	// dead lifetime — clear it (E). Best-effort with a warning: the pre-create
	// clear in execSbxRunAndRecordCreate is the correctness backstop.
	if err := clearRemovedSandboxReceipt(name); err != nil {
		fmt.Fprintf(os.Stderr, "pix: warning: removed sandbox %q but could not clear its mcp receipt: %v\n", name, err)
	}
	return nil
}

// sandboxPackMarkerPath is <workspace>/.pix/sandbox.pack: the pack root
// this workspace's sandbox was CREATED with (finding G). Written on every
// create (removed when created pack-less), never on a re-attach, so a later
// re-attach compares create-time truth against the CURRENT active pack instead
// of guessing from the active pack alone.
func sandboxPackMarkerPath(workspace string) string {
	return filepath.Join(workspace, ".pix", "sandbox.pack")
}

// writeSandboxPackMarker records the pack root a sandbox is being created with
// (or removes the marker when creating pack-less). Best-effort: a failed write
// only costs a future stale-pack reminder, never the launch. Symlink-safe via
// writeWorkspaceStateFile (a cloned repo can ship .pix/sandbox.pack as a
// tracked symlink) and removeWorkspaceStateFile (a cloned repo can ship
// .pix ITSELF as a symlink to another repo's .pix, which a plain
// os.Remove would traverse and delete through).
func writeSandboxPackMarker(workspace, packRoot string) {
	if strings.TrimSpace(packRoot) == "" {
		_ = removeWorkspaceStateFile(workspace, "sandbox.pack")
		return
	}
	_ = writeWorkspaceStateFile(workspace, "sandbox.pack", []byte(canonicalizePackRoot(packRoot)+"\n"), 0o644)
}

// readSandboxPackMarker returns the create-time pack root recorded for this
// workspace's sandbox, or "" when no marker exists (a sandbox created before
// markers existed, or created pack-less).
func readSandboxPackMarker(workspace string) string {
	b, err := os.ReadFile(sandboxPackMarkerPath(workspace))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// stalePackReattachWarning returns the "stale pack" reminder `runRun` prints
// when RE-ATTACHING (not creating, not --replace) to a sandbox whose
// CREATE-TIME pack differs from the current active pack (finding G). The
// create-time pack comes from the workspace marker written at create
// (writeSandboxPackMarker); comparing marker vs active is what makes the
// message honest in BOTH directions:
//   - no false warning when the sandbox already carries the current pack
//     (marker == active pack), and
//   - a warning after `pack rm` (marker set, active empty): the old sandbox
//     still has the removed pack's create-time bin/skills baked in.
//
// No marker => no warning: a sandbox created before markers existed (or
// pack-less) gives us nothing to compare, and guessing from the active pack
// alone is exactly what produced the old false positives.
//
// Deliberately says nothing about MCP: mcpReattachWarning (product gap #2)
// owns that claim PRECISELY, via the launcher's own receipt, for every
// desired server regardless of whether a pack changed. Folding a vaguer
// "mcp may be stale" guess in here would duplicate that check and could
// contradict it (e.g. this warning firing on pack drift while the receipt
// proves every MCP server is in fact already attached).
func stalePackReattachWarning(cfg *config.Config, o runOpts, reattaching bool) string {
	if !reattaching || o.Replace {
		return ""
	}
	created := readSandboxPackMarker(o.Workspace)
	if created == "" {
		return ""
	}
	active := ""
	if root := activePackRoot(cfg.Pack, o.Pack); root != "" {
		active = canonicalizePackRoot(root)
	}
	if created == active {
		return ""
	}
	if active == "" {
		return fmt.Sprintf("pix: re-attaching without --replace — this sandbox was created with pack %q (since detached); its bin/skills are still attached until you recreate: %s", created, runReplaceCommand(o.Workspace))
	}
	return fmt.Sprintf("pix: re-attaching without --replace — this sandbox was created with pack %q, not the active pack %q; the active pack's bin/skills won't attach until you recreate: %s", created, active, runReplaceCommand(o.Workspace))
}

// desiredMCPUniverse computes the FULL set of MCP server names this
// invocation would preload at CREATE: cfg.MCP, the active/transient pack's
// integration servers (packMcpNames), and any explicit --mcp, deduped via
// allPreloadedMCP. It is the read-only twin of applyPackToLaunch's pack-fold
// step (pack.go) used ONLY for this comparison: a re-attach never mounts a
// pack (skills/bin/knowledge are create-time only) and must never trigger
// applyPackToLaunch's mount/kit-synthesis side effects just to answer "what
// would this invocation want". A pack that fails to load degrades to
// cfg.MCP+o.MCP alone here (the same as it always did before packs existed)
// rather than blocking a reattach comparison on a broken pack.
func desiredMCPUniverse(cfg *config.Config, o runOpts) []string {
	names := append([]string(nil), cfg.MCP...)
	if root := activePackRoot(cfg.Pack, o.Pack); root != "" {
		if p, err := loadPack(root); err == nil {
			names = append(names, packMcpNames(p)...)
		}
	}
	names = append(names, o.MCP...)
	return allPreloadedMCP(names)
}

// mcpLoadCommand returns the exact `pix mcp load NAME [WORKSPACE]`
// command for name, workspace-qualified the same way runReplaceCommand is
// (bare for ".", quoted otherwise) so the two recovery commands read
// consistently. Both name and workspace are shell-quoted via the shared
// shellQuoteArg (closure finding #3) — a server name is ordinarily a plain
// token, but quoting it too costs nothing and keeps every generated
// copy-paste command uniformly safe.
func mcpLoadCommand(name, workspace string) string {
	if workspace == "" || workspace == "." {
		return "pix mcp load " + shellQuoteArg(name)
	}
	return "pix mcp load " + shellQuoteArg(name) + " " + shellQuoteArg(workspace)
}

// mcpLoadHints joins one mcpLoadCommand per name (mcp load only ever attaches
// one server at a time, so N missing names need N commands).
func mcpLoadHints(names []string, workspace string) string {
	cmds := make([]string, 0, len(names))
	for _, n := range names {
		cmds = append(cmds, mcpLoadCommand(n, workspace))
	}
	return strings.Join(cmds, "; ")
}

// mcpReattachWarning is `pix run`'s reattach honesty check (product gap
// #2): on a RE-ATTACH (not a create, not --replace) it compares the DESIRED
// MCP universe for THIS invocation (desiredMCPUniverse) against the
// sandbox's own launcher receipt (sandboxmcpstate.go) and warns, BEFORE
// reattaching, about any desired name the receipt cannot PROVE is attached
// (a positive preloaded/loaded claim in a valid receipt is proof, anything
// else is a gap). It never auto-loads, only reports, and always offers
// BOTH exact remediation paths: a live `pix mcp load NAME <workspace>`
// per missing name, or `pix run <workspace> --replace` to recreate with
// the current context. A receipt entry for a name that is no longer desired
// (dropped from config since create) is legitimate history and is never
// mentioned; only desired names are ever checked.
//
// No desired servers at all -> nothing to check, silent. An unresolvable
// state dir, an absent receipt, or an unverifiable one (corrupt / wrong
// schema / wrong sandbox identity) all mean the SAME honest thing for every
// desired name: attachment cannot be verified from here.
func mcpReattachWarning(cfg *config.Config, o runOpts, reattaching bool) string {
	if !reattaching || o.Replace {
		return ""
	}
	desired := desiredMCPUniverse(cfg, o)
	if len(desired) == 0 {
		return ""
	}
	stateDir, err := sandboxMCPStateDirFn()
	if err != nil {
		return fmt.Sprintf("pix: re-attaching without --replace: could not resolve local state (%v), so attachment for %s cannot be verified. Attach live: %s. Or recreate with current context: %s",
			err, strings.Join(desired, ", "), mcpLoadHints(desired, o.Workspace), runReplaceCommand(o.Workspace))
	}
	receipt, rstatus, _ := readSandboxMCPReceipt(stateDir, o.Name)
	if rstatus.Unverifiable() {
		return fmt.Sprintf("pix: re-attaching without --replace: this sandbox's MCP receipt is %s, so attachment for %s cannot be verified. Attach live: %s. Or recreate with current context: %s",
			rstatus.String(), strings.Join(desired, ", "), mcpLoadHints(desired, o.Workspace), runReplaceCommand(o.Workspace))
	}
	if rstatus == sandboxMCPStateAbsent {
		return fmt.Sprintf("pix: re-attaching without --replace: no MCP receipt for this sandbox, so attachment for %s cannot be verified. Attach live: %s. Or recreate with current context: %s",
			strings.Join(desired, ", "), mcpLoadHints(desired, o.Workspace), runReplaceCommand(o.Workspace))
	}
	var missing []string
	for _, name := range desired {
		if receiptClaim(receipt, rstatus, name) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return fmt.Sprintf("pix: re-attaching without --replace: %s not proven attached to this sandbox (no receipted preload or load). Attach live: %s. Or recreate with current context: %s",
		strings.Join(missing, ", "), mcpLoadHints(missing, o.Workspace), runReplaceCommand(o.Workspace))
}

// runReplaceCommand returns the exact `pix run [WORKSPACE] --replace`
// recovery command to print for workspace, POSIX-shell-safe via
// shellQuoteArg. Bare "pix run --replace" is only correct for the "."
// default (the sandbox name derives from cwd, so a bare re-run from the SAME
// cwd targets the same sandbox); an EXPLICIT workspace must be echoed back
// verbatim (quoted) — omitting it would target whatever sandbox the CURRENT
// cwd derives, which can be a completely different sandbox than the one that
// just failed to reattach or is carrying a stale pack. Printing the wrong
// recovery command is worse than a slightly longer one.
func runReplaceCommand(workspace string) string {
	if workspace == "" || workspace == "." {
		return "pix run --replace"
	}
	return "pix run " + shellQuoteArg(workspace) + " --replace"
}

// modelProviders are the model-provider secret keys a pi session needs at least
// one of to run. github is deliberately excluded: it authorizes git operations,
// not the model.
var modelProviders = []string{"anthropic", "openai", "google"}

// modelKeyMissingMessage is the guidance printed when no model key could be put
// in place. (The launch-blocking presence CHECK lives in runRun/launchTask via
// sbxModelKeyState's tri-state; this is only the how-to-fix text.)
func modelKeyMissingMessage(env shellEnv) string {
	msg := fmt.Sprintf("pix run: no model provider key is set (need one of %s).\n",
		strings.Join(modelProviders, ", "))
	if providerKeyRefsPresent(env) {
		msg += "You have 1Password key refs; resolve them into sbx with:\n  pix secret sync\n"
	} else {
		msg += "Keys come from 1Password (op is required). Configure them, then re-run:\n" +
			"  pix setup                                                       (guided, all providers)\n" +
			"  pix secret set ANTHROPIC_API_KEY op://vault/item/field && pix secret sync   (one provider)\n"
	}
	return msg
}

// parseRunArgs is a small hand-rolled parser (no cobra, no third-party flags) so
// DIR can appear before or after the flags, matching the flexibility of the old
// bin/pix shell launcher. Everything after `--` is pi passthrough.
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
		case name == "--kit-ref":
			v, err := valueOf(a, &i)
			if err != nil {
				return o, err
			}
			o.KitRef = normalizeKitRef(v)
		case name == "--template":
			v, err := valueOf(a, &i)
			if err != nil {
				return o, err
			}
			o.Template = v
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
	// (`pix run help`, `run doctro`) would silently boot a junk sandbox named
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
		return fmt.Errorf("%q is not a directory. Did you mean `pix %s`?", ws, ws)
	}
	return fmt.Errorf("%q is not a directory", ws)
}

// resolveRepoRoot finds a pix repo checkout for the local kit path, in
// order: $PIX_DEV_ROOT if set, else walking up from the current working
// directory, else the launcher binary's own location (make install symlinks
// ~/.local/bin/pix -> <repo>/out/pix, so the repo is two levels up
// from the resolved binary). Fails when none resolves.
func resolveRepoRoot() (string, error) {
	if r := strings.TrimSpace(os.Getenv("PIX_DEV_ROOT")); r != "" {
		if isRepoRoot(r) {
			return r, nil
		}
		return "", fmt.Errorf("$PIX_DEV_ROOT=%q is not a pix checkout (no pi-kit/spec.yaml)", r)
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
	return "", fmt.Errorf("no pix checkout found (set $PIX_DEV_ROOT or run from inside a checkout)")
}

// repoFromBinary resolves the launcher binary (following symlinks) and reports
// the repo root two levels up (<repo>/out/pix -> <repo>) when it looks
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

// localImageLoaded reports whether sbx's template store carries the local image
// tag. Used to refuse a launch that would otherwise make sbx PULL a
// never-published local-* image (the confusing "pull? use cached?" prompt/stall).
//
// It matches the tag as a SUBSTRING anywhere in `sbx template ls` output, which
// is both format-independent (works for `repo tag id`, a combined `repo:tag id`,
// headers, warnings) and catches the fully-pruned case (no matching line at all
// -> not loaded -> refuse). The tag is a unique local-<unixts>, so a substring
// match can't collide with anything else. It fails OPEN (returns true) only when
// there's NO signal to judge from: no sbx, an ls error, or empty output.
func localImageLoaded(env shellEnv, tag string) bool {
	if tag == "" || (env.run == nil && env.probe == nil) {
		return true
	}
	if env.lookPath != nil {
		if _, err := env.lookPath("sbx"); err != nil {
			return true
		}
	}
	// BOUNDED (probeRun): a hung `sbx template ls` is a timeout, which is the
	// same "no signal" as an error — fail open, never wedge the launch.
	out, timedOut, err := probeRun(env, "sbx", "template", "ls")
	if timedOut || err != nil || strings.TrimSpace(out) == "" {
		return true // no signal -> don't block
	}
	return strings.Contains(out, tag)
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

// isRepoRoot reports whether dir looks like a pix repo checkout.
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
// <workspace>/.pix/knowledge (if any). All ids are canonicalized the SAME
// way the store keys its `bundle` column so `WHERE bundle IN (…)` matches.
//
//   - Lazy reindex: when the daemon is up and the project bundle is NOT already
//     in health.bundles, fire one reindex for it. Skipped entirely when the
//     daemon is down (serve not running) or the bundle is already indexed.
//   - Scope file: write <workspace>/.pix/knowledge.scope, one canonical id
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
		_ = removeWorkspaceStateFile(workspace, "knowledge.scope")
		return
	}
	_ = writeKnowledgeScope(workspace, ids)
}

// projectBundle reads <workspace>/.pix/knowledge and resolves it to a
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

// writeOllamaBridgeFile writes <workspace>/.pix/ollama-bridge.model: the
// local model tag the in-VM ollama-bridge should expose (interactive cycle + the
// router's local option). Configured on the host with `pix config set
// ollama_bridge_model`; the bridge reads it (env var still overrides). Per-run,
// gitignored, best-effort — an absent file just means the bridge uses its default.
// Symlink-safe via writeWorkspaceStateFile.
func writeOllamaBridgeFile(workspace, model string) {
	model = strings.TrimSpace(model)
	if model == "" {
		model = config.DefaultOllamaBridgeModel
	}
	_ = writeWorkspaceStateFile(workspace, "ollama-bridge.model", []byte(model+"\n"), 0o644)
}

// writeKnowledgeScope writes <workspace>/.pix/knowledge.scope: one canonical
// bundle id per line, trailing newline. This is the launcher-generated,
// per-run, gitignored file the recall extension reads (the committed pointer is
// .pix/knowledge). Symlink-safe via writeWorkspaceStateFile.
func writeKnowledgeScope(workspace string, ids []string) error {
	content := strings.Join(ids, "\n") + "\n"
	return writeWorkspaceStateFile(workspace, "knowledge.scope", []byte(content), 0o644)
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

// deriveSandboxName mirrors sbx's default `pix-<workspace-basename>` so the
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
	return "pix-" + base
}

// The tri-state sandbox probe (running/stopped/absent/unknown) that drives the
// create-vs-reattach-vs-replace decision lives in task.go as probeTaskSandbox +
// sbxState — run.go reuses it rather than duplicating the `sbx ls` parse.
