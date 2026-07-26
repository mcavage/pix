package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"pix/host/config"
)

// kitRepo is the canonical git-hosted kit source. The launcher pins it to the
// stamped version so a consumer's `pix run` resolves the exact image/kit
// that was published for this launcher build.
const kitRepo = "git+https://github.com/mcavage/pix.git"

// dockerImageRepo is the published image repo. A local build pins a locally
// loaded tag from <repo>/out/.local-image-tag via --template, mirroring the
// `make run` target.
const dockerImageRepo = "docker.io/mcavage/pix"

// releasedVersionRE matches a CLEAN released semver like "0.0.16" — the shape a
// CI release stamps, for which a matching git tag "v0.0.16" is expected to
// exist. Anything else (an unstamped "dev" build, a "0.0.16+local" local
// build, or non-semver) is treated as UNRELEASED, so the launcher never pins a
// nonexistent v<version> tag.
var releasedVersionRE = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// isReleased reports whether version is a clean released semver whose git tag
// is expected to exist.
func isReleased(version string) bool {
	return releasedVersionRE.MatchString(version)
}

// runOpts carries everything the arg builder needs, resolved by the caller. It
// is deliberately side-effect-free (no filesystem probing, no token minting) so
// buildSbxArgs stays a pure function that the tests can drive without sbx or a
// real config on disk.
type runOpts struct {
	Workspace     string   // positional DIR (default ".")
	Dev           bool     // --dev: Mode B, skills load live from a repo checkout
	DevRoot       string   // resolved repo root when Dev is set (caller resolves)
	LocalKit      string   // resolved local checkout kit dir (<repo>/pi-kit); set for --dev and for an unreleased build with a resolvable checkout. Replaces the git pin.
	LocalImageTag string   // contents of <repo>/out/.local-image-tag; pins --template to the locally loaded image when set (caller reads it)
	Template      string   // --template REF: explicit image override (e.g. the full ref `make load` prints). Works from ANY directory — no checkout needed — and takes precedence over the auto LocalImageTag pin.
	Skills        []string // --skills DIR: extra live skill trees
	Kits          []string // --kit K: escape-hatch kit(s). When present they REPLACE the auto git/local pin (a user override), then config stack applies.
	// KitRef overrides the git ref the auto-pinned kit resolves (e.g. "v0.1.2",
	// "main"). Resolved by the caller via resolveKitRef — see kitref.go for the
	// precedence chain. Empty means "use this build's stamped version", the
	// original lockstep behaviour. Distinct from Kits: this steers the AUTO pin
	// rather than replacing it, so --dev / local-checkout selection still wins.
	KitRef    string
	MCP       []string // --mcp M: extra MCP servers on top of config.MCP (folded into StaticMCP by the caller)
	StaticMCP []string // RESOLVED set to attach at create (emitted as --static-mcp); the caller computes it from cfg.MCP+MCP via allPreloadedMCP — S01: every configured/pack server preloads, no eager/lazy split
	Name      string   // --name N: sandbox name
	Model     string   // --model M: active pi model (passed through to pi)
	Intent    string   // --intent NAME: resolve the session model via the router (unless --model overrides)
	Replace   bool     // --replace: force a recreate (rm -f then create) instead of re-attaching to an existing sandbox
	Pack      string   // --pack PATH: active pack for this run (overrides config.Pack); mounts its skills + knowledge
	// PackKits are ephemeral mixin kit dir(s) synthesized from the active pack's
	// bin/ wrappers (F2, see synthesizePackKit). Deliberately SEPARATE from Kits:
	// Kits non-empty is the --kit ESCAPE HATCH that replaces the auto git/local
	// pin (see kitOverride in buildSbxArgs); a pack-synthesized kit must stack
	// alongside the base kit, never suppress it, so it gets its own field and its
	// own unconditional --kit loop.
	PackKits    []string
	Passthrough []string // args after `--`, handed straight to pi
	// Token is the credential bearer for an OPTIONAL external credential broker
	// plugin. The default path leaves it empty and forwards no bearer (gog
	// authenticates host-side in the gateway-spawned MCP server). Reserved for
	// the dormant generic broker seam; never emitted on argv (it would go via
	// the sbx child env, like run.go).
	Token string
}

// kitRef returns the git ref fragment the launcher pins. A clean released
// version (e.g. "0.0.16") pins the tag "v0.0.16"; any UNRELEASED version
// (a "dev"/"+local" or non-semver build, whose tag does not exist) tracks
// "main" instead of pinning a bogus v<version>.
func kitRef(version string) string {
	if isReleased(version) {
		return "v" + version
	}
	return "main"
}

// gitKitURL is the full --kit URL for a repo-less consumer run, pinned to the
// stamped version (or main for an unreleased build).
func gitKitURL(version string) string {
	return gitKitURLRef(kitRef(version))
}

// gitKitURLRef is gitKitURL for an ALREADY-RESOLVED ref (see kitref.go), which
// may be a release tag this binary was not stamped with.
func gitKitURLRef(ref string) string {
	return kitRepo + "#ref=" + ref + "&dir=pi-kit"
}

// refOrStamped falls back to this build's own ref when nothing overrode it, so
// every failure path in the resolver lands on the original lockstep behaviour.
func refOrStamped(ref, version string) string {
	if ref != "" {
		return ref
	}
	return kitRef(version)
}

// localImageRef is the --template ref for a locally loaded image tag.
func localImageRef(tag string) string {
	return dockerImageRepo + ":" + tag
}

// templateTag returns the tag portion of a --template image ref (everything after
// the final ':'), or "" if the ref carries no tag. It splits on the LAST colon so
// a registry port (e.g. localhost:5000/foo:tag) doesn't confuse it.
func templateTag(ref string) string {
	i := strings.LastIndexByte(ref, ':')
	if i < 0 || strings.IndexByte(ref[i:], '/') >= 0 {
		return ""
	}
	return ref[i+1:]
}

// mcpCatalogBundleName is the bundle name `pix mcp bundle` registers the
// public catalog under, so `sbx mcp bundle rm pix-catalog` removes the set.
const mcpCatalogBundleName = "pix-catalog"

// mcpCatalogBundleURL is the raw-GitHub URL of the shipped public MCP catalog
// bundle (notion/atlassian/granola), pinned to THIS build's ref exactly like the
// kit (a released build → v<version>, an unreleased build → main), so a consumer
// registers the remote set that matches their launcher with one command.
func mcpCatalogBundleURL(version string) string {
	return "https://raw.githubusercontent.com/mcavage/pix/" + kitRef(version) + "/config/mcp-catalog.bundle.json"
}

// buildSbxArgs composes the full argv for `sbx <args...>` (i.e. it returns
// everything AFTER the "sbx" program name, starting with "run"). It is pure: no
// exec, no filesystem, no token minting — the caller does all of that and feeds
// the results in via cfg + o. This is the function the tests exercise.
func buildSbxArgs(cfg *config.Config, o runOpts, version string) []string {
	args := []string{"run", "pix"}

	if o.Name != "" {
		args = append(args, "--name", o.Name)
	}

	// --kit is an escape hatch: when present it REPLACES the auto git/local pin
	// (so a user can work around an unresolvable release tag). Without it, use
	// the resolved local checkout kit if we have one, else pin the published git
	// kit for this build's version.
	kitOverride := len(o.Kits) > 0

	// Image pin. An explicit --template REF wins over everything: it works from any
	// directory (no checkout needed) and is orthogonal to kit selection, so it is
	// NOT gated on kitOverride. Otherwise, mirror `make run`: pin the locally loaded
	// image when we resolved a local checkout that carries out/.local-image-tag
	// (skipped when --kit overrides the whole spec).
	if o.Template != "" {
		args = append(args, "--template", o.Template)
	} else if !kitOverride && o.LocalKit != "" && o.LocalImageTag != "" {
		args = append(args, "--template", localImageRef(o.LocalImageTag))
	}

	if !kitOverride {
		if o.LocalKit != "" {
			args = append(args, "--kit", o.LocalKit)
		} else {
			args = append(args, "--kit", gitKitURLRef(refOrStamped(o.KitRef, version)))
		}
	}
	// User --kit flags are the base when present (escape hatch).
	for _, k := range o.Kits {
		args = append(args, "--kit", k)
	}
	// Pack-synthesized kit(s) (F2 sandbox bin/ wrappers) ALWAYS stack, regardless
	// of the --kit escape hatch: they are never the base image kit, only an
	// additive mixin, so they must not be folded into kitOverride's replace
	// semantics (see the PackKits field doc).
	for _, k := range o.PackKits {
		args = append(args, "--kit", k)
	}
	// Config-stacked kits always apply on top of the base.
	for _, k := range cfg.Kits.Stack {
		args = append(args, "--kit", k)
	}

	// MCP servers: emit --static-mcp for every preloaded server (o.StaticMCP,
	// computed by the caller via allPreloadedMCP — S01: all configured/pack
	// servers preload, no eager/lazy split). sbx's flag is --static-mcp (the
	// fixed set chosen at CREATE; can't change on re-attach). The local
	// data-plane gateway serves them with no SBX_MCP_URL. Attach one to an
	// ALREADY-RUNNING sandbox live (no recreate) with `pix mcp load`.
	for _, m := range o.StaticMCP {
		args = append(args, "--static-mcp", m)
	}

	// The default path forwards NO credential bearer: gog authenticates on the
	// host inside the gateway-spawned MCP server, so nothing needs injecting. If a
	// future external credential broker plugin sets o.Token, it goes via the sbx CHILD PROCESS ENV
	// (never argv, which `ps`/EDR can read) — keep this arg builder token-free.

	// Workspace (first non-flag positional).
	args = append(args, o.Workspace)

	// Live skill trees: config paths + --skills flags. Each is mounted as an
	// extra workspace so pi can read it inside the sandbox.
	liveSkills := append(append([]string(nil), cfg.Skills.Paths...), o.Skills...)
	args = append(args, liveSkills...)
	// Dev mode also mounts the repo's own skills tree.
	if o.Dev {
		args = append(args, filepath.Join(o.DevRoot, "skills"))
	}

	// Compose the pi passthrough args (after `--`).
	var piArgs []string
	if o.Dev {
		// Mode B: turn off baked skills and load the repo tree live.
		piArgs = append(piArgs, "--no-skills", "--skill", filepath.Join(o.DevRoot, "skills"))
	}
	for _, s := range liveSkills {
		piArgs = append(piArgs, "--skill", s)
	}
	if o.Model != "" {
		piArgs = append(piArgs, "--model", o.Model)
	}
	piArgs = append(piArgs, o.Passthrough...)

	if len(piArgs) > 0 {
		args = append(args, "--")
		args = append(args, piArgs...)
	}
	return args
}

// runLaunchPlan is the OUTCOME of the create-vs-reattach-vs-replace decision:
// the sbx argv to exec, whether it must be preceded by `sbx rm -f <name>` first,
// and whether Args is the thin re-attach form (vs. a full create) — the caller
// uses Reattach to pick its stderr message and to print the re-attach-failed
// hint if the exec fails.
type runLaunchPlan struct {
	RmFirst  bool     // run `sbx rm -f <name>` before Args
	Args     []string // sbx argv (after "sbx")
	Reattach bool     // true => Args is the thin re-attach form, not a full create
	// Err is set ONLY for the fail-closed --replace-on-unknown-state case (see
	// planSandboxLaunch below). A non-nil Err means Args/RmFirst/Reattach are
	// meaningless zero values — the caller MUST check Err first and abort
	// before printing anything that claims a replace/create/reattach is
	// happening, and before running RmFirst or exec'ing sbx at all.
	Err error
}

// planSandboxLaunch is the pure decision at the heart of `pix run`'s
// lifecycle, matching sbx's own re-attach model: a create-only flag
// (--kit/--template/--mcp) only makes sense when the sandbox does not already
// exist. It mirrors buildSbxArgs — no exec, no filesystem — so every branch is
// unit-testable without sbx installed.
//
//   - absent (or unknown, when `sbx ls` itself failed) -> CREATE: the full
//     buildSbxArgs, unchanged.
//   - running or stopped, no --replace -> RE-ATTACH: sbx reads the agent from
//     the existing sandbox's spec, so none of the create-only flags apply; see
//     buildReattachArgs.
//   - any state, --replace -> REPLACE: `sbx rm -f <name>` (skipped when the
//     sandbox is already absent) then a full create, so changed kit/mcp/
//     create-only flags take effect. This is today's implicit recreate, now
//     explicit and available for a RUNNING sandbox too.
//   - --replace requested but the sandbox's existence could not be determined
//     (state == sbxUnknown, i.e. the run lifecycle's OWN probe of `sbx ls`
//     failed or sbx is unavailable): FAIL CLOSED, not create. "Replace" means
//     "remove whatever is there, then create" — with no reliable read on
//     whether there IS anything there, an unconditional create can collide
//     with a sandbox that in fact exists (sbx may itself reattach it with
//     stale kit/mcp/create-only flags, exactly what --replace exists to
//     avoid), while RmFirst stays false regardless (planSandboxLaunch never
//     rm's on an unknown probe) so the two paths would silently disagree about
//     what "replacing" even did. Refusing before doing anything mirrors
//     setup.go's own sbxUnknown fail-closed posture (runSetupHandoff) at this
//     lifecycle's independent probe site, rather than assuming setup's guard
//     covers every path that can reach a replace. A plain (non-replace) launch
//     on sbxUnknown is unaffected — it still optimistically creates via
//     willCreate, same as always.
func planSandboxLaunch(state sbxState, replace bool, cfg *config.Config, o runOpts, version string) runLaunchPlan {
	if replace && state == sbxUnknown {
		return runLaunchPlan{Err: fmt.Errorf("--replace requested but could not determine whether sandbox %q exists (`sbx ls` failed or sbx is unavailable); refusing to replace blind — fix sbx and retry (or run without --replace to attempt a plain launch)", o.Name)}
	}
	if !willCreate(state, replace) {
		return runLaunchPlan{Args: buildReattachArgs(o), Reattach: true}
	}
	return runLaunchPlan{
		RmFirst: replace && (state == sbxRunning || state == sbxStopped),
		Args:    buildSbxArgs(cfg, o, version),
	}
}

// willCreate reports whether planSandboxLaunch(state, replace, ...) will
// create (or replace) rather than plainly re-attach — i.e. whether the
// create-only inputs (repo checkout / --dev / kit resolution) are even needed.
// Kept as the single source of truth for that branching so run.go can decide
// to SKIP resolving those create-only inputs before a plain re-attach (which
// must never fail on a --dev/checkout problem it doesn't need) without
// duplicating — and risking drifting from — planSandboxLaunch's own logic.
func willCreate(state sbxState, replace bool) bool {
	if replace {
		return true
	}
	switch state {
	case sbxRunning, sbxStopped:
		return false
	default: // sbxAbsent or sbxUnknown: nothing (known) is in the way, create fresh.
		return true
	}
}

// definitelyCreating reports whether the launch is CERTAIN to create a fresh
// sandbox: a POSITIVE "not present" probe, or --replace when removal actually
// happens (planSandboxLaunch only runs `sbx rm -f` for a POSITIVELY known
// running/stopped sandbox). It deliberately differs from willCreate on
// sbxUnknown (round-3 R3 + round-4 F3): willCreate optimistically prepares
// create args when the probe FAILED — sbx itself may still re-attach the
// existing sandbox, and on sbxUnknown even --replace skips the rm (RmFirst is
// false), so the old sandbox can come back — so persisted create-time state
// (the workspace sandbox.pack marker) must gate on THIS stricter predicate,
// never on willCreate or on --replace alone, or a transient `sbx ls` failure
// would overwrite the marker for a sandbox that was in fact re-attached (and
// wrongly silence stalePackReattachWarning).
func definitelyCreating(state sbxState, replace bool) bool {
	return state == sbxAbsent || (replace && state != sbxUnknown)
}

// buildReattachArgs composes the argv for RE-ATTACHING to an existing sandbox:
// `run --name <name>`, deliberately WITHOUT any create-only flag
// (--kit/--template/--mcp, the config-stacked kits, or the --dev/--skills live
// trees) — sbx reads the agent from the existing sandbox's own spec, so
// reapplying them would be a no-op at best and a lie about what's running at
// worst. --model/--intent are NOT create-only, though: they are pi RUNTIME args
// (forwarded after `--`, same as buildSbxArgs), so a resolved o.Model (whether
// the user passed --model directly or run.go resolved it from --intent) still
// needs to reach the pi session on a re-attach, exactly like a fresh create.
// The user's own pi passthrough (o.Passthrough) forwards after it, mirroring
// buildSbxArgs' own passthrough handling.
func buildReattachArgs(o runOpts) []string {
	args := []string{"run", "--name", o.Name}
	var piArgs []string
	if o.Model != "" {
		piArgs = append(piArgs, "--model", o.Model)
	}
	piArgs = append(piArgs, o.Passthrough...)
	if len(piArgs) > 0 {
		args = append(args, "--")
		args = append(args, piArgs...)
	}
	return args
}

// pinnedGitKit returns the launcher's own pinned git kit URL if it appears in
// the composed args, else "". Used to key the graceful post-exec failure
// message on whether we pinned a git #ref (vs. a local checkout kit).
func pinnedGitKit(args []string) string {
	for _, a := range args {
		if strings.HasPrefix(a, kitRepo) && strings.Contains(a, "#ref=") {
			return a
		}
	}
	return ""
}

// kitResolveFailureMsg formats an actionable error for when `sbx run` fails and
// the composed kit used a git #ref (a version tag that may not be published, or
// a fallback to main). Returns "" when no git-ref kit was pinned (so the caller
// leaks nothing extra on unrelated failures). Pure + testable.
func kitResolveFailureMsg(pinnedKit string) string {
	if pinnedKit == "" {
		return ""
	}
	ref := "main"
	if i := strings.Index(pinnedKit, "#ref="); i >= 0 {
		rest := pinnedKit[i+len("#ref="):]
		if amp := strings.IndexByte(rest, '&'); amp >= 0 {
			rest = rest[:amp]
		}
		ref = rest
	}
	// This fires on ANY `sbx run` failure that had a git-ref kit pinned — it has
	// NOT inspected why. So it must not assert a cause. It used to lead with
	// "release <ref> may not be published yet", which reads as a diagnosis and
	// sent at least one person hunting a release problem when the real error,
	// printed directly above it, was git refusing to start in a deleted cwd.
	lead := fmt.Sprintf("sbx could not resolve the kit at ref %q.", ref)
	return "pix: " + lead + `
Check the error above first — it is the actual failure. Common causes:
  - the shell's working directory no longer exists (git cannot start there):
    "fatal: Unable to read current working directory" — cd somewhere real
  - no network / GitHub unreachable
  - the ref genuinely is not published yet
Options:
  - pick another ref:                          pix run --kit-ref <tag-or-branch>
  - run a local build from your pix checkout:  pix run --dev
  - override the kit entirely:                 pix run --kit <path-or-git-url>
See ` + "`pix help run`" + ` for the released-vs-local behavior.`
}
