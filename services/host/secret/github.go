package secret

import (
	"strings"

	"pix/host/hostenv"
)

// github.go — is a GitHub credential available to EVERY sandbox?
//
// A sandbox holds no GitHub credential of its own. Without one the agent can
// commit inside the box and cannot push or open a pull request, which it
// discovers at the end of a task rather than the start.
//
// Scope is the whole subtlety. `sbx secret set github` stores a GLOBAL service
// secret that every sandbox reads; `--sandbox <name>` pins it to one box. Both
// appear in `sbx secret ls`, so a substring search for "github" reports a
// credential that only one sandbox can actually use. That is exactly the state
// this host was in when an agent failed to push while pix's own host state told
// it github was available.
//
// So the question is asked with sbx's own filter, `--global`, rather than by
// parsing a SCOPE column whose formatting is not ours to depend on.

// GitHubSecretState is why a sandbox does or does not have a GitHub credential.
type GitHubSecretState int

const (
	// GitHubSecretUnknown: sbx could not answer. Never reported as a gap, for
	// the same reason a failed key probe is not "you have no key".
	GitHubSecretUnknown GitHubSecretState = iota
	// GitHubSecretGlobal: every sandbox has it, including ones created later.
	GitHubSecretGlobal
	// GitHubSecretScoped: stored, but only for specific sandboxes.
	GitHubSecretScoped
	// GitHubSecretAbsent: sbx answered and there is no github secret at all.
	GitHubSecretAbsent
)

// GitHubSecretFix is the one command that fixes the absent case: configure the
// ref, and every sandbox Pix launches is given a `github` service secret
// scoped to it (secret.PrepareSandboxSecrets). It does NOT name a global sbx
// secret: Pix neither writes nor reads one.
const GitHubSecretFix = "pix secret set GITHUB_TOKEN op://vault/item/field"

// ConfiguredGitHubRef reports whether THIS PIX_HOME configures a filled
// GITHUB_TOKEN op:// ref — the only thing that decides whether a sandbox Pix
// launches can push.
func ConfiguredGitHubRef(env hostenv.Env) bool {
	_, content, exists := OpRefsContent(env)
	if !exists {
		return false
	}
	r, ok := firstRefsIn(content, map[string]string{GitHubKeyRef.EnvVar: GitHubKeyRef.Name})[GitHubKeyRef.EnvVar]
	return ok && !r.Placeholder
}

// ProbeGitHubCredential answers doctor's github row from the configured refs
// rather than from the host-wide store. GitHubSecretGlobal here means "every
// sandbox PIX launches gets it", which is what a configured ref buys; a
// host-global secret is reported separately, as something Pix ignores
// (ProbeGlobalSecrets).
func ProbeGitHubCredential(env hostenv.Env) (state GitHubSecretState, sandboxes []string) {
	path := DefaultOpRefsPath()
	if _, err := env.ReadFile(path); err != nil && env.IsFile(path) {
		return GitHubSecretUnknown, nil // it is there and unreadable: do not guess
	}
	if ConfiguredGitHubRef(env) {
		return GitHubSecretGlobal, nil
	}
	return GitHubSecretAbsent, nil
}

// GlobalSecretRemoveFix is the exact, OPTIONAL manual removal command for a
// global secret Pix ignores. Nothing in Pix runs it: `pix doctor` reports, it
// never repairs (safety invariant 12), and deleting a credential someone else
// on this host may still be using is not a launcher's call.
const GlobalSecretRemoveFix = "sbx secret rm -g <name>   (optional; Pix never removes it)"

// ProbeGlobalSecrets enumerates the GLOBAL provider/github service secrets
// this host holds — read-only, and only so doctor can say "these exist and Pix
// ignores them". known is false when sbx could not be asked or its answer
// could not be trusted: an unknown enumeration is reported as unknown, never
// as "there are none".
func ProbeGlobalSecrets(env hostenv.Env) (names []string, known bool) {
	if _, err := env.LookPath("sbx"); err != nil {
		return nil, false
	}
	out, timedOut, err := env.RunTimed("sbx", "secret", "ls", "--global")
	if err != nil || timedOut {
		return nil, false
	}
	interesting := map[string]bool{GitHubKeyRef.Name: true}
	for _, p := range ProviderKeyRefOrder {
		interesting[p.Name] = true
	}
	for _, p := range ToolKeyRefOrder {
		interesting[p.Name] = true
	}
	for _, row := range secretRows(out) {
		if interesting[row.name] {
			names = append(names, row.name)
		}
	}
	return names, true
}

// ProbeGitHubSecret classifies the GitHub credential's scope.
//
// Two calls, deliberately. `--global` alone cannot distinguish "no secret" from
// "scoped to a sandbox", and those need different words: the first is a thing
// you have not done, the second is a thing you did that will not carry to the
// next sandbox.
func ProbeGitHubSecret(env hostenv.Env) (state GitHubSecretState, sandboxes []string) {
	if _, err := env.LookPath("sbx"); err != nil {
		return GitHubSecretUnknown, nil
	}
	global, timedOut, err := env.RunTimed("sbx", "secret", "ls", "--global")
	if err != nil || timedOut {
		return GitHubSecretUnknown, nil
	}
	if hasGitHubRow(global) {
		return GitHubSecretGlobal, nil
	}
	all, timedOut, err := env.RunTimed("sbx", "secret", "ls")
	if err != nil || timedOut {
		return GitHubSecretUnknown, nil
	}
	if boxes := gitHubScopes(all); len(boxes) > 0 {
		return GitHubSecretScoped, boxes
	}
	return GitHubSecretAbsent, nil
}

// hasGitHubRow reports whether a `sbx secret ls` table contains a github row.
// It matches the NAME column rather than the line, so a sandbox that happens to
// be called "github-something" cannot be mistaken for the secret.
func hasGitHubRow(out string) bool {
	for _, f := range secretRows(out) {
		if f.name == "github" {
			return true
		}
	}
	return false
}

// gitHubScopes returns the sandbox names a github secret is pinned to.
func gitHubScopes(out string) []string {
	var boxes []string
	for _, f := range secretRows(out) {
		if f.name == "github" && f.scope != "" {
			boxes = append(boxes, f.scope)
		}
	}
	return boxes
}

type secretRow struct{ scope, name string }

// secretRows parses `sbx secret ls`'s table: SCOPE TYPE NAME SECRET. The header
// and sbx's trailing notes are skipped by requiring four fields and a known
// type column, so a formatting note never parses as a credential.
func secretRows(out string) []secretRow {
	var rows []secretRow
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] == "SCOPE" {
			continue
		}
		if fields[1] != "service" && fields[1] != "registry" {
			continue
		}
		rows = append(rows, secretRow{scope: fields[0], name: fields[2]})
	}
	return rows
}
