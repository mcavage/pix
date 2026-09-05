// secret_cmd.go — `pix secret`: list | set | rm | check (docs/design/
// pix-v2-surface.md §3.5). It manages `~/.pix/secrets.env` references only;
// values are never stored, printed, or returned. Behavior lives in
// pix/host/secret's pixhome.go file (LoadRefs/SetRef/RemoveRef/CheckRef).
package main

import (
	"fmt"
	"os/exec"
	"strings"

	"pix/host/cli"
	"pix/host/pixhome"
	"pix/host/secret"
)

const secretDescriptionV2 = `API keys, as 1Password references, under ~/.pix/secrets.env.

Pix never stores a secret value: secrets.env maps ENV_VAR to an op://
reference, and the value is resolved just-in-time through 'op'. The secret
never touches disk or the sandbox beyond that reference.`

func (c *SecretCmd) Help() string { return secretDescriptionV2 }

// SecretCmd is a child of the kong root; bare 'pix secret' is 'secret ls'.
type SecretCmd struct {
	Ls    SecretLsCmd    `cmd:"" default:"1" help:"List configured references and whether each is well-formed."`
	Set   SecretSetCmd   `cmd:"" help:"Point an environment variable at an op:// reference. (WRITES)"`
	Rm    SecretRmCmd    `cmd:"" help:"Remove a reference. (WRITES)"`
	Check SecretCheckCmd `cmd:"" help:"Resolve every reference (or one) through op, without printing values."`
}

func secretHome() (pixhome.Paths, error) {
	home, err := pixhome.Resolve()
	if err != nil {
		return pixhome.Paths{}, err
	}
	return home, nil
}

type SecretLsCmd struct{}

func (c *SecretLsCmd) Run(d *cli.Deps) error {
	home, err := secretHome()
	if err != nil {
		return err
	}
	refs, err := secret.LoadRefs(home)
	if err != nil {
		return err
	}
	if len(refs) == 0 {
		fmt.Fprintln(d.Out, "No secrets configured. Set one with: pix secret set NAME op://vault/item/field")
		return nil
	}
	for _, r := range refs {
		state := "ok"
		if !r.IsRef {
			state = "NOT an op:// reference"
		}
		fmt.Fprintf(d.Out, "%s\t%s\t%s\n", r.Key, r.Value, state)
	}
	return nil
}

type SecretSetCmd struct {
	EnvVar string `arg:"" help:"Environment variable name (e.g. ANTHROPIC_API_KEY)."`
	Ref    string `arg:"" help:"1Password reference (op://vault/item/field)."`
}

func (c *SecretSetCmd) Run(d *cli.Deps) error {
	home, err := secretHome()
	if err != nil {
		return err
	}
	if err := secret.SetRef(home, c.EnvVar, c.Ref); err != nil {
		return err
	}
	fmt.Fprintf(d.Out, "pix: %s -> %s\n", c.EnvVar, c.Ref)
	return nil
}

type SecretRmCmd struct {
	EnvVar string `arg:"" help:"Environment variable name to stop referencing."`
}

func (c *SecretRmCmd) Run(d *cli.Deps) error {
	home, err := secretHome()
	if err != nil {
		return err
	}
	if err := secret.RemoveRef(home, c.EnvVar); err != nil {
		return err
	}
	fmt.Fprintf(d.Out, "pix: removed %s\n", c.EnvVar)
	return nil
}

type SecretCheckCmd struct {
	Name string `arg:"" optional:"" help:"Check only this reference's env var (default: all configured)."`
}

// opReader shells out to `op read <ref>` with stdout discarded — the value
// is never captured by this process, only success/failure.
type opReader struct{}

func (opReader) ReadRef(ref string) error {
	cmd := exec.Command("op", "read", ref)
	cmd.Stdout = nil
	return cmd.Run()
}

func (c *SecretCheckCmd) Run(d *cli.Deps) error {
	home, err := secretHome()
	if err != nil {
		return err
	}
	refs, err := secret.LoadRefs(home)
	if err != nil {
		return err
	}
	if _, lerr := exec.LookPath("op"); lerr != nil {
		fmt.Fprintln(d.Err, "pix secret check: `op` is not installed; cannot resolve references.")
		return cli.SilentError{Code: 3}
	}
	failed := 0
	checked := 0
	for _, r := range refs {
		if c.Name != "" && !strings.EqualFold(r.Key, c.Name) {
			continue
		}
		checked++
		err := secret.CheckRef(opReader{}, r.Value)
		if err != nil {
			failed++
			fmt.Fprintf(d.Out, "%s\tFAIL\t%v\n", r.Key, err)
		} else {
			fmt.Fprintf(d.Out, "%s\tok\n", r.Key)
		}
	}
	if c.Name != "" && checked == 0 {
		return fmt.Errorf("no secret named %q is configured", c.Name)
	}
	if failed > 0 {
		return cli.SilentError{Code: 1}
	}
	return nil
}
