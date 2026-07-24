package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	getenv   func(name string) string
	dial     func(port int) bool
	statFile func(path string) bool            // does a regular file exist at path?
	readFile func(path string) (string, error) // read a file's contents
	homeDir  func() string                     // the user's home directory ($HOME)
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
	// probe runs an UNTRUSTED registered command with a hard timeout + capped
	// output, so doctor never hangs (or floods) on a misbehaving MCP server. It
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
// parsing detail text.
type check struct {
	label       string
	state       checkState
	detail      string // short human note after the label
	todo        string // exact copy-pasteable command when state == stateTODO
	requirement Requirement
	evidence    Evidence
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
	sbxAbsent bool     // sbx not on PATH — provider/mcp checks can't be verified here
	services  []string // configured SERVICES, for the footer
	mcp       []string // configured MCP, for the footer
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
			if c.state != stateTODO || c.todo == "" {
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

// gatewayDownDetail / gatewayTODO describe the HOST condition where sbx IS
// present (secret ls succeeded) but `sbx mcp ls` failed. The MCP gateway is now
// the local data-plane one (always available, no SBX_MCP_URL), so a failed
// listing means the sbx daemon/gateway is unhealthy, not "gateway off". This is
// NOT "sbx unavailable": the CLI is here, only the MCP-registration listing failed.
const (
	gatewayDownDetail = "sbx present but couldn't list MCP registrations — check the sbx daemon (sbx mcp status; sbx daemon status)"
	gatewayTODO       = "check the sbx MCP gateway: run `sbx mcp status` and `sbx daemon status`, then re-run doctor"
)

// runDoctor builds the report. Pure apart from env: no direct OS access, so the
// tests feed a faked shellEnv and assert on the rendered output.
func runDoctor(cfg *config.Config, env shellEnv) *report {
	r := &report{}

	// sbx presence gates the provider + mcp checks (they read `sbx secret ls` /
	// `sbx mcp ls`). Inside the sandbox sbx is absent — say so, don't crash.
	sbxOut, sbxOK := "", false
	if _, err := env.lookPath("sbx"); err == nil {
		if out, err := env.run("sbx", "secret", "ls"); err == nil {
			sbxOut, sbxOK = out, true
		}
	}
	r.sbxAbsent = !sbxOK

	// MCP registrations (`sbx mcp ls`), listed once and reused by the gog group
	// (its gateway registration) and the MCP group below. sbxOK (sbx PRESENT +
	// `sbx secret ls` ok) is tracked SEPARATELY from mcpOK (`sbx mcp ls` ok): on
	// the host the CLI is present but the MCP listing can fail (gateway off), and
	// that must not be reported as "sbx unavailable".
	mcpOut, mcpOK := "", false
	if sbxOK {
		if out, err := env.run("sbx", "mcp", "ls"); err == nil {
			mcpOut, mcpOK = out, true
		}
	}

	// (a) provider secrets — proxy-injected, never in the VM. Anthropic/OpenAI/
	// Google are CORE: a verified missing key is the one thing that makes doctor
	// exit 1. GitHub stays configured-optional per current runtime policy (no
	// runtime path hard-requires it today).
	providers := group{title: "Providers / keys (proxy-injected, never in the VM)"}
	for _, p := range []struct {
		label, key string
		req        Requirement
	}{
		{"anthropic", "anthropic", RequirementCore},
		{"openai", "openai", RequirementCore},
		{"google", "google", RequirementCore},
		{"github", "github", RequirementConfiguredOptional},
	} {
		providers.checks = append(providers.checks, secretCheck(p.label, p.key, sbxOut, sbxOK, p.req))
	}
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
	if p.installed {
		ev := EvidenceHealthy
		if !p.daemonUp {
			ev = EvidenceFailed
		}
		ollama.checks = append(ollama.checks, check{
			label:       "ollama",
			state:       stateOK,
			detail:      "installed, :11434 " + upDown(p.daemonUp),
			requirement: ollamaReq,
			evidence:    ev,
		})
	} else {
		ollama.checks = append(ollama.checks, check{
			label:       "ollama",
			state:       stateTODO,
			detail:      "not installed",
			todo:        "install ollama — https://ollama.com",
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
		mcp := group{title: "Other MCP servers (local stdio, run by the sbx gateway)"}
		for _, m := range others {
			mcp.checks = append(mcp.checks, mcpProbeCheck(env, m, mcpOut, mcpOK, sbxOK))
		}
		r.groups = append(r.groups, mcp)
	case len(cfg.MCP) == 0:
		mcp := group{title: "Other MCP servers (local stdio, run by the sbx gateway)"}
		mcp.checks = append(mcp.checks, check{
			label:       "other servers",
			state:       stateInfo,
			detail:      "no other MCP servers configured — add one with `pi-stack config set mcp <server>`",
			requirement: RequirementUnconfiguredOptional,
			evidence:    EvidenceNotConfigured,
		})
		r.groups = append(r.groups, mcp)
		// else: only gog is configured — it already has its own group, so this
		// section is omitted rather than rendering a misleading empty one.
	}

	return r
}

// secretCheck reports whether a provider secret is set. When sbx is
// unreachable (e.g. inside the sandbox) it emits a TODO rather than a false OK
// — but its evidence is unverifiable, not failed, so req=core still never
// blocks in that case (only a CONFIRMED absence does; see AC-05).
func secretCheck(label, key, sbxOut string, sbxOK bool, req Requirement) check {
	cmd := "sbx secret set -g " + key
	if !sbxOK {
		return check{label: label, state: stateTODO, detail: "sbx unavailable here (set on the host)", todo: cmd,
			requirement: req, evidence: EvidenceUnverifiable}
	}
	if grepWord(sbxOut, key) {
		return check{label: label, state: stateOK, detail: "set", requirement: req, evidence: EvidenceHealthy}
	}
	return check{label: label, state: stateTODO, detail: "not set", todo: cmd,
		requirement: req, evidence: EvidenceFailed}
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
		return check{label: label, state: stateOK, detail: "pulled — " + detail, requirement: m.Requirement, evidence: m.Evidence}
	case EvidenceNotConfigured:
		return check{label: label, state: stateTODO, detail: detail + " — needs ollama", todo: m.PullCmd,
			requirement: m.Requirement, evidence: m.Evidence}
	case EvidenceUnverifiable:
		return check{label: label, state: stateWarn, detail: detail + " — could not verify (ollama list unavailable)",
			requirement: m.Requirement, evidence: m.Evidence}
	default: // EvidenceFailed
		return check{label: label, state: stateTODO, detail: detail + " — not pulled", todo: m.PullCmd,
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
		return check{label: label, state: stateOK, detail: fmt.Sprintf(":%d up", port), requirement: req, evidence: EvidenceHealthy}
	}
	if isEnabled {
		return check{label: label, state: stateTODO, detail: fmt.Sprintf(":%d down", port), todo: startCmd,
			requirement: req, evidence: EvidenceFailed}
	}
	return check{label: label, state: stateInfo, detail: fmt.Sprintf(":%d down (not in configured services)", port),
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
		return check{label: "fact capture", state: stateInfo, detail: "could not query daemon health",
			requirement: captureReq, evidence: EvidenceUnverifiable}
	}
	httpReq.Header.Set("content-type", "application/json")
	res, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return check{label: "fact capture", state: stateInfo, detail: "could not query daemon health",
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
		return check{label: "fact capture", state: stateInfo, detail: "could not read daemon health",
			requirement: captureReq, evidence: EvidenceUnverifiable}
	}
	m := parsed.Result.WatcherModel
	if parsed.Result.Capture {
		return check{label: "fact capture", state: stateOK, detail: fmt.Sprintf("on (watcher %s)", m),
			requirement: captureReq, evidence: EvidenceHealthy}
	}
	// Prefer the daemon's own live reason (e.g. a watcher inference timeout while
	// Ollama is wedged) over the generic "unavailable" text — that's the whole
	// point of surfacing captureReason.
	detail := fmt.Sprintf("OFF — watcher %q unavailable (recall still works)", m)
	if parsed.Result.CaptureReason != "" {
		detail = fmt.Sprintf("OFF — %s (recall still works)", parsed.Result.CaptureReason)
	}
	return check{
		label:       "fact capture",
		state:       stateTODO,
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
			return check{label: name, state: stateTODO, detail: gatewayDownDetail, todo: gatewayTODO,
				requirement: req, evidence: EvidenceUnverifiable}
		}
		return check{label: name, state: stateTODO, detail: "sbx unavailable here (register on the host)", todo: cmd,
			requirement: req, evidence: EvidenceUnverifiable}
	}
	if grepWord(mcpOut, name) {
		return check{label: name, state: stateOK, detail: "registered", requirement: req, evidence: EvidenceHealthy}
	}
	return check{label: name, state: stateTODO, detail: "not registered", todo: cmd, requirement: req, evidence: EvidenceFailed}
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
			return check{label: name, state: stateTODO, detail: gatewayDownDetail, todo: gatewayTODO,
				requirement: req, evidence: EvidenceUnverifiable}
		}
		return check{label: name, state: stateTODO, detail: "sbx unavailable here (register on the host)", todo: cmd,
			requirement: req, evidence: EvidenceUnverifiable}
	}
	if !grepWord(mcpOut, name) {
		return check{label: name, state: stateTODO, detail: "not registered", todo: cmd, requirement: req, evidence: EvidenceFailed}
	}
	// Registered — try the honest headless probe of the registered command.
	argv, ok := registeredMCPCommand(env, name)
	if !ok {
		return check{label: name, state: stateOK, detail: "registered (tool probe unavailable)", requirement: req, evidence: EvidenceHealthy}
	}
	// SAFETY: only exec a command whose SHAPE we trust. sbx will hand us whatever
	// argv someone registered for <name>; doctor must not blindly run it. If it is
	// not the known gog form or a `pi-stack-host mcp <name>` spawn, skip the probe
	// (still a confirmed registration, just no tool count) rather than exec it.
	if !recognizedMCPArgv(argv, name) {
		return check{label: name, state: stateOK, detail: "registered (probe skipped: unrecognized command)", requirement: req, evidence: EvidenceHealthy}
	}
	n, probed := probeRegisteredMCP(env, argv)
	if !probed {
		return check{label: name, state: stateOK, detail: "registered (tool probe unavailable)", requirement: req, evidence: EvidenceHealthy}
	}
	if n == 0 {
		return check{label: name, state: stateTODO,
			detail:      "registered but the spawned command returns 0 tools — headless creds/keyring",
			todo:        "review the registered command: sbx mcp get " + name,
			requirement: req, evidence: EvidenceFailed}
	}
	return check{label: name, state: stateOK, detail: fmt.Sprintf("registered, spawns %s", plural(n, "tool")), requirement: req, evidence: EvidenceHealthy}
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
	if env.lookPath == nil || env.run == nil {
		return nil, false
	}
	if _, err := env.lookPath("sbx"); err != nil {
		return nil, false
	}
	if out, err := env.run("sbx", "mcp", "get", name); err == nil {
		if argv, ok := parseMCPCommandLine(out); ok {
			return argv, true
		}
	}
	if out, err := env.run("sbx", "mcp", "ls", "-o", "json"); err == nil {
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
// probe: either the known gog spawn form, or (optionally wrapped in
// `op run … -- …`) an ABSOLUTE path whose basename is `pi-stack-host` followed
// by `mcp <name>` — exactly how mcp.go registers a local stdio server. Anything
// else is an arbitrary command someone put in the registration, which doctor
// must NOT run.
func recognizedMCPArgv(argv []string, name string) bool {
	if _, ok := gogSpawnArgv(argv); ok {
		return true
	}
	// Unwrap ONLY a trusted `op run … -- <cmd…>` prefix. A `--` behind any other
	// argv[0] is rejected: the probe execs argv[0] verbatim, so unwrapping a
	// prefix like `/tmp/evil -- pi-stack-host mcp slack` would exec /tmp/evil.
	cmd, ok := unwrapOpRun(argv)
	if !ok {
		return false
	}
	if len(cmd) < 3 {
		return false
	}
	if !filepath.IsAbs(cmd[0]) || filepath.Base(cmd[0]) != "pi-stack-host" {
		return false
	}
	return cmd[1] == "mcp" && cmd[2] == name
}

// unwrapOpRun returns the effective command doctor would trust to exec. With no
// `--` it is argv itself (a bare command). With a `--`, it unwraps the prefix
// ONLY when argv[0] is an absolute `op` binary (the real command runs via op,
// which is trusted); a `--` behind any other argv[0] returns ok=false so a
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
	// execs argv[0] verbatim, so a `--` behind a FOREIGN argv[0] (e.g.
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

// probeRegisteredMCP runs the registered command with `--list-tools` appended
// (the same handshake probeRegisteredGog uses), BOUNDED by probeRun's timeout +
// output cap, and returns the count of tool lines it prints plus whether the
// command ran cleanly. A timeout, a non-zero exit, or a server that doesn't
// support `--list-tools` -> (0,false), so the caller reports "registered"
// without a bogus tool count instead of hanging or emitting a false failure.
func probeRegisteredMCP(env shellEnv, argv []string) (int, bool) {
	if len(argv) == 0 {
		return 0, false
	}
	full := append(append([]string{}, argv...), "--list-tools")
	out, timedOut, err := probeRun(env, full[0], full[1:]...)
	if timedOut || err != nil {
		return 0, false
	}
	n := 0
	for _, ln := range strings.Split(out, "\n") {
		if strings.TrimSpace(ln) != "" {
			n++
		}
	}
	return n, true
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

// gogHeadlessOK runs the gateway-EQUIVALENT probe — list gog's tools the exact
// way the sbx gateway spawns it: headless, in a bare env, through the same
// `op run --env-file=config/op-refs.env` wrapper mcp-register uses — and reports
// whether it yields a NON-EMPTY tool list. This is the ONLY check that proves
// the real path; `gog auth doctor` in a logged-in shell passes and lies. It
// degrades cleanly (returns false, never crashes) when gog/op/account are
// absent. shellEnv keeps it unit-testable.
func gogHeadlessOK(env shellEnv, acct, opRefs string) bool {
	if acct == "" || opRefs == "" {
		return false
	}
	if _, err := env.lookPath("op"); err != nil {
		return false
	}
	out, timedOut, err := probeRun(env, "op", "run", "--env-file="+opRefs, "--",
		"gog", "--account", acct, "mcp", "--list-tools")
	return !timedOut && err == nil && strings.TrimSpace(out) != ""
}

// gogGroup builds the gog check cluster. The HONEST path reads the ACTUAL
// command the sbx gateway registered for gog and probes THAT (so it verifies
// the registered account, op-refs path, and op/gog binaries as-registered). Only
// when sbx is absent (or exposes no command) does it fall back to a best-effort
// reconstruction from config — clearly labeled, and never a confirmed green.
// Every probe degrades to a TODO rather than crashing, so this runs cleanly
// in-sandbox (gog/sbx/op all absent).
func gogGroup(cfg *config.Config, env shellEnv, mcpOut string, mcpOK, sbxPresent bool) group {
	g := group{title: "gog (Google Workspace via host MCP — read-only)"}
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
		g.checks = append(g.checks, check{label: "registration", state: stateInfo,
			detail:      "probing the sbx-registered command: " + redactRegisteredCommand(argv),
			requirement: req, evidence: EvidenceUnverifiable})
		if probeRegisteredGog(env, argv) {
			// Distinguish the op-wrapped path (op-refs resolved) from a BARE spawn so a
			// bare green never implies 1Password creds were involved.
			detail := "registered command exposes tools (verified as-registered, via op run)"
			if !gogSpawnIsOpWrapped(argv) {
				detail = "registered command exposes tools (verified as-registered) — spawned BARE (no op-refs involved)"
			}
			g.checks = append(g.checks, check{label: "headless spawn", state: stateOK,
				detail: detail, requirement: req, evidence: EvidenceHealthy})
		} else {
			g.checks = append(g.checks, check{label: "headless spawn", state: stateTODO,
				detail:      "the registered command returns 0 tools — keyring not headless",
				todo:        "add GOG_KEYRING_BACKEND=file + GOG_KEYRING_PASSWORD + GOG_ACCOUNT + GOG_HOME to " + defaultOpRefsPath(env),
				requirement: req, evidence: EvidenceFailed})
		}
		g.checks = append(g.checks, mcpCheck("gog", mcpOut, mcpOK, sbxPresent, req))
		g.checks = append(g.checks, gogAttachCheck(cfg))
		return g
	}

	// 1. gog CLI installed (the reconstruction probe uses it).
	if _, err := env.lookPath("gog"); err != nil {
		g.checks = append(g.checks, check{label: "gog CLI", state: stateTODO,
			detail: "not found", todo: "brew install gog", requirement: req, evidence: EvidenceNotConfigured})
		return g
	}
	g.checks = append(g.checks, check{label: "gog CLI", state: stateOK, detail: "installed", requirement: req, evidence: EvidenceHealthy})

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
		fallbackWhy = "best-effort (couldn't read sbx MCP registrations — check the sbx daemon: sbx mcp status)"
	}
	g.checks = append(g.checks,
		check{label: "verifying", state: stateInfo,
			detail:      fallbackWhy + " — verifies " + acctShown + " via " + refsShown,
			requirement: req, evidence: EvidenceUnverifiable},
		// AC-03: no more `make mcp-register` language — name the registration
		// itself, not a specific make target that may not even be how it was run.
		check{label: "note", state: stateInfo,
			detail:      "must match the sbx-registered gog command (config.toml gog_account + config/op-refs.env)",
			requirement: req, evidence: EvidenceUnverifiable})

	if acct == "" {
		// 2'. No account configured — can't probe auth or the headless path. Not a
		// confirmed failure, just genuinely not set up: NotConfigured, not Failed.
		g.checks = append(g.checks, check{label: "account", state: stateTODO,
			detail:      "cannot verify (gog_account unset in config.toml/env)",
			todo:        "pi-stack config set gog_account <you@example.com>",
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
			check{label: "account", state: stateInfo, detail: acct + " set (unconfirmed vs registration)",
				requirement: req, evidence: EvidenceUnverifiable},
			check{label: "op-refs", state: stateInfo,
				detail:      "op-refs.env not found — only needed if the gateway can't unlock gog's keyring headlessly",
				requirement: req, evidence: EvidenceNotConfigured})
		g.checks = append(g.checks, mcpCheck("gog", mcpOut, mcpOK, sbxPresent, req))
		g.checks = append(g.checks, gogAttachCheck(cfg))
		return g
	}

	// 2. account authorized (interactive). 3. THE GOTCHA — headless spawn.
	_, interErr := env.run("gog", "--account", acct, "auth", "doctor", "--check")
	_, opErr := env.lookPath("op")
	headOK := gogHeadlessOK(env, acct, opRefs)
	switch {
	case interErr != nil:
		// Auth itself isn't set up — don't double-report the keyring below.
		g.checks = append(g.checks, check{label: "account", state: stateTODO,
			detail:      acct + " not authorized",
			todo:        "gog auth add-client <client.json> && gog --account " + acct + " auth login",
			requirement: req, evidence: EvidenceFailed})
	case opErr != nil:
		// Interactive auth OK, but op is absent so we can't run the gateway-
		// equivalent probe. Say so rather than blaming the keyring.
		g.checks = append(g.checks,
			check{label: "account", state: stateOK, detail: acct + " authorized (interactive)",
				requirement: req, evidence: EvidenceHealthy},
			check{label: "headless spawn", state: stateTODO,
				detail:      "can't verify the gateway spawn — op (1Password CLI) not found",
				todo:        "install the 1Password CLI (op) so doctor can probe the real headless path",
				requirement: req, evidence: EvidenceUnverifiable})
	case !headOK:
		// THE TRAP: interactive passes, the headless gateway spawn gets 0 tools.
		// This IS a verified failure (we ran the exact gateway-equivalent probe and
		// it returned nothing) — just never a core one, so it still can't block.
		g.checks = append(g.checks,
			check{label: "account", state: stateOK, detail: acct + " authorized (interactive)",
				requirement: req, evidence: EvidenceHealthy},
			check{label: "headless spawn", state: stateTODO,
				detail:      "auth OK in your shell but the gateway spawn gets 0 tools — keyring not headless",
				todo:        "add GOG_KEYRING_BACKEND=file + GOG_KEYRING_PASSWORD + GOG_ACCOUNT + GOG_HOME to " + defaultOpRefsPath(env),
				requirement: req, evidence: EvidenceFailed})
	default:
		// AC-01: best-effort success — this account authenticates headlessly, but
		// we could NOT confirm it is the command the sbx gateway actually
		// registered. That is UNVERIFIABLE, not a failure: doctor genuinely does
		// not know whether it matches the real registration, so this must render
		// as a warning (⚠), never a ✗, and carry no fix-it TODO (there's nothing
		// confirmed broken to fix). Only the honest path above (registered
		// command read + probed) earns a confirmed ✓.
		g.checks = append(g.checks,
			check{label: "account", state: stateInfo, detail: acct + " authorized (best-effort, unconfirmed vs registration)",
				requirement: req, evidence: EvidenceUnverifiable},
			check{label: "headless spawn", state: stateWarn,
				detail:      "best-effort headless spawn succeeded, but the sbx-registered command could not be confirmed — unverifiable, not a failure",
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
		g.checks = append(g.checks, check{label: "1Password", state: stateInfo,
			detail:      "no credentialed host MCP servers configured — 1Password not needed",
			requirement: RequirementUnconfiguredOptional, evidence: EvidenceNotConfigured})
		return g
	}

	// op installed? (advisory sign-in only when installed — never a blocker).
	if opInstalled(env) {
		g.checks = append(g.checks, check{label: "op CLI", state: stateOK, detail: "installed", requirement: req, evidence: EvidenceHealthy})
		if opSignedIn(env) {
			g.checks = append(g.checks, check{label: "account configured", state: stateOK,
				detail:      "op account list ok (advisory — not a proof of an unlocked session)",
				requirement: req, evidence: EvidenceHealthy})
		} else {
			g.checks = append(g.checks, check{label: "account configured", state: stateInfo,
				detail:      "no account configured (advisory) — run: op signin",
				requirement: req, evidence: EvidenceUnverifiable})
		}
	} else {
		g.checks = append(g.checks, check{label: "op CLI", state: stateTODO,
			detail:      "not installed",
			todo:        "install the 1Password CLI (op) — https://developer.1password.com/docs/cli",
			requirement: req, evidence: EvidenceNotConfigured})
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
		g.checks = append(g.checks, check{label: "op-refs.env", state: stateTODO,
			detail:      "not present at " + path,
			todo:        "pi-stack secret set <ENV_VAR> op://vault/item/field",
			requirement: req, evidence: EvidenceNotConfigured})
		return g
	}
	g.checks = append(g.checks, check{label: "op-refs.env", state: stateInfo, detail: path, requirement: req, evidence: EvidenceHealthy})

	// Perms: the file AND its dir must not be group/other-accessible.
	if env.fileMode != nil {
		if m, ok := env.fileMode(path); ok && m.Perm()&0o077 != 0 {
			g.checks = append(g.checks, check{label: "perms", state: stateTODO,
				detail: fmt.Sprintf("op-refs.env is %04o — group/other accessible", m.Perm()),
				todo:   "chmod 600 " + path, requirement: req, evidence: EvidenceFailed})
		}
		dir := filepath.Dir(path)
		if m, ok := env.fileMode(dir); ok && m.Perm()&0o077 != 0 {
			g.checks = append(g.checks, check{label: "dir perms", state: stateTODO,
				detail: fmt.Sprintf("%s is %04o — group/other accessible", dir, m.Perm()),
				todo:   "chmod 700 " + dir, requirement: req, evidence: EvidenceFailed})
		}
	}

	// Per-ref: filled vs placeholder, plus the refs-only lint. NEVER print a value.
	for _, rf := range parseOpRefs(content) {
		switch {
		case rf.nonSecret:
			g.checks = append(g.checks, check{label: rf.key, state: stateInfo, detail: "non-secret env (allowed literal)",
				requirement: req, evidence: EvidenceHealthy})
		case rf.isRef && rf.placeholder:
			g.checks = append(g.checks, check{label: rf.key, state: stateTODO,
				detail: "unfilled placeholder — set the op:// ref",
				todo:   "pi-stack secret set <ENV_VAR> op://vault/item/field", requirement: req, evidence: EvidenceFailed})
		case rf.isRef:
			g.checks = append(g.checks, check{label: rf.key, state: stateOK, detail: "op:// ref filled", requirement: req, evidence: EvidenceHealthy})
		case rf.placeholder:
			// A non-ref value still carrying an unfilled <...> placeholder.
			g.checks = append(g.checks, check{label: rf.key, state: stateTODO,
				detail: "unfilled placeholder — set the op:// ref",
				todo:   "pi-stack secret set <ENV_VAR> op://vault/item/field", requirement: req, evidence: EvidenceFailed})
		case looksSecretShaped(rf.key, rf.value):
			// MEDIUM finding — a pasted secret. NEVER echo the value.
			g.checks = append(g.checks, check{label: rf.key, state: stateTODO,
				detail: "possible pasted secret — replace with op://vault/item/field",
				todo:   "pi-stack secret set <ENV_VAR> op://vault/item/field", requirement: req, evidence: EvidenceFailed})
		default:
			// Refs-only policy: ANY other non-ref, non-allowlisted value is flagged.
			// NEVER echo the value.
			g.checks = append(g.checks, check{label: rf.key, state: stateTODO,
				detail: "not an op:// ref — this file is refs-only; use op://vault/item/field or move it to the non-secret allowlist",
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
	if env.lookPath == nil || env.run == nil {
		return nil, false
	}
	if _, err := env.lookPath("sbx"); err != nil {
		return nil, false
	}
	if out, err := env.run("sbx", "mcp", "get", "gog"); err == nil {
		if argv, ok := parseGogCommandLine(out); ok {
			return argv, true
		}
	}
	if out, err := env.run("sbx", "mcp", "ls", "-o", "json"); err == nil {
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
	// argv[0] is rejected (the probe execs argv[0] verbatim).
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

// probeRegisteredGog runs the EXACT registered command with `--list-tools`
// appended and reports whether it yields a non-empty tool list — verifying the
// real gateway spawn (account, op-refs, op/gog binaries) all as-registered.
// This works for BOTH registration forms unchanged: the op-wrapped form runs
// `op run … -- gog … mcp … --list-tools`, and the bare form runs
// `gog … mcp … --list-tools` (argv[0] is gog itself). It degrades cleanly
// (returns false, never crashes) on any error.
func probeRegisteredGog(env shellEnv, argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	full := append(append([]string{}, argv...), "--list-tools")
	out, timedOut, err := probeRun(env, full[0], full[1:]...)
	return !timedOut && err == nil && strings.TrimSpace(out) != ""
}

// gogAttachCheck is the informational check 5: is gog in the configured MCP set,
// so `pi-stack run` auto-attaches it (--mcp gog)?
func gogAttachCheck(cfg *config.Config) check {
	req := gogRequirement(cfg, shellEnv{})
	if !mcpConfigured(cfg, "gog") {
		return check{label: "attached", state: stateInfo,
			detail:      "run `pi-stack config set mcp gog` to attach it",
			requirement: req, evidence: EvidenceNotConfigured}
	}
	// AC-03: describe gog as registered/dynamically discoverable rather than the
	// stale "auto-attached on run (--mcp gog)" language — attach mode is now
	// eager-vs-lazy (mcp_static/mcp_dynamic), not implied by cfg.MCP membership.
	if len(resolveStaticMCP([]string{"gog"}, cfg)) > 0 {
		return check{label: "attached", state: stateInfo,
			detail:      "registered; eager (pinned via mcp_static — tools in context from create)",
			requirement: req, evidence: EvidenceHealthy}
	}
	return check{label: "attached", state: stateInfo,
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

	// One-line verdict up front — the whole point of the Go rewrite. Only a
	// VERIFIED core failure (r.blocking()) is a hard ✗; anything else
	// outstanding (optional gaps, unverifiable checks) is a ⚠, never blocking
	// doctor's exit code (see AC-05 / readiness.go).
	switch {
	case len(todos) == 0:
		fmt.Fprintln(w, "✓ pi-stack: all checks pass — you're ready to `pi-stack serve` + `pi-stack`.")
	case r.blocking():
		fmt.Fprintf(w, "✗ pi-stack: a verified core check is failing — see below (exit 1).\n")
	default:
		fmt.Fprintf(w, "⚠ pi-stack: %s outstanding — see the TODOs below.\n", plural(len(todos), "item"))
	}
	if r.sbxAbsent {
		fmt.Fprintln(w, "  note: sbx not on PATH (you're likely inside the sandbox) — provider/MCP")
		fmt.Fprintln(w, "        checks can't be verified here; run `pi-stack doctor` on the host.")
	}
	fmt.Fprintln(w)

	for _, g := range r.groups {
		fmt.Fprintf(w, "%s:\n", g.title)
		shown := 0
		for _, c := range g.checks {
			if !verbose && c.evidence == EvidenceHealthy {
				continue // concise: collapse healthy detail
			}
			fmt.Fprintf(w, "  %s %-12s %s\n", glyph(c.state), c.label, c.detail)
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
	if !verbose {
		fmt.Fprintln(w, "(concise output — run `pi-stack doctor --verbose` for full group detail)")
	}
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
// a machine consumer might depend on. v2 adds schema_version itself plus the
// per-check requirement/evidence readiness fields (AC-04); all v1 fields are
// kept unchanged for compatibility (see AC-04 note in doctor_test.go).
const doctorSchemaVersion = 2

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
	verdict := "pass"
	if len(todos) > 0 {
		verdict = "outstanding"
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
	}
	for _, g := range r.groups {
		gj := doctorGroupJSON{Title: g.title}
		for _, c := range g.checks {
			gj.Checks = append(gj.Checks, doctorCheckJSON{
				Label: c.label, State: stateName(c.state), Detail: c.detail, Todo: c.todo,
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
