package main

import (
	"fmt"
	"path/filepath"

	"pi-stack/host/config"
)

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

	if !anyOpWrappedServer(cfg) {
		g.checks = append(g.checks, check{label: "1Password", note: true,
			detail: "no credentialed host MCP servers configured — 1Password not needed"})
		return g
	}

	// op installed? (advisory sign-in only when installed — never a blocker).
	if opInstalled(env) {
		g.checks = append(g.checks, check{label: "op CLI", verdict: verdictReady, detail: "installed"})
		if opSignedIn(env) {
			g.checks = append(g.checks, check{label: "account configured", verdict: verdictReady,
				detail: "op account list ok (advisory — not a proof of an unlocked session)"})
		} else {
			g.checks = append(g.checks, check{label: "account configured", note: true,
				detail: "no account configured (advisory) — run: op signin"})
		}
	} else {
		g.checks = append(g.checks, check{label: "op CLI", verdict: verdictTodo,
			detail: "not installed",
			todo:   "install the 1Password CLI (op) — https://developer.1password.com/docs/cli"})
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
		g.checks = append(g.checks, check{label: "op-refs.env", verdict: verdictTodo,
			detail: "not present at " + path,
			todo:   "pi-stack secret set <ENV_VAR> op://vault/item/field"})
		return g
	}
	g.checks = append(g.checks, check{label: "op-refs.env", note: true, detail: path})

	// Perms: the file AND its dir must not be group/other-accessible.
	if env.fileMode != nil {
		if m, ok := env.fileMode(path); ok && m.Perm()&0o077 != 0 {
			g.checks = append(g.checks, check{label: "perms", verdict: verdictTodo,
				detail: fmt.Sprintf("op-refs.env is %04o — group/other accessible", m.Perm()),
				todo:   "chmod 600 " + path})
		}
		dir := filepath.Dir(path)
		if m, ok := env.fileMode(dir); ok && m.Perm()&0o077 != 0 {
			g.checks = append(g.checks, check{label: "dir perms", verdict: verdictTodo,
				detail: fmt.Sprintf("%s is %04o — group/other accessible", dir, m.Perm()),
				todo:   "chmod 700 " + dir})
		}
	}

	// Per-ref: filled vs placeholder, plus the refs-only lint. NEVER print a value.
	for _, rf := range parseOpRefs(content) {
		switch {
		case rf.nonSecret:
			g.checks = append(g.checks, check{label: rf.key, note: true, detail: "non-secret env (allowed literal)"})
		case rf.isRef && rf.placeholder:
			g.checks = append(g.checks, check{label: rf.key, verdict: verdictTodo,
				detail: "unfilled placeholder — set the op:// ref",
				todo:   "pi-stack secret set <ENV_VAR> op://vault/item/field"})
		case rf.isRef:
			g.checks = append(g.checks, check{label: rf.key, verdict: verdictReady, detail: "op:// ref filled"})
		case rf.placeholder:
			// A non-ref value still carrying an unfilled <...> placeholder.
			g.checks = append(g.checks, check{label: rf.key, verdict: verdictTodo,
				detail: "unfilled placeholder — set the op:// ref",
				todo:   "pi-stack secret set <ENV_VAR> op://vault/item/field"})
		case looksSecretShaped(rf.key, rf.value):
			// MEDIUM finding — a pasted secret. NEVER echo the value.
			g.checks = append(g.checks, check{label: rf.key, verdict: verdictTodo,
				detail: "possible pasted secret — replace with op://vault/item/field",
				todo:   "pi-stack secret set <ENV_VAR> op://vault/item/field"})
		default:
			// Refs-only policy: ANY other non-ref, non-allowlisted value is flagged.
			// NEVER echo the value.
			g.checks = append(g.checks, check{label: rf.key, verdict: verdictTodo,
				detail: "not an op:// ref — this file is refs-only; use op://vault/item/field or move it to the non-secret allowlist",
				todo:   "pi-stack secret set <ENV_VAR> op://vault/item/field"})
		}
	}
	return g
}

// looksSecretShaped reports whether a NON-ref, non-allowlisted op-refs.env value
// looks like a pasted secret. Thin wrapper over the shared config.LooksSecretShaped
// so doctor's lint and backup's pre-archive warning judge identically.
func looksSecretShaped(key, val string) bool { return config.LooksSecretShaped(key, val) }
