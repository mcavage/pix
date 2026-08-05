package launch

import (
	"fmt"
	"path/filepath"
	"strings"

	"pix/host/config"
	"pix/host/launcher"
	"pix/host/sandbox"
	"pix/host/sys"
)

// kitRepo is the canonical git-hosted kit source. The launcher pins it to the
// stamped version so a consumer's `pix run` resolves the exact image/kit that
// was published for this launcher build.
const kitRepo = "git+https://github.com/mcavage/pix.git"

// DockerImageRepo is the published image repo. A local build pins a locally
// loaded tag from <repo>/out/.local-image-tag via --template.
const DockerImageRepo = "docker.io/mcavage/pix"

// RunOpts carries everything the arg builder needs, resolved by the caller. It
// is deliberately side-effect-free (no filesystem probing, no token minting) so
// BuildSbxArgs stays a pure function the tests can drive without sbx or a real
// config on disk.
type RunOpts struct {
	Workspace     string   // positional DIR (default ".")
	Dev           bool     // --dev: Mode B, skills load live from a repo checkout
	DevRoot       string   // resolved repo root when Dev is set (caller resolves)
	LocalKit      string   // resolved local checkout kit dir (<repo>/pi-kit); replaces the git pin
	LocalImageTag string   // <repo>/out/.local-image-tag; pins --template to the locally loaded image
	Template      string   // --template REF: explicit image override; works from ANY directory and beats LocalImageTag
	Skills        []string // --skills DIR: extra live skill trees
	Kits          []string // --kit K: escape hatch. When present they REPLACE the auto git/local pin, then the config stack applies.
	// KitRef steers the AUTO pin's git ref (e.g. "v0.1.2", "main"), resolved by
	// the caller via ResolveKitRef. Empty means this build's stamped version.
	// Distinct from Kits, which replaces the pin outright.
	KitRef    string
	MCP       []string // --mcp M: extra servers on top of config.MCP (folded into StaticMCP by the caller)
	StaticMCP []string // RESOLVED create-time set, emitted as --static-mcp (mcp.AllPreloadedMCP of cfg.MCP+MCP)
	Name      string   // --name N: sandbox name
	Model     string   // --model M: active pi model (passed through to pi)
	Models    []string // create-time callable model cycle, derived from probed bindings
	Intent    string   // --intent NAME: resolve the session model via the router (unless --model overrides)
	Replace   bool     // --replace: recreate (rm -f then create) instead of re-attaching
	Pack      string   // --pack PATH: active pack for this run (overrides config.Pack)
	// Keep is -k/--keep: bind a sticky, identity-bound keep marker to this
	// session (see SetSessionKeep). Story04c wiring only — there is no reaper
	// yet to consult it (see session.go's file doc).
	Keep bool
	// PackKits are ephemeral mixin kit dir(s) synthesized from the active pack's
	// bin/ wrappers. Deliberately SEPARATE from Kits: a non-empty Kits is the
	// escape hatch that REPLACES the base pin, and a pack mixin must stack
	// alongside the base kit, never suppress it.
	PackKits    []string
	Passthrough []string // args after `--`, handed straight to pi
	// Token is the credential bearer for an OPTIONAL external credential broker
	// plugin, reserved for the dormant generic broker seam. The default path
	// leaves it empty; it would travel via the sbx child env, never argv.
	Token string
}

// gitKitURLRef is the full --kit URL for an ALREADY-RESOLVED ref, which may be
// a release tag this binary was not stamped with.
func gitKitURLRef(ref string) string {
	return kitRepo + "#ref=" + ref + "&dir=pi-kit"
}

// refOrStamped falls back to this build's own ref when nothing overrode it, so
// every failure path in the resolver lands on the original lockstep behaviour.
func refOrStamped(ref, version string) string {
	if ref != "" {
		return ref
	}
	return launcher.KitRef(version)
}

// localImageRef is the --template ref for a locally loaded image tag.
func localImageRef(tag string) string {
	return DockerImageRepo + ":" + tag
}

// TemplateTag returns the tag portion of a --template image ref (everything
// after the final ':'), or "" if the ref carries no tag. Splitting on the LAST
// colon keeps a registry port (localhost:5000/foo:tag) from confusing it.
func TemplateTag(ref string) string {
	i := strings.LastIndexByte(ref, ':')
	if i < 0 || strings.IndexByte(ref[i:], '/') >= 0 {
		return ""
	}
	return ref[i+1:]
}

// BuildSbxArgs composes the full argv for `sbx <args...>` (everything AFTER the
// program name, starting with "run"). Pure: no exec, no filesystem, no token
// minting — the caller does all of that and feeds the results in via cfg + o.
func BuildSbxArgs(cfg *config.Config, o RunOpts, version string) []string {
	args := []string{"run", "pix"}

	if o.Name != "" {
		args = append(args, "--name", o.Name)
	}

	// --kit is an escape hatch: when present it REPLACES the auto git/local pin
	// (so a user can work around an unresolvable release tag).
	kitOverride := len(o.Kits) > 0

	// Image pin. An explicit --template REF wins over everything: it needs no
	// checkout and is orthogonal to kit selection, so it is NOT gated on
	// kitOverride. Otherwise mirror `make run`: pin the locally loaded image
	// when the resolved checkout carries out/.local-image-tag.
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
	// User --kit flags are the base when present; pack-synthesized kits ALWAYS
	// stack (they are an additive mixin, never the base image kit); config
	// kits always apply on top.
	for _, k := range o.Kits {
		args = append(args, "--kit", k)
	}
	for _, k := range o.PackKits {
		args = append(args, "--kit", k)
	}
	for _, k := range cfg.Kits.Stack {
		args = append(args, "--kit", k)
	}

	// --static-mcp is the fixed set chosen at CREATE (it cannot change on a
	// re-attach); the local data-plane gateway serves them. Attach one to an
	// already-running sandbox with `pix mcp load`.
	for _, m := range o.StaticMCP {
		args = append(args, "--static-mcp", m)
	}

	// The default path forwards NO credential bearer: gog authenticates on the
	// host inside the gateway-spawned MCP server. A future broker's o.Token
	// would go via the sbx child ENV, never argv (which `ps`/EDR can read).

	// Workspace (first non-flag positional).
	args = append(args, o.Workspace)

	// Live skill trees: config paths + personal dir + --skills flags. Each is
	// mounted as an extra workspace so pi can read it inside the sandbox.
	liveSkills := LiveSkillDirs(cfg, o)
	args = append(args, liveSkills...)
	if o.Dev {
		args = append(args, filepath.Join(o.DevRoot, "skills"))
	}

	// pi passthrough args (after `--`), the SAME builder a later attach's
	// stored (or, absent a record, freshly recomputed) invocation reuses — see
	// BuildPiInvocation.
	if piArgs := BuildPiInvocation(liveSkills, o); len(piArgs) > 0 {
		args = append(args, "--")
		args = append(args, piArgs...)
	}
	return args
}

// LiveSkillDirs is the ordered set of extra skill trees a launch mounts:
// config's own Skills.Paths, the personal context dir (only when it has
// entries), then any --skills flags. Extracted from BuildSbxArgs so the SAME
// list feeds both the create-time mount args and BuildPiInvocation's "--"
// tail — one list, never two that could drift apart.
func LiveSkillDirs(cfg *config.Config, o RunOpts) []string {
	liveSkills := append([]string(nil), cfg.Skills.Paths...)
	if personal := filepath.Join(config.ContextDir(), "skills"); sys.DirHasEntries(personal) {
		liveSkills = append(liveSkills, personal)
	}
	liveSkills = append(liveSkills, o.Skills...)
	return liveSkills
}

// BuildPiInvocation composes the pi argv a launch sends after "--": the pi
// COMMAND, not the sbx wrapper around it. It is deliberately the ONE place
// this is built, used for THREE callers that must never drift apart:
//   - BuildSbxArgs' own "--" tail on create,
//   - the invocation STORED at create time (session.WriteSessionInvocation),
//     replayed verbatim by a later `sbx exec ... pi <invocation>` attach, and
//   - the "safe current/default invocation" a caller recomputes on the spot
//     when an attach finds no stored record (a legacy or never-owned
//     sandbox) — same inputs, same function, so "default" can never mean
//     something subtly different from what create would have sent.
//
// liveSkills is passed in (rather than recomputed from cfg) so a caller that
// already has it (BuildSbxArgs) does not pay for or risk desyncing a second
// LiveSkillDirs call; a caller building a fresh default passes
// LiveSkillDirs(cfg, o) itself.
func BuildPiInvocation(liveSkills []string, o RunOpts) []string {
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
	if len(o.Models) > 0 {
		piArgs = append(piArgs, "--models", strings.Join(o.Models, ","))
	}
	piArgs = append(piArgs, o.Passthrough...)
	return piArgs
}

// BuildAttachArgv composes `sbx exec` argv to re-attach to an existing,
// POSITIVELY IDENTIFIED, RUNNING sandbox by re-invoking pi directly with
// invocation — replacing `sbx run --name`, which asks sbx to re-derive a pi
// command from the container's own spec, with an explicit exec pix fully
// controls. tty selects "-it" (interactive) vs "-i" (piped/scripted), the
// same convention sandbox.ExecOpts/CreateOpts already use everywhere else.
// A sandbox sbx cannot verify (unschema'd row) or that is merely STOPPED
// still uses the legacy BuildReattachArgs path — exec has no "start" of its
// own, and this package plans no destructive fallback for that case.
func BuildAttachArgv(name string, tty bool, invocation []string) ([]string, error) {
	return sandbox.ExecArgv(sandbox.ExecOpts{
		Name:    name,
		TTY:     tty,
		Command: append([]string{"pi"}, invocation...),
	})
}

// RunLaunchPlan is the OUTCOME of the create-vs-reattach-vs-replace decision.
// Err is set ONLY for the fail-closed unknown-state case: a non-nil Err means
// Args/RmFirst/Reattach are meaningless zero values, and the caller MUST check
// it before printing anything that claims a launch is happening, before
// running RmFirst, and before exec'ing sbx at all.
type RunLaunchPlan struct {
	RmFirst  bool     // run `sbx rm -f <name>` before Args
	Args     []string // sbx argv (after "sbx")
	Reattach bool     // Args is the thin re-attach form, not a full create
	Err      error
}

// PlanSandboxLaunch is the pure decision at the heart of `pix run`'s lifecycle,
// matching sbx's own re-attach model: a create-only flag (--kit/--template/
// --static-mcp) only makes sense when the sandbox does not already exist.
//
//   - absent -> CREATE: the full BuildSbxArgs, unchanged.
//   - unknown -> FAIL CLOSED: never guess create vs reattach. `sbx run` may
//     reattach an existing sandbox, so guessing "absent" could replay runtime
//     arguments into a live session.
//   - running or stopped, no --replace -> RE-ATTACH (BuildReattachArgs).
//   - any state, --replace -> `sbx rm -f <name>` (skipped when already absent)
//     then a full create, so changed create-only flags take effect.
func PlanSandboxLaunch(state SbxState, replace bool, cfg *config.Config, o RunOpts, version string) RunLaunchPlan {
	if state == SbxUnknown {
		return RunLaunchPlan{Err: fmt.Errorf("could not determine whether sandbox %q exists (`sbx ls` failed or sbx is unavailable); refusing to create or reattach blind — fix sbx and retry", o.Name)}
	}
	if !WillCreate(state, replace) {
		return RunLaunchPlan{Args: BuildReattachArgs(o), Reattach: true}
	}
	return RunLaunchPlan{
		RmFirst: replace && (state == SbxRunning || state == SbxStopped),
		Args:    BuildSbxArgs(cfg, o, version),
	}
}

// WillCreate reports whether PlanSandboxLaunch will create (or replace) rather
// than plainly re-attach — i.e. whether the create-only inputs (repo checkout /
// --dev / kit resolution) are even needed. It is the single source of truth for
// that branch so run.go can SKIP resolving create-only inputs before a plain
// re-attach (which must never fail on a --dev problem it doesn't need) without
// duplicating, and drifting from, PlanSandboxLaunch.
func WillCreate(state SbxState, replace bool) bool {
	if state == SbxUnknown {
		return false
	}
	if replace {
		return true
	}
	return state == SbxAbsent
}

// DefinitelyCreating reports whether the launch is CERTAIN to create a fresh
// sandbox: a POSITIVE "not present" probe, or --replace when removal actually
// happens. Unknown is false here just as in WillCreate; this stricter predicate
// guards persisted create-time state, which only positive creation evidence may
// update.
func DefinitelyCreating(state SbxState, replace bool) bool {
	return state == SbxAbsent || (replace && state != SbxUnknown)
}

// BuildReattachArgs composes the argv for RE-ATTACHING: `run --name <name>`,
// deliberately WITHOUT any create-only flag — sbx reads the agent from the
// existing sandbox's own spec, so reapplying them would be a no-op at best and
// a lie about what's running at worst. --model is NOT create-only: it is a pi
// RUNTIME arg, so a resolved o.Model (from --model or --intent) still reaches
// the session, exactly as on a fresh create.
func BuildReattachArgs(o RunOpts) []string {
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

// PinnedGitKit returns the launcher's own pinned git kit URL if it appears in
// the composed args, else "". Keys the post-exec failure message on whether we
// pinned a git #ref (vs. a local checkout kit).
func PinnedGitKit(args []string) string {
	for _, a := range args {
		if strings.HasPrefix(a, kitRepo) && strings.Contains(a, "#ref=") {
			return a
		}
	}
	return ""
}

// KitResolveFailureMsg formats an actionable error for when `sbx run` fails and
// the composed kit used a git #ref. Returns "" when no git-ref kit was pinned.
//
// This fires on ANY `sbx run` failure that had a git-ref kit pinned — it has
// NOT inspected why, so it must not assert a cause. It used to lead with
// "release <ref> may not be published yet", which reads as a diagnosis and sent
// someone hunting a release problem when the real error, printed directly
// above, was git refusing to start in a deleted cwd.
func KitResolveFailureMsg(pinnedKit string) string {
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
	return fmt.Sprintf("pix: sbx could not resolve the kit at ref %q.", ref) + `
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
