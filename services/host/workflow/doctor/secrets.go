package doctor

import (
	"fmt"
	"path/filepath"
	"pix/host/hostenv"
	"pix/host/readiness"
	"pix/host/secret"

	"pix/host/config"
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
func secretsGroup(cfg *config.Config, env hostenv.Env) readiness.Group {
	g := readiness.Group{Title: "Secrets (1Password, host MCP creds via op-refs.env)"}

	if !secret.AnyOpWrappedServer(cfg) {
		// Positive info: verified there is nothing 1Password-dependent configured,
		// so there is genuinely nothing to set up here.
		g.Checks = append(g.Checks, readiness.Check{Label: "1Password", Note: true, Verdict: readiness.VerdictReady,
			Detail: "no credentialed host MCP servers configured — 1Password not needed"})
		return g
	}

	// op installed? (advisory sign-in only when installed — never a blocker).
	if secret.OpInstalled(env) {
		g.Checks = append(g.Checks, readiness.Check{Label: "op CLI", Verdict: readiness.VerdictReady, Detail: "installed"})
		if secret.OpSignedIn(env) {
			g.Checks = append(g.Checks, readiness.Check{Label: "account configured", Verdict: readiness.VerdictReady,
				Detail: "op account list ok (advisory — not a proof of an unlocked session)"})
		} else {
			g.Checks = append(g.Checks, readiness.Check{Label: "account configured", Note: true, Verdict: readiness.VerdictUnverifiable,
				Detail: "no account configured (advisory) — run: op signin"})
		}
	} else {
		g.Checks = append(g.Checks, readiness.Check{Label: "op CLI", Verdict: readiness.VerdictTodo,
			Detail: "not installed",
			Todo:   "install the 1Password CLI (op) — https://developer.1password.com/docs/cli"})
	}

	// op-refs.env present at the absolute XDG path?
	path := secret.DefaultOpRefsPath(env)
	content, exists := "", false
	if c, err := env.ReadFile(path); err == nil {
		content, exists = c, true
	}
	if !exists {
		g.Checks = append(g.Checks, readiness.Check{Label: "op-refs.env", Verdict: readiness.VerdictTodo,
			Detail: "not present at " + path,
			Todo:   "pix secret set <ENV_VAR> op://vault/item/field"})
		return g
	}
	g.Checks = append(g.Checks, readiness.Check{Label: "op-refs.env", Note: true, Verdict: readiness.VerdictReady, Detail: path})

	// Perms: the file AND its dir must not be group/other-accessible.
	if m, ok := env.Mode(path); ok && m.Perm()&0o077 != 0 {
		g.Checks = append(g.Checks, readiness.Check{Label: "perms", Verdict: readiness.VerdictTodo,
			Detail: fmt.Sprintf("op-refs.env is %04o — group/other accessible", m.Perm()),
			Todo:   "chmod 600 " + path})
	}
	dir := filepath.Dir(path)
	if m, ok := env.Mode(dir); ok && m.Perm()&0o077 != 0 {
		g.Checks = append(g.Checks, readiness.Check{Label: "dir perms", Verdict: readiness.VerdictTodo,
			Detail: fmt.Sprintf("%s is %04o — group/other accessible", dir, m.Perm()),
			Todo:   "chmod 700 " + dir})
	}

	// Per-ref: filled vs placeholder, plus the refs-only lint. NEVER print a value.
	for _, rf := range secret.ParseOpRefs(content) {
		switch {
		case rf.NonSecret:
			g.Checks = append(g.Checks, readiness.Check{Label: rf.Key, Note: true, Verdict: readiness.VerdictReady, Detail: "non-secret env (allowed literal)"})
		case rf.IsRef && rf.Placeholder:
			g.Checks = append(g.Checks, readiness.Check{Label: rf.Key, Verdict: readiness.VerdictTodo,
				Detail: "unfilled placeholder — set the op:// ref",
				Todo:   "pix secret set <ENV_VAR> op://vault/item/field"})
		case rf.IsRef:
			g.Checks = append(g.Checks, readiness.Check{Label: rf.Key, Verdict: readiness.VerdictReady, Detail: "op:// ref filled"})
		case rf.Placeholder:
			// A non-ref value still carrying an unfilled <...> placeholder.
			g.Checks = append(g.Checks, readiness.Check{Label: rf.Key, Verdict: readiness.VerdictTodo,
				Detail: "unfilled placeholder — set the op:// ref",
				Todo:   "pix secret set <ENV_VAR> op://vault/item/field"})
		case config.LooksSecretShaped(rf.Key, rf.Value):
			// MEDIUM finding — a pasted secret. NEVER echo the value.
			g.Checks = append(g.Checks, readiness.Check{Label: rf.Key, Verdict: readiness.VerdictTodo,
				Detail: "possible pasted secret — replace with op://vault/item/field",
				Todo:   "pix secret set <ENV_VAR> op://vault/item/field"})
		default:
			// Refs-only policy: ANY other non-ref, non-allowlisted value is flagged.
			// NEVER echo the value.
			g.Checks = append(g.Checks, readiness.Check{Label: rf.Key, Verdict: readiness.VerdictTodo,
				Detail: "not an op:// ref — this file is refs-only; use op://vault/item/field or move it to the non-secret allowlist",
				Todo:   "pix secret set <ENV_VAR> op://vault/item/field"})
		}
	}
	return g
}
