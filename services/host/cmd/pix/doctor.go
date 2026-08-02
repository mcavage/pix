package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/readiness"
	"pix/host/rpc"
	"pix/host/secret"
	"pix/host/sys"
	"pix/host/workflow/upgrade"
	"pix/host/workspace"
)

// doctor ports the Makefile `doctor:` target into Go. Unlike the shell version
// it LEADS WITH A ONE-LINE VERDICT, then details the checks grouped in
// dependency order (keys -> ollama/models -> memory -> gog -> mcp), keeping the
// copy-pasteable `TODO: <exact command>` lines for anything not set up.
//
// It must RUN cleanly inside the sandbox, where sbx and ollama are absent: every
// probe degrades to a sane TODO rather than crashing. All the OS-touching work
// goes through a hostenv.Env of function values so the tests drive it hermetically.

// hostenv.Env abstracts the ways doctor/setup touch the host: locating a binary,
// running a command for its output, reading an env var, and dialing a local TCP
// port. Tests substitute fakes; defaultShellEnv() wires the real thing.
// hostenv.Env is an ALIAS for hostenv.Env, not a distinct type.
//
// The bundle moved to its own package so the seven domains still trapped in
// this file's package can be extracted at all: an extracted package cannot name
// a parameter type declared in `package main`. The alias means every one of the
// ~250 signatures that says `hostenv.Env` keeps working unchanged, so the move
// cost nothing at the call sites.
//
// The name stays because `hostenv.Env` is what this codebase calls it everywhere,
// and renaming 250 signatures to prove a point is not a refactor. See
// hostenv.Env for what it holds and where each field is going.

// fake returns the embedded System as the test double, for fixtures that build
// a base env and then override one seam. TEST-ONLY: it panics on a real env,
// which is the right outcome for test-only code reached in production — the
// alternative is a silent no-op, and silent no-ops are what this refactor
// exists to delete.
// probeTimeout bounds every registered-command probe so doctor can never wedge
// on a hung MCP server; probeMaxOutput caps how much of its output we capture.
const (
	probeTimeout   = 5 * time.Second
	probeMaxOutput = 64 << 10 // 64KB
)

// probeRun is gone: with a non-nullable System it was `env.RunTimed(...)` with
// extra steps. It used to fall back to env.Run when env.probe was nil, and to
// `fmt.Errorf("no runner")` when both were — one of the fourteen disagreeing
// answers to a missing seam. runWithTimeout/runWithTimeoutD moved to
// sys.RunTimed, which is where a bounded exec belongs.

// defaultShellEnv returns a hostenv.Env backed by the real OS.
func defaultShellEnv() hostenv.Env {
	return hostenv.Env{
		System: sys.Real{}, HostBinary: func() (string, error) { return hostBinaryResolver() }, IdentityProbe: rpc.IdentityProbe, SlackAuth: liveSlackAuthTest, DirectInference: liveDirectInferenceProbe, OllamaInference: liveOllamaInferenceProbe}
}

// unwrapOpRun returns the effective command doctor would trust to exec. With
// no `--` it is argv itself (a bare command). With a `--`, it unwraps ONLY the
// EXACT wrapper grammar the launcher generates (mcpRegistrar.execArgv via the
// shared opRunWrapPrefix):
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
func unwrapOpRun(env hostenv.Env, argv []string) ([]string, bool) {
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
	refs := secret.FindOpRefs(env)
	if refs == "" {
		return nil, false
	}
	want := opRunWrapPrefix(opTok, refs)
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
			if !cut || filepath.Clean(val) != filepath.Clean(refs) {
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

// mcpConfigured reports whether name is in the configured MCP set (so `run`
// auto-attaches it via --mcp).
func mcpConfigured(cfg *config.Config, name string) bool {
	for _, m := range cfg.MCP {
		if m == name {
			return true
		}
	}
	return false
}

// runDoctor builds the report. Pure apart from env: no direct OS access, so the
// tests feed a faked hostenv.Env and assert on the rendered output. Each group is
// built by its own builder (doctor_providers.go, doctor_ollama.go,
// doctor_memory.go, doctor_gog.go, doctor_secrets.go, doctor_mcp.go) so later
// stories can rework one group without touching the others.
func runDoctor(cfg *config.Config, env hostenv.Env) *readiness.Report {
	r := &readiness.Report{}
	if g := upgrade.InstallDuplicatesGroup(env); len(g.Checks) > 0 {
		r.Groups = append(r.Groups, g)
	}

	// sbx presence gates the provider + mcp checks (they read `sbx secret ls` /
	// `sbx mcp ls`). Inside the sandbox sbx is absent — say so, don't crash.
	// secret.ProbeSbxSecrets is the ONE shared probe (bootstrap.go's tri-state helpers
	// use it too) so this never reimplements a divergent "is sbx reachable"
	// check.
	sbxOut, sbxState := secret.ProbeSbxSecrets(env)
	sbxOK := sbxState == secret.SbxSecretsOK
	// sbxAbsent means POSITIVELY absent (lookPath could not find sbx) — never
	// a generic probe failure: sbx present with `sbx secret ls` erroring or
	// timing out is a different, diagnosable host state (secret.SbxSecretsError) and
	// must not render the "you're likely inside the sandbox" note.
	r.SbxAbsent = sbxState == secret.SbxSecretsAbsent
	// sbxOnPath is tracked INDEPENDENTLY of sbxOK (finding #4): sbx being on
	// PATH but `sbx secret ls` failing/timing out (secret.SbxSecretsError) is a
	// DIFFERENT state from sbx being entirely absent, and the MCP/gog groups
	// must still get to try their OWN probe (`sbx mcp ls`) rather than being
	// falsely gated off by an unrelated secret-probe failure.
	sbxOnPath := !r.SbxAbsent

	// MCP registrations (`sbx mcp ls`), listed once and reused by the gog group
	// (its gateway registration) and the MCP group below. Gated on sbxOnPath,
	// NOT sbxOK: `sbx secret ls` failing must never prevent this independent
	// probe from running — on the host the CLI can be present with the
	// secret listing erroring while the MCP gateway is perfectly reachable.
	mcpOut, mcpOK := "", false
	if sbxOnPath {
		// BOUNDED (probeRun): a hung `sbx mcp ls` degrades to mcpOK=false —
		// every dependent check renders unverifiable — never a wedged doctor.
		if out, timedOut, err := env.RunTimed("sbx", "mcp", "ls"); err == nil && !timedOut {
			mcpOut, mcpOK = out, true
		}
	}

	// (a) provider secrets — proxy-injected, never in the VM. Genuinely gated
	// on sbxOK: this group's OWN probe (`sbx secret ls`) is the one that
	// failed, so it stays unverifiable regardless of sbxOnPath/mcpOK.
	r.Groups = append(r.Groups, providersGroup(cfg, sbxOut, sbxOK))
	// (b) ollama + the configured watcher/embed models.
	r.Groups = append(r.Groups, ollamaGroup(cfg, env))
	// (c) memory service on :11435.
	r.Groups = append(r.Groups, memoryGroup(cfg, env))
	// The ONE workspace-sandbox context (hardened resolver + receipt read),
	// shared by the gog and MCP groups so both render attachment truth from
	// the SAME receipt-backed join rows — never two different stories.
	sandboxCtx := resolveMCPSandboxContext(env)
	// (d) gog: Google Workspace via a host-side stdio MCP server the sbx gateway
	// spawns (the slack pattern). Passed sbxOnPath (not sbxOK) as its "sbx
	// present" signal so a secret-probe failure never masquerades as sbx
	// being off PATH.
	r.Groups = append(r.Groups, gogGroup(cfg, env, mcpOut, mcpOK, sbxOnPath, sandboxCtx))
	// (d2) Secrets (1Password) — its OWN top-level group, honest and separate.
	r.Groups = append(r.Groups, secretsGroup(cfg, env))
	// (e) MCP servers registered with sbx. Same sbxOnPath signal as gog.
	r.Groups = append(r.Groups, mcpGroup(cfg, env, mcpOut, mcpOK, sbxOnPath, sandboxCtx))

	return r
}

// runDoctorCmd is the CLI entry point wired into main's dispatch. Exit codes
// are part of the shared contract (snapshot.ExitCode): 0 = every core and
// requested axis is ready, 1 = a POSITIVELY VERIFIED core/requested failure
// (verdict todo/denied) or a config-load error, 2 = usage error, 3 = a
// core/requested axis could not be verified from here.
func runDoctorCmd(argv []string) {
	jsonOut, verbose, err := parseDoctorArgs(argv)
	if err != nil {
		if err == cli.ErrHelpRequested {
			fmt.Print(doctorUsage)
			return
		}
		fmt.Fprintf(os.Stderr, "pix doctor: %v\n\n%s", err, doctorUsage)
		os.Exit(2)
	}
	cfg, _, err := workspace.LoadResolvedConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix doctor: %v\n", err)
		os.Exit(1)
	}
	r := runDoctor(cfg, defaultShellEnv())
	r.Services = cfg.Services
	r.MCP = cfg.MCP
	if jsonOut {
		_ = cli.WriteJSONOut(os.Stdout, jsonView(r, ""))
	} else {
		r.Render(os.Stdout, verbose, doctorHints())
	}
	// The exit code is derived by the SHARED contract (snapshot.ExitCode):
	// 0 ready, 1 a verified core/requested failure, 3 core/requested axes
	// that could not be verified from here. Usage errors above already exit 2.
	// Doctor used to collapse 3 into 0; two exit contracts over one snapshot
	// would reintroduce exactly the disagreement this wave removes.
	if code := r.Snapshot().ExitCode(); code != readiness.ExitReady {
		os.Exit(code)
	}
}

// parseDoctorArgs validates doctor flags: -h/--help returns cli.ErrHelpRequested,
// --json sets jsonOut, --verbose sets verbose (full per-check detail; the
// default is concise and collapses ready checks), any other token is a usage
// error (exit 2).
func parseDoctorArgs(argv []string) (jsonOut, verbose bool, err error) {
	for _, a := range argv {
		switch a {
		case "-h", "--help":
			return false, false, cli.ErrHelpRequested
		case "--json":
			jsonOut = true
		case "--verbose":
			verbose = true
		default:
			return false, false, fmt.Errorf("unknown flag %q", a)
		}
	}
	return jsonOut, verbose, nil
}
