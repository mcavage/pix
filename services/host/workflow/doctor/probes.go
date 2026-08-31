package doctor

import (
	"context"
	"time"

	"pix/host/config"
	"pix/host/health"
	"pix/host/hostenv"
	"pix/host/inference"
	"pix/host/secret"
)

// probes.go is where a config becomes a list of things to go and check. It is
// the whole of doctor's and status's knowledge of WHAT a healthy host is; both
// verbs render the same Snapshot, so they cannot disagree about the same host.
//
// The probes themselves live in health: they cross real boundaries (exec a
// binary, dial a port, stat a directory) and classify what they find. Nothing
// here interprets a result; that is the model's job.

// Options are the seams. The zero value is what the CLI uses; tests point the
// binaries at fixtures and the roots at temp dirs, so every probe still runs
// its real code path against a real process, socket or file.
type Options struct {
	// Budget bounds a single probe. Zero picks health.DefaultBudget.
	Budget time.Duration
	// SbxBin and KeyStoreBin name the executables. Empty means the real ones,
	// found on PATH.
	SbxBin      string
	KeyStoreBin string
	// SbxArgs and KeyStoreArgs override each probe's argv (defaults:
	// `--version`, `secret ls`). They are the seam the tests drive: a probe
	// still execs a REAL process, it is just pointed at a fixture that can be
	// made to fail on purpose.
	SbxArgs      []string
	KeyStoreArgs []string
	// Env is the host environment a probe may need (e.g. to ask sbx for the
	// GitHub secret's scope).
	Env hostenv.Env
	// GitHubScope overrides how the github row is answered. Nil means read
	// this PIX_HOME's configured refs.
	GitHubScope func() (int, []string)
	// GlobalSecrets overrides the read-only enumeration of host-global sbx
	// secrets Pix ignores. Nil means ask sbx.
	GlobalSecrets func() ([]string, bool)
	// Workspace is the directory whose sandbox the attachment answer is
	// about. Empty means "no sandbox context", and attachment is unknown.
	Workspace string
}

// Probes builds the host's probe set, in the order a report reads best:
// the CLI everything else needs, the pack that shapes the session, the keys
// that let a model answer, then the host service and the agent that keeps it
// alive.
//
// Every probe is included unconditionally. A capability the host has not
// enabled is reported as OPTIONAL, never omitted: a missing line is a fact a
// reader cannot see.
func Probes(cfg *config.Config, o Options) []health.Probe {
	sbxBin := orElse(o.SbxBin, "sbx")
	keyBin := orElse(o.KeyStoreBin, sbxBin)
	keyArgs := o.KeyStoreArgs
	if len(keyArgs) == 0 {
		keyArgs = []string{"secret", "ls"}
	}
	return []health.Probe{
		health.SbxProbe{Bin: sbxBin, Args: o.SbxArgs},
		// The model keys are ANY-OF: one of anthropic/openai/google is enough
		// to launch, which is the same definition `run`'s launch gate uses.
		// Reporting the other two as gaps would hand a working host two repair
		// commands it does not need.
		// Callable closes the gap between "the key store has a key" and "the router
		// can call that vendor": those are different facts, and only reporting the
		// first is how a host with three green providers routes every role to one.
		// Keyless is the prior question to both: whether a key is this host's
		// credential at all.
		health.ProviderKeyProbe{Bin: keyBin, Args: keyArgs, Want: secret.ModelProviders, AnyOf: true,
			Label: "providers", Callable: inference.CallableProviders(cfg), Keyless: inference.KeylessBackends(cfg)},
		// A sandbox holds no GitHub credential of its own, so without a global
		// secret the agent commits and then cannot push. Reported here rather
		// than discovered at the end of a task.
		health.GitHubSecretProbe{Fix: secret.GitHubSecretFix, Scope: githubScope(o)},
		// Read-only, and deliberately not a repair: a host can hold GLOBAL sbx
		// secrets (an older pix, another stack, a hand-run `sbx secret set`)
		// that Pix ignores entirely. Saying so is the difference between "my
		// key is right there" and understanding why it does nothing.
		health.IgnoredGlobalSecretsProbe{Fix: secret.GlobalSecretRemoveFix, Scan: globalSecretScan(o)},
	}
}

// githubScope resolves the GitHub credential's scope, through the Options seam
// when a test supplies one. Tests cannot ask the real sbx: the answer would be
// whatever the developer's own machine happens to hold.
func githubScope(o Options) func() (int, []string) {
	if o.GitHubScope != nil {
		return o.GitHubScope
	}
	return func() (int, []string) {
		state, boxes := secret.ProbeGitHubCredential(o.Env)
		return int(state), boxes
	}
}

// globalSecretScan enumerates the global secrets Pix ignores, through the
// Options seam when a test supplies one.
func globalSecretScan(o Options) func() ([]string, bool) {
	if o.GlobalSecrets != nil {
		return o.GlobalSecrets
	}
	return func() ([]string, bool) { return secret.ProbeGlobalSecrets(o.Env) }
}

// Check runs the whole set and returns the one Snapshot both verbs render.
func Check(ctx context.Context, cfg *config.Config, o Options) health.Snapshot {
	return health.Run(ctx, o.Budget, Probes(cfg, o)...)
}

func orElse(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
