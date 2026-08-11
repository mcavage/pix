package main

// secret_cmd.go is `pix secret` under the cli command contract: the argument
// count IS the struct. `Ref string \`arg:""\`` means exactly one, required,
// named in generated help, with kong producing the arity error.

import (
	"fmt"
	"strings"

	"pix/host/cli"
	"pix/host/packinfo"
	"pix/host/secret"
	"pix/host/workspace"
)

// secretDescription is GENERATED from the key registries, never hand-listed.
// Naming one key in prose ("PARALLEL_API_KEY buys web search") makes that key
// look like a special case and goes stale the moment a second one is added; the
// distinction that actually matters is the CATEGORY, and the members are data.
func secretDescription() string {
	var b strings.Builder
	b.WriteString(`API keys, as 1Password references.

Pix never stores a secret value: op-refs.env maps ENV_VAR to an op:// reference,
and the value is resolved just-in-time. The secret never touches disk or the
sandbox.

Two categories live here. They are stored and resolved identically; what differs
is what they buy.

`)
	fmt.Fprintf(&b, "  model keys  %s\n", strings.Join(envVarsOf(secret.ProviderKeyRefOrder), ", "))
	b.WriteString("              A vendor you can route models to. Wire one with\n")
	b.WriteString("              'pix models add <provider>'; at least one is needed to launch.\n\n")
	fmt.Fprintf(&b, "  tool keys   %s\n", strings.Join(envVarsOf(secret.ToolKeyRefOrder), ", "))
	b.WriteString("              A capability the agent calls. Nothing routes to these,\n")
	b.WriteString("              'pix models add' does not take them, and a missing one\n")
	b.WriteString("              degrades that capability instead of blocking a launch.\n\n")
	b.WriteString("Keys held directly by the sandbox runtime (GitHub, for example) are set with\n")
	b.WriteString("'sbx secret set', not here.")
	return b.String()
}

func envVarsOf(refs []secret.ProviderKeyRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.EnvVar)
	}
	return out
}

// SecretCmd is a child of the kong root; the verb tree, its arities and its
// usage are these tags, and the behaviour lives in pix/host/secret.
func (c *SecretCmd) Help() string { return secretDescription() }

type SecretCmd struct {
	Ls    SecretLsCmd    `cmd:"" default:"1" help:"List configured references and whether they resolve."`
	Set   SecretSetCmd   `cmd:"" help:"Point an environment variable at a 1Password reference. (WRITES)"`
	Rm    SecretRmCmd    `cmd:"" help:"Remove a reference. (WRITES)"`
	Check SecretCheckCmd `cmd:"" help:"Resolve every reference and report which fail."`
	Sync  SecretSyncCmd  `cmd:"" help:"Reconcile keys into sbx. (WRITES)"`
}

type SecretLsCmd struct{}

func (c *SecretLsCmd) Run(d *cli.Deps) error {
	secret.RunSecretLs(defaultShellEnv(), d.Out, packNonSecretEnv())
	return nil
}

// SecretSetCmd takes exactly two arguments because it declares exactly two.
type SecretSetCmd struct {
	EnvVar string `arg:"" help:"Environment variable name (e.g. ANTHROPIC_API_KEY)."`
	Ref    string `arg:"" help:"1Password reference (op://vault/item/field)."`
}

func (c *SecretSetCmd) Run(d *cli.Deps) error {
	// A returned error is exit 1: a mirror failure must never leave the CLI
	// exiting 0 while quietly reporting a shortfall.
	return secret.RunSecretSet(defaultShellEnv(), d.Out, c.EnvVar, c.Ref, packNonSecretEnv())
}

type SecretRmCmd struct {
	EnvVar string `arg:"" help:"Environment variable name to stop referencing."`
}

func (c *SecretRmCmd) Run(d *cli.Deps) error {
	return secret.RunSecretRm(defaultShellEnv(), d.Out, c.EnvVar)
}

type SecretCheckCmd struct{}

// A returned error is the capability's own exit code (3 when it could not
// check at all, 1 when a ref failed to resolve), already worded on d.Out.
func (c *SecretCheckCmd) Run(d *cli.Deps) error {
	return secret.RunSecretCheck(defaultShellEnv(), d.Out)
}

type SecretSyncCmd struct{}

func (c *SecretSyncCmd) Run(d *cli.Deps) error {
	return secret.RunSecretSync(defaultShellEnv(), d.Out)
}

// packNonSecretEnv resolves which env vars the ACTIVE PACK authorized to carry
// a literal value in op-refs.env. Composition, not policy: only this layer can
// load config and the pack, and secret must not reach for either. A host with
// no pack gets nil, which means "everything here must be an op:// ref" — the
// right default, and the one that cannot be widened by accident.
func packNonSecretEnv() secret.NonSecret {
	cfg, _, err := workspace.LoadResolvedConfig()
	if err != nil {
		return nil
	}
	return packinfo.ActiveNonSecretEnvNames(cfg)
}
