package launch

import (
	"fmt"
	"path/filepath"
	"strings"

	"pix/host/config"
	"pix/host/launcher"
	"pix/host/sandbox"
)

const kitRepo = "git+https://github.com/mcavage/pix.git"

// DockerImageRepo is the published image repo. A local build pins a locally
// loaded tag from <repo>/out/.local-image-tag via --template.
const DockerImageRepo = "docker.io/mcavage/pix-agent"

type RunOpts struct {
	Verbose       bool
	Workspace     string   // positional DIR (default ".")
	Dev           bool     // --dev: Mode B, skills load live from a repo checkout
	DevRoot       string   // resolved repo root when Dev is set (caller resolves)
	LocalKit      string   // resolved local checkout kit dir (<repo>/pi-kit); replaces the git pin
	LocalImageTag string   // <repo>/out/.local-image-tag; pins --template to the locally loaded image
	Template      string   // --template REF: explicit image override; works from ANY directory and beats LocalImageTag
	Skills        []string // --skills DIR: extra live skill trees
	Kits          []string // --kit K: escape hatch. When present they REPLACE the auto git/local pin, then the config stack applies.
	KitRef        string
	MCP           []string // --mcp M: flag-requested servers (folded into StaticMCP by the caller)
	StaticMCP     []string // RESOLVED create-time set, emitted as --static-mcp (mcp.AllPreloadedMCP of MCP + built-ins)
	Name          string   // --name N: sandbox name
	// Env is `--env NAME`: the EXACT registered environment this run
	// launches under, overriding the configured default for this run only
	// (never written back to config). EnvName is the name that actually
	// RESOLVED (the explicit one, else the machine default, else "" for
	// D17's `none`), filled in by the command layer once selection ran.
	Env     string
	EnvName string
	Model   string   // --model M: active pi model (passed through to pi)
	Resume  string   // --resume SESSION: resume this pi session, on every path (create or attach)
	Models  []string // create-time callable model cycle, derived from probed bindings
	// Keep is -k/--keep: bind a sticky, identity-bound keep marker to this
	// session — what the teardown and the orphan sweep refuse on.
	Keep        bool
	PackKits    []string
	Passthrough []string // args after `--`, handed straight to pi
	Token       string
	// Recreated marks the retry of a launch whose sandbox was removed by a
	// SAFE AUTOMATIC RECREATE on this invocation. It exists so exactly one
	// recreate can happen per `pix run`: a second recreation-safe drift on
	// the retry is a refusal with the manual sequence, never a loop.
	Recreated bool
	// LauncherVersion is the version string THIS pix binary was stamped
	// with (`main.version`, set by the Makefile's LAUNCHER_VERSION
	// -ldflags). It is carried on RunOpts rather than threaded as yet
	// another `version string` parameter so there is ONE field every launch
	// decision that depends on the build identity reads: the session
	// fingerprint's `launcher_version` component and the Pix-managed
	// `PIX_LAUNCHER_VERSION` environment fact both come from here, so they
	// can never disagree about which build created a sandbox.
	LauncherVersion string
}

func gitKitURLRef(ref, version string) string {
	if ref == "" {
		ref = launcher.KitRef(version)
	}
	return kitRepo + "#ref=" + ref + "&dir=pi-kit"
}

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
		args = append(args, "--template", DockerImageRepo+":"+o.LocalImageTag)
	}

	if !kitOverride {
		if o.LocalKit != "" {
			args = append(args, "--kit", o.LocalKit)
		} else {
			args = append(args, "--kit", gitKitURLRef(o.KitRef, version))
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

	// Workspace (first non-flag positional).
	args = append(args, o.Workspace)

	// Live skill trees: config paths + personal dir + --skills flags. Each is
	// mounted as an extra workspace so pi can read it inside the sandbox.
	//
	// The MOUNT set is not the SKILL set. For personal context, pi is pointed at
	// <context>/skills while the mount is <context> itself, so AGENTS.md next to
	// it is editable and committable from inside the sandbox too (see
	// MountDirs). Mounting the parent also means the skills dir needs no separate
	// mount: it is inside it.
	liveSkills := LiveSkillDirs(cfg, o)
	args = append(args, MountDirs(cfg, o)...)
	if o.Dev && o.DevRoot != "" {
		args = append(args, filepath.Join(o.DevRoot, "skills"))
	}

	// pi passthrough args (after `--`), from the SAME builder a later attach's
	// stored (or freshly recomputed) invocation reuses.
	if piArgs := BuildPiInvocation(liveSkills, o); len(piArgs) > 0 {
		args = append(args, "--")
		args = append(args, piArgs...)
	}
	return args
}

// LiveSkillDirs is the ordered set of extra skill trees pi is told to LOAD
// (`--skill DIR`). The personal tree is included unconditionally, not only when
// it already has entries: an empty dir is a valid "nothing yet", and gating on
// content is what made the first skill impossible to write from inside a
// sandbox (nothing was mounted, so there was nowhere to write it). pi ignores a
// skill dir with no skills in it.
func LiveSkillDirs(cfg *config.Config, o RunOpts) []string {
	liveSkills := append([]string(nil), cfg.Skills.Paths...)
	liveSkills = append(liveSkills, PersonalSkillsDir())
	liveSkills = append(liveSkills, o.Skills...)
	return liveSkills
}

// PersonalSkillsDir is the one spelling of the personal skill tree's path,
// shared by the loader and the mount so they can never point at different dirs.
func PersonalSkillsDir() string { return filepath.Join(config.ContextDir(), "skills") }

// MountDirs is the ordered set of extra host dirs to bind into the sandbox as
// additional workspaces (read-write, at their host path).
//
// It differs from LiveSkillDirs in exactly one place, deliberately: personal
// context mounts the CONTEXT ROOT rather than its skills/ subdir, so a session
// can edit both the skills AND the standing AGENTS.md beside them, and a user
// can keep the whole directory in git. Editing AGENTS.md mid-session does not
// change the CURRENT session's instructions (they were inlined into a kit at
// launch, the same way Claude Code reads CLAUDE.md once at start); the next
// sandbox picks it up.
func MountDirs(cfg *config.Config, o RunOpts) []string {
	mounts := append([]string(nil), cfg.Skills.Paths...)
	mounts = append(mounts, config.ContextDir())
	mounts = append(mounts, o.Skills...)
	return mounts
}

func BuildPiInvocation(liveSkills []string, o RunOpts) []string {
	piArgs := []string{"--session-dir", ".pi-sessions"}
	if o.Dev && o.DevRoot != "" {
		// Mode B: turn off baked skills and load the repo tree live.
		piArgs = append(piArgs, "--no-skills", "--skill", filepath.Join(o.DevRoot, "skills"))
	}
	for _, s := range liveSkills {
		piArgs = append(piArgs, "--skill", s)
	}
	if o.Model != "" {
		piArgs = append(piArgs, "--model", o.Model)
	}
	if o.Resume != "" {
		piArgs = append(piArgs, "--session", o.Resume)
	}
	if len(o.Models) > 0 {
		piArgs = append(piArgs, "--models", strings.Join(o.Models, ","))
	}
	piArgs = append(piArgs, o.Passthrough...)
	return piArgs
}

// BuildAttachArgv composes `sbx exec` argv to re-attach to an existing,
// POSITIVELY IDENTIFIED sandbox (running or stopped: exec starts a stopped
// one) by re-invoking pi directly with invocation, rather than asking sbx to
// re-derive a pi command from the container's own spec. tty selects "-it"
// (interactive) vs "-i" (piped/scripted), the same convention
// sandbox.ExecOpts/CreateOpts already use everywhere else.
func BuildAttachArgv(name string, tty bool, invocation []string) ([]string, error) {
	return sandbox.ExecArgv(sandbox.ExecOpts{
		Name:    name,
		TTY:     tty,
		Command: append([]string{"pi"}, invocation...),
	})
}

type RunLaunchPlan struct {
	Args     []string // sbx argv (after "sbx")
	Reattach bool     // Args is the thin re-attach form, not a full create
	Err      error
}

// PlanSandboxLaunch is the pure decision at the heart of `pix run`'s lifecycle,
// matching sbx's own re-attach model: a create-only flag (--kit/--template/
// --static-mcp) only makes sense when the sandbox does not already exist.
func PlanSandboxLaunch(state SbxState, cfg *config.Config, o RunOpts, version string) RunLaunchPlan {
	if state == SbxUnknown {
		return RunLaunchPlan{Err: fmt.Errorf("could not determine whether sandbox %q exists (`sbx ls` failed or sbx is unavailable); refusing to create or attach blind — fix sbx and retry", o.Name)}
	}
	if !WillCreate(state) {
		return RunLaunchPlan{Args: BuildReattachArgs(o), Reattach: true}
	}
	return RunLaunchPlan{Args: BuildSbxArgs(cfg, o, version)}
}

// WillCreate is THE create-vs-attach predicate: a POSITIVE "not present" probe,
// and nothing else. Unknown is false — an indeterminate read never authorizes
// create-only work, exactly as PlanSandboxLaunch refuses to plan one.
func WillCreate(state SbxState) bool { return state == SbxAbsent }

// BuildReattachArgs composes the PRE-CUTOVER argv for ATTACHING: `run --name <name>`,
// deliberately WITHOUT any create-only flag — sbx reads the agent from the
// existing sandbox's own spec, so reapplying them would be a no-op at best and
// a lie about what's running at worst. --model is NOT create-only: it is a pi
// RUNTIME arg, so a resolved o.Model (from --model or --intent) still reaches
// the session, exactly as on a fresh create.
//
// No live sandbox is attached this way any more: every positively identified
// row execs (BuildAttachArgv), the only form carrying this launch's complete pi
// invocation. This stays as PlanSandboxLaunch's reattach argv and as
// SessionSpec.AttachArgs' unselected fallback.
func BuildReattachArgs(o RunOpts) []string {
	args := []string{"run", "--name", o.Name}
	piArgs := []string{"--session-dir", ".pi-sessions"}
	if o.Model != "" {
		piArgs = append(piArgs, "--model", o.Model)
	}
	if o.Resume != "" {
		piArgs = append(piArgs, "--session", o.Resume)
	}
	piArgs = append(piArgs, o.Passthrough...)
	if len(piArgs) > 0 {
		args = append(args, "--")
		args = append(args, piArgs...)
	}
	return args
}
