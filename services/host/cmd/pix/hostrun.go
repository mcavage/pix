package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"pix/host/config"
)

// runHost implements `pix host` — the UNSANDBOXED escape hatch that execs
// the host-installed `pi` directly (no sbx, no VM). It exists for exactly one
// narrow case: working on pix itself, which needs the host's Docker/sbx/
// make. It is gated OFF by default (`host.enabled`), loudly signposted, and its
// guardrails (host-guard extension, workspace refusals, disabled subagents) are
// protection against ACCIDENTS — not a security boundary. Full design + threat
// model: docs/design/host-mode.md.
//
// ── HOST-MODE CHILD ENV CONTRACT (the TS extensions match these EXACTLY) ──
//
//	PI_CODING_AGENT_DIR   = <state>/pix/host-agent  (pi's config dir)
//	MEMORY_URL            = http://127.0.0.1:11435       (memory-recall/capture;
//	                             port honors MEMORY_PORT, like serve.go)
//	KNOWLEDGE_URL         = http://127.0.0.1:11436       (knowledge-recall;
//	                             port honors KNOWLEDGE_PORT)
//	OLLAMA_BRIDGE_MODEL   = <config ollama_bridge_model> (when set — the local
//	                             model the bridge exposes; env half of run.go's
//	                             workspace file)
//	OLLAMA_HOSTMODE       = 1    ollama-bridge.ts MUST skip its reverse proxy
//	                             (which self-loops on the host) and register the
//	                             provider straight at OLLAMA_URL instead
//	OLLAMA_URL            = http://127.0.0.1:11434/v1    (the real local ollama)
//	PI_SUBAGENT_DISABLED  = 1    subagents.ts MUST refuse to spawn (children run
//	                             --no-extensions, so they'd escape the guard)
//	PI_SUBAGENT_MAX_DEPTH = 0    belt-and-suspenders: today's depth guard already
//	                             refuses at depth 0/0 with no TS change
//	PATH                  = <hostPackBinDir()>:<PATH>  (F3: the active pack's
//	                             ACCEPTED host wrappers — host mode ONLY; the
//	                             sandbox and the login shell never see this dir)
//
// All of it goes through the child's cmd.Env — never exported to the shell,
// never persisted, gone when pi exits.
func runHost(argv []string) {
	sub, o, err := parseHostArgs(argv)
	if err != nil {
		if err == errHelpRequested {
			fmt.Print(hostUsage)
			return
		}
		fmt.Fprintf(os.Stderr, "pix host: %v\n\n", err)
		fmt.Fprint(os.Stderr, hostUsage)
		os.Exit(2)
	}

	// The gate is checked on the BASE config (host.enabled is global, never
	// per-profile): leaving the sandbox is a machine-level decision.
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix host: %v\n", err)
		os.Exit(1)
	}
	switch sub {
	case "setup":
		// `host setup` PROVISIONS host mode AND enables it when provisioning
		// actually succeeds — one command, no separate `config set host.enabled
		// true` step. It must NOT be gated behind host.enabled itself (that would
		// be chicken-and-egg); the gate only guards LAUNCH. The provision-before-
		// enable invariant holds: runHostSetup is lenient (returns nil even when
		// `pi` is missing), so we verify with hostProvisioned() and never flip the
		// gate on with nothing behind it.
		if err := runHostSetup(os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "pix host setup: %v\n", err)
			os.Exit(1)
		}
		if !hostProvisioned() {
			fmt.Fprintln(os.Stderr, "pix host setup: not fully provisioned (usually a missing `pi`) — left DISABLED.")
			fmt.Fprintln(os.Stderr, "Install the pinned `pi` (see above), then re-run: pix host setup")
			os.Exit(1)
		}
		if !cfg.Host.Enabled {
			cfg.Host.Enabled = true
			if err := cfg.Save(); err != nil {
				fmt.Fprintf(os.Stderr, "pix host setup: provisioned but could not enable (%v); run: pix config set host.enabled true\n", err)
				os.Exit(1)
			}
		}
		fmt.Fprintln(os.Stderr, "pix host setup: host mode enabled (UNSANDBOXED). Launch: pix host")
	default:
		if !cfg.Host.Enabled {
			fmt.Fprint(os.Stderr, hostGateMessage())
			os.Exit(1)
		}
		runHostLaunch(o)
	}
}

// hostGateMessage is the default-off refusal. The friction is intentional: the
// user must understand what they are turning off before host mode will run.
func hostGateMessage() string {
	return `pix host is DISABLED (host.enabled = false, the default).

This command runs the agent directly on YOUR machine, and that deletes all
three of pix's safety properties at once:
  - no sandbox: commands, file writes, and deletes hit your real files
  - no network fence: the egress allowlist does not exist here
  - real credentials: any key in the session env is the real key

pi has no built-in permission prompts and no built-in sandbox. The host-mode
guardrails (guard extension, workspace checks, disabled subagents) reduce
accidents — they are NOT a security boundary. For anything you wouldn't hand a
shell to, use ` + "`pix run`" + ` (the sandbox).

If you understand the above and want it anyway, set it up (this provisions AND
enables it — the gate stays off unless provisioning actually succeeds):

  pix host setup

then launch with ` + "`pix host`" + `.
`
}

// hostAgentDir is PI_CODING_AGENT_DIR for host mode:
// $XDG_STATE_HOME/pix/host-agent (default ~/.local/state/pix/host-agent).
// State-flavored on purpose (rebuildable symlinks + installs, never precious),
// beside tasks/ — honoring XDG_STATE_HOME exactly like taskStateRoot.
func hostAgentDir() string {
	if x := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); x != "" {
		return filepath.Join(x, "pix", "host-agent")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".local", "state", "pix", "host-agent")
}

// hostHarnessDirs are the repo harness dirs symlinked into the host agent dir.
// Only dirs that EXIST in the checkout are linked (e.g. prompts/ was removed
// from the tree; a missing dir is skipped, never an error).
//
// lib/ is not a pi discovery root — it is here because extensions/ imports out
// of it (`../lib/recall-message.ts`). Today that resolves anyway, since the
// extensions symlink points back at the checkout and Node resolves relative
// imports from the realpath. Linking it explicitly means host mode does not
// quietly depend on that, and keeps it in step with the image, which COPYs both.
// scripts/check-recall-transport.sh (R4) enforces the pairing on both paths.
var hostHarnessDirs = []string{"skills", "agents", "extensions", "lib", "prompts", "themes"}

// hostPinnedPiPackage mirrors the Dockerfile's `ARG PI_PACKAGE` pin. The
// "install pi" hints tell the user to match the image's version, so they must
// name the ACTUAL pinned version, not an unversioned latest. When you bump the
// Dockerfile ARG, bump this in the same commit (a test cross-checks the two).
const hostPinnedPiPackage = "@earendil-works/pi-coding-agent@0.82.1"
const hostPiVersionProbeTimeout = 2 * time.Second

func checkHostPiVersion(piBin string) error {
	want := strings.TrimPrefix(hostPinnedPiPackage, "@earendil-works/pi-coding-agent@")
	ctx, cancel := context.WithTimeout(context.Background(), hostPiVersionProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, piBin, "--version")
	cmd.WaitDelay = 250 * time.Millisecond
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("`pi --version` timed out after %s", hostPiVersionProbeTimeout)
	}
	if err != nil {
		return fmt.Errorf("could not read `pi --version`: %w", err)
	}
	got := strings.TrimSpace(string(out))
	if got != want {
		return fmt.Errorf("found pi %q, need %q", got, want)
	}
	return nil
}

// hostPiPackages mirrors the Dockerfile's curated `pi install` loop (the PINNED
// set — see the Dockerfile comment on why these must be version-locked to the
// PI_PACKAGE release). When you re-pin the Dockerfile list, re-pin this one.
var hostPiPackages = []string{
	"pi-plan@0.1.1",
	"pi-mcp-adapter@2.13.0",
	"pi-manage-todo-list@0.4.0",
	"pi-simplify@0.2.3",
	"pi-web-access@0.13.0",
	"@juanibiapina/pi-extension-settings@0.9.1",
	"pi-usage@0.3.0",
}

// hostPiExtensionsLockFile records the EXACT hostPiPackages set successfully
// installed into a host agent dir, so a re-run of `pix setup` skips the
// (slow, silent-looking) reinstall unless the package list actually changed
// (a version bump busts it). Lives in the host agent dir itself — host-owned,
// so a plain read/write is fine, but the marker is never FOLLOWED if it's a
// symlink (hostPiExtensionsInstalled/writeHostPiExtensionsMarker both Lstat).
const hostPiExtensionsLockFile = ".pi-extensions.lock"

// hostPiExtensionsMarker is the marker's canonical content: the exact
// hostPiPackages set, one per line.
func hostPiExtensionsMarker() string {
	return strings.Join(hostPiPackages, "\n") + "\n"
}

// hostPiExtensionsInstalled reports whether dir's marker exists and matches
// the CURRENT hostPiPackages set exactly. A symlinked marker is never
// followed — treated as absent (untrusted), so it always falls through to a
// real (re)install rather than trusting a link it didn't write.
func hostPiExtensionsInstalled(dir string) bool {
	path := filepath.Join(dir, hostPiExtensionsLockFile)
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 {
		return false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return string(b) == hostPiExtensionsMarker()
}

// writeHostPiExtensionsMarker refreshes the marker AFTER every package in
// hostPiPackages installs successfully. Symlink-safe: a pre-existing symlink
// at the marker path is removed (never written through) before the real file
// is written.
func writeHostPiExtensionsMarker(dir string) error {
	path := filepath.Join(dir, hostPiExtensionsLockFile)
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if rerr := os.Remove(path); rerr != nil {
			return rerr
		}
	}
	return os.WriteFile(path, []byte(hostPiExtensionsMarker()), 0o644)
}

// installHostPiExtensions installs the curated pi extension packages into dir
// (PI_CODING_AGENT_DIR), mirroring the Dockerfile's curated `pi install` loop.
// Returns the packages that failed to install (empty on full success, or the
// whole set when `pi` isn't on PATH). Split out of runHostSetup so tests can
// drive it directly against a fake `pi` on PATH.
func installHostPiExtensions(errw io.Writer, dir string) []string {
	piBin, lookErr := exec.LookPath("pi")
	if lookErr != nil {
		fmt.Fprintln(errw, "pix host setup: `pi` not found on PATH — install the image's pinned version:")
		fmt.Fprintln(errw, "  npm install -g "+hostPinnedPiPackage)
		return hostPiPackages
	}
	if err := checkHostPiVersion(piBin); err != nil {
		fmt.Fprintf(errw, "pix host setup: incompatible `pi`: %v\n", err)
		fmt.Fprintln(errw, "  npm install -g "+hostPinnedPiPackage)
		return hostPiPackages
	}
	if hostPiExtensionsInstalled(dir) {
		fmt.Fprintf(errw, "pix host setup: pi extensions: already installed (%d), skipping\n", len(hostPiPackages))
		return nil
	}
	fmt.Fprintf(errw, "pix host setup: installing %d pi extensions...\n", len(hostPiPackages))
	var failed []string
	for i, p := range hostPiPackages {
		// A progress line BEFORE the install so a slow/noisy npm install is never a
		// silent multi-minute hang — the noisy output itself stays captured and
		// only surfaces on failure.
		fmt.Fprintf(errw, "  installing pi extension %s (%d/%d)...\n", p, i+1, len(hostPiPackages))
		cmd := exec.Command(piBin, "install", "npm:"+p)
		cmd.Env = append(os.Environ(), "PI_CODING_AGENT_DIR="+dir)
		// Capture the (very noisy) npm output; only surface it if the install
		// FAILS, so a clean setup stays quiet but real errors still show.
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		if err := cmd.Run(); err != nil {
			failed = append(failed, p)
			fmt.Fprintf(errw, "  ✗ %s: %v\n%s\n", p, err, strings.TrimRight(buf.String(), "\n"))
		}
	}
	if len(failed) == 0 {
		// Write/refresh the marker ONLY after every package installs
		// successfully — a partial failure must keep re-attempting next run.
		if merr := writeHostPiExtensionsMarker(dir); merr != nil {
			fmt.Fprintf(errw, "pix host setup: could not write extensions marker: %v\n", merr)
		}
	}
	return failed
}

// hostSettingsJSON is the host-specific pi settings file. It deliberately does
// NOT copy the sandbox settings: defaultProjectTrust is "ask" (in the sandbox
// it is "always" — safe only because the VM is the boundary; on the host,
// auto-trusting arbitrary project-local extensions would run unreviewed code).
// theme is the red-tinted "host" variant (themes/host.json, symlinked in like
// every other harness dir) — the persistent in-session skin that says
// unsandboxed at a glance, per docs/design/host-mode.md "UX".
const hostSettingsJSON = `{
  "defaultProjectTrust": "ask",
  "enableInstallTelemetry": false,
  "theme": "host",
  "hideThinkingBlock": true
}
`

// hostContextMD is the host-mode system preamble, appended to pi's system
// prompt at launch. It exists because the sandbox agentContext (pi-kit/
// spec.yaml) asserts a disposable, network-fenced, full-auto VM — isolation
// that does NOT exist here. Copying it onto the host would be actively
// dangerous, so host mode asserts the opposite, explicitly.
const hostContextMD = `# HOST MODE — you are NOT in a sandbox

You are running directly on the user's real machine. Everything the sandbox
context promised is absent here:

- This machine is NOT disposable. Files you change or delete are the user's
  real files; there is no VM to throw away and no snapshot to restore.
- There is NO network fence. Any host you can reach, you are really reaching.
- Credentials in this session are REAL. Treat every key and token as live.
- There are no permission prompts. Autonomy is the user's responsibility, and
  yours: prefer reversible actions, confirm before anything destructive or
  hard to undo (rm -rf, git push --force, sudo, global installs, writes
  outside the working directory), and stay inside the workspace.
- A host-guard extension watches tool calls, but it is a guardrail against
  accidents, NOT a boundary. Do not rely on it to stop you.
- Subagent spawning is disabled in host mode.

You are here for one narrow purpose: host-only work (typically developing
pix itself, which needs the host's Docker/sbx/make). Do that work, stay
in the tree, and nothing else.
`

// runHostSetup provisions the host agent dir: harness symlinks into the repo
// checkout, a host-specific settings.json, a sessions/ dir, the host context
// preamble, and the curated pi extension packages (best-effort: when `pi` is
// missing or an install fails, it prints the exact commands to run instead of
// silently producing a broken dir). Idempotent.
func runHostSetup(errw *os.File) error {
	root, err := resolveRepoRoot()
	if err != nil {
		return fmt.Errorf("host setup needs a pix checkout to symlink the harness from: %w", err)
	}
	dir := hostAgentDir()
	if err := provisionHostAgentDir(root, dir, errw); err != nil {
		return err
	}

	// F3: lay down the ACTIVE pack's accepted host-mode wrappers (idempotent;
	// missing or Tier-0 pack is a no-op). Best-effort by contract: a failed
	// install prints a TODO — exactly like the pi-extension loop below — and
	// never fails setup. The strict re-hash gate runs at every host LAUNCH.
	if cfg, cerr := config.Load(); cerr == nil {
		if _, werr := refreshHostPackWrappers(errw, cfg, false); werr != nil {
			fmt.Fprintf(errw, "pix host setup: TODO — pack host wrappers not installed: %v\n", werr)
		}
	} else {
		fmt.Fprintf(errw, "pix host setup: TODO — could not load config to install pack host wrappers: %v\n", cerr)
	}

	// Curated pi extension packages, mirroring the Dockerfile install loop. `pi
	// install` writes into the config dir's npm/node_modules, so point it at the
	// host agent dir via PI_CODING_AGENT_DIR. IDEMPOTENT (installHostPiExtensions):
	// skips the whole install when the marker matches the current hostPiPackages
	// set, so a re-run of `pix setup` isn't a silent multi-minute reinstall.
	failed := installHostPiExtensions(errw, dir)
	if len(failed) > 0 {
		fmt.Fprintln(errw, "pix host setup: TODO — these pi extension packages did not install;")
		fmt.Fprintln(errw, "run the following once `pi` works (they land in "+dir+"):")
		for _, p := range failed {
			fmt.Fprintf(errw, "  PI_CODING_AGENT_DIR=%s pi install npm:%s\n", dir, p)
		}
	}

	fmt.Fprintf(errw, "pix host setup: provisioned %s (harness -> %s)\n", dir, root)
	// Host mode reaches cloud models only through hostmode.env refs. A user may
	// deliberately trust existing sbx keys during setup, leaving host mode local.
	// Report the actual state without treating that choice as a setup failure.
	env := defaultShellEnv()
	// "configured", not "wired": this only checks that hostmode.env carries a
	// syntactically filled op:// ref per provider name — it does NOT run `op
	// read` here, so it proves nothing about whether the ref actually resolves.
	// Real validation happens at every host LAUNCH via `op run --env-file`
	// (runHostLaunch); this line must never overclaim that as already done.
	// Tri-state: an unreadable hostmode.env is neither "local-only" nor
	// "configured" — both would be a confident guess about state we couldn't
	// actually read. Host mode itself is already provisioned above regardless.
	keys, kerr := hostModeProviderKeys(env)
	switch {
	case kerr != nil:
		fmt.Fprintf(errw, "Cloud keys: credential state unreadable: %v\n", kerr)
	case len(keys) > 0:
		fmt.Fprintf(errw, "Cloud refs configured (%s); resolved just-in-time at each host launch via `op run` (not verified here).\n", strings.Join(keys, ", "))
	default:
		fmt.Fprintln(errw, "Cloud keys: not wired; host mode is local/Ollama-only.")
		fmt.Fprintln(errw, "  To add cloud providers later, re-run `pix setup` and choose the 1Password path.")
	}
	return nil
}

// hostHarnessFiles are repo-root harness FILES symlinked into the host agent
// dir alongside the dirs. capabilities.json: capability-routing (and every data
// skill through it) reads $PI_CODING_AGENT_DIR/capabilities.json — without it
// capability resolution breaks in every host session. routing.json:
// subagents.ts resolves intent→model from it. keybindings.json: same bindings
// as the sandbox. mcp.json is deliberately NOT linked — it registers the sbx
// Cloud MCP Gateway, which only exists inside a sandbox; on the host it would
// just fail to connect every session.
var hostHarnessFiles = []string{"capabilities.json", "routing.json", "keybindings.json"}

// provisionHostAgentDir does the filesystem half of `host setup` (symlinks,
// settings.json, host-context.md, sessions/). Split from runHostSetup so tests
// can drive it against a temp root/dir without exec'ing `pi install`.
func provisionHostAgentDir(root, dir string, errw *os.File) error {
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o755); err != nil {
		return err
	}

	// Symlink the harness dirs + files that exist in the checkout; skip missing
	// ones (e.g. prompts/ removed from the tree — skip, don't error).
	link := func(name string, wantDir bool) error {
		src := filepath.Join(root, name)
		fi, err := os.Stat(src)
		if err != nil || fi.IsDir() != wantDir {
			return nil // missing (or the wrong kind) — skip
		}
		dst := filepath.Join(dir, name)
		if fi, err := os.Lstat(dst); err == nil {
			if fi.Mode()&os.ModeSymlink == 0 {
				fmt.Fprintf(errw, "pix host setup: %s exists and is not a symlink — leaving it alone\n", dst)
				return nil
			}
			_ = os.Remove(dst) // relink (the checkout may have moved)
		}
		if err := os.Symlink(src, dst); err != nil {
			return fmt.Errorf("symlink %s -> %s: %w", dst, src, err)
		}
		return nil
	}
	for _, d := range hostHarnessDirs {
		if err := link(d, true); err != nil {
			return err
		}
	}
	for _, f := range hostHarnessFiles {
		if err := link(f, false); err != nil {
			return err
		}
	}

	// Host-specific settings.json: written once, never clobbered (the user may
	// have tuned it).
	settings := filepath.Join(dir, "settings.json")
	if _, err := os.Stat(settings); os.IsNotExist(err) {
		if err := os.WriteFile(settings, []byte(hostSettingsJSON), 0o644); err != nil {
			return err
		}
	}

	// The host context preamble (also refreshed at every launch).
	return os.WriteFile(filepath.Join(dir, "host-context.md"), []byte(hostContextMD), 0o644)
}

// hostSecretDirs are $HOME-relative directories that hold SSH/cloud/password-
// store secrets. A host-mode workspace may neither BE inside one of these nor
// directly CONTAIN one (launching where the agent can read live keys is the
// accident this refuses).
var hostSecretDirs = []string{
	".ssh", ".gnupg", ".aws", ".azure", ".kube", ".password-store",
	filepath.Join(".config", "gcloud"), filepath.Join(".config", "op"),
	filepath.Join(".config", "gh"),
}

// resolveHostWorkspace canonicalizes ws (Abs + EvalSymlinks — a
// /tmp/link-to-home symlink must NOT defeat the check) and validates it against
// the host-mode refusal list. It returns the resolved real path.
func resolveHostWorkspace(ws string) (string, error) {
	abs, err := filepath.Abs(ws)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("cannot resolve workspace %q: %w", ws, err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	if err := validateHostWorkspace(real, home); err != nil {
		return "", err
	}
	return real, nil
}

// validateHostWorkspace is the stronger host-mode sibling of
// validateRunWorkspace: it takes an already-CANONICALIZED real path (the caller
// EvalSymlinks'd it) and refuses $HOME, /, /etc (and anything under /etc), any
// workspace inside a secret dir, and any workspace that directly contains one.
// Pure given (real, home) so tests can drive it hermetically.
func validateHostWorkspace(real, home string) error {
	if real == "/" {
		return fmt.Errorf("refusing to run host mode with the workspace at / — pick a project directory")
	}
	etc := "/etc"
	if real == etc || strings.HasPrefix(real, etc+string(filepath.Separator)) {
		return fmt.Errorf("refusing to run host mode in %s (system configuration)", real)
	}
	if home != "" {
		// Canonicalize home too, so a symlinked $HOME still matches.
		if h, err := filepath.EvalSymlinks(home); err == nil {
			home = h
		}
		if real == home {
			return fmt.Errorf("refusing to run host mode with the workspace at your home directory — pick a project directory")
		}
		for _, s := range hostSecretDirs {
			sd := filepath.Join(home, s)
			// Canonicalize the secret dir too: `real` is already
			// EvalSymlinks-resolved, so a symlinked ~/.ssh (sd lexical, real
			// canonical) would never prefix-match without this.
			if r, err := filepath.EvalSymlinks(sd); err == nil {
				sd = r
			}
			// (a) the workspace is AT or INSIDE a secret dir.
			if real == sd || strings.HasPrefix(real, sd+string(filepath.Separator)) {
				return fmt.Errorf("refusing to run host mode inside %s (holds secrets)", sd)
			}
			// (b) the workspace CONTAINS a home secret dir. This is what catches
			// nested entries like .config/gcloud when the workspace is $HOME/.config
			// — joining the full relative entry onto the workspace (the old check)
			// built $HOME/.config/.config/gcloud and wrongly accepted it.
			if strings.HasPrefix(sd, real+string(filepath.Separator)) {
				if fi, err := os.Stat(sd); err == nil && fi.IsDir() {
					return fmt.Errorf("refusing to run host mode in %s: it contains %s (holds secrets)", real, sd)
				}
			}
		}
	}
	// A workspace anywhere (not just under $HOME) that carries its own secret
	// dir, e.g. proj/.aws with live keys.
	for _, s := range hostSecretDirs {
		if fi, err := os.Stat(filepath.Join(real, s)); err == nil && fi.IsDir() {
			return fmt.Errorf("refusing to run host mode in %s: it contains %s (holds secrets)", real, s)
		}
	}
	return nil
}

// hostChildEnv returns the host-mode env contract (see the runHost comment),
// appended to os.Environ() for the child only — never exported, never persisted.
// MEMORY_URL/KNOWLEDGE_URL honor the same MEMORY_PORT/KNOWLEDGE_PORT overrides
// the services themselves honor (serve.go), so a non-default `pix serve`
// and the host session agree on where the daemons live. ollamaModel (config
// ollama_bridge_model) rides along as OLLAMA_BRIDGE_MODEL — the env-var half of
// what run.go does with the workspace file, and the bridge's strongest override.
func hostChildEnv(agentDir, ollamaModel string) []string {
	env := []string{
		"PI_CODING_AGENT_DIR=" + agentDir,
		fmt.Sprintf("MEMORY_URL=http://127.0.0.1:%d", portFromEnv("MEMORY_PORT", memoryPortDefault)),
		fmt.Sprintf("KNOWLEDGE_URL=http://127.0.0.1:%d", portFromEnv("KNOWLEDGE_PORT", knowledgePortDefault)),
		"OLLAMA_HOSTMODE=1",
		"OLLAMA_URL=http://127.0.0.1:11434/v1",
		"PI_SUBAGENT_DISABLED=1",
		"PI_SUBAGENT_MAX_DEPTH=0",
		// F3: pack host-mode wrappers, prepended so an accepted wrapper shadows
		// a same-named host tool by design (the BoM screen names every wrapper
		// before adoption). exec.Cmd de-duplicates env keys keeping the LAST
		// entry, so this wins over os.Environ()'s PATH for the child only —
		// never exported, never persisted. A not-yet-existing dir is harmless.
		"PATH=" + hostPackBinDir() + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
	if m := strings.TrimSpace(ollamaModel); m != "" {
		env = append(env, "OLLAMA_BRIDGE_MODEL="+m)
	}
	return env
}

// buildHostArgs composes pi's argv (after the binary name). Pure + testable.
// The guard extension and the host preamble are Phase-1 security blockers, so
// they are always present — the caller refuses to launch when either is missing.
func buildHostArgs(agentDir, preamble string, o hostOpts) []string {
	args := []string{
		"--session-dir", filepath.Join(agentDir, "sessions"),
		"-e", filepath.Join(agentDir, "extensions", "host-guard.ts"),
		"--append-system-prompt", preamble,
	}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	args = append(args, o.Passthrough...)
	return args
}

// hostBanner is the per-launch stderr warning (red when stderr is a TTY). A
// banner scrolls away — the persistent in-session HOST badge is the TS side's
// job (status.ts) — but leaving the boundary must be loud at the threshold too.
func hostBanner(tty bool) string {
	b := "⚠ HOST MODE — commands run on YOUR machine. No sandbox, no network fence, real credentials. Ctrl-C to abort."
	if tty {
		return "\x1b[1;31m" + b + "\x1b[0m\n"
	}
	return b + "\n"
}

// hostSelfDevFooter is printed when the workspace is a pix checkout (the
// primary host-mode case): it surfaces the Mode-A/B rule so the edit loop is
// legible without remembering AGENTS.md.
const hostSelfDevFooter = `pix checkout detected: skill/extension/agent edits load live on /reload;
Dockerfile / pi-kit / baked-file edits need ` + "`make load`" + ` + a fresh sandbox.
`

// runHostLaunch validates, wires the shared launcher machinery (profile,
// knowledge scope, memory scope), and execs the host-installed pi — via
// `op run --env-file` when the host refs file exists (keys resolved
// just-in-time, never persisted), else keyless (Ollama-only).
func runHostLaunch(o hostOpts) {
	ws, err := resolveHostWorkspace(o.Workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix host: %v\n", err)
		os.Exit(1)
	}

	agentDir := hostAgentDir()
	if _, err := os.Stat(filepath.Join(agentDir, "settings.json")); err != nil {
		fmt.Fprintf(os.Stderr, "pix host: %s is not provisioned — run `pix host setup` first\n", agentDir)
		os.Exit(1)
	}
	// The guard extension is a Phase-1 security blocker: refuse to launch
	// without it rather than degrade to an unguarded session. (It ships as
	// extensions/host-guard.ts in the checkout; the setup symlink surfaces it.)
	guard := filepath.Join(agentDir, "extensions", "host-guard.ts")
	if _, err := os.Stat(guard); err != nil {
		fmt.Fprintf(os.Stderr, "pix host: guard extension missing (%s).\n", guard)
		fmt.Fprintln(os.Stderr, "Host mode never launches unguarded. Update your pix checkout (it ships")
		fmt.Fprintln(os.Stderr, "extensions/host-guard.ts) and re-run `pix host setup`.")
		os.Exit(1)
	}
	piBin, err := exec.LookPath("pi")
	if err != nil {
		fmt.Fprintln(os.Stderr, "pix host: `pi` not found on PATH. Install the same version the image pins:")
		fmt.Fprintln(os.Stderr, "  npm install -g "+hostPinnedPiPackage)
		os.Exit(1)
	}
	if err := checkHostPiVersion(piBin); err != nil {
		fmt.Fprintf(os.Stderr, "pix host: incompatible `pi`: %v\n", err)
		fmt.Fprintln(os.Stderr, "  npm install -g "+hostPinnedPiPackage)
		os.Exit(1)
	}
	if !hostPiExtensionsInstalled(agentDir) {
		fmt.Fprintln(os.Stderr, "pix host: curated pi extensions are missing or stale.")
		fmt.Fprintln(os.Stderr, "  pix host setup")
		os.Exit(1)
	}

	// Refresh the host preamble (the checkout may have shipped new wording) and
	// read it back for --append-system-prompt. A launch without the host context
	// would run under NO context asserting the missing isolation — refuse.
	preamblePath := filepath.Join(agentDir, "host-context.md")
	if err := os.WriteFile(preamblePath, []byte(hostContextMD), 0o644); err != nil {
		// A failed refresh would launch under a STALE (or absent) host context —
		// the one preamble asserting the missing isolation. Refuse, don't degrade.
		fmt.Fprintf(os.Stderr, "pix host: cannot write host preamble %s: %v\n", preamblePath, err)
		os.Exit(1)
	}
	preamble, err := os.ReadFile(preamblePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix host: cannot read host preamble %s: %v\n", preamblePath, err)
		os.Exit(1)
	}

	// Shared launcher machinery: profile resolution (skill/memory/knowledge
	// scoping — the sandbox-name half is meaningless here), knowledge scope, and
	// the per-run profile file. All best-effort, exactly like run.go.
	cfg, _, err := loadResolvedConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix host: %v\n", err)
		os.Exit(1)
	}
	wireKnowledgeScope(cfg, ws, defaultKnowledgeRPC())

	// F3: refresh the active pack's host wrappers so a `pack use` since the
	// last `host setup` takes effect, re-hashing every ACCEPTED [[bin]] against
	// its pinned sha — a tampered external binary REFUSES the launch (fail
	// closed; packs.md §9 safeguard 2). The wrappers land in hostPackBinDir(),
	// which hostChildEnv prepends to the child PATH — this launch path is the
	// ONLY thing that ever puts them on a PATH.
	activePack, perr := refreshHostPackWrappers(os.Stderr, cfg, true)
	if perr != nil {
		fmt.Fprintf(os.Stderr, "pix host: %v\n", perr)
		os.Exit(1)
	}
	// F4: tag the host session's memory scope from the active pack — the same
	// .pix/profile file `pix run` writes (memory-recall/capture read
	// it). Best-effort by contract (writeMemoryScope discards errors).
	writeMemoryScope(ws, activePack)

	// Credentials: op:// refs resolved just-in-time by `op run`, or Ollama-only.
	// hostModelPreflight replaces modelProviderPreflight (which reads sbx
	// secrets that don't apply here) and NEVER blocks a keyless launch.
	refs := config.HostRefsPath()
	useOp := false
	if _, err := os.Stat(refs); err == nil {
		if _, err := exec.LookPath("op"); err == nil {
			useOp = true
		} else {
			fmt.Fprintf(os.Stderr, "pix host: %s exists but `op` (1Password CLI) is not on PATH — launching without cloud keys (Ollama-only)\n", refs)
		}
	} else {
		fmt.Fprintf(os.Stderr, "pix host: no cloud keys (Ollama-only). To add them, put op:// refs in\n  %s\n(e.g. ANTHROPIC_API_KEY=op://vault/item/field) — resolved at launch, never stored.\n", refs)
	}

	piArgs := buildHostArgs(agentDir, string(preamble), o)
	var cmd *exec.Cmd
	if useOp {
		// --no-masking is REQUIRED for an interactive launch: op's default output
		// masking pipes the child's stdout/stderr through a secret-scanning filter,
		// which makes them non-TTYs — pi's TUI then sees no terminal and exits
		// immediately (banner, then straight back to the shell, exit 0). Masking is
		// also pointless here: pi is a full-screen TUI, not a secret-echoing script,
		// and the mcp-gateway op-run path (Makefile mcp-register) already runs
		// --no-masking for the same reason. The real key still never enters this
		// process — op injects it into pi's env only.
		opArgs := append([]string{"run", "--no-masking", "--env-file=" + refs, "--", piBin}, piArgs...)
		cmd = exec.Command("op", opArgs...)
	} else {
		cmd = exec.Command(piBin, piArgs...)
	}
	cmd.Dir = ws
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), hostChildEnv(agentDir, cfg.OllamaBridgeModel)...)

	fmt.Fprint(os.Stderr, hostBanner(isTTY(os.Stderr)))
	if isRepoRoot(ws) {
		fmt.Fprint(os.Stderr, hostSelfDevFooter)
	}
	if os.Getenv("PIX_DEBUG") != "" {
		fmt.Fprintln(os.Stderr, "+ "+cmd.Path+" "+strings.Join(cmd.Args[1:], " "))
	}

	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code := exit.ExitCode()
			// A clean interactive pi quit returns exit 0 (err == nil, not here). A
			// NON-zero exit is usually the `op run` wrapper failing BEFORE pi ever
			// starts — a bad op:// ref, 1Password locked/not signed in, or a vault/item
			// mismatch. Never exit silently: say exactly what to check (this is the
			// "banner then straight back to the shell, no error" failure).
			if useOp {
				fmt.Fprintf(os.Stderr, "\npix host: launch failed (exit %d). If pi never opened, `op run` could not resolve your cloud keys from\n  %s\n", code, refs)
				// `-- true` surfaces op's OWN resolution error (op fails before it runs the
				// command) without printing the resolved key values into the terminal /
				// scrollback / CI logs, which `printenv <KEY>` would have done.
				fmt.Fprintf(os.Stderr, "Reproduce op's own error (without printing your keys):  op run --env-file=%s -- true\n", refs)
				fmt.Fprintln(os.Stderr, "Common causes: 1Password locked / not signed in; or a bad op:// ref — a field name with a space must be a literal space, not URL-encoded (op://Vault/Item/api key). Delete the file to run Ollama-only.")
			} else {
				fmt.Fprintf(os.Stderr, "\npix host: pi exited with code %d.\n", code)
			}
			os.Exit(code)
		}
		fmt.Fprintf(os.Stderr, "pix host: exec: %v\n", err)
		os.Exit(1)
	}
}
