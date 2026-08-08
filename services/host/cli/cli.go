// Package cli is the command contract: how a pix verb is declared, what it is
// given, and who owns the exit code.
//
// The contract, in three rules:
//
//  1. A command RETURNS an error. It never calls os.Exit. main owns the single
//     exit point and the single error renderer, so exit codes are one table
//     rather than 266 decisions.
//  2. A command writes to Deps.Out / Deps.Err, never to os.Stdout / os.Stderr.
//     That is what makes output assertable without a subprocess.
//  3. Flags and args are STRUCT FIELDS with tags. Usage is generated from the
//     same declaration that parses them, so it cannot describe a flag that does
//     not exist, or omit one that does.
//
// One root parses everything (RunRoot): a verb that has not been migrated yet
// declares a passthrough tail and hands its own seam the argv verbatim, so the
// tree can absorb a verb at a time without a flag day.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/alecthomas/kong"
	"golang.org/x/term"

	"pix/host/config"
	"pix/host/sys"
)

// Deps is everything a command may reach. It is a struct rather than a
// context.Context value because a command should not have to know a key to find
// its dependencies, and because the compiler should reject a command that wants
// something the process does not provide.
type Deps struct {
	// Sys is every OS seam. A command that needs less should still take Deps —
	// the narrowing happens at the function it calls, not at the verb boundary.
	Sys sys.System
	// Out is the command's stdout. Machine-readable output goes here and nowhere
	// else, so `--json` can never be polluted by a stray progress line.
	Out io.Writer
	// Err is the command's stderr: progress, warnings, and usage on the error
	// path.
	Err io.Writer
	// In is the command's stdin, for prompts.
	In io.Reader
	// Interactive reports whether In is a terminal. Commands must consult this
	// rather than probing os.Stdin, or a piped run behaves differently under
	// test than in production.
	Interactive bool

	// cfg is loaded lazily and memoized: most commands need it, a few (version,
	// help) must work when it is missing or corrupt, and loading it eagerly in
	// main would make those fail for an unrelated reason.
	cfg    *config.Config
	cfgErr error
}

// Config loads config.toml once per command. The error is returned rather than
// fatal so a command can decide whether it can proceed without one.
func (d *Deps) Config() (*config.Config, error) {
	if d.cfg == nil && d.cfgErr == nil {
		d.cfg, d.cfgErr = config.Load()
	}
	return d.cfg, d.cfgErr
}

// SetConfig injects a config, for tests and for commands that have already
// mutated one in memory.
func (d *Deps) SetConfig(c *config.Config) { d.cfg, d.cfgErr = c, nil }

// UsageError marks an error as the user's mistake rather than a failure:
// exit 2, and print usage. A plain error is exit 1 with no usage, because
// dumping a usage screen after a network timeout tells the user nothing.
type UsageError struct{ Err error }

func (e UsageError) Error() string { return e.Err.Error() }
func (e UsageError) Unwrap() error { return e.Err }

// Usagef builds a UsageError.
func Usagef(format string, a ...any) error {
	return UsageError{Err: fmt.Errorf(format, a...)}
}

// SilentError carries an exit code but no message, for a command that has
// already reported the problem in its own words (a rendered doctor table, say).
type SilentError struct{ Code int }

func (e SilentError) Error() string { return fmt.Sprintf("exit %d", e.Code) }

// ExitCode is the ONE place a Go error becomes a process exit code.
//
//	0  success
//	1  the command failed
//	2  the user's invocation was wrong (usage printed)
//	n  whatever a SilentError asked for
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var silent SilentError
	if errors.As(err, &silent) {
		return silent.Code
	}
	var usage UsageError
	if errors.As(err, &usage) {
		return 2
	}
	return 1
}

// RunRoot parses argv against the ROOT command tree T — the whole verb table —
// and runs the selected leaf. Two properties a root must have:
//
//   - help goes to Deps.Out, because `pix ls --help` is a successful answer to
//     a question, not a diagnostic;
//   - a help request for the root ITSELF prints rootHelp verbatim, when one is
//     given. The tiered landing screen is a curated, deliberately short
//     document. Pass "" to get kong's own generated listing instead — that is
//     the `help --all` tier, generated from the same tags that parse argv.
//
// kong's default exits the process on --help and on a parse error. Both are
// errors here instead: this package owns no exit.
func RunRoot[T any](name, description, rootHelp string, argv []string, d *Deps) error {
	var cmd T
	parser, err := kong.New(&cmd,
		kong.Name(name),
		kong.Description(description),
		kong.Writers(d.Out, d.Err),
		kong.Exit(func(int) { panic(errHelpRequested) }),
		kong.Help(func(o kong.HelpOptions, ctx *kong.Context) error {
			if ctx.Selected() == nil && rootHelp != "" {
				fmt.Fprint(d.Out, rootHelp)
				return nil
			}
			return kong.DefaultHelpPrinter(o, ctx)
		}),
	)
	if err != nil {
		return fmt.Errorf("internal: building the %s parser: %w", name, err)
	}
	ctx, err := parse(parser, argv)
	if err != nil || ctx == nil {
		return err // ctx == nil means --help was printed
	}
	return ctx.Run(d)
}

// errHelpRequested is the sentinel kong's exit hook panics with. Recovering a
// panic is not a style anyone enjoys, but kong offers no other way to intercept
// its terminal paths, and confining the ugliness to this one function is better
// than letting every verb inherit an os.Exit it cannot test.
var errHelpRequested = errors.New("help requested")

func parse(parser *kong.Kong, argv []string) (ctx *kong.Context, err error) {
	defer func() {
		if r := recover(); r != nil {
			if r == errHelpRequested {
				ctx, err = nil, nil // --help printed; exit 0
				return
			}
			panic(r)
		}
	}()
	c, perr := parser.Parse(argv)
	if perr != nil {
		return nil, UsageError{Err: perr}
	}
	return c, nil
}

// ── shared launcher primitives ──────────────────────────────────────────────
//
// These are the handful of helpers every verb needed, which is why they showed
// up as an inbound dependency of every domain measured for extraction
// (scripts/extract-pkg). They live here rather than in a grab-bag `util`
// package because each one is about presenting a command to a user, which is
// what this package is for.

// WantsHelp reports whether argv requests help. A `--` terminator stops the
// scan: everything after it belongs to the wrapped program, and a `--help` there
// is theirs, not ours.
func WantsHelp(argv []string) bool {
	for _, a := range argv {
		if a == "--" {
			return false
		}
		if a == "-h" || a == "--help" {
			return true
		}
	}
	return false
}

// IsTTY reports whether r is a terminal. Commands consult Deps.Interactive
// rather than calling this directly; it is exported for the one caller that
// builds a Deps.
func IsTTY(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// UpDown renders a liveness boolean. Trivial, and shared because two renderers
// disagreeing about the word for "not running" is a real bug class in a
// diagnostic tool.
func UpDown(up bool) string {
	if up {
		return "up"
	}
	return "down"
}

// SafeGitURL rejects a URL that git could read as a FLAG rather than a remote.
// It is argument hygiene shared by every verb that clones something.
func SafeGitURL(url string) bool {
	if url == "" || strings.HasPrefix(url, "-") {
		return false
	}
	if strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "ssh://") {
		return true
	}
	// scp-style user@host:path (no scheme). Must contain ':' and not be a
	// transport helper like ext::/fd:: (those contain '::').
	if strings.Contains(url, "::") {
		return false
	}
	if at := strings.IndexByte(url, '@'); at > 0 && strings.Contains(url[at:], ":") {
		return true
	}
	return false
}

// grepWord reports whether out contains name as a whole word (matches the
// Makefile's `grep -qw`).
func GrepWord(out, name string) bool {
	for _, line := range strings.Split(out, "\n") {
		for _, f := range strings.FieldsFunc(line, func(r rune) bool {
			return r == ' ' || r == '\t' || r == ',' || r == ':' || r == '/' || r == '"' || r == '='
		}) {
			if f == name {
				return true
			}
		}
	}
	return false
}

// confirmYN reads a [Y/n] answer. def is the answer for a bare Enter.
func ConfirmYN(in io.Reader, out io.Writer, prompt string, def bool) bool {
	fmt.Fprint(out, prompt)
	var line string
	fmt.Fscanln(in, &line)
	ans := strings.ToLower(strings.TrimSpace(line))
	if ans == "" {
		return def
	}
	return ans == "y" || ans == "yes"
}
