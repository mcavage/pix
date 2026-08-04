package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/inference"
	"pix/host/readiness"
	"pix/host/secret"
	"pix/host/sys"
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

// probeRun is gone: with a non-nullable System it was `env.RunTimed(...)` with
// extra steps. It used to fall back to env.Run when env.probe was nil, and to
// `fmt.Errorf("no runner")` when both were — one of the fourteen disagreeing
// answers to a missing seam. runWithTimeout/runWithTimeoutD moved to
// sys.RunTimed, which is where a bounded exec belongs.

// RunDoctor builds the report. Pure apart from env: no direct OS access, so the
// tests feed a faked hostenv.Env and assert on the rendered output. Each group is
// built by its own builder (doctor_providers.go, doctor_ollama.go,
// doctor_memory.go, doctor_gog.go, doctor_secrets.go, doctor_mcp.go) so later
// stories can rework one group without touching the others.
func RunDoctor(cfg *config.Config, env hostenv.Env) *readiness.Report {
	r := &readiness.Report{}

	// sbx presence gates the provider + mcp checks (they read `sbx secret ls` /
	// `sbx mcp ls`). Inside the sandbox sbx is absent — say so, don't crash.
	// secret.ProbeSbxSecrets is the ONE shared probe (bootstrap.go's tri-state helpers
	// use it too) so this never reimplements a divergent "is sbx reachable"
	// check.
	sbxOut, SbxState := secret.ProbeSbxSecrets(env)
	sbxOK := SbxState == secret.SbxSecretsOK
	// sbxAbsent means POSITIVELY absent (lookPath could not find sbx) — never
	// a generic probe failure: sbx present with `sbx secret ls` erroring or
	// timing out is a different, diagnosable host state (secret.SbxSecretsError) and
	// must not render the "you're likely inside the sandbox" note.
	r.SbxAbsent = SbxState == secret.SbxSecretsAbsent
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
	r.Groups = append(r.Groups, ProvidersGroup(cfg, env, sbxOut, sbxOK))
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

// ParseDoctorArgs validates doctor flags: -h/--help returns cli.ErrHelpRequested,
// --json sets jsonOut, --verbose sets verbose (full per-check detail; the
// default is concise and collapses ready checks), any other token is a usage
// error (exit 2).
func ParseDoctorArgs(argv []string) (jsonOut, verbose bool, err error) {
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

const Usage = `usage: pix doctor [--json] [--verbose]

Diagnose host + sandbox health (provider keys, ollama/models, memory, Google Workspace, mcp),
leading with a one-line verdict and copy-pasteable TODO commands. The default
output is concise (verified-ready checks collapse per readiness.Group); --verbose shows
every readiness.Check.

flags:
  --json      emit the machine-readable readiness.Report (schema_version 2)
  --verbose   show every readiness.Check, including verified-ready detail

exit codes:
  0  ready, or only optional/unverifiable gaps (nothing verified-broken that
     pix requires)
  1  a positively verified core failure (or the config failed to load)
  2  usage error
`

const StatusUsage = `usage: pix status [--json]

Fast, read-only control panel: services, provider keys, knowledge bundles,
MCP registration, and running pix-* sandboxes. Launches nothing.

flags:
  --json   emit the machine-readable status snapshot
`

// SbxInstallHint is shared by doctor, run, and setup because the nightly tap
// name is expected to change when MCP support stabilizes. README.md carries the
// one accepted non-Go copy and must change at the same time.
const SbxInstallHint = "brew install docker/tap/sbx@nightly"

// UnwiredProviderKeys is the gap this whole feature closes, as a fact both the
// status screen and doctor can read: a provider whose key RESOLVES on this host
// but which has no native binding in config, i.e. a key that is present,
// correct, and doing nothing.
//
// It reports absence of wiring, never a verdict about the key's validity. A
// binding that exists but failed its probe is NOT reported here — that is a
// different problem with a different fix, and conflating them would send a user
// to `models add` for a credential their provider rejected.
//
// Silent when a pack owns inference (its bindings are the pack's business) and
// when the key list is unreadable, since an unreadable list is not evidence of
// a gap.
func UnwiredProviderKeys(cfg *config.Config, env hostenv.Env) []string {
	if cfg == nil || cfg.Inference.ExclusiveSource != "" {
		return nil
	}
	names, err := secret.HostModeProviderKeys(env)
	if err != nil || len(names) == 0 {
		return nil
	}
	bound := inference.BoundNativeProviders(cfg)
	var gaps []string
	for _, n := range names {
		if !bound[n] {
			gaps = append(gaps, n)
		}
	}
	sort.Strings(gaps)
	return gaps
}

// SbxState is the tri-state a task probe resolves a sandbox to. The whole point
// is that an errored/unreachable `sbx` invocation is UNKNOWN, distinct from a
// clean "not in the list" ABSENT: callers must refuse destructive action on
// UNKNOWN rather than assume the safe-looking absent value.
type SbxState int

// taskStateSummary walks the task state + artifact roots to a global count of
// task clones and the on-disk size of harvested artifacts, so `pix status`
// can surface the pile without any per-repo git probing (that needs a cwd/repo).
// Best-effort: an unreadable tree contributes 0.
func taskStateSummary() (tasks int, artifactBytes int64) {
	repos, _ := os.ReadDir(workspace.TaskStateRoot())
	for _, r := range repos {
		if !r.IsDir() {
			continue
		}
		metas, _ := os.ReadDir(filepath.Join(workspace.TaskStateRoot(), r.Name(), "meta"))
		for _, m := range metas {
			if !m.IsDir() && strings.HasSuffix(m.Name(), ".json") {
				tasks++
			}
		}
	}
	_, artifactBytes = sys.DirSize(workspace.TaskArtifactRoot())
	return tasks, artifactBytes
}
