// pi-stack paths — a read-only report of where pi-stack keeps its data. It names
// the three XDG bases (CONFIG / DATA / STATE) with a one-word gloss, lists any
// active env overrides, and — when a pre-XDG install left data at a legacy
// location — points at `pi-stack migrate`. It resolves paths only (no sqlite, no
// fs mutation) and never launches anything, so it is safe to run anytime.

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"pi-stack/host/config"
)

// pathsReport is the resolved, testable snapshot `pi-stack paths` renders.
type pathsReport struct {
	configDir string
	dataDir   string
	stateDir  string
	overrides []string // "NAME=value" for each set override env var (nil when none)
	legacy    []string // legacy locations that currently exist (nil when none)
}

// overrideVars are the env vars that redirect a resolver away from its XDG
// default, surfaced in the report so a surprising path is explained.
var overrideVars = []string{
	"MEMORY_DB", "KNOWLEDGE_DB", "KNOWLEDGE_CACHE_DIR",
	"PI_STACK_CONFIG", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME",
}

// buildPathsReport resolves the three bases (via the config module, so it matches
// what every consumer actually uses), the active overrides, and the legacy
// locations that are STILL PENDING migration. getenv/homeDir/lstat/readlink are
// injected so the report is hermetic in tests.
//
// The pending set is computed by the shared detectPendingLegacy, so `pi-stack
// paths` and doctor agree EXACTLY on what "pending" means: only a legacy memory,
// bundle, or cache path that is still a REAL directory. A symlink→new is
// converged (resolved + compared, not just "is a symlink"); the legacy knowledge
// index dir, serve.pid, and retained backups are excluded (migrate leaves them),
// so a successful migrate clears the note. lstat is os.Lstat (never follows a
// symlink); readlink resolves a symlink's target.
func buildPathsReport(getenv func(string) string, homeDir func() (string, error), lstat func(string) (os.FileInfo, error), readlink func(string) (string, error)) pathsReport {
	var rep pathsReport
	rep.configDir, _ = config.ConfigDir()
	rep.dataDir, _ = config.DataDir()
	rep.stateDir, _ = config.StateDir()

	for _, v := range overrideVars {
		if val := strings.TrimSpace(getenv(v)); val != "" {
			rep.overrides = append(rep.overrides, v+"="+val)
		}
	}

	home, err := homeDir()
	if err == nil && home != "" {
		// Adapt the os.Lstat-shaped seam to detectPendingLegacy's (mode, exists) form.
		lstatMode := func(p string) (os.FileMode, bool) {
			fi, serr := lstat(p)
			if serr != nil {
				return 0, false
			}
			return fi.Mode(), true
		}
		rep.legacy = detectPendingLegacy(home, lstatMode, readlink)
	}
	return rep
}

// tildeHome abbreviates home (and its descendants) to ~ for display; pass "" to
// print the path verbatim.
func tildeHome(p, home string) string {
	if home == "" || p == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if strings.HasPrefix(p, home+string(filepath.Separator)) {
		return "~" + p[len(home):]
	}
	return p
}

// printPathsReport renders the report in the Part D layout. home tildes the
// displayed paths; pass "" to print them verbatim.
func printPathsReport(w io.Writer, rep pathsReport, home string) {
	td := func(p string) string { return tildeHome(p, home) }
	fmt.Fprintf(w, "  %-8s %-26s %s\n", "config", td(rep.configDir), "config.toml, op-refs.env, broker-token")
	fmt.Fprintf(w, "  %-8s %-26s %s\n", "data", td(rep.dataDir), "memory, knowledge, backups   (precious)")
	fmt.Fprintf(w, "  %-8s %-26s %s\n", "state", td(rep.stateDir), "index, caches, tasks, serve.pid   (regenerable)")
	if len(rep.overrides) > 0 {
		fmt.Fprintf(w, "  %-8s %s\n", "overrides", strings.Join(rep.overrides, "  "))
	} else {
		fmt.Fprintf(w, "  %-8s (none set)\n", "overrides")
	}
	if len(rep.legacy) > 0 {
		legacy := make([]string, len(rep.legacy))
		for i, p := range rep.legacy {
			legacy[i] = td(p)
		}
		fmt.Fprintln(w, "  ! some data is at legacy locations — run `pi-stack migrate` to relocate:")
		fmt.Fprintf(w, "      %s\n", strings.Join(legacy, ", "))
	}
}

const pathsUsage = `usage: pi-stack paths

Print where pi-stack keeps its data — the three XDG bases (config / data / state),
any active env overrides, and a note when a pre-XDG install still has data at a
legacy location (run 'pi-stack migrate' to relocate). Read-only; launches nothing.
`

// runPaths is the `paths` verb entry point.
func runPaths(argv []string) {
	for _, a := range argv {
		if a == "-h" || a == "--help" {
			fmt.Print(pathsUsage)
			return
		}
		fmt.Fprintf(os.Stderr, "pi-stack paths: unexpected argument %q\n\n%s", a, pathsUsage)
		os.Exit(2)
	}
	rep := buildPathsReport(os.Getenv, os.UserHomeDir, os.Lstat, os.Readlink)
	home, _ := os.UserHomeDir()
	printPathsReport(os.Stdout, rep, home)
}
