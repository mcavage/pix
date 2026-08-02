package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"pix/host/cli"
)

// manPage is the authored roff man page, embedded at build time so `pix
// man` never depends on MANPATH, an installed page, or the repo checkout. The
// .1 source lives in-package (go:embed cannot reach outside the package dir),
// and is the canonical copy — `make install` also drops it on the user manpath
// as a convenience, but the embed is the guarantee.
//
//go:embed pix.1
var manPage []byte

const manUsage = `usage: pix man

Render the embedded pix manual page. The page is baked into the binary, so
no MANPATH entry is ever required. It is rendered through the first available of
mandoc, man, or groff; if none is installed the raw roff source is printed.

When stdout is not a terminal (piped or redirected), the pager is skipped and
the page is written straight to stdout, so 'pix man | grep foo' works.

The global alias '--man' is accepted anywhere on the command line.
`

// extractManFlag reports whether a global `--man` appears anywhere in argv
// before a `--` terminator (everything after `--` is pi passthrough and must
// not be scanned). It returns argv with the `--man` token removed, so any
// remaining -h/--help still reaches runMan. ok is false when `--man` is absent.
func extractManFlag(argv []string) ([]string, bool) {
	found := false
	var rest []string
	for i, a := range argv {
		if a == "--" {
			rest = append(rest, argv[i:]...)
			break
		}
		if a == "--man" {
			found = true
			continue
		}
		rest = append(rest, a)
	}
	return rest, found
}

// runMan is the `man` verb entry point. It honors -h/--help, otherwise renders
// the embedded roff to stdout (paging when attached to a TTY). It never errors
// out and never nags to install a man toolchain — at worst it emits raw roff
// with a one-line stderr hint.
func runMan(argv []string) {
	for _, a := range argv {
		if a == "-h" || a == "--help" {
			fmt.Print(manUsage)
			return
		}
	}
	renderMan(manEnv{
		lookPath: exec.LookPath,
		getenv:   os.Getenv,
		isTTY:    cli.IsTTY(os.Stdout),
		stdout:   os.Stdout,
		stderr:   os.Stderr,
		run:      runManCmd,
	})
}

// manEnv is the injected surface renderMan needs, so a test can drive the
// renderer with a fake lookPath + buffer writers and assert the fallback chain
// without a real man toolchain or a TTY.
type manEnv struct {
	lookPath func(name string) (string, error)
	getenv   func(name string) string
	isTTY    bool
	stdout   io.Writer
	stderr   io.Writer
	// run executes a rendering pipeline: it feeds the roff on stdin to the tool
	// (plus optional args), writes the tool's output to out, and returns an error
	// if the tool could not run. When pager is non-empty, out is ignored and the
	// tool's output is piped into the pager instead (interactive paging).
	run func(env manEnv, tool string, args []string, pager string, out io.Writer) error
}

// renderMan walks the fallback chain and writes the man page, always succeeding.
//
// Fallback chain (first available wins):
//  1. mandoc -a          (renders roff on stdin and pages it itself)
//  2. man -l -           (GNU man reading roff from stdin)
//  3. groff -man -Tutf8  piped into $PAGER (else less -R)
//  4. $PAGER / less      on the raw roff
//  5. raw roff to stdout
//
// When stdout is not a TTY, paging is skipped entirely: the rendered page (or
// raw roff if no renderer is available) is written straight to stdout so the
// output can be piped or redirected.
func renderMan(env manEnv) {
	tty := env.isTTY

	if tty {
		// mandoc -a renders AND pages on its own.
		if p, err := env.lookPath("mandoc"); err == nil {
			if err := env.run(env, p, []string{"-a"}, "", env.stdout); err == nil {
				return
			}
		}
		// GNU man -l - reads roff from stdin and pages it.
		if p, err := env.lookPath("man"); err == nil {
			if err := env.run(env, p, []string{"-l", "-"}, "", env.stdout); err == nil {
				return
			}
		}
		// groff renders; pipe the result into a pager.
		if p, err := env.lookPath("groff"); err == nil {
			if err := env.run(env, p, []string{"-man", "-Tutf8"}, resolvePager(env), env.stdout); err == nil {
				return
			}
		}
		// No renderer — page the raw roff so the user at least sees it.
		if pg := resolvePager(env); pg != "" {
			if err := env.run(env, pg, nil, "", env.stdout); err == nil {
				fmt.Fprintln(env.stderr, "pix man: no man renderer (mandoc/man/groff) found; showing raw roff")
				return
			}
		}
		fmt.Fprintln(env.stderr, "pix man: no man renderer found; showing raw roff")
		_, _ = env.stdout.Write(manPage)
		return
	}

	// Non-TTY: render if we can, but never page — write straight to stdout.
	if p, err := env.lookPath("mandoc"); err == nil {
		if err := env.run(env, p, []string{"-T", "utf8"}, "", env.stdout); err == nil {
			return
		}
	}
	if p, err := env.lookPath("groff"); err == nil {
		if err := env.run(env, p, []string{"-man", "-Tutf8"}, "", env.stdout); err == nil {
			return
		}
	}
	// Last resort: raw roff. Still succeeds so `pix man | grep foo` works.
	_, _ = env.stdout.Write(manPage)
}

// resolvePager picks the interactive pager: $PAGER if set, else less, else cat,
// else empty (no pager available).
func resolvePager(env manEnv) string {
	if pg := env.getenv("PAGER"); pg != "" {
		if p, err := env.lookPath(pg); err == nil {
			return p
		}
		// $PAGER may itself be a command line ("less -R"); trust it as-is if it
		// at least names a resolvable first word.
		return pg
	}
	if p, err := env.lookPath("less"); err == nil {
		return p
	}
	if p, err := env.lookPath("cat"); err == nil {
		return p
	}
	return ""
}

// runManCmd is the real pipeline: feed manPage on stdin to tool (with args),
// then either write its stdout to out, or pipe it into pager for interactive
// paging. It is the injected `run` used outside tests.
func runManCmd(env manEnv, tool string, args []string, pager string, out io.Writer) error {
	cmd := exec.Command(tool, args...)
	cmd.Stdin = newRoffReader()
	cmd.Stderr = env.stderr

	if pager == "" {
		// Tool pages itself (mandoc -a / man -l -) or we just want its output.
		// Wire stdout straight through so an interactive tool controls the tty.
		if f, ok := out.(*os.File); ok && f == os.Stdout {
			cmd.Stdout = os.Stdout
		} else {
			cmd.Stdout = out
		}
		return cmd.Run()
	}

	// Pipe tool | pager.
	pg := exec.Command("sh", "-c", pager)
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	pg.Stdin = pr
	pg.Stdout = os.Stdout
	pg.Stderr = env.stderr
	if err := pg.Start(); err != nil {
		_ = pw.Close()
		return err
	}
	rErr := cmd.Run()
	_ = pw.Close()
	pgErr := pg.Wait()
	if rErr != nil {
		return rErr
	}
	return pgErr
}

// newRoffReader returns a fresh reader over the embedded roff for each pipeline
// run (a bytes reader is single-use once consumed).
func newRoffReader() io.Reader {
	return bytes.NewReader(manPage)
}
