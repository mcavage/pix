package main

// config_cmd.go is `pix config`: the subcommand tree, its arguments and its
// per-command help are the struct tags, so kong renders `pix config set --help`
// from the SAME declaration that parses the command. No config domain logic lives
// here — the key table, arity and validation stay in provision.

import (
	"fmt"

	"github.com/BurntSushi/toml"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/service"
	"pix/host/workflow/provision"
)

// configDescription is the verb's long help: the mental model generated usage
// cannot infer, plus ConfigKeysHelp rather than a second copy of the keys.
func configDescription() string {
	return `The single managed config: one config.toml, mutated only through
'pix config set'/'unset' (never hand-edited — see AGENTS.md).

'get' is the machine-readable read half: one resolved value, no decoration,
so scripts (and the Makefile's operational targets) source config from here
instead of a second config file.

` + provision.ConfigKeysHelp
}

// configCmd is the verb tree. Bare `pix config` is `show`, so Show is kong's
// default command rather than a subcommand a user must know.
func (c *configCmd) Help() string { return configDescription() }

type configCmd struct {
	Show  configShowCmd  `cmd:"" default:"1" help:"Print the resolved config path + contents."`
	Path  configPathCmd  `cmd:"" help:"Print the config file path (or the op-refs.env path)."`
	Get   configGetCmd   `cmd:"" help:"Print ONE resolved value, no decoration; for scripts to source."`
	Set   configSetCmd   `cmd:"" help:"Set a config key (never hand-edit the toml). (WRITES)"`
	Unset configUnsetCmd `cmd:"" help:"Reset/clear a scalar key, or remove one value from a list key. (WRITES)"`
}

// ── show ─────────────────────────────────────────────────────────────────

type configShowCmd struct{}

func (c *configShowCmd) Run(d *cli.Deps) error {
	cfg, err := d.Config()
	if err != nil {
		return err
	}
	fmt.Fprintf(d.Out, "# path: %s\n", config.Path())
	return toml.NewEncoder(d.Out).Encode(cfg)
}

// ── path ─────────────────────────────────────────────────────────────────

// configPathCmd takes one optional positional because `pix config path op-refs` is
// sugar on the same noun, not a distinct switch. Kind is validated by hand, not
// `enum:""`, because the empty default must also be valid.
type configPathCmd struct {
	Kind string `arg:"" optional:"" help:"'op-refs' to print the op-refs.env path instead of config.toml." placeholder:"op-refs"`
}

func (c *configPathCmd) Run(d *cli.Deps) error {
	switch c.Kind {
	case "":
		fmt.Fprintln(d.Out, config.Path())
	case "op-refs":
		fmt.Fprintln(d.Out, config.OpRefsPath())
	default:
		return cli.Usagef("unexpected argument %q (want: op-refs)", c.Kind)
	}
	return nil
}

// ── get ──────────────────────────────────────────────────────────────────

// configGetCmd prints ONE resolved value with no decoration — the accessor the
// Makefile shells out to. provision.ConfigValue owns the key table (including the
// retired-key refusals).
type configGetCmd struct {
	Key string `arg:"" help:"Config key (see 'pix config --help' for the list)."`
}

func (c *configGetCmd) Help() string { return provision.ConfigKeysHelp }

func (c *configGetCmd) Run(d *cli.Deps) error {
	cfg, err := d.Config()
	if err != nil {
		return err
	}
	val, err := provision.ConfigValue(cfg, c.Key)
	if err != nil {
		return cli.Usagef("%v", err)
	}
	fmt.Fprintln(d.Out, val)
	return nil
}

// ── set / unset ──────────────────────────────────────────────────────────

// configSetCmd and configUnsetCmd both take a key plus zero-or-more trailing
// values: the per-key arity contract is provision.ApplyConfigChange's, so both
// trailing arities are just `[]string` rather than duplicated domain logic.
type configSetCmd struct {
	Key    string   `arg:"" help:"Config key."`
	Values []string `arg:"" optional:"" help:"Value for the key (see 'pix config --help' for the list)."`
}

func (c *configSetCmd) Help() string { return provision.ConfigKeysHelp }

func (c *configSetCmd) Run(d *cli.Deps) error {
	return runConfigChange(d, false, c.Key, c.Values)
}

type configUnsetCmd struct {
	Key    string   `arg:"" help:"Config key."`
	Values []string `arg:"" optional:"" help:"Value to remove, for a list key (mcp/services)."`
}

func (c *configUnsetCmd) Help() string { return provision.ConfigKeysHelp }

func (c *configUnsetCmd) Run(d *cli.Deps) error {
	return runConfigChange(d, true, c.Key, c.Values)
}

// runConfigChange loads the config, applies a set/unset, Save()s it, prints the new
// value + path, then propagates to a running serve when the key is daemon-
// affecting. It owns exactly two things: wiring Deps.Out/Save into provision's pure
// ApplyConfigChange, and mapping its error to a usage failure (exit 2).
func runConfigChange(d *cli.Deps, unset bool, key string, values []string) error {
	cfg, err := d.Config()
	if err != nil {
		return err
	}
	summary, err := provision.ApplyConfigChange(cfg, unset, key, values)
	if err != nil {
		return cli.Usagef("%v", err)
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Fprintf(d.Out, "%s\n# saved to %s\n", summary, config.Path())
	// A daemon-affecting key only takes effect when serve restarts — do that for the
	// user, per the detected lifecycle mode.
	if service.IsDaemonAffecting(key) {
		service.PropagateConfig(service.DefaultReloader(d.Err), d.Out)
	}
	return nil
}
