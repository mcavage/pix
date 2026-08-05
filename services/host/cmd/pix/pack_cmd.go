package main

// pack_cmd.go is `pix pack`, plus the composition the pack capability
// deliberately does not do for itself: building the real env, supplying the
// MCP register function, pinning the local-MCP classifier, and turning the
// typed error a pack verb returns into an exit code.
//
// What is NOT here: pack's trust and service admission. `use`/`add mcp` still
// go through the same Tier-1 host bill-of-materials gate, the same
// fingerprint, and the same host-state acceptance store; kong decides the
// grammar and nothing else.

import (
	"errors"
	"fmt"

	"pix/host/cli"
	"pix/host/workflow/pack"
)

// packRun is the ONE place a pack operation's typed error becomes an exit
// code: pack returns errors and writes to the injected writer, this layer owns
// the streams and the codes. A cli.UsageError is a bad invocation (bare message
// on stderr, exit 2); anything else is a failed operation ("pix pack <verb>:
// <err>" on the verb's OWN stream, exit 1). Both come back SILENT so the
// root's "pix: %v" does not say it twice.
func packRun(d *cli.Deps, verb string, err error) error {
	if err == nil {
		return nil
	}
	var ue cli.UsageError
	if errors.As(err, &ue) {
		fmt.Fprintln(d.Err, ue.Error())
		return cli.SilentError{Code: 2}
	}
	fmt.Fprintf(d.Out, "pix pack %s: %v\n", verb, err)
	return cli.SilentError{Code: 1}
}

// init supplies the real classifier PackLocalMCP defaults to a safe no-op for:
// only the composition root can build a real env, so only it can answer "is
// this MCP server local" — pack asks, cmd/pix answers.
func init() {
	pack.PackLocalMCP = func() func(string) bool {
		env := defaultShellEnv()
		return pack.LocalMCPClassifier(env, env.HostBinary)
	}
}

// packCmd is a child of the kong root; bare `pix pack` is `pack ls`.
func (c *packCmd) Help() string {
	return `A pack is your context: a git-backed bundle of skills, knowledge, MCP
integrations, proxy wrappers and config. See docs/design/packs.md.

Adopting a pack that runs HOST code (a local MCP server, a host wrapper, an
external [[bin]]) halts at a Tier-1 bill-of-materials review ([y/N], default
No). A non-TTY adoption fails closed unless --yes. MCP attach and sandbox bin/
wrappers need a recreate ('pix rm <box>', then 'pix run') to take effect.

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
	return packRun(d, "new", pack.RunPackNew(defaultShellEnv(), d.Out, packPath(c.Path)))
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
	return packRun(d, "add", pack.RunPackAdd(env, d.Out, packAddArgs(c), registerServers))
}

// packAddArgs renders the typed fields into the argv pack's writer still
// takes. pack's own parse stays on purpose: it is load-bearing for the trust
// tests that drive RunPackAdd/RunPackUse directly.
func packAddArgs(c *packAddCmd) []string {
	args := append([]string{c.Kind, c.Name}, packPath(c.Path)...)
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
	return packRun(d, "ls", pack.RunPackLs(d.Out))
}

type packShowCmd struct {
	Path string `arg:"" optional:"" help:"Pack root to inspect (default: the active pack)."`
}

func (c *packShowCmd) Run(d *cli.Deps) error {
	return packRun(d, "show", pack.RunPackShow(defaultShellEnv(), d.Out, packPath(c.Path)))
}

// packUseCmd sets the active pack. "default" is a built-in alias for the
// default pack root (not $PWD/default), "personal" a deprecated one; a git URL
// is cloned to ~/.local/share/pix/packs/<name> (optional #ref pin).
type packUseCmd struct {
	Target string `arg:"" help:"path | git-url | default"`
	Yes    bool   `short:"y" help:"Accept the Tier-1 host bill-of-materials review without prompting."`
}

func (c *packUseCmd) Run(d *cli.Deps) error {
	args := []string{c.Target}
	if c.Yes {
		args = append(args, "--yes")
	}
	return packRun(d, "use", pack.RunPackUse(defaultShellEnv(), d.Out, args, registerServers))
}

type packRmCmd struct{}

func (c *packRmCmd) Run(d *cli.Deps) error {
	return packRun(d, "rm", pack.RunPackRm(d.Out, nil))
}

// packPath renders an optional PATH positional as the tail pack's target
// resolver reads; empty means "the default pack root", which pack decides.
func packPath(path string) []string {
	if path == "" {
		return nil
	}
	return []string{path}
}
