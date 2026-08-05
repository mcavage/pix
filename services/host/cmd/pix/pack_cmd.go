package main

// pack_cmd.go is `pix pack`, plus the composition the pack capability
// deliberately does not do for itself: building the real env, supplying the
// MCP register function, and pinning the local-MCP classifier.
//
// What is NOT here: pack's trust and service admission. `use`/`add mcp` still
// go through the same Tier-1 host bill-of-materials gate, the same
// fingerprint, and the same host-state acceptance store; kong decides the
// grammar and nothing else.

import (
	"pix/host/cli"
	"pix/host/workflow/pack"
)

// init supplies the real classifier PackLocalMCP defaults to a safe no-op for.
// Only the composition root can build a real env, so only it can answer "is
// this MCP server local"; pack asks the question and cmd/pix owns the answer.
func init() {
	pack.PackLocalMCP = func() func(string) bool {
		env := defaultShellEnv()
		return pack.LocalMCPClassifier(env, env.HostBinary)
	}
}

// packCmd is a child of the kong root. Bare `pix pack` is `pack ls`, which is
// what the switch's default arm did.
func (c *packCmd) Help() string {
	return `A pack is your context: a git-backed bundle of skills, knowledge, MCP
integrations, proxy wrappers and config. See docs/design/packs.md.

Adopting a pack that runs HOST code (a local MCP server, a host wrapper, an
external [[bin]]) halts at a Tier-1 bill-of-materials review ([y/N], default
No). A non-TTY adoption fails closed unless --yes. MCP attach and sandbox bin/
wrappers need a recreate ('pix run --replace') to take effect.

Paths default to the default pack root (~/.local/share/pix/default); every
"add" implicit-creates the pack it writes into.`
}

type packCmd struct {
	New  packNewCmd  `cmd:"" help:"Adopt a repo as a pack, or git-init a fresh one. (WRITES)"`
	Add  packAddCmd  `cmd:"" help:"Add a skill | knowledge | proxy | mcp artifact. (WRITES)"`
	Ls   packLsCmd   `cmd:"" default:"1" help:"Show the active pack."`
	Show packShowCmd `cmd:"" help:"Inspect a pack (default: the active one)."`
	Use  packUseCmd  `cmd:"" help:"Set the active pack — one transaction, Tier-1 gated. (WRITES)"`
	Rm   packRmCmd   `cmd:"" help:"Detach the active pack (files untouched). (WRITES)"`
}

type packNewCmd struct {
	Path string `arg:"" optional:"" help:"Pack root (default: the default pack root)."`
}

func (c *packNewCmd) Run(d *cli.Deps) error {
	pack.RunPackNew(defaultShellEnv(), d.Out, packPath(c.Path))
	return nil
}

// packAddCmd writes one artifact into a pack. The kind is an `enum`, so an
// unknown kind is kong's error against the list the code implements.
type packAddCmd struct {
	Kind string `arg:"" enum:"skill,knowledge,proxy,mcp" help:"skill | knowledge | proxy | mcp"`
	Name string `arg:"" help:"Artifact name (letters, digits, -, _, . only)."`
	Path string `arg:"" optional:"" help:"Pack root (default: the default pack root)."`
	Host bool   `help:"proxy only: a HOST-mode wrapper (Tier-1, on PATH for 'pix host' only)."`
	Env  string `help:"mcp only: the op-refs.env credential variable this server needs." placeholder:"VAR"`
	Yes  bool   `short:"y" help:"Accept the Tier-1 host bill-of-materials review without prompting."`
}

func (c *packAddCmd) Run(d *cli.Deps) error {
	env := defaultShellEnv()
	pack.RunPackAdd(env, d.Out, packAddArgs(c), registerServers)
	return nil
}

// packAddArgs renders the typed fields into the argv pack's writer still
// takes. pack's own parse stays on purpose: it is load-bearing for the trust
// tests that drive RunPackAdd/RunPackUse directly.
func packAddArgs(c *packAddCmd) []string {
	args := []string{c.Kind, c.Name}
	if c.Path != "" {
		args = append(args, c.Path)
	}
	if c.Host {
		args = append(args, "--host")
	}
	if c.Env != "" {
		args = append(args, "--env", c.Env)
	}
	if c.Yes {
		args = append(args, "--yes")
	}
	return args
}

type packLsCmd struct{}

func (c *packLsCmd) Run(d *cli.Deps) error {
	pack.RunPackLs(d.Out)
	return nil
}

type packShowCmd struct {
	Path string `arg:"" optional:"" help:"Pack root to inspect (default: the active pack)."`
}

func (c *packShowCmd) Run(d *cli.Deps) error {
	pack.RunPackShow(defaultShellEnv(), d.Out, packPath(c.Path))
	return nil
}

// packUseCmd sets the active pack. "default" is a built-in alias for the
// default pack root (not $PWD/default); "personal" still works as a deprecated
// alias. A git URL is cloned to ~/.local/share/pix/packs/<name> (optional #ref
// pin).
type packUseCmd struct {
	Target string `arg:"" help:"path | git-url | default"`
	Yes    bool   `short:"y" help:"Accept the Tier-1 host bill-of-materials review without prompting."`
}

func (c *packUseCmd) Run(d *cli.Deps) error {
	args := []string{c.Target}
	if c.Yes {
		args = append(args, "--yes")
	}
	pack.RunPackUse(defaultShellEnv(), d.Out, args, registerServers)
	return nil
}

type packRmCmd struct{}

func (c *packRmCmd) Run(d *cli.Deps) error {
	pack.RunPackRm(d.Out, nil)
	return nil
}

// packPath renders an optional PATH positional as the tail pack's target
// resolver reads. An empty one means "the default pack root", which pack
// decides, not this file.
func packPath(path string) []string {
	if path == "" {
		return nil
	}
	return []string{path}
}
