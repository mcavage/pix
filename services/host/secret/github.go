package secret

import (
	"strings"

	"pix/host/hostenv"
)

// GitHub credentials come from this Pix home; global secrets are diagnostic only.

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
