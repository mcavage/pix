package main

// secret_cmd.go is `pix secret` under the cli command contract.
//
// It is the clearest case for the migration so far, because almost everything
// it replaced was arity checking. The old dispatcher spent forty lines on two
// switch statements — one to validate `len(rest)` per subcommand and print a
// bespoke "want exactly N arguments (got %d)" message, another to actually
// dispatch — plus a hand-written usage constant listing the same argument
// counts a third time. All three had to agree, and nothing made them.
//
// Here the argument count IS the struct. `Ref string \`arg:""\`` means exactly
// one, required, named in generated help, with kong producing the error.

import (
	"pix/host/cli"
	"pix/host/secret"
)

const secretDescription = `Provider credentials, as 1Password references.

Pix never stores a secret value: op-refs.env maps ENV_VAR to an op:// reference,
and the value is resolved just-in-time when a host MCP server is spawned. The
secret never touches disk or the sandbox.`

// SecretCmd is a child of the kong root; the verb tree, its arities and its
// usage are these tags, and the behaviour lives in pix/host/secret.
func (c *SecretCmd) Help() string { return secretDescription }

type SecretCmd struct {
	Ls    SecretLsCmd    `cmd:"" default:"1" help:"List configured references and whether they resolve."`
	Set   SecretSetCmd   `cmd:"" help:"Point an environment variable at a 1Password reference. (WRITES)"`
	Rm    SecretRmCmd    `cmd:"" help:"Remove a reference. (WRITES)"`
	Check SecretCheckCmd `cmd:"" help:"Resolve every reference and report which fail."`
	Sync  SecretSyncCmd  `cmd:"" help:"Reconcile provider keys into sbx. (WRITES)"`
}

type SecretLsCmd struct{}

func (c *SecretLsCmd) Run(d *cli.Deps) error {
	secret.RunSecretLs(defaultShellEnv(), d.Out)
	return nil
}

// SecretSetCmd takes exactly two arguments because it declares exactly two.
// The old version counted them by hand and produced its own error.
type SecretSetCmd struct {
	EnvVar string `arg:"" help:"Environment variable name (e.g. ANTHROPIC_API_KEY)."`
	Ref    string `arg:"" help:"1Password reference (op://vault/item/field)."`
}

func (c *SecretSetCmd) Run(d *cli.Deps) error {
	// A returned error is exit 1: a mirror failure must never leave the CLI
	// exiting 0 while quietly reporting a shortfall.
	return secret.RunSecretSet(defaultShellEnv(), d.Out, c.EnvVar, c.Ref)
}

type SecretRmCmd struct {
	EnvVar string `arg:"" help:"Environment variable name to stop referencing."`
}

func (c *SecretRmCmd) Run(d *cli.Deps) error {
	return secret.RunSecretRm(defaultShellEnv(), d.Out, c.EnvVar)
}

type SecretCheckCmd struct{}

func (c *SecretCheckCmd) Run(d *cli.Deps) error {
	secret.RunSecretCheck(defaultShellEnv(), d.Out)
	return nil
}

type SecretSyncCmd struct{}

func (c *SecretSyncCmd) Run(d *cli.Deps) error {
	secret.RunSecretSync(defaultShellEnv(), d.Out)
	return nil
}
