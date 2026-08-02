package mcp

import (
	"os"
	"path/filepath"
	"pix/host/hostenv"
	"strings"
)

// trust.go — whether a REGISTERED command is safe to exec as a probe.
//
// Doctor and status both spawn what the gateway has registered, to ask it
// whether it works. That is only safe if the registered argv matches a shape
// pix itself generates AND its executable tokens resolve to the canonical
// binaries — never the registered spelling, which a look-alike on PATH could
// have supplied. The question is about MCP registrations, so it lives with
// them rather than in whichever readiness builder asked first.

// trustedExecPath is the canonical-executable gate: it returns the exec token
// doctor may run for base, and whether the registered token is trusted. A bare
// name (no path separator) is trusted as-is — exec resolves it through PATH at
// spawn time, which IS lookPath's answer; there is no recorded path for an
// attacker to swap. A path-carrying token must be byte-equal (cleaned) to the
// PATH-resolved binary — STRICT equality only, with symlink resolution
// deliberately NOT consulted (a check-time symlink bless followed by exec of
// the registered path is a race the attacker wins by swapping the link). On
// success the returned token is the RESOLVER's canonical path, never the
// registered spelling, so the exec'd token is the trusted one by construction.
// Anything else (a look-alike /tmp/gog, a fake op) is untrusted and never
// executed.
func trustedExecPath(env hostenv.Env, tok, base string) (string, bool) {
	if filepath.Base(tok) != base {
		return "", false
	}
	if !strings.ContainsAny(tok, `/\`) {
		return tok, true // bare name: exec resolves via PATH = lookPath's answer
	}

	canonical, err := env.LookPath(base)
	if err != nil || canonical == "" {
		return "", false
	}
	if filepath.Clean(tok) != filepath.Clean(canonical) {
		return "", false
	}
	return filepath.Clean(canonical), true
}

// TrustedGogSpawn reports whether a registered gog command is BOTH the
// recognized gog shape (GogSpawnArgv) AND built from canonical executables:
// the inner gog binary must match env.LookPath("gog"), and — when op-wrapped —
// the op binary must match env.LookPath("op"). On success it returns the
// NORMALIZED argv: the gog/op executable tokens replaced with the resolvers'
// canonical paths, so the caller execs the TRUSTED tokens, never the
// registered spelling. Only that normalized spawn is ever executed as a probe.
func TrustedGogSpawn(env hostenv.Env, argv []string, opRefs string) ([]string, bool) {
	inner, ok := GogSpawnArgv(env, argv, opRefs)
	if !ok {
		return nil, false
	}
	gogTok, gogOK := trustedExecPath(env, inner[0], "gog")
	if !gogOK {
		return nil, false
	}
	norm := append([]string(nil), argv...)
	// inner is the suffix GogSpawnArgv/UnwrapOpRun peeled off argv, so the
	// inner executable sits at len(argv)-len(inner); >0 means op-wrapped.
	innerStart := len(argv) - len(inner)
	norm[innerStart] = gogTok
	if innerStart > 0 {
		opTok, opOK := trustedExecPath(env, argv[0], "op")
		if !opOK {
			return nil, false
		}
		norm[0] = opTok
	}
	return norm, true
}

// UnwrapOpRun returns the effective command doctor would trust to exec. With
// no `--` it is argv itself (a bare command). With a `--`, it unwraps ONLY the
// EXACT wrapper grammar the launcher generates (mcp.McpRegistrar.execArgv via the
// shared mcp.OpRunWrapPrefix):
//
//	<canonical op> run --no-masking --env-file=<launcher op-refs.env> -- <cmd…>
//
// token for token: a canonical op executable (trustedExecPath — a bare `op`
// or lookPath's exact answer, never a look-alike path), the literal `run`
// subcommand (never signin/plugin/anything else), the exact generated option
// set in the generated order (no missing/extra/reordered options; the
// --env-file value must Clean-equal resolveOpRefs' answer — the same file
// registration wires — and the launcher only ever emits the one-token
// `--env-file=<refs>` form, so the two-token form is rejected), EXACTLY one
// `--`, and a non-empty inner command. Anything else returns ok=false so a
// hostile or drifted prefix is never exec'd — the caller reports the
// registration unverifiable instead of probing it.
func UnwrapOpRun(env hostenv.Env, argv []string, opRefs string) ([]string, bool) {
	if len(argv) == 0 {
		return nil, false
	}
	sep := -1
	for i, a := range argv {
		if a != "--" {
			continue
		}
		if sep >= 0 {
			return nil, false // multiple separators: never launcher-generated
		}
		sep = i
	}
	if sep < 0 {
		return argv, true // bare command, nothing to unwrap
	}
	inner := argv[sep+1:]
	if len(inner) == 0 {
		return nil, false
	}
	// The wrapper must run the SAME op binary env.LookPath resolves — a
	// foreign argv[0] (`/tmp/evil -- …`) or a look-alike `/tmp/op` is never
	// unwrapped, because the probe would exec that token verbatim.
	opTok, ok := trustedExecPath(env, argv[0], "op")
	if !ok {
		return nil, false
	}
	// No resolvable launcher refs file means no legitimate op-run wrapper can
	// exist for this host — fail closed rather than bless an unknown env file.
	if opRefs == "" {
		return nil, false
	}
	want := OpRunWrapPrefix(opTok, opRefs)
	prefix := argv[:sep+1]
	if len(prefix) != len(want) {
		return nil, false
	}
	const envFileOpt = "--env-file="
	for i, tok := range prefix {
		switch {
		case i == 0:
			// argv[0] already vetted canonical above.
		case strings.HasPrefix(want[i], envFileOpt):
			// Compare the env-file PATH cleaned, so a `/a//b` spelling can
			// neither dodge nor spuriously fail the equality.
			val, cut := strings.CutPrefix(tok, envFileOpt)
			if !cut || filepath.Clean(val) != filepath.Clean(opRefs) {
				return nil, false
			}
		default:
			if tok != want[i] {
				return nil, false
			}
		}
	}
	return inner, true
}

// TrustedHostBinaryExecPath is the canonical-pix-host gate: mcp.go
// registration (mcp.RegisterServers/serverCmd) ALWAYS spawns the ABSOLUTE path
// hostBinaryResolver (launcher.FindHostBinary) resolves — never a bare name. Trusting
// an absolute path's basename alone would let a malicious
// `/tmp/malicious/pix-host mcp slack` registration pass. env.HostBinary
// is the injected/hermetic trust seam mirroring hostBinaryResolver, so this
// compares against the SAME canonical answer the real registration used. tok
// must be absolute AND byte-equal (cleaned) to the resolved binary — STRICT
// equality only. Symlink resolution is deliberately NOT consulted: blessing an
// alternate symlink path at check time and exec'ing it afterwards is a
// check-then-exec race an attacker wins by swapping the link between the two.
// On success it returns the RESOLVER's canonical token — the only thing the
// caller may exec. An unresolvable canonical answer (env.HostBinary nil or
// erroring) fails CLOSED: never fall back to trusting the basename alone.
func TrustedHostBinaryExecPath(env hostenv.Env, tok string) (string, bool) {
	if filepath.Base(tok) != "pix-host" {
		return "", false
	}
	if !filepath.IsAbs(tok) {
		return "", false // never trust a bare/relative name for pix-host
	}
	if env.HostBinary == nil {
		return "", false
	}
	canonical, err := env.HostBinary()
	if err != nil || canonical == "" || !filepath.IsAbs(canonical) {
		return "", false
	}
	if filepath.Clean(tok) == filepath.Clean(canonical) {
		return filepath.Clean(canonical), true
	}
	// `make install` exposes the PATH-resolved pix-host as a symlink to the
	// checkout's out/pix-host. os.Executable may resolve the launcher symlink
	// while sbx keeps the installed spelling. Accept that alternate spelling
	// only when it is EXACTLY the pix-host this process resolves from PATH and
	// both paths currently identify the same file. This deliberately rejects
	// arbitrary lookalike symlinks (for example /tmp/pix-host -> canonical),
	// which could otherwise be retargeted after this check.

	pathHost, pathErr := env.LookPath("pix-host")
	if pathErr != nil || !filepath.IsAbs(pathHost) || filepath.Clean(tok) != filepath.Clean(pathHost) {
		return "", false
	}
	registeredInfo, registeredErr := os.Stat(tok)
	canonicalInfo, canonicalErr := os.Stat(canonical)
	if registeredErr != nil || canonicalErr != nil || !os.SameFile(registeredInfo, canonicalInfo) {
		return "", false
	}
	return filepath.Clean(canonical), true
}

// GogSpawnArgv extracts the effective gog spawn argv from a registered command,
// handling both the op-wrapped form (`op run … -- gog … mcp …`) and the bare
// form (`gog … mcp …`). It returns (cmd,true) when the resolved binary's
// basename is `gog` and its args contain the `mcp` subcommand; (nil,false)
// otherwise. Guards against index-out-of-range on short/empty argv.
func GogSpawnArgv(env hostenv.Env, argv []string, opRefs string) ([]string, bool) {
	if len(argv) == 0 {
		return nil, false
	}
	// Unwrap ONLY the exact launcher-generated `op run … --` wrapper grammar;
	// anything else (foreign argv[0], other op subcommands, alternate env
	// files, extra options) is rejected — the probe execs these tokens.
	cmd, ok := UnwrapOpRun(env, argv, opRefs)
	if !ok {
		return nil, false
	}
	if len(cmd) == 0 || strings.TrimSpace(cmd[0]) == "" {
		return nil, false
	}
	if filepath.Base(cmd[0]) != "gog" {
		return nil, false
	}
	for _, a := range cmd[1:] {
		if a == "mcp" {
			return cmd, true
		}
	}
	return nil, false
}
