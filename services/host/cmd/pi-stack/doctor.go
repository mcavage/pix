package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"pi-stack/host/config"
)

// doctor ports the Makefile `doctor:` target into Go. Unlike the shell version
// it LEADS WITH A ONE-LINE VERDICT, then details the checks grouped in
// dependency order (keys -> ollama/models -> memory -> gog -> mcp), keeping the
// copy-pasteable `TODO: <exact command>` lines for anything not set up.
//
// It must RUN cleanly inside the sandbox, where sbx and ollama are absent: every
// probe degrades to a sane TODO rather than crashing. All the OS-touching work
// goes through a shellEnv of function values so the tests drive it hermetically.

// shellEnv abstracts the ways doctor/setup touch the host: locating a binary,
// running a command for its output, reading an env var, and dialing a local TCP
// port. Tests substitute fakes; defaultShellEnv() wires the real thing.
type shellEnv struct {
	lookPath func(name string) (string, error)
	run      func(name string, args ...string) (string, error)
	// hostBinary resolves the CANONICAL pi-stack-host binary path — the same
	// injected/hermetic seam mcp.go registration actually uses
	// (hostBinaryResolver/findHostBinary: sibling-to-launcher first, PATH
	// fallback, never a bare name). recognizedMCPArgv's pi-stack-host trust
	// gate (R2-01) compares a registered argv[0] against THIS answer rather
	// than trusting an absolute path's basename alone — basename-only trust
	// let a malicious `/tmp/malicious/pi-stack-host mcp slack` pass. Nil in
	// tests that don't exercise the mcp probe path; defaultShellEnv wires
	// hostBinaryResolver.
	hostBinary func() (string, error)
	getenv     func(name string) string
	dial       func(port int) bool
	statFile   func(path string) bool            // does a regular file exist at path?
	readFile   func(path string) (string, error) // read a file's contents
	homeDir    func() string                     // the user's home directory ($HOME)
	// fileMode returns a path's mode bits + whether it exists (file OR dir). The
	// Secrets group's perms check uses it to flag a group/other-accessible
	// op-refs.env or its dir. Nil in tests that don't exercise perms.
	fileMode func(path string) (os.FileMode, bool)
	// writeFile writes data to path (creating parent dirs). Nil in tests so
	// seeding stays hermetic; defaultShellEnv wires the real os-backed writer.
	writeFile func(path string, data []byte, perm os.FileMode) error
	// flock serializes a cross-process critical section on lockPath (an
	// advisory exclusive file lock). Nil in tests, which run fn directly so
	// hermetic unit tests never create a real lock file (the lock path derives
	// from defaultOpRefsPath, which those tests fake anyway); defaultShellEnv
	// wires the real blocking withFlock. See withProviderRefsLock.
	flock func(lockPath string, fn func() error) error
	// probe runs an UNTRUSTED registered command — and (R2-02) EVERY
	// noninteractive discovery subprocess (`sbx secret ls`, `sbx mcp ls`,
	// `sbx mcp get`, `sbx mcp ls -o json`, `ollama list`, `op account list`)
	// — with a hard timeout + capped output, so doctor never hangs (or
	// floods) on a misbehaving MCP server, sbx daemon, or ollama. It
	// returns (output, timedOut, err). Nil in tests, which fall back to run so
	// they stay hermetic; defaultShellEnv wires runWithTimeout.
	probe func(name string, args ...string) (out string, timedOut bool, err error)
	// runInteractive execs a command that may need a REAL terminal/browser (gog's
	// OAuth authorization steps): stdin/stdout/stderr are inherited from the
	// current process rather than captured, so a browser can open and a device
	// code/prompt can render. Nil in tests, which supply a fake that records the
	// call instead of touching a real terminal; defaultShellEnv wires the real
	// inherited-stdio exec. See gog_setup.go.
	runInteractive func(name string, args ...string) error
}

// probeTimeout bounds every registered-command probe so doctor can never wedge
// on a hung MCP server; probeMaxOutput caps how much of its output we capture.
const (
	probeTimeout   = 5 * time.Second
	probeMaxOutput = 64 << 10 // 64KB
)

// runWithTimeout execs name+args under a hard context deadline with capped
// captured output. It is the bounded alternative to shellEnv.run for probing
// untrusted registered commands: a server that hangs is killed at probeTimeout
// rather than freezing doctor, and runaway output is truncated at
// probeMaxOutput. Returns (output, timedOut, err).
func runWithTimeout(name string, args ...string) (string, bool, error) {
	return runWithTimeoutD(probeTimeout, name, args...)
}

// runWithTimeoutD is runWithTimeout with a caller-chosen deadline, so a fast
// command (e.g. `status`'s gog auth probe) can bound itself tighter than the
// default probeTimeout.
func runWithTimeoutD(timeout time.Duration, name string, args ...string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	// Hard wall-clock bound: if the child (or a descendant it spawned that still
	// holds stdout/stderr) is alive when the context fires, WaitDelay forces the
	// pipes closed + the process killed so CombinedOutput can't hang past it.
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	if len(out) > probeMaxOutput {
		out = out[:probeMaxOutput]
	}
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), true, ctx.Err()
	}
	return string(out), false, err
}

// probeRun invokes the bounded env.probe when wired (the real path), else falls
// back to env.run (tests). Returns (output, timedOut, err).
func probeRun(env shellEnv, name string, args ...string) (string, bool, error) {
	if env.probe != nil {
		return env.probe(name, args...)
	}
	if env.run == nil {
		return "", false, fmt.Errorf("no runner")
	}
	out, err := env.run(name, args...)
	return out, false, err
}

// defaultShellEnv returns a shellEnv backed by the real OS.
func defaultShellEnv() shellEnv {
	return shellEnv{
		lookPath: exec.LookPath,
		// hostBinaryResolver (findHostBinary) is the SAME resolver mcp.go
		// registration uses — wrapped in a closure (not bound directly) so a
		// test that swaps the package var mid-run (pack_v2_trust_*_test.go) is
		// still honored by any defaultShellEnv() created before the swap.
		hostBinary: func() (string, error) { return hostBinaryResolver() },
		run: func(name string, args ...string) (string, error) {
			out, err := exec.Command(name, args...).CombinedOutput()
			return string(out), err
		},
		getenv: os.Getenv,
		dial: func(port int) bool {
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 400*time.Millisecond)
			if err != nil {
				return false
			}
			_ = conn.Close()
			return true
		},
		statFile: func(path string) bool {
			info, err := os.Stat(path)
			return err == nil && !info.IsDir()
		},
		readFile: func(path string) (string, error) {
			b, err := os.ReadFile(path)
			return string(b), err
		},
		homeDir: func() string {
			h, _ := os.UserHomeDir()
			return h
		},
		fileMode: func(path string) (os.FileMode, bool) {
			fi, err := os.Stat(path)
			if err != nil {
				return 0, false
			}
			return fi.Mode(), true
		},
		// writeFile is LEAF-symlink-safe (parent-directory symlinks are a
		// separate, honestly out-of-scope concern — see atomicWriteInDir's doc
		// comment): the destination is never opened directly, so a leaf that is
		// itself a symlink is REPLACED by an atomic same-directory temp file +
		// rename, never followed/truncated through. Parent creation stays 0700
		// (unchanged perm posture). Shared with writeWorkspaceStateFile's exact
		// mechanism (workspacestate.go) so there is one hardened writer, not two.
		writeFile: func(path string, data []byte, perm os.FileMode) error {
			dir := filepath.Dir(path)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
			return atomicWriteInDir(dir, filepath.Base(path), data, perm)
		},
		flock: withFlock,
		probe: runWithTimeout,
		runInteractive: func(name string, args ...string) error {
			cmd := exec.Command(name, args...)
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			return cmd.Run()
		},
	}
}

// checkState is the rendered status of a single check.
type checkState int

const (
	stateOK   checkState = iota // set up / running
	stateTODO                   // needs action; carries an exact command
	stateInfo                   // informational, no action implied
	// stateWarn is a WARNING glyph (⚠), distinct from both stateOK (✓) and
	// stateTODO (✗): something could not be confirmed one way or the other, but
	// it is a best-effort success, not a known failure. It never carries a todo
	// (see AC-01: a best-effort gog headless success with an unreadable
	// sbx-registered command is unverifiable, never a ✗ with a fix-it command).
	stateWarn
)

// check is one line in a doctor group. requirement + evidence are the
// structured readiness axes (see readiness.go): every check must carry both
// so doctor's exit code and JSON output never have to re-derive state by
// parsing detail text. The rendered checkState is DERIVED from evidence (see
// check.state) — there is no stored legacy state field, so a glyph/evidence
// contradiction (R1-03) is impossible by construction.
type check struct {
	label       string
	detail      string // short human note after the label
	todo        string // exact copy-pasteable command; surfaced only when evidence == failed
	requirement Requirement
	evidence    Evidence
	// note marks a pure annotation line (transparency/context, e.g. "probing
	// the sbx-registered command: …"): it renders as · and makes no health
	// claim of its own, so it never counts toward the unverified tally.
	note bool
}

// state derives the rendered checkState from the structured readiness axes
// (R1-03): evidence is AUTHORITATIVE for the glyph. healthy → ✓, a verified
// failure → ✗, unverifiable → ⚠, not-configured (expected absence) → ·.
func (c check) state() checkState {
	if c.note {
		return stateInfo
	}
	switch c.evidence {
	case EvidenceHealthy:
		return stateOK
	case EvidenceFailed:
		return stateTODO
	case EvidenceUnverifiable:
		return stateWarn
	default: // EvidenceNotConfigured
		return stateInfo
	}
}

// group is a titled cluster of checks in dependency order.
type group struct {
	title  string
	checks []check
}

// report is the full doctor result: an ordered set of groups. It knows how to
// count outstanding TODOs (for the verdict) and render itself.
type report struct {
	groups    []group
	sbxAbsent bool // the sbx BINARY is not on PATH — provider/mcp checks can't be verified here
	// sbxProbeFailed: the sbx binary IS on PATH but `sbx secret ls` failed —
	// the host sbx probe/gateway is unavailable (R1-11). Distinct from
	// sbxAbsent so doctor never claims "not on PATH / inside the sandbox"
	// when the binary is plainly there.
	sbxProbeFailed bool
	services       []string // configured SERVICES, for the footer
	mcp            []string // configured MCP, for the footer
}

// todos returns every outstanding TODO command across all groups, in order,
// with duplicate commands dropped (so e.g. a `pi-stack mcp register` that two
// groups both surface only appears once). Dedup is normalized via todoDedupKey
// so two commands that differ only in a trailing parenthetical collapse. Order
// is preserved: the first occurrence's full string wins.
func (r *report) todos() []string {
	var out []string
	seen := map[string]bool{}
	for _, g := range r.groups {
		for _, c := range g.checks {
			// Evidence-authoritative (R1-03): only a VERIFIED failure may surface
			// a repair TODO. Unverifiable / not-configured checks never do, even
			// if a constructor left a suggestion in the todo field.
			if c.evidence != EvidenceFailed || c.todo == "" {
				continue
			}
			key := todoDedupKey(c.todo)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, c.todo)
		}
	}
	return out
}

// todoDedupKey normalizes a TODO for dedup so two commands that share the same
// leading command but differ only in a trailing parenthetical (e.g. `pi-stack
// secret set <ENV_VAR> op://vault/item/field` vs the same command with a
// trailing `  (creates …)`) collapse to one. It keys
// on the string up to the first `  (` separator, trimmed.
func todoDedupKey(todo string) string {
	if i := strings.Index(todo, "  ("); i >= 0 {
		return strings.TrimSpace(todo[:i])
	}
	return strings.TrimSpace(todo)
}

// gatewayDownDetail describes the HOST condition where sbx IS present (secret
// ls succeeded) but `sbx mcp ls` failed. The MCP gateway is now the local
// data-plane one (always available, no SBX_MCP_URL), so a failed listing means
// the sbx daemon/gateway is unhealthy, not "gateway off". This is NOT "sbx
// unavailable": the CLI is here, only the MCP-registration listing failed.
const gatewayDownDetail = "sbx present but couldn't list MCP registrations: check the sbx daemon (sbx mcp status; sbx daemon status)"

// sbxUnverifiableDetail is the per-check wording when provider/MCP state
// couldn't be read from sbx: it distinguishes the binary being absent (likely
// inside the sandbox) from the binary being present but the probe failing —
// the host sbx probe/gateway being unavailable (R1-11).
func sbxUnverifiableDetail(sbxPresent bool) string {
	if sbxPresent {
		return "sbx present but `sbx secret ls` failed: host sbx probe/gateway unavailable (try `sbx daemon status`)"
	}
	return "sbx unavailable here (verify on the host: sbx secret ls)"
}

// runDoctor builds the report. Pure apart from env: no direct OS access, so the
// tests feed a faked shellEnv and assert on the rendered output.
func runDoctor(cfg *config.Config, env shellEnv) *report {
	r := &report{}

	// sbx presence gates the provider + mcp checks (they read `sbx secret ls` /
	// `sbx mcp ls`). Inside the sandbox sbx is absent — say so, don't crash.
	// R1-11: the BINARY being present is tracked separately from the probe
	// (`sbx secret ls`) succeeding, so a present-but-unhealthy sbx is reported
	// as "host sbx probe/gateway unavailable", never "not on PATH".
	sbxOut, sbxOK, sbxPresent := "", false, false
	if _, err := env.lookPath("sbx"); err == nil {
		sbxPresent = true
		// R2-02: BOUNDED — a hung `sbx secret ls` is killed at probeTimeout and
		// classifies as present-but-probe-failed, never a wedged doctor.
		if out, timedOut, err := probeRun(env, "sbx", "secret", "ls"); err == nil && !timedOut {
			sbxOut, sbxOK = out, true
		}
	}
	r.sbxAbsent = !sbxPresent
	r.sbxProbeFailed = sbxPresent && !sbxOK

	// MCP registrations (`sbx mcp ls`), listed once and reused by the gog group
	// (its gateway registration) and the MCP group below. sbxOK (sbx PRESENT +
	// `sbx secret ls` ok) is tracked SEPARATELY from mcpOK (`sbx mcp ls` ok): on
	// the host the CLI is present but the MCP listing can fail (gateway off), and
	// that must not be reported as "sbx unavailable".
	mcpOut, mcpOK := "", false
	if sbxOK {
		// R2-02: BOUNDED — a hung `sbx mcp ls` classifies as the gateway-down
		// condition rather than hanging doctor.
		if out, timedOut, err := probeRun(env, "sbx", "mcp", "ls"); err == nil && !timedOut {
			mcpOut, mcpOK = out, true
		}
	}

	// (a) provider secrets — proxy-injected, never in the VM. R1-04: the
	// runtime needs AT LEAST ONE model-provider key (anthropic/openai/google),
	// so core-ness lives on ONE aggregate check; an individually-missing
	// provider is merely not-configured and never blocks on its own. (Setup's
	// separate all-three provisioning policy is untouched — see setup.go.)
	// GitHub stays configured-optional per current runtime policy.
	providers := group{title: "Providers / keys (proxy-injected, never in the VM)"}
	presentKeys := 0
	var perKey []check
	for _, key := range []string{"anthropic", "openai", "google"} {
		c := modelProviderKeyCheck(key, sbxOut, sbxOK, sbxPresent)
		if c.evidence == EvidenceHealthy {
			presentKeys++
		}
		perKey = append(perKey, c)
	}
	providers.checks = append(providers.checks, modelProviderAggregateCheck(presentKeys, sbxOK, sbxPresent))
	providers.checks = append(providers.checks, perKey...)
	providers.checks = append(providers.checks, secretCheck("github", "github", sbxOut, sbxOK, sbxPresent, RequirementConfiguredOptional))
	r.groups = append(r.groups, providers)

	// (b) ollama + the configured watcher/embed models. Ollama is ALWAYS
	// optional (its absence is a degraded-but-fine state, never a core
	// blocker); it is configured-optional when the memory service is in the
	// configured SERVICES set (something actually depends on it), else
	// unconfigured-optional.
	memoryEnabled := enabled(cfg, "memory")
	ollamaReq := RequirementUnconfiguredOptional
	if memoryEnabled {
		ollamaReq = RequirementConfiguredOptional
	}
	// probeOllama runs lookPath + a daemon dial + `ollama list` exactly ONCE
	// (modelreadiness.go); the watcher/embed checks below derive from this same
	// probe rather than each re-execing `ollama list`.
	ollama := group{title: "Ollama / local models (optional: fact capture + semantic recall)"}
	p := probeOllama(env)
	// `ollama list` succeeding proves the daemon answered even when the :11434
	// dial was blocked, so either signal counts as "daemon up".
	daemonUp := p.daemonUp || p.listOK
	switch {
	case p.installed && daemonUp:
		ollama.checks = append(ollama.checks, check{
			label:       "ollama",
			detail:      "installed, :11434 up",
			requirement: ollamaReq,
			evidence:    EvidenceHealthy,
		})
	case p.installed:
		// R1-05: installed but the daemon is down is a VERIFIED failure (never
		// a ✓/ok), and the action is starting the daemon — not a blind claim
		// about pulled models (those stay unverifiable below).
		ollama.checks = append(ollama.checks, check{
			label:       "ollama",
			detail:      "installed but the daemon is not running (:11434 down)",
			todo:        "start the Ollama daemon: `ollama serve` (or open the Ollama app), then re-run `pi-stack doctor`",
			requirement: ollamaReq,
			evidence:    EvidenceFailed,
		})
	default:
		ollama.checks = append(ollama.checks, check{
			label:       "ollama",
			detail:      "not installed; install: https://ollama.com",
			requirement: ollamaReq,
			evidence:    EvidenceNotConfigured,
		})
	}
	ollama.checks = append(ollama.checks,
		modelCheck(modelReadiness("watcher", cfg.MemoryWatcherModel, "fact capture", p, ollamaReq)),
		modelCheck(modelReadiness("embed", cfg.MemoryEmbedModel, "semantic recall", p, ollamaReq)),
	)
	r.groups = append(r.groups, ollama)

	// (c) memory service on :11435. Configured-optional when the user has
	// opted memory into SERVICES, else unconfigured-optional — never core, so
	// a down memory daemon never blocks doctor's exit code.
	memory := group{title: "Memory service (recall + capture)"}
	memUp := env.dial(11435)
	memory.checks = append(memory.checks, serviceCheck("memory", 11435, memUp, "pi-stack serve", memoryEnabled))
	// Live capture status straight from the daemon's health, not just "is the
	// model in ollama": this is the flag that decides whether observe() actually
	// stores anything. A latched-off watcher (daemon booted before the model was
	// pulled) shows here even when `ollama list` now has the model.
	if memUp {
		memory.checks = append(memory.checks, memCaptureCheck())
	}
	r.groups = append(r.groups, memory)

	// (d) gog: Google Workspace via a host-side stdio MCP server the sbx gateway
	// spawns (the slack pattern). No CLI in the VM, no token service, no bearer.
	// Checks run in strict dependency order and DELIBERATELY probe the REAL path
	// the gateway uses (headless, through `op run --env-file=config/op-refs.env`),
	// because `gog auth doctor` in a logged-in shell passes and lies.
	r.groups = append(r.groups, gogGroup(cfg, env, mcpOut, mcpOK, sbxOK))

	// (d2) Secrets (1Password) — its OWN top-level group, honest and separate.
	// Runs whenever ANY op-wrapped host MCP server is configured (slack, fastmail,
	// gog, ...), not just gog: op install + sign-in (SAFE metadata only), op-refs.env
	// presence + perms, and per-ref filled-vs-placeholder + a refs-only lint that
	// never prints an offending value.
	r.groups = append(r.groups, secretsGroup(cfg, env))

	// (e) OTHER MCP servers registered with sbx (everything besides gog, which
	// the dedicated gog group above already owns — its registration check +
	// TODO, so probing it again would emit a duplicate `pi-stack mcp register`).
	// AC-02: with gog the only configured server, this group must never claim
	// "(none configured)" — that reads as "nothing is set up" when gog plainly
	// is, just reported elsewhere. So when there is nothing OTHER than gog to
	// show, the group is omitted entirely; it only renders an explicit
	// no-other-servers line when cfg.MCP itself is empty.
	var others []string
	for _, m := range cfg.MCP {
		if m == "gog" {
			continue
		}
		others = append(others, m)
	}
	switch {
	case len(others) > 0:
		// R1-01/finding-#1: gog aside, a configured name is EITHER a local stdio
		// server this host can spawn (probe/exec it — mcpProbeCheck) OR a remote
		// gateway-catalog server (notion/atlassian/… — never locally probed or
		// exec'd; mcpRemoteCheck). localMCPNames (`pi-stack-host mcp --list`) is
		// the SAME source of truth registerServers already uses to decide how to
		// register a name; reusing it here means doctor's classification can never
		// drift from what registration actually does. When the classification
		// itself can't be established, doctor must not guess either way.
		localSet, localKnown := localMCPNames(env, env.hostBinary)
		mcp := group{title: "Other MCP servers (local stdio, run by the sbx gateway)"}
		for _, m := range others {
			switch classifyMCP(m, localSet, localKnown) {
			case mcpClassLocal:
				mcp.checks = append(mcp.checks, mcpProbeCheck(env, m, mcpOut, mcpOK, sbxOK))
			case mcpClassRemote:
				mcp.checks = append(mcp.checks, mcpRemoteCheck(env, m, mcpOut, mcpOK, sbxOK))
			default: // mcpClassUnknown
				mcp.checks = append(mcp.checks, mcpUnknownClassificationCheck(m))
			}
		}
		r.groups = append(r.groups, mcp)
	case len(cfg.MCP) == 0:
		mcp := group{title: "Other MCP servers (local stdio, run by the sbx gateway)"}
		mcp.checks = append(mcp.checks, check{
			label:       "other servers",
			detail:      "no other MCP servers configured; add one with `pi-stack config set mcp <server>`",
			requirement: RequirementUnconfiguredOptional,
			evidence:    EvidenceNotConfigured,
		})
		r.groups = append(r.groups, mcp)
		// else: only gog is configured — it already has its own group, so this
		// section is omitted rather than rendering a misleading empty one.
	}

	return r
}

// secretCheck reports whether a provider secret is set. When sbx couldn't be
// probed its evidence is unverifiable, not failed, so req=core still never
// blocks in that case (only a CONFIRMED absence does; see AC-05).
//
// finding #4: a req=Core secret missing is a verified failure (todo, ✗) --
// but an OPTIONAL one (either flavor; e.g. github) missing is expected/not-yet
// -opted-in, never a verified failure, so it must not add a repair TODO or
// count as outstanding (mirrors modelProviderKeyCheck's per-key treatment).
func secretCheck(label, key, sbxOut string, sbxOK, sbxPresent bool, req Requirement) check {
	cmd := "sbx secret set -g " + key
	if !sbxOK {
		return check{label: label, detail: sbxUnverifiableDetail(sbxPresent),
			requirement: req, evidence: EvidenceUnverifiable}
	}
	if grepWord(sbxOut, key) {
		return check{label: label, detail: "set", requirement: req, evidence: EvidenceHealthy}
	}
	if req != RequirementCore {
		return check{label: label, detail: "not set (optional)",
			requirement: req, evidence: EvidenceNotConfigured}
	}
	return check{label: label, detail: "not set", todo: cmd,
		requirement: req, evidence: EvidenceFailed}
}

// modelProviderKeyCheck reports ONE model-provider key (anthropic/openai/
// google). R1-04: an individually-missing key is NOT a failure — the runtime
// needs any one of the three, so a single absent provider is merely
// not-configured; the core verdict lives on modelProviderAggregateCheck.
func modelProviderKeyCheck(key, sbxOut string, sbxOK, sbxPresent bool) check {
	if !sbxOK {
		return check{label: key, detail: sbxUnverifiableDetail(sbxPresent),
			requirement: RequirementConfiguredOptional, evidence: EvidenceUnverifiable}
	}
	if grepWord(sbxOut, key) {
		return check{label: key, detail: "set", requirement: RequirementConfiguredOptional, evidence: EvidenceHealthy}
	}
	return check{label: key, detail: "not set (optional; any one model-provider key is enough)",
		requirement: RequirementUnconfiguredOptional, evidence: EvidenceNotConfigured}
}

// modelProviderAggregateCheck is the CORE runtime readiness for model keys
// (R1-04): at least one of anthropic/openai/google must be present. Zero of
// three, verified via sbx, is the one provider condition that blocks doctor.
func modelProviderAggregateCheck(present int, sbxOK, sbxPresent bool) check {
	const label = "model keys"
	if !sbxOK {
		return check{label: label, detail: sbxUnverifiableDetail(sbxPresent),
			requirement: RequirementCore, evidence: EvidenceUnverifiable}
	}
	if present > 0 {
		return check{label: label,
			detail:      fmt.Sprintf("%d of 3 set: at least one of anthropic/openai/google is required", present),
			requirement: RequirementCore, evidence: EvidenceHealthy}
	}
	return check{label: label,
		// DX-3: the todo itself stays a BARE, copy-pasteable command (no trailing
		// parenthetical) — the any-one-of-three alternative + caveat live in
		// detail instead, where they read as context rather than breaking a
		// straight paste of the fix-it line.
		detail:      "NONE of anthropic/openai/google set: at least one model-provider key is required (any one of the three is enough; e.g. `sbx secret set -g openai` or `-g google` work just as well)",
		todo:        "sbx secret set -g anthropic",
		requirement: RequirementCore, evidence: EvidenceFailed}
}

// modelCheck renders a shared ModelReadiness (modelreadiness.go) as a doctor
// check. healthy/not-configured/failed keep the EXACT wording doctor has
// always used (U1 regression guard); unverifiable (ollama installed but
// `ollama list` itself could not be confirmed) is new: it renders as stateWarn
// with NO todo, same convention as every other unverifiable check (AC-01) —
// doctor must never claim a confirmed "not pulled" it didn't actually see.
func modelCheck(m ModelReadiness) check {
	label := "  " + m.Role
	detail := m.Purpose + " [" + m.Model + "]"
	switch m.Evidence {
	case EvidenceHealthy:
		return check{label: label, detail: "pulled: " + detail, requirement: m.Requirement, evidence: m.Evidence}
	case EvidenceNotConfigured:
		return check{label: label, detail: detail + "; needs ollama (then: " + m.PullCmd + ")",
			requirement: m.Requirement, evidence: m.Evidence}
	case EvidenceUnverifiable:
		return check{label: label, detail: detail + "; could not verify (ollama list unavailable)",
			requirement: m.Requirement, evidence: m.Evidence}
	default: // EvidenceFailed
		return check{label: label, detail: detail + "; not pulled", todo: m.PullCmd,
			requirement: m.Requirement, evidence: m.Evidence}
	}
}

// serviceCheck reports a host service's port state. A down service that is in
// the configured SERVICES set gets a `pi-stack serve` TODO; one that isn't
// enabled is merely informational. Never core: a down/unconfigured host
// service is a degraded-but-fine state, not something that blocks doctor's
// exit code.
func serviceCheck(label string, port int, up bool, startCmd string, isEnabled bool) check {
	req := RequirementUnconfiguredOptional
	if isEnabled {
		req = RequirementConfiguredOptional
	}
	if up {
		return check{label: label, detail: fmt.Sprintf(":%d up", port), requirement: req, evidence: EvidenceHealthy}
	}
	if isEnabled {
		return check{label: label, detail: fmt.Sprintf(":%d down", port), todo: startCmd,
			requirement: req, evidence: EvidenceFailed}
	}
	return check{label: label, detail: fmt.Sprintf(":%d down (not in configured services)", port),
		requirement: req, evidence: EvidenceNotConfigured}
}

// memCaptureCheck asks the running memory daemon (:11435) whether automatic fact
// capture is live. It reads the daemon's own health.capture flag (which re-probes
// the watcher model), so it catches the latched-off case a plain `ollama list`
// check misses. Off => the exact `ollama pull` fix.
func memCaptureCheck() check {
	// Fact capture is a configured-optional feature of an already-running (and
	// therefore configured-optional) memory service — never core.
	const captureReq = RequirementConfiguredOptional
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"health","params":{}}`)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:11435", bytes.NewReader(body))
	if err != nil {
		return check{label: "fact capture", detail: "could not query daemon health",
			requirement: captureReq, evidence: EvidenceUnverifiable}
	}
	httpReq.Header.Set("content-type", "application/json")
	res, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return check{label: "fact capture", detail: "could not query daemon health",
			requirement: captureReq, evidence: EvidenceUnverifiable}
	}
	defer res.Body.Close()
	var parsed struct {
		Result struct {
			Capture       bool   `json:"capture"`
			CaptureReason string `json:"captureReason"`
			WatcherModel  string `json:"watcherModel"`
		} `json:"result"`
	}
	if json.NewDecoder(io.LimitReader(res.Body, 1<<16)).Decode(&parsed) != nil {
		return check{label: "fact capture", detail: "could not read daemon health",
			requirement: captureReq, evidence: EvidenceUnverifiable}
	}
	m := parsed.Result.WatcherModel
	if parsed.Result.Capture {
		return check{label: "fact capture", detail: fmt.Sprintf("on (watcher %s)", m),
			requirement: captureReq, evidence: EvidenceHealthy}
	}
	// Prefer the daemon's own live reason (e.g. a watcher inference timeout while
	// Ollama is wedged) over the generic "unavailable" text — that's the whole
	// point of surfacing captureReason.
	detail := fmt.Sprintf("OFF: watcher %q unavailable (recall still works)", m)
	if parsed.Result.CaptureReason != "" {
		detail = fmt.Sprintf("OFF: %s (recall still works)", parsed.Result.CaptureReason)
	}
	return check{
		label:       "fact capture",
		detail:      detail,
		todo:        "ollama pull " + m,
		requirement: captureReq,
		evidence:    EvidenceFailed,
	}
}

// mcpCheck reports whether an MCP server is registered with sbx. When the
// sandbox — a register-on-the-host TODO) from sbx being PRESENT but the listing
// having failed (host, sbx daemon/gateway likely unhealthy — a check-the-daemon TODO).
func mcpCheck(name, mcpOut string, mcpOK, sbxPresent bool, req Requirement) check {
	cmd := "pi-stack mcp register"
	if !mcpOK {
		if sbxPresent {
			return check{label: name, detail: gatewayDownDetail,
				requirement: req, evidence: EvidenceUnverifiable}
		}
		return check{label: name, detail: "sbx unavailable here (register on the host: pi-stack mcp register)",
			requirement: req, evidence: EvidenceUnverifiable}
	}
	if grepWord(mcpOut, name) {
		return check{label: name, detail: "registered", requirement: req, evidence: EvidenceHealthy}
	}
	return check{label: name, detail: "not registered", todo: cmd, requirement: req, evidence: EvidenceFailed}
}

// mcpProbeCheck is the HONEST, generalized MCP check: for every configured local
// stdio server (slack, an overlay `pio`/`fastmail`, …), not just gog, it reports
// registered -> spawns -> returns N tools. It reads the command sbx ACTUALLY
// registered for <name> and probes THAT (the same honest path the gog group
// uses), so a pass proves the real gateway spawn, not a config reconstruction.
// It degrades cleanly: sbx absent -> a register TODO; registered but no readable
// command / no --list-tools support -> a confirmed "registered" without the tool
// count (never a false TODO); registered but 0 tools -> a TODO naming the
// headless-creds fix (the same trap the gog headless-spawn check catches).
func mcpProbeCheck(env shellEnv, name, mcpOut string, mcpOK, sbxPresent bool) check {
	// Every server this is called for came out of cfg.MCP — it was explicitly
	// configured, so it is always configured-optional, never unconfigured.
	const req = RequirementConfiguredOptional
	cmd := "pi-stack mcp register"
	if !mcpOK {
		if sbxPresent {
			return check{label: name, detail: gatewayDownDetail,
				requirement: req, evidence: EvidenceUnverifiable}
		}
		return check{label: name, detail: "sbx unavailable here (register on the host: pi-stack mcp register)",
			requirement: req, evidence: EvidenceUnverifiable}
	}
	if !grepWord(mcpOut, name) {
		return check{label: name, detail: "not registered", todo: cmd, requirement: req, evidence: EvidenceFailed}
	}
	// Registered — try the honest headless probe of the registered command.
	argv, ok := registeredMCPCommand(env, name)
	if !ok {
		return check{label: name, detail: "registered (tool probe unavailable: couldn't read the registered command)",
			requirement: req, evidence: EvidenceUnverifiable}
	}
	// SAFETY (R1-01): only exec a command whose shape AND executables we trust.
	// sbx will hand us whatever argv someone registered for <name>; doctor must
	// not blindly run it. If it is not the known gog form or a canonical
	// `pi-stack-host mcp <name>` spawn (with any op wrapper's binary matching
	// env.lookPath("op")), skip the probe — registration seen, health unverifiable.
	// R2-01: what gets exec'd is the NORMALIZED argv recognizedMCPArgv returns
	// (executable tokens replaced with the resolvers' canonical paths), never
	// the registered spelling.
	trustedArgv, ok := recognizedMCPArgv(env, argv, name)
	if !ok {
		return check{label: name, detail: "registered (probe skipped: unrecognized/untrusted command, never executed)",
			requirement: req, evidence: EvidenceUnverifiable}
	}
	res := probeListTools(env, trustedArgv)
	switch res.status {
	case probeToolsOK:
		return check{label: name, detail: fmt.Sprintf("registered, spawns %s", plural(res.tools, "tool")), requirement: req, evidence: EvidenceHealthy}
	case probeNoTools:
		return check{label: name,
			detail:      "registered but the spawned command returns 0 tools; headless creds/keyring",
			todo:        "review the registered command: sbx mcp get " + name,
			requirement: req, evidence: EvidenceFailed}
	case probeTimedOut:
		return check{label: name, detail: "registered but the tool probe " + res.detail + "; could not verify",
			requirement: req, evidence: EvidenceUnverifiable}
	default: // probeError
		return check{label: name, detail: "registered but the tool probe " + res.detail + "; could not verify",
			requirement: req, evidence: EvidenceUnverifiable}
	}
}

// mcpUnknownClassificationCheck is the finding-#1 honest degrade: when
// localMCPNames itself could not be established, doctor genuinely does not
// know whether name is a local stdio server or a remote gateway-catalog one.
// It must not guess — guessing local risks probing/exec'ing a command that
// doesn't exist as a local subcommand; guessing remote risks recommending
// `pi-stack mcp bundle` for something that was actually meant to be a local
// server (or vice versa). So this renders unverifiable with NO todo at all:
// there is no repair command that is safe to recommend without knowing which
// kind of server this is.
func mcpUnknownClassificationCheck(name string) check {
	return check{
		label: name,
		detail: "could not determine whether this is a local stdio server or a remote " +
			"gateway-catalog server (pi-stack-host mcp --list unavailable); no repair command " +
			"can be safely recommended: build/resolve pi-stack-host, then re-run",
		requirement: RequirementConfiguredOptional,
		evidence:    EvidenceUnverifiable,
	}
}

// mcpRemoteCheck is the HONEST check for a CONFIRMED remote gateway-catalog
// MCP server (notion/atlassian/granola/…): unlike mcpProbeCheck it NEVER
// reads or execs a local command for these — there is no local spawn to
// probe, and doing so (or recommending `pi-stack mcp register`, which only
// knows local stdio servers) would be actively wrong. It verifies
// registration from the already-fetched `sbx mcp ls` output (mcpOut/mcpOK,
// same as mcpProbeCheck), then boundedly inspects native auth status via
// `sbx mcp auth status <name>` — the exact syntax runMcpAuth already forwards
// verbatim (mcp.go). Missing registration recommends `pi-stack mcp bundle`
// (the remote-catalog registration path); an auth failure recommends
// `pi-stack mcp auth <name>`.
func mcpRemoteCheck(env shellEnv, name, mcpOut string, mcpOK, sbxPresent bool) check {
	const req = RequirementConfiguredOptional
	if !mcpOK {
		if sbxPresent {
			return check{label: name, detail: gatewayDownDetail,
				requirement: req, evidence: EvidenceUnverifiable}
		}
		return check{label: name, detail: "sbx unavailable here (register on the host: pi-stack mcp bundle)",
			requirement: req, evidence: EvidenceUnverifiable}
	}
	if !grepWord(mcpOut, name) {
		return check{label: name, detail: "not registered (remote gateway-catalog server)",
			todo: "pi-stack mcp bundle", requirement: req, evidence: EvidenceFailed}
	}
	// Registered — bounded native auth-status probe. R2-02: goes through
	// probeRun, never a raw exec, so a slow/hung control-plane round trip
	// degrades to unverifiable rather than wedging doctor.
	out, timedOut, err := probeRun(env, "sbx", "mcp", "auth", "status", name)
	if timedOut || err != nil {
		return check{label: name,
			detail:      "registered; auth status could not be verified (sbx mcp auth status " + name + ")",
			requirement: req, evidence: EvidenceUnverifiable}
	}
	switch mcpAuthStatus(out) {
	case mcpAuthOK:
		return check{label: name, detail: "registered, authenticated", requirement: req, evidence: EvidenceHealthy}
	case mcpAuthFailed:
		return check{label: name, detail: "registered but not authenticated",
			todo: "pi-stack mcp auth " + name, requirement: req, evidence: EvidenceFailed}
	default: // mcpAuthUnknown
		return check{label: name,
			detail:      "registered; auth status unclear (sbx mcp auth status " + name + "); could not verify",
			requirement: req, evidence: EvidenceUnverifiable}
	}
}

// registeredMCPCommand is the generalized sibling of registeredGogCommand: it
// asks sbx for the command ACTUALLY registered for <name>, so doctor can probe
// the real registration for any local stdio server. It tries `sbx mcp get
// <name>` then `sbx mcp ls -o json`, returning the parsed argv. Unlike the gog
// parsers it applies no gog-specific completeness bar — any non-empty,
// unambiguous (unquoted) command counts. Returns (nil,false) when sbx is absent
// or exposes no command; the caller then reports "registered" without a tool
// count rather than a false TODO.
func registeredMCPCommand(env shellEnv, name string) ([]string, bool) {
	if env.lookPath == nil {
		return nil, false
	}
	if _, err := env.lookPath("sbx"); err != nil {
		return nil, false
	}
	// R2-02: BOUNDED — a hung `sbx mcp get`/`sbx mcp ls -o json` degrades to
	// "couldn't read the registered command", never a wedged doctor.
	if out, timedOut, err := probeRun(env, "sbx", "mcp", "get", name); err == nil && !timedOut {
		if argv, ok := parseMCPCommandLine(out); ok {
			return argv, true
		}
	}
	if out, timedOut, err := probeRun(env, "sbx", "mcp", "ls", "-o", "json"); err == nil && !timedOut {
		if argv, ok := parseMCPCommandJSON(out, name); ok {
			return argv, true
		}
	}
	return nil, false
}

// parseMCPCommandLine extracts a registered argv from a `sbx mcp get <name>`
// text dump: the `command:` line split into fields. A shell-quoted line (which
// strings.Fields cannot split reliably) or an empty command returns (nil,false)
// so registeredMCPCommand falls through to the structured JSON parser.
func parseMCPCommandLine(out string) ([]string, bool) {
	m := gogCommandLineRe.FindStringSubmatch(out)
	if len(m) < 2 {
		return nil, false
	}
	cmd := strings.TrimSpace(m[1])
	if cmd == "" || strings.ContainsAny(cmd, "\"'") {
		return nil, false
	}
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return nil, false
	}
	return fields, true
}

// parseMCPCommandJSON extracts the registered argv for <name> from `sbx mcp ls
// -o json` (an array of {name, command, args}). Returns (nil,false) when there
// is no matching entry or the JSON doesn't parse.
func parseMCPCommandJSON(out, name string) ([]string, bool) {
	var servers []struct {
		Name    string   `json:"name"`
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	if err := json.Unmarshal([]byte(out), &servers); err != nil {
		return nil, false
	}
	for _, s := range servers {
		if s.Name != name || strings.TrimSpace(s.Command) == "" {
			continue
		}
		return append([]string{s.Command}, s.Args...), true
	}
	return nil, false
}

// recognizedMCPArgv reports whether argv is a shape doctor TRUSTS to exec as a
// probe: either a TRUSTED gog spawn (canonical gog/op executables — R1-01), or
// (optionally wrapped in `op run … -- …`, with the op binary itself canonical)
// an ABSOLUTE path equal to the canonical `pi-stack-host` followed by
// `mcp <name>` — exactly how mcp.go registers a local stdio server. Anything
// else is an arbitrary command someone put in the registration, which doctor
// must NOT run. On success it returns the NORMALIZED argv (R2-01): every
// executable token replaced with the resolver's canonical path, so the caller
// execs the TRUSTED tokens, never the registered spelling — there is no
// check-then-exec window on a path an attacker controls.
func recognizedMCPArgv(env shellEnv, argv []string, name string) ([]string, bool) {
	if norm, ok := trustedGogSpawn(env, argv); ok {
		return norm, true
	}
	// Unwrap ONLY a trusted `op run … -- <cmd…>` prefix. A `--` behind any other
	// argv[0] is rejected: the probe execs the wrapper token, so unwrapping a
	// prefix like `/tmp/evil -- pi-stack-host mcp slack` would exec /tmp/evil.
	cmd, ok := unwrapOpRun(argv)
	if !ok {
		return nil, false
	}
	norm := append([]string(nil), argv...)
	innerStart := len(argv) - len(cmd)
	if innerStart > 0 {
		// R1-01: an op-wrapped command must run the SAME op binary env.lookPath
		// finds — a look-alike `/tmp/op` is never executed.
		opTok, opOK := trustedExecPath(env, argv[0], "op")
		if !opOK {
			return nil, false
		}
		norm[0] = opTok
	}
	if len(cmd) < 3 {
		return nil, false
	}
	if cmd[1] != "mcp" || cmd[2] != name {
		return nil, false
	}
	// R2-01: basename alone ("pi-stack-host") is NOT enough — an absolute path
	// anywhere on disk with that basename (e.g. /tmp/malicious/pi-stack-host)
	// satisfied the old check. Require the CANONICAL binary registration
	// actually uses, and exec THAT token.
	hostTok, hostOK := trustedHostBinaryExecPath(env, cmd[0])
	if !hostOK {
		return nil, false
	}
	norm[innerStart] = hostTok
	return norm, true
}

// trustedHostBinaryExecPath is the R2-01 canonical-pi-stack-host gate: mcp.go
// registration (registerServers/serverCmd) ALWAYS spawns the ABSOLUTE path
// hostBinaryResolver (findHostBinary) resolves — sibling-to-launcher first,
// PATH fallback — never a bare name. Trusting an absolute path's basename
// alone let a malicious `/tmp/malicious/pi-stack-host mcp slack` registration
// pass. env.hostBinary is the injected/hermetic trust seam mirroring
// hostBinaryResolver so this compares against the SAME canonical answer the
// real registration used, without doctor ever re-deriving install-path logic
// of its own (a second, drifting implementation would be its own bug class).
// tok must be absolute AND byte-equal (cleaned) to the resolved binary —
// STRICT equality only. Symlink resolution is deliberately NOT consulted
// (R2-01 round 2b): blessing an alternate symlink path at check time and
// exec'ing it afterwards is a check-then-exec race an attacker wins by
// swapping the link between the two. On success it returns the RESOLVER's
// canonical token — the only thing the caller may exec. An unresolvable
// canonical answer (env.hostBinary nil or erroring) fails CLOSED: never fall
// back to trusting basename alone.
func trustedHostBinaryExecPath(env shellEnv, tok string) (string, bool) {
	if filepath.Base(tok) != "pi-stack-host" {
		return "", false
	}
	if !filepath.IsAbs(tok) {
		return "", false // never trust a bare/relative name for pi-stack-host
	}
	if env.hostBinary == nil {
		return "", false
	}
	canonical, err := env.hostBinary()
	if err != nil || canonical == "" || !filepath.IsAbs(canonical) {
		return "", false
	}
	if filepath.Clean(tok) != filepath.Clean(canonical) {
		return "", false
	}
	return filepath.Clean(canonical), true
}

// trustedExecPath is the R1-01/R2-01 canonical-executable gate: it returns
// the exec token doctor may run for base, and whether the registered token is
// trusted. A bare name (no path separator) is trusted as-is — exec resolves
// it through PATH at spawn time, which IS lookPath's answer; there is no
// recorded path for an attacker to swap. A path-carrying token must be
// byte-equal (cleaned) to the PATH-resolved binary — STRICT equality only,
// with symlink resolution deliberately NOT consulted (R2-01 round 2b: a
// check-time symlink bless followed by exec of the registered path is a race
// the attacker wins by swapping the link). On success the returned token is
// the RESOLVER's canonical path, never the registered spelling, so the exec'd
// token is the trusted one by construction. Anything else (a look-alike
// /tmp/gog, a fake op) is untrusted and never executed.
func trustedExecPath(env shellEnv, tok, base string) (string, bool) {
	if filepath.Base(tok) != base {
		return "", false
	}
	if !strings.ContainsAny(tok, `/\`) {
		return tok, true // bare name: exec resolves via PATH = lookPath's answer
	}
	if env.lookPath == nil {
		return "", false
	}
	canonical, err := env.lookPath(base)
	if err != nil || canonical == "" {
		return "", false
	}
	if filepath.Clean(tok) != filepath.Clean(canonical) {
		return "", false
	}
	return filepath.Clean(canonical), true
}

// trustedGogSpawn reports whether a registered gog command is BOTH the
// recognized gog shape (gogSpawnArgv) AND built from canonical executables
// (R1-01): the inner gog binary must match env.lookPath("gog"), and — when
// op-wrapped — the op binary must match env.lookPath("op"). On success it
// returns the NORMALIZED argv (R2-01): the gog/op executable tokens replaced
// with the resolvers' canonical paths, so the caller execs the TRUSTED
// tokens, never the registered spelling. Only that normalized spawn is ever
// executed as a probe.
func trustedGogSpawn(env shellEnv, argv []string) ([]string, bool) {
	inner, ok := gogSpawnArgv(argv)
	if !ok {
		return nil, false
	}
	gogTok, gogOK := trustedExecPath(env, inner[0], "gog")
	if !gogOK {
		return nil, false
	}
	norm := append([]string(nil), argv...)
	// inner is the suffix gogSpawnArgv/unwrapOpRun peeled off argv, so the
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

// unwrapOpRun returns the effective command doctor would trust to exec. With no
// `--` it is argv itself (a bare command). With a `--`, it unwraps the prefix
// ONLY when argv[0] has basename `op` (the real command runs via op; the
// caller still verifies the op token against lookPath and execs the canonical
// path — R1-01/R2-01); a `--` behind any other argv[0] returns ok=false so a
// hostile prefix is never exec'd.
func unwrapOpRun(argv []string) ([]string, bool) {
	if len(argv) == 0 {
		return nil, false
	}
	sep := -1
	for i, a := range argv {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 0 {
		return argv, true
	}
	// Only a `op run … -- <cmd>` wrapper is trusted to be unwrapped: the probe
	// execs the wrapper token, so a `--` behind a FOREIGN argv[0] (e.g.
	// `/tmp/evil -- pi-stack-host mcp slack`) would run /tmp/evil. Requiring
	// basename "op" blocks that. (Residual, accepted: a registration whose argv[0]
	// is a binary literally named `op` on the exec path would pass — but that
	// presupposes an attacker who can already write arbitrary sbx registrations,
	// i.e. owns the gateway, which is outside doctor's threat model.)
	if filepath.Base(argv[0]) != "op" {
		return nil, false
	}
	return argv[sep+1:], true
}

// probeStatus/probeResult are the STRUCTURED outcome of a `--list-tools` probe
// (R1-07): a clean non-empty list is healthy; a clean EMPTY list is a verified
// zero-tools failure (the headless creds/keyring trap); a timeout or exec
// error is unverifiable — doctor doesn't know, so it must never mislabel those
// as a keyring failure.
type probeStatus int

const (
	probeToolsOK  probeStatus = iota // clean exit, non-empty tool list
	probeNoTools                     // clean exit, ZERO tools — a verified failure
	probeTimedOut                    // hit the probe deadline — unverifiable
	probeError                       // exec failure / non-zero exit / missing binary — unverifiable
)

type probeResult struct {
	status probeStatus
	detail string // short, value-free diagnostic for timeout/error outcomes
	tools  int    // tool-line count on a clean exit
}

// probeListTools runs argv with `--list-tools` appended, BOUNDED by probeRun's
// timeout + output cap, and classifies the outcome. The diagnostic is
// deliberately generic (never raw error text), so a registered command's
// tokens — which may carry pasted secrets — can never leak through an error
// message.
func probeListTools(env shellEnv, argv []string) probeResult {
	if len(argv) == 0 {
		return probeResult{status: probeError, detail: "has no command to run"}
	}
	full := append(append([]string{}, argv...), "--list-tools")
	out, timedOut, err := probeRun(env, full[0], full[1:]...)
	if timedOut {
		return probeResult{status: probeTimedOut, detail: fmt.Sprintf("timed out after %s", probeTimeout)}
	}
	if err != nil {
		return probeResult{status: probeError, detail: classifyProbeErr(err)}
	}
	n := 0
	for _, ln := range strings.Split(out, "\n") {
		if strings.TrimSpace(ln) != "" {
			n++
		}
	}
	if n == 0 {
		return probeResult{status: probeNoTools}
	}
	return probeResult{status: probeToolsOK, tools: n}
}

// classifyProbeErr maps a probe error to a short, value-free diagnostic: it
// distinguishes a missing/non-executable binary from a non-zero exit without
// ever echoing raw error text (which could embed registered-command tokens).
func classifyProbeErr(err error) string {
	var xe *exec.Error
	if errors.As(err, &xe) {
		return "could not run (binary not found or not executable)"
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Sprintf("exited non-zero (%s)", ee.ProcessState)
	}
	return "could not be run"
}

// enabled reports whether a service name is in the configured SERVICES set.
func enabled(cfg *config.Config, name string) bool {
	for _, s := range cfg.Services {
		if s == name {
			return true
		}
	}
	return false
}

// mcpConfigured reports whether name is in the configured MCP set. Attach mode
// is eager-vs-lazy, not implied by set membership: a configured server is
// DYNAMIC (discovered/called on demand via mcp-find/mcp-exec) by default, and
// only EAGER (its tools loaded into context at sandbox create) when pinned via
// mcp_static (resolveStaticMCP) — see gogAttachCheck and AGENTS.md's MCP notes.
func mcpConfigured(cfg *config.Config, name string) bool {
	for _, m := range cfg.MCP {
		if m == name {
			return true
		}
	}
	return false
}

// gogAccount resolves the Google Workspace account the best-effort fallback
// probe runs against. config.toml's `gog_account` is the SINGLE source of truth
// (it is what `make mcp-register` / `pi-stack mcp register` hand the gateway,
// both sourced via `pi-stack config get gog_account`):
//  1. config.toml's `gog_account` (cfg.GogAccount, profile-resolved),
//  2. the $GOG_ACCOUNT env var.
//
// NEVER a hardcoded address. Empty means "not configured" and the caller emits a
// "cannot verify" TODO rather than reporting green.
func gogAccount(cfg *config.Config, env shellEnv) string {
	if cfg != nil {
		if a := strings.TrimSpace(cfg.GogAccount); a != "" {
			return a
		}
	}
	if env.getenv != nil {
		if a := strings.TrimSpace(env.getenv("GOG_ACCOUNT")); a != "" {
			return a
		}
	}
	return ""
}

// findUpward walks up from the current working directory looking for a directory
// that contains BOTH a Makefile and the given repo-relative file, returning the
// absolute path to that file (or "" if none is found before the filesystem root).
// This is how doctor locates a repo checkout's config files (op-refs.env)
// regardless of where it was invoked from within the tree.
func findUpward(env shellEnv, rel string) string {
	if env.statFile == nil {
		return ""
	}
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if env.statFile(filepath.Join(dir, "Makefile")) && env.statFile(filepath.Join(dir, rel)) {
			return filepath.Join(dir, rel)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// resolveOpRefs resolves config/op-refs.env to an ABSOLUTE, canonical location
// so doctor's headless probe matches the gateway registration exactly (`make
// mcp-register` registers the gog spawn with an absolute --env-file; a relative
// one here would resolve against doctor's cwd and could probe a different file
// than the gateway actually uses). It searches, in order, and returns the FIRST
// that exists:
//  1. $PI_STACK_CONFIG's directory + op-refs.env,
//  2. a repo checkout's config/op-refs.env (walk up for Makefile + that file),
//  3. ~/.config/pi-stack/op-refs.env.
//
// Returns "" when none exists, so the caller reports "cannot verify" rather than
// probing (and blessing) a file the gateway never uses.
func resolveOpRefs(env shellEnv) string {
	// abs makes every resolved path ABSOLUTE regardless of doctor's cwd: a
	// relative $PI_STACK_CONFIG (e.g. `config/config.toml`) would otherwise yield
	// a cwd-relative op-refs path that need not match the gateway's --env-file.
	abs := func(p string) string {
		if a, err := filepath.Abs(p); err == nil {
			return a
		}
		return p
	}
	if env.getenv != nil {
		if p := env.getenv("PI_STACK_CONFIG"); p != "" {
			cand := filepath.Join(filepath.Dir(p), "op-refs.env")
			if env.statFile != nil && env.statFile(cand) {
				return abs(cand)
			}
		}
	}
	if p := findUpward(env, filepath.Join("config", "op-refs.env")); p != "" {
		return abs(p)
	}
	if env.homeDir != nil && env.statFile != nil {
		if home := env.homeDir(); home != "" {
			cand := filepath.Join(home, ".config", "pi-stack", "op-refs.env")
			if env.statFile(cand) {
				return abs(cand)
			}
		}
	}
	return ""
}

// gogHeadlessProbe runs the gateway-EQUIVALENT probe — list gog's tools the
// EXACT way the sbx gateway spawns it: the same hardened invocation +
// `op run --no-masking --env-file=config/op-refs.env` wrapper mcp-register
// actually registers (gogRegisteredArgv), never a lighter reconstruction — and
// returns the STRUCTURED outcome (R1-07). This is the only check that proves
// the real path; `gog auth doctor` in a logged-in shell passes and lies. op is
// OPTIONAL (gog authenticates via OAuth; op-refs only supplies a headless
// keyring password), so a missing op or empty opRefs falls back to a bare
// (still hardened) probe, mirroring registerServers' opReady gate — op and
// op-refs are only ever used together. Degrades cleanly (probeError, never a
// crash) when gog/account are absent.
func gogHeadlessProbe(env shellEnv, acct, opRefs string) probeResult {
	if acct == "" {
		return probeResult{status: probeError, detail: "could not run (account unresolved)"}
	}
	gogPath, err := env.lookPath("gog")
	if err != nil {
		return probeResult{status: probeError, detail: "could not run (gog not found)"}
	}
	opPath, opErr := env.lookPath("op")
	if opErr != nil || opRefs == "" {
		opPath, opRefs = "", ""
	}
	return probeListTools(env, gogRegisteredArgv(gogPath, opPath, opRefs, acct))
}

// gogHeadlessOK is gogHeadlessProbe collapsed to a bool for callers that only
// need pass/fail (gog setup's verification, setup's follow-up gate).
func gogHeadlessOK(env shellEnv, acct, opRefs string) bool {
	return gogHeadlessProbe(env, acct, opRefs).status == probeToolsOK
}

// gogGroup builds the gog check cluster. The HONEST path reads the ACTUAL
// command the sbx gateway registered for gog and probes THAT (so it verifies
// the registered account, op-refs path, and op/gog binaries as-registered). Only
// when sbx is absent (or exposes no command) does it fall back to a best-effort
// reconstruction from config — clearly labeled, and never a confirmed green.
// Every probe degrades to a TODO rather than crashing, so this runs cleanly
// in-sandbox (gog/sbx/op all absent).
func gogGroup(cfg *config.Config, env shellEnv, mcpOut string, mcpOK, sbxPresent bool) group {
	g := group{title: "gog (Google Workspace via host MCP, read-only)"}
	// gog is UNCONFIGURED-optional by default (nobody has opted in); it becomes
	// configured-optional once an account is set or it's in the configured MCP
	// set. Either way it is never core, so nothing in this group can block
	// doctor's exit code (AC-05) — but the distinction still matters for the
	// concise summary and JSON output ("Optional absent must not count as an
	// outstanding failure").
	req := gogRequirement(cfg, env)

	// HONEST PATH: probe the command sbx ACTUALLY registered for gog. This is the
	// only check that proves the real registration — account, op-refs path, and
	// op/gog binaries all exactly as the gateway will spawn them.
	if argv, ok := registeredGogCommand(env); ok {
		g.checks = append(g.checks, check{label: "registration", note: true,
			detail:      "probing the sbx-registered command: " + redactRegisteredCommand(argv),
			requirement: req, evidence: EvidenceUnverifiable})
		// R1-01: NEVER exec a registered command whose gog/op executable is not
		// the canonical PATH-resolved binary — a look-alike /tmp/gog or fake op
		// is skipped (unverifiable), not probed. R2-01: on trust, exec the
		// NORMALIZED argv (canonical executable tokens), never the registered
		// spelling.
		trustedArgv, trusted := trustedGogSpawn(env, argv)
		if !trusted {
			g.checks = append(g.checks, check{label: "headless spawn",
				detail:      "probe skipped: the registered command's gog/op executable does not match the PATH-resolved binary (inspect: sbx mcp get gog); never executed",
				requirement: req, evidence: EvidenceUnverifiable})
			g.checks = append(g.checks, mcpCheck("gog", mcpOut, mcpOK, sbxPresent, req))
			g.checks = append(g.checks, gogAttachCheck(cfg))
			return g
		}
		res := probeListTools(env, trustedArgv)
		switch res.status {
		case probeToolsOK:
			// Distinguish the op-wrapped path (op-refs resolved) from a BARE spawn so a
			// bare green never implies 1Password creds were involved.
			detail := "registered command exposes tools (verified as-registered, via op run)"
			if !gogSpawnIsOpWrapped(argv) {
				detail = "registered command exposes tools (verified as-registered); spawned BARE (no op-refs involved)"
			}
			g.checks = append(g.checks, check{label: "headless spawn",
				detail: detail, requirement: req, evidence: EvidenceHealthy})
		case probeNoTools:
			// A CLEAN empty tool list is the verified keyring/headless-creds trap.
			g.checks = append(g.checks, check{label: "headless spawn",
				detail:      "the registered command returns 0 tools; keyring not headless",
				todo:        "add GOG_KEYRING_BACKEND=file + GOG_KEYRING_PASSWORD + GOG_ACCOUNT + GOG_HOME to " + defaultOpRefsPath(env),
				requirement: req, evidence: EvidenceFailed})
		default:
			// R1-07: a timeout or exec error is NOT a keyring failure — doctor
			// doesn't know, so it says exactly what happened and stays ⚠.
			g.checks = append(g.checks, check{label: "headless spawn",
				detail:      "probe of the registered command " + res.detail + "; could not verify (inspect: sbx mcp get gog)",
				requirement: req, evidence: EvidenceUnverifiable})
		}
		g.checks = append(g.checks, mcpCheck("gog", mcpOut, mcpOK, sbxPresent, req))
		g.checks = append(g.checks, gogAttachCheck(cfg))
		return g
	}

	// 1. gog CLI installed (the reconstruction probe uses it).
	if _, err := env.lookPath("gog"); err != nil {
		g.checks = append(g.checks, check{label: "gog CLI",
			detail: "not found; install: brew install gog", requirement: req, evidence: EvidenceNotConfigured})
		return g
	}
	g.checks = append(g.checks, check{label: "gog CLI", detail: "installed", requirement: req, evidence: EvidenceHealthy})

	acct := gogAccount(cfg, env)
	opRefs := resolveOpRefs(env)

	// FALLBACK / TRANSPARENCY: sbx couldn't tell us the registered command, so we
	// reconstruct the probe from config and LABEL it best-effort — we can verify
	// THIS account/op-refs authenticates, but NOT that it matches what the gateway
	// registered. Name exactly what we're checking so a pass can never silently
	// mean "checked a different account/path than the sbx gateway got".
	acctShown, refsShown := acct, opRefs
	if acctShown == "" {
		acctShown = "<unknown>"
	}
	if refsShown == "" {
		refsShown = "<not found>"
	}
	// The fallback reason depends on sbx presence: if sbx is PRESENT but its
	// registration couldn't be read (host, gateway likely off), say so; only call
	// it "sbx unavailable" when sbx is actually absent (in the sandbox).
	fallbackWhy := "best-effort (sbx unavailable)"
	if sbxPresent {
		fallbackWhy = "best-effort (couldn't read sbx MCP registrations; check the sbx daemon: sbx mcp status)"
	}
	g.checks = append(g.checks,
		check{label: "verifying", note: true,
			detail:      fallbackWhy + "; verifies " + acctShown + " via " + refsShown,
			requirement: req, evidence: EvidenceUnverifiable},
		// AC-03: no more `make mcp-register` language — name the registration
		// itself, not a specific make target that may not even be how it was run.
		check{label: "note", note: true,
			detail:      "must match the sbx-registered gog command (config.toml gog_account + config/op-refs.env)",
			requirement: req, evidence: EvidenceUnverifiable})

	if acct == "" {
		// 2'. No account configured — can't probe auth or the headless path. Not a
		// confirmed failure, just genuinely not set up: NotConfigured, not Failed
		// (so no ✗ and no repair TODO — the setup command lives in the detail).
		g.checks = append(g.checks, check{label: "account",
			detail:      "cannot verify (gog_account unset in config.toml/env); set up: pi-stack gog setup (or: pi-stack config set gog_account <you@example.com>)",
			requirement: req, evidence: EvidenceNotConfigured})
		g.checks = append(g.checks, mcpCheck("gog", mcpOut, mcpOK, sbxPresent, req))
		g.checks = append(g.checks, gogAttachCheck(cfg))
		return g
	}

	if opRefs == "" {
		// Can't run the gateway-equivalent headless probe without op-refs.env. But
		// op-refs is OPTIONAL for gog: it authenticates via OAuth (gog auth login),
		// and only needs op-refs to inject a headless keyring PASSWORD when the
		// gateway can't unlock its keyring otherwise. So this is an info line, not a
		// TODO — and it is self-contained (a gog-only config renders no Secrets
		// group, so we must not point at one).
		g.checks = append(g.checks,
			check{label: "account", detail: acct + " set (unconfirmed vs registration)",
				requirement: req, evidence: EvidenceUnverifiable},
			check{label: "op-refs",
				detail:      "op-refs.env not found; only needed if the gateway can't unlock gog's keyring headlessly",
				requirement: req, evidence: EvidenceNotConfigured})
		g.checks = append(g.checks, mcpCheck("gog", mcpOut, mcpOK, sbxPresent, req))
		g.checks = append(g.checks, gogAttachCheck(cfg))
		return g
	}

	// 2. account authorized (interactive). 3. THE GOTCHA — headless spawn.
	// R1-14: the auth check runs through the BOUNDED probe machinery, so a hung
	// `gog auth doctor --check` can never wedge doctor.
	_, interTimedOut, interErr := probeRun(env, "gog", "--account", acct, "auth", "doctor", "--check")
	_, opErr := env.lookPath("op")
	head := gogHeadlessProbe(env, acct, opRefs)
	switch {
	case interTimedOut:
		// A timed-out auth check is UNVERIFIABLE, not "not authorized" (R1-07/14).
		g.checks = append(g.checks, check{label: "account",
			detail:      acct + "; `gog auth doctor --check` timed out; could not verify",
			requirement: req, evidence: EvidenceUnverifiable})
	case interErr != nil:
		// Auth itself isn't set up — don't double-report the keyring below. Point
		// at the guided command, never the raw legacy `gog auth add-client` +
		// `auth login` recipe (finding #3).
		g.checks = append(g.checks, check{label: "account",
			detail:      acct + " not authorized",
			todo:        "pi-stack gog setup",
			requirement: req, evidence: EvidenceFailed})
	case opErr != nil:
		// Interactive auth OK, but op is absent so we can't run the gateway-
		// equivalent probe. Say so rather than blaming the keyring.
		g.checks = append(g.checks,
			check{label: "account", detail: acct + " authorized (interactive)",
				requirement: req, evidence: EvidenceHealthy},
			check{label: "headless spawn",
				detail:      "can't verify the gateway spawn: op (1Password CLI) not found; install it so doctor can probe the real headless path",
				requirement: req, evidence: EvidenceUnverifiable})
	case head.status == probeNoTools:
		// THE TRAP: interactive passes, the headless gateway spawn gets 0 tools.
		// This IS a verified failure (we ran the exact gateway-equivalent probe and
		// it returned nothing) — just never a core one, so it still can't block.
		g.checks = append(g.checks,
			check{label: "account", detail: acct + " authorized (interactive)",
				requirement: req, evidence: EvidenceHealthy},
			check{label: "headless spawn",
				detail:      "auth OK in your shell but the gateway spawn gets 0 tools; keyring not headless",
				todo:        "add GOG_KEYRING_BACKEND=file + GOG_KEYRING_PASSWORD + GOG_ACCOUNT + GOG_HOME to " + defaultOpRefsPath(env),
				requirement: req, evidence: EvidenceFailed})
	case head.status != probeToolsOK:
		// R1-07: a timeout / exec error is NOT the keyring trap — say what
		// happened and stay unverifiable.
		g.checks = append(g.checks,
			check{label: "account", detail: acct + " authorized (interactive)",
				requirement: req, evidence: EvidenceHealthy},
			check{label: "headless spawn",
				detail:      "headless probe " + head.detail + "; could not verify",
				requirement: req, evidence: EvidenceUnverifiable})
	default:
		// AC-01: best-effort success — this account authenticates headlessly, but
		// we could NOT confirm it is the command the sbx gateway actually
		// registered. That is UNVERIFIABLE, not a failure: doctor genuinely does
		// not know whether it matches the real registration, so this must render
		// as a warning (⚠), never a ✗, and carry no fix-it TODO (there's nothing
		// confirmed broken to fix). Only the honest path above (registered
		// command read + probed) earns a confirmed ✓.
		g.checks = append(g.checks,
			check{label: "account", detail: acct + " authorized (best-effort, unconfirmed vs registration)",
				requirement: req, evidence: EvidenceUnverifiable},
			check{label: "headless spawn",
				detail:      "best-effort headless spawn succeeded, but the sbx-registered command could not be confirmed; unverifiable, not a failure",
				requirement: req, evidence: EvidenceUnverifiable})
	}

	// 4. registered with the gateway. 5. attached on run?
	g.checks = append(g.checks, mcpCheck("gog", mcpOut, mcpOK, sbxPresent, req))
	g.checks = append(g.checks, gogAttachCheck(cfg))
	return g
}

// gogRequirement reports gog's readiness Requirement: configured-optional once
// the user has opted in (an account configured, or gog in the configured MCP
// set), else unconfigured-optional. Never core — gog absence is expected on a
// fresh install.
func gogRequirement(cfg *config.Config, env shellEnv) Requirement {
	if mcpConfigured(cfg, "gog") || gogAccount(cfg, env) != "" {
		return RequirementConfiguredOptional
	}
	return RequirementUnconfiguredOptional
}

// secretsGroup builds the standalone "Secrets (1Password)" cluster. It runs
// whenever ANY op-wrapped host MCP server is configured; with none it stays a
// single green info line ("1Password not needed"). It reports op install +
// sign-in state (op --version / `op account list` — SAFE metadata ONLY, never
// `op read` or a to-disk `op signin`), op-refs.env presence at the absolute XDG
// path, its perms (group/other access -> a chmod finding), and per configured
// ref: filled vs placeholder, plus a refs-only lint that flags a secret-shaped
// literal WITHOUT ever printing its value. op sign-in is advisory (never a
// standalone green); the confirmed "creds actually resolve" proof stays the gog
// group's headless op-run probe.
func secretsGroup(cfg *config.Config, env shellEnv) group {
	g := group{title: "Secrets (1Password, host MCP creds via op-refs.env)"}
	// This whole group only exists once a credentialed server is configured, so
	// every check in it is configured-optional (opted in, never core).
	const req = RequirementConfiguredOptional

	if !anyOpWrappedServer(cfg) {
		g.checks = append(g.checks, check{label: "1Password",
			detail:      "no credentialed host MCP servers configured; 1Password not needed",
			requirement: RequirementUnconfiguredOptional, evidence: EvidenceNotConfigured})
		return g
	}

	// op installed? (advisory sign-in only when installed — never a blocker).
	if opInstalled(env) {
		g.checks = append(g.checks, check{label: "op CLI", detail: "installed", requirement: req, evidence: EvidenceHealthy})
		if opSignedIn(env) {
			g.checks = append(g.checks, check{label: "account configured",
				detail:      "op account list ok (advisory; not a proof of an unlocked session)",
				requirement: req, evidence: EvidenceHealthy})
		} else {
			g.checks = append(g.checks, check{label: "account configured",
				detail:      "no account configured (advisory); run: op signin",
				requirement: req, evidence: EvidenceUnverifiable})
		}
	} else {
		g.checks = append(g.checks, check{label: "op CLI",
			detail: "not installed",
			todo:   "install the 1Password CLI (op): https://developer.1password.com/docs/cli",
			// A credentialed host MCP server IS configured, so a missing op is a
			// confirmed gap for it — a verified failure, not mere absence.
			requirement: req, evidence: EvidenceFailed})
	}

	// op-refs.env present at the absolute XDG path?
	path := defaultOpRefsPath(env)
	content, exists := "", false
	if env.readFile != nil {
		if c, err := env.readFile(path); err == nil {
			content, exists = c, true
		}
	}
	if !exists {
		g.checks = append(g.checks, check{label: "op-refs.env",
			detail: "not present at " + path,
			todo:   "pi-stack secret set <ENV_VAR> op://vault/item/field",
			// Same reasoning as the op CLI above: a configured credentialed
			// server with no op-refs.env is a confirmed gap.
			requirement: req, evidence: EvidenceFailed})
		return g
	}
	g.checks = append(g.checks, check{label: "op-refs.env", detail: path, requirement: req, evidence: EvidenceHealthy})

	// Perms: the file AND its dir must not be group/other-accessible.
	if env.fileMode != nil {
		if m, ok := env.fileMode(path); ok && m.Perm()&0o077 != 0 {
			g.checks = append(g.checks, check{label: "perms",
				detail: fmt.Sprintf("op-refs.env is %04o: group/other accessible", m.Perm()),
				todo:   "chmod 600 " + path, requirement: req, evidence: EvidenceFailed})
		}
		dir := filepath.Dir(path)
		if m, ok := env.fileMode(dir); ok && m.Perm()&0o077 != 0 {
			g.checks = append(g.checks, check{label: "dir perms",
				detail: fmt.Sprintf("%s is %04o: group/other accessible", dir, m.Perm()),
				todo:   "chmod 700 " + dir, requirement: req, evidence: EvidenceFailed})
		}
	}

	// Per-ref: filled vs placeholder, plus the refs-only lint. NEVER print a value.
	for _, rf := range parseOpRefs(content) {
		switch {
		case rf.nonSecret:
			g.checks = append(g.checks, check{label: rf.key, detail: "non-secret env (allowed literal)",
				requirement: req, evidence: EvidenceHealthy})
		case rf.isRef && rf.placeholder:
			g.checks = append(g.checks, check{label: rf.key,
				detail: "unfilled placeholder; set the op:// ref",
				todo:   "pi-stack secret set <ENV_VAR> op://vault/item/field", requirement: req, evidence: EvidenceFailed})
		case rf.isRef:
			g.checks = append(g.checks, check{label: rf.key, detail: "op:// ref filled", requirement: req, evidence: EvidenceHealthy})
		case rf.placeholder:
			// A non-ref value still carrying an unfilled <...> placeholder.
			g.checks = append(g.checks, check{label: rf.key,
				detail: "unfilled placeholder; set the op:// ref",
				todo:   "pi-stack secret set <ENV_VAR> op://vault/item/field", requirement: req, evidence: EvidenceFailed})
		case looksSecretShaped(rf.key, rf.value):
			// MEDIUM finding — a pasted secret. NEVER echo the value.
			g.checks = append(g.checks, check{label: rf.key,
				detail: "possible pasted secret; replace with op://vault/item/field",
				todo:   "pi-stack secret set <ENV_VAR> op://vault/item/field", requirement: req, evidence: EvidenceFailed})
		default:
			// Refs-only policy: ANY other non-ref, non-allowlisted value is flagged.
			// NEVER echo the value.
			g.checks = append(g.checks, check{label: rf.key,
				detail: "not an op:// ref; this file is refs-only; use op://vault/item/field or move it to the non-secret allowlist",
				todo:   "pi-stack secret set <ENV_VAR> op://vault/item/field", requirement: req, evidence: EvidenceFailed})
		}
	}
	return g
}

// redactRegisteredCommand renders a registered MCP argv SAFELY for display: it
// keeps argv[0]'s basename plus recognizable subcommands/flag NAMES (run, mcp,
// gog, op, pi-stack-host, --account, --env-file=…, etc.) and replaces every
// other token — any of which could be a pasted value/secret — with ‹redacted›.
// It NEVER echoes an unrecognized token verbatim.
func redactRegisteredCommand(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	// Bare words + flag NAMES doctor recognizes as non-secret structure. Anything
	// NOT here is treated as a potential value and redacted, so an unrecognized
	// token is never echoed verbatim.
	recognized := map[string]bool{
		// binaries / subcommands
		"run": true, "mcp": true, "gog": true, "op": true, "pi-stack-host": true,
		"slack": true, "auth": true, "doctor": true, "--": true,
		// flag NAMES (their VALUES are still redacted)
		"--list-tools": true, "--account": true, "--env-file": true, "--check": true,
		"--gmail-no-send": true, "--wrap-untrusted": true, "--readonly": true,
		"--allow-tool": true,
	}
	out := make([]string, 0, len(argv))
	for i, tok := range argv {
		if i == 0 {
			out = append(out, filepath.Base(tok))
			continue
		}
		// A --flag=value token: keep the recognized flag NAME, elide the value.
		if strings.HasPrefix(tok, "--") {
			if eq := strings.IndexByte(tok, '='); eq > 0 {
				name := tok[:eq]
				if recognized[name] {
					out = append(out, name+"=…")
					continue
				}
				out = append(out, "‹redacted›")
				continue
			}
		}
		if recognized[tok] {
			out = append(out, tok)
			continue
		}
		out = append(out, "‹redacted›")
	}
	return strings.Join(out, " ")
}

// gogSpawnIsOpWrapped reports whether the registered gog command runs via the
// `op run --env-file=… -- gog … mcp …` wrapper (argv[0] is the op binary) rather
// than a BARE `gog … mcp …` spawn. Used so a bare-spawn green never implies
// op-refs were resolved.
func gogSpawnIsOpWrapped(argv []string) bool {
	return len(argv) > 0 && filepath.Base(argv[0]) == "op"
}

// looksSecretShaped reports whether a NON-ref, non-allowlisted op-refs.env value
// looks like a pasted secret. Thin wrapper over the shared config.LooksSecretShaped
// so doctor's lint and backup's pre-archive warning judge identically.
func looksSecretShaped(key, val string) bool { return config.LooksSecretShaped(key, val) }

// registeredGogCommand asks sbx what command it ACTUALLY registered for the gog
// MCP server, so doctor can probe the real registration instead of a config
// reconstruction that may have drifted from what `make mcp-register` wired up.
// It tries, in order, `sbx mcp get gog`, then `sbx mcp ls -o json`, returning
// the parsed argv. Returns (nil,false) when sbx is absent or exposes no command
// — the caller then falls back to the best-effort reconstruction.
func registeredGogCommand(env shellEnv) ([]string, bool) {
	if env.lookPath == nil {
		return nil, false
	}
	if _, err := env.lookPath("sbx"); err != nil {
		return nil, false
	}
	// R2-02: BOUNDED — a hung `sbx mcp get`/`sbx mcp ls -o json` falls back to
	// the best-effort reconstruction, never a wedged doctor.
	if out, timedOut, err := probeRun(env, "sbx", "mcp", "get", "gog"); err == nil && !timedOut {
		if argv, ok := parseGogCommandLine(out); ok {
			return argv, true
		}
	}
	if out, timedOut, err := probeRun(env, "sbx", "mcp", "ls", "-o", "json"); err == nil && !timedOut {
		if argv, ok := parseGogCommandJSON(out); ok {
			return argv, true
		}
	}
	return nil, false
}

// gogCommandLineRe matches a `command: <full command>` (or `command = ...`) line
// in `sbx mcp get gog` output.
var gogCommandLineRe = regexp.MustCompile(`(?im)^\s*command\s*[:=]\s*(.+?)\s*$`)

// parseGogCommandLine extracts the registered argv from a `sbx mcp get gog`
// text dump: the `command:` line, split into fields. It only accepts an
// UNAMBIGUOUS, COMPLETE command (see gogCommandComplete). A shell-quoted line
// (which strings.Fields cannot split reliably), or a partial capture — just
// `op`, `op run`, or the command line when the args landed on a separate line —
// returns (nil,false) so registeredGogCommand falls through to the structured
// JSON parser rather than probing a truncated/wrong argv.
func parseGogCommandLine(out string) ([]string, bool) {
	m := gogCommandLineRe.FindStringSubmatch(out)
	if len(m) < 2 {
		return nil, false
	}
	cmd := strings.TrimSpace(m[1])
	if cmd == "" {
		return nil, false
	}
	// Shell-quoted args are ambiguous under strings.Fields — fall through to JSON.
	if strings.ContainsAny(cmd, "\"'") {
		return nil, false
	}
	fields := strings.Fields(cmd)
	if !gogCommandComplete(fields) {
		return nil, false
	}
	return fields, true
}

// gogCommandComplete reports whether argv is a full, unambiguous gog spawn. gog
// can be registered TWO ways (see mcp.go serverCmd/addArgs): op-wrapped
// (`op run --env-file=… -- gog … mcp …`, when op-refs is present) or BARE
// (`gog … mcp …`, when op-refs is absent — 1Password is optional for gog). A
// command is complete in EITHER form: it resolves (unwrapping any `op run … --`
// prefix) to a binary whose basename is `gog` and whose args carry the `mcp`
// subcommand. A partial capture (`op`, `op run`, args on a separate line) does
// not, so the caller keeps looking rather than probe a truncated command.
func gogCommandComplete(argv []string) bool {
	_, ok := gogSpawnArgv(argv)
	return ok
}

// gogSpawnArgv extracts the effective gog spawn argv from a registered command,
// handling both the op-wrapped form (`op run … -- gog … mcp …`) and the bare
// form (`gog … mcp …`). It returns (cmd,true) when the resolved binary's
// basename is `gog` and its args contain the `mcp` subcommand; (nil,false)
// otherwise. Guards against index-out-of-range on short/empty argv.
func gogSpawnArgv(argv []string) ([]string, bool) {
	if len(argv) == 0 {
		return nil, false
	}
	// Unwrap ONLY a trusted `op run … -- <cmd…>` prefix; a `--` behind a non-op
	// argv[0] is rejected (never a token doctor would exec).
	cmd, ok := unwrapOpRun(argv)
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

// parseGogCommandJSON extracts the registered argv from `sbx mcp ls -o json`
// (an array of {name, command, args}). Returns (nil,false) when there is no gog
// entry or the JSON doesn't parse.
func parseGogCommandJSON(out string) ([]string, bool) {
	var servers []struct {
		Name    string   `json:"name"`
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	if err := json.Unmarshal([]byte(out), &servers); err != nil {
		return nil, false
	}
	for _, s := range servers {
		if s.Name != "gog" || strings.TrimSpace(s.Command) == "" {
			continue
		}
		argv := append([]string{s.Command}, s.Args...)
		// Same completeness bar as the line form: a JSON entry that does not resolve
		// to a `gog … mcp …` spawn (op-wrapped or bare) is not a confident command,
		// so return not-found and let doctor take the honest best-effort fallback.
		if !gogCommandComplete(argv) {
			return nil, false
		}
		return argv, true
	}
	return nil, false
}

// gogAttachCheck is the informational check 5: is gog in the configured MCP
// set at all, and if so is it registered eager (pinned via mcp_static — tools
// in context from sandbox create) or the default dynamic (discovered/called on
// demand via mcp-find/mcp-exec)? There is no --mcp-at-create auto-attach
// anymore (see resolveStaticMCP): every configured server is attached at
// create via the gateway, and mcp_static/mcp_dynamic decide eager vs. lazy.
func gogAttachCheck(cfg *config.Config) check {
	req := gogRequirement(cfg, shellEnv{})
	if !mcpConfigured(cfg, "gog") {
		return check{label: "attached",
			detail:      "run `pi-stack config set mcp gog` to attach it",
			requirement: req, evidence: EvidenceNotConfigured}
	}
	// AC-03: describe gog as registered/dynamically discoverable rather than the
	// stale "auto-attached on run (--mcp gog)" language — attach mode is now
	// eager-vs-lazy (mcp_static/mcp_dynamic), not implied by cfg.MCP membership.
	if len(resolveStaticMCP([]string{"gog"}, cfg)) > 0 {
		return check{label: "attached",
			detail:      "registered; eager (pinned via mcp_static; tools in context from create)",
			requirement: req, evidence: EvidenceHealthy}
	}
	return check{label: "attached",
		detail:      "registered; dynamically discoverable (mcp-find/mcp-exec on demand)",
		requirement: req, evidence: EvidenceHealthy}
}

// render writes the verdict-first report to w. Default (verbose=false) is
// CONCISE: healthy checks (evidence=healthy) collapse to a single per-group
// summary line so the output leads with what needs attention. verbose=true
// retains the full detailed group evidence, one line per check, exactly as
// before.
func (r *report) render(w io.Writer, verbose bool) {
	todos := r.todos()

	// One-line verdict up front — the whole point of the Go rewrite. Derived
	// entirely from requirement+evidence (R1-03): a VERIFIED core failure is
	// the hard ✗; verified optional failures are the ⚠ outstanding count;
	// an unverifiable CORE check is called out as "could not verify" (never
	// "outstanding" — there is nothing confirmed to fix).
	unvCore := r.unverifiedCore()
	switch {
	case r.blocking():
		fmt.Fprintf(w, "✗ pi-stack: a verified core check is failing; see below (exit 1).\n")
	case len(todos) > 0:
		fmt.Fprintf(w, "⚠ pi-stack: %s outstanding; see the TODOs below.\n", plural(len(todos), "item"))
	case unvCore > 0:
		fmt.Fprintf(w, "⚠ pi-stack: no verified failures, but %s could not be verified from here.\n", plural(unvCore, "core check"))
	default:
		fmt.Fprintln(w, "✓ pi-stack: all checks pass; you're ready to `pi-stack serve` + `pi-stack`.")
	}
	if r.sbxAbsent {
		fmt.Fprintln(w, "  note: sbx not on PATH (you're likely inside the sandbox); provider/MCP")
		fmt.Fprintln(w, "        checks can't be verified here; run `pi-stack doctor` on the host.")
	}
	if r.sbxProbeFailed {
		fmt.Fprintln(w, "  note: sbx is on PATH but probing it failed (`sbx secret ls`); the host sbx")
		fmt.Fprintln(w, "        probe/gateway is unavailable; try `sbx daemon status`, then re-run doctor.")
	}
	fmt.Fprintln(w)

	// DX-5: only hint at --verbose when concise mode actually HID something.
	// A cold/all-todo run (nothing healthy to collapse) shows every check
	// already, so the hint would point at detail that doesn't exist.
	collapsedAny := false
	for _, g := range r.groups {
		fmt.Fprintf(w, "%s:\n", g.title)
		shown := 0
		for _, c := range g.checks {
			if !verbose && c.evidence == EvidenceHealthy {
				collapsedAny = true
				continue // concise: collapse healthy detail
			}
			fmt.Fprintf(w, "  %s %-12s %s\n", glyph(c.state()), checkLabel(c), c.detail)
			shown++
		}
		if !verbose && shown == 0 {
			fmt.Fprintf(w, "  ✓ all %s healthy\n", plural(len(g.checks), "check"))
		}
		fmt.Fprintln(w)
	}

	if len(todos) > 0 {
		fmt.Fprintln(w, "TODO (copy-paste, in dependency order):")
		for _, t := range todos {
			fmt.Fprintf(w, "  TODO: %s\n", t)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "Config: %s   (services=%s, mcp=%s)\n",
		config.Path(), strings.Join(r.cfgServices(), " "), r.cfgMCP())
	// DX-5: the hint is only useful — and only printed — when concise mode
	// actually hid a healthy detail line; otherwise --verbose would show
	// nothing new and the hint is noise.
	if !verbose && collapsedAny {
		fmt.Fprintln(w, "(concise output; run `pi-stack doctor --verbose` for full group detail)")
	}
}

// checkLabel is the DX-4 rendered label: a verified CORE (blocking) failure
// is marked "(required)" so it reads as distinct from an optional ✗ with no
// such hierarchy — both glyphs are ✗, but only one of them actually blocks
// `pi-stack doctor`'s exit code.
func checkLabel(c check) string {
	if Blocking(c.requirement, c.evidence) {
		return c.label + " (required)"
	}
	return c.label
}

// unverifiedCore counts the CORE checks whose evidence is unverifiable (note
// annotations excluded): they never block or count as outstanding, but the
// headline must not claim "all checks pass" over an unverified core axis.
func (r *report) unverifiedCore() int {
	n := 0
	for _, g := range r.groups {
		for _, c := range g.checks {
			if !c.note && c.requirement == RequirementCore && c.evidence == EvidenceUnverifiable {
				n++
			}
		}
	}
	return n
}

// cfgServices / cfgMCP are filled by runDoctor via closure-free re-derivation;
// keep them on the report so render stays config-free. Stored at build time.
func (r *report) cfgServices() []string { return r.services }
func (r *report) cfgMCP() string {
	if len(r.mcp) == 0 {
		return "<none>"
	}
	return strings.Join(r.mcp, " ")
}

func glyph(s checkState) string {
	switch s {
	case stateOK:
		return "✓"
	case stateTODO:
		return "✗"
	case stateWarn:
		return "⚠"
	default:
		return "·"
	}
}

func upDown(up bool) string {
	if up {
		return "up"
	}
	return "down"
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// grepWord reports whether out contains name as a whole word (matches the
// Makefile's `grep -qw`).
func grepWord(out, name string) bool {
	for _, line := range strings.Split(out, "\n") {
		for _, f := range strings.FieldsFunc(line, func(r rune) bool {
			return r == ' ' || r == '\t' || r == ',' || r == ':' || r == '/' || r == '"' || r == '='
		}) {
			if f == name {
				return true
			}
		}
	}
	return false
}

// modelPulled reports whether `ollama list` output lists the given model. The
// first column may carry a :tag suffix (e.g. "gemma4:latest").
func modelPulled(listOut, model string) bool {
	for _, line := range strings.Split(listOut, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		if name == model || strings.HasPrefix(name, model+":") {
			return true
		}
	}
	return false
}

// runDoctorCmd is the CLI entry point wired into main's dispatch.
func runDoctorCmd(argv []string) {
	jsonOut, verbose, err := parseDoctorArgs(argv)
	if err != nil {
		if err == errHelpRequested {
			fmt.Print(doctorUsage)
			return
		}
		fmt.Fprintf(os.Stderr, "pi-stack doctor: %v\n\n%s", err, doctorUsage)
		os.Exit(2)
	}
	cfg, _, err := loadResolvedConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack doctor: %v\n", err)
		os.Exit(1)
	}
	r := runDoctor(cfg, defaultShellEnv())
	r.services = cfg.Services
	r.mcp = cfg.MCP
	if jsonOut {
		_ = writeJSONOut(os.Stdout, r.jsonView(""))
		if r.blocking() {
			os.Exit(1)
		}
		return
	}
	r.render(os.Stdout, verbose)
	// AC-05 / readiness.go: only a VERIFIED core failure exits 1. Unverifiable
	// checks (including running inside the sandbox with sbx absent) and any
	// optional gap — configured or unconfigured — exit 0. Usage errors above
	// already exit 2.
	if r.blocking() {
		os.Exit(1)
	}
}

// parseDoctorArgs validates doctor flags: -h/--help returns errHelpRequested,
// --json sets jsonOut, --verbose sets verbose (retains full per-check group
// detail; default is concise), any other token is a usage error (exit 2).
func parseDoctorArgs(argv []string) (jsonOut, verbose bool, err error) {
	for _, a := range argv {
		switch a {
		case "-h", "--help":
			return false, false, errHelpRequested
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

// doctorJSON is the machine-readable doctor report (behind --json).
// doctorSchemaVersion is bumped whenever the JSON shape gains/changes fields
// a machine consumer might depend on. v2 added schema_version itself plus the
// per-check requirement/evidence readiness fields (AC-04). v3 (review round
// 1): per-check state is now DERIVED from evidence (R1-03), todos contain
// only verified failures, verdict gains "unverified", the providers group
// carries the aggregate "model keys" core check (R1-04), and sbx_probe_failed
// distinguishes a present-but-unhealthy sbx from an absent one (R1-11).
const doctorSchemaVersion = 3

type doctorJSON struct {
	SchemaVersion int               `json:"schema_version"`
	Verdict       string            `json:"verdict"`
	Blocking      bool              `json:"blocking"` // true iff a verified core failure -> exit 1
	Profile       string            `json:"profile"`
	Todos         []string          `json:"todos"`
	Groups        []doctorGroupJSON `json:"groups"`
	Services      []string          `json:"services"`
	MCP           []string          `json:"mcp"`
	SbxAbsent     bool              `json:"sbx_absent"`
	SbxProbeFail  bool              `json:"sbx_probe_failed"`
}

type doctorGroupJSON struct {
	Title  string            `json:"title"`
	Checks []doctorCheckJSON `json:"checks"`
}

type doctorCheckJSON struct {
	Label       string `json:"label"`
	State       string `json:"state"` // ok | todo | info | warn
	Detail      string `json:"detail"`
	Todo        string `json:"todo,omitempty"`
	Requirement string `json:"requirement"` // core | configured-optional | unconfigured-optional
	Evidence    string `json:"evidence"`    // healthy | failed | unverifiable | not-configured
}

// jsonView renders the report into its serializable form (the same data render
// prints, minus the ANSI/glyph presentation).
func (r *report) jsonView(profile string) doctorJSON {
	todos := r.todos()
	// Verdict derives from the same evidence axes the headline uses (R1-03):
	// verified failures → outstanding; an unverified core axis → unverified;
	// else pass. Blocking stays its own boolean.
	verdict := "pass"
	switch {
	case len(todos) > 0:
		verdict = "outstanding"
	case r.unverifiedCore() > 0:
		verdict = "unverified"
	}
	v := doctorJSON{
		SchemaVersion: doctorSchemaVersion,
		Verdict:       verdict,
		Blocking:      r.blocking(),
		Profile:       profile,
		Todos:         todos,
		Services:      r.services,
		MCP:           r.mcp,
		SbxAbsent:     r.sbxAbsent,
		SbxProbeFail:  r.sbxProbeFailed,
	}
	for _, g := range r.groups {
		gj := doctorGroupJSON{Title: g.title}
		for _, c := range g.checks {
			todo := c.todo
			if c.evidence != EvidenceFailed {
				todo = "" // R1-03: only a verified failure carries a repair command
			}
			gj.Checks = append(gj.Checks, doctorCheckJSON{
				Label: c.label, State: stateName(c.state()), Detail: c.detail, Todo: todo,
				Requirement: string(c.requirement), Evidence: string(c.evidence),
			})
		}
		v.Groups = append(v.Groups, gj)
	}
	return v
}

// stateName maps a checkState to its JSON string. stateWarn renders as its
// own "warn" string (distinct from both "todo" and "info") so a JSON consumer
// can tell a best-effort-unverifiable result apart from a plain info line.
func stateName(s checkState) string {
	switch s {
	case stateOK:
		return "ok"
	case stateTODO:
		return "todo"
	case stateWarn:
		return "warn"
	default:
		return "info"
	}
}
