package main

import (
	"path/filepath"

	"pi-stack/host/config"
)

// kitRepo is the canonical git-hosted kit source. The launcher pins it to the
// stamped version so a consumer's `pi-stack run` resolves the exact image/kit
// that was published for this launcher build.
const kitRepo = "git+https://github.com/mcavage/pi-stack.git"

// runOpts carries everything the arg builder needs, resolved by the caller. It
// is deliberately side-effect-free (no filesystem probing, no token minting) so
// buildSbxArgs stays a pure function that the tests can drive without sbx or a
// real config on disk.
type runOpts struct {
	Workspace   string   // positional DIR (default ".")
	Dev         bool     // --dev: Mode B, skills load live from a repo checkout
	DevRoot     string   // resolved repo root when Dev is set (caller resolves)
	Skills      []string // --skills DIR: extra live skill trees
	Kits        []string // --kit K: extra kits stacked on top of config.Kits.Stack
	MCP         []string // --mcp M: extra MCP servers on top of config.MCP
	Name        string   // --name N: sandbox name
	Model       string   // --model M: active pi model (passed through to pi)
	Passthrough []string // args after `--`, handed straight to pi
	Token       string   // broker bearer; injected via the sbx PROCESS ENV (run.go), never argv
}

// kitRef returns the git ref fragment the launcher pins. A stamped release
// (version like "0.0.99") pins the tag "v0.0.99"; an unstamped dev build
// (version=="dev") tracks "main".
func kitRef(version string) string {
	if version == "dev" {
		return "main"
	}
	return "v" + version
}

// gitKitURL is the full --kit URL for a repo-less consumer run, pinned to the
// stamped version (or main for a dev build).
func gitKitURL(version string) string {
	return kitRepo + "#ref=" + kitRef(version) + "&dir=pi-kit"
}

// buildSbxArgs composes the full argv for `sbx <args...>` (i.e. it returns
// everything AFTER the "sbx" program name, starting with "run"). It is pure: no
// exec, no filesystem, no token minting — the caller does all of that and feeds
// the results in via cfg + o. This is the function the tests exercise.
func buildSbxArgs(cfg *config.Config, o runOpts, version string) []string {
	args := []string{"run", "pi-stack"}

	if o.Name != "" {
		args = append(args, "--name", o.Name)
	}

	// Kit selection. Dev mode uses the LOCAL repo kit (Mode B, matching
	// bin/pi-stack --dev); otherwise pin the published git kit to this build's
	// version.
	if o.Dev {
		args = append(args, "--kit", filepath.Join(o.DevRoot, "pi-kit"))
	} else {
		args = append(args, "--kit", gitKitURL(version))
	}

	// Stack additional kits: config first, then --kit flags.
	for _, k := range cfg.Kits.Stack {
		args = append(args, "--kit", k)
	}
	for _, k := range o.Kits {
		args = append(args, "--kit", k)
	}

	// MCP servers: config first, then --mcp flags.
	for _, m := range cfg.MCP {
		args = append(args, "--mcp", m)
	}
	for _, m := range o.MCP {
		args = append(args, "--mcp", m)
	}

	// SECURITY: the shared broker bearer (o.Token) is DELIBERATELY NOT emitted on
	// argv here. argv is process-inspectable (anyone with `ps`, or an EDR agent,
	// can read a plain `--env GWS_TOKEN_AUTH=<token>`), so the launcher instead
	// sets GWS_TOKEN_AUTH in the sbx CHILD PROCESS ENVIRONMENT (see run.go), and
	// pi-kit/spec.yaml declares it so the sandbox picks it up from that forwarded
	// host env. Keep this arg builder token-free.

	// Workspace (first non-flag positional).
	args = append(args, o.Workspace)

	// Live skill trees: config paths + --skills flags. Each is mounted as an
	// extra workspace so pi can read it inside the sandbox.
	liveSkills := append(append([]string(nil), cfg.Skills.Paths...), o.Skills...)
	for _, s := range liveSkills {
		args = append(args, s)
	}
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
