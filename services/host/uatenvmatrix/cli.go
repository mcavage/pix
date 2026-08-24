package uatenvmatrix

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// uatImageNamespacePrefix is the reserved candidate-build image namespace
// (docs/design/self-development-uat.md, "Candidate build isolation"): the
// planner never tags a candidate outside it, so `pix-host uat-env-matrix`
// refuses any --image-tag that is not fully qualified inside it — the
// production pix image, `latest`, and every tag outside this prefix are
// caller bugs, not supported ways to invoke this subcommand.
const uatImageNamespacePrefix = "docker.io/mcavage/pix:uat-"

// CLIArgs is `pix-host uat-env-matrix`'s validated argv. It is intentionally
// the whole entry point's decision surface: main.go's case arm does nothing
// but call ParseArgs and RunCLI, so every argv-shaped bug (a missing flag, a
// relative path, an out-of-namespace image tag, a stray positional argument)
// is provable with `go test ./uatenvmatrix/...` alone, with no real `sbx`
// binary and no subprocess.
type CLIArgs struct {
	ListChecks bool
	OutDir     string
	StepsDir   string
	ImageTag   string
}

// ParseArgs validates `pix-host uat-env-matrix`'s argv.
//
// --list-checks is a pure reporting mode: it needs and validates none of the
// run-local flags, and RunCLI never touches a host tool while honoring it —
// the exact "report candidate CheckNames without executing host tools"
// contract docs/design/self-development-uat.md calls for.
//
// Every other invocation requires all three flags: --out-dir and --steps-dir
// must be absolute paths to directories that already exist (the worker
// always creates both before invoking this subcommand; a caller passing
// anything else is a caller bug, not a supported way to skip validation),
// and --image-tag must be fully qualified inside uatImageNamespacePrefix. No
// positional arguments are ever accepted, in either mode.
func ParseArgs(args []string) (CLIArgs, error) {
	fs := flag.NewFlagSet("uat-env-matrix", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	listChecks := fs.Bool("list-checks", false, "report candidate CheckNames without executing host tools")
	outDir := fs.String("out-dir", "", "absolute path to the candidate build's out directory")
	stepsDir := fs.String("steps-dir", "", "absolute path to the run's steps directory")
	imageTag := fs.String("image-tag", "", "fully qualified candidate image tag ("+uatImageNamespacePrefix+"...)")

	if err := fs.Parse(args); err != nil {
		return CLIArgs{}, err
	}
	if fs.NArg() > 0 {
		return CLIArgs{}, fmt.Errorf("uat-env-matrix: no positional arguments are accepted, got %v", fs.Args())
	}

	if *listChecks {
		return CLIArgs{ListChecks: true}, nil
	}

	if *outDir == "" || !filepath.IsAbs(*outDir) {
		return CLIArgs{}, fmt.Errorf("uat-env-matrix: --out-dir is required and must be an absolute path")
	}
	if st, err := os.Stat(*outDir); err != nil || !st.IsDir() {
		return CLIArgs{}, fmt.Errorf("uat-env-matrix: --out-dir must be an existing directory: %s", *outDir)
	}
	if *stepsDir == "" || !filepath.IsAbs(*stepsDir) {
		return CLIArgs{}, fmt.Errorf("uat-env-matrix: --steps-dir is required and must be an absolute path")
	}
	if st, err := os.Stat(*stepsDir); err != nil || !st.IsDir() {
		return CLIArgs{}, fmt.Errorf("uat-env-matrix: --steps-dir must be an existing directory: %s", *stepsDir)
	}
	if *imageTag == "" || !strings.HasPrefix(*imageTag, uatImageNamespacePrefix) || *imageTag == uatImageNamespacePrefix {
		return CLIArgs{}, fmt.Errorf("uat-env-matrix: --image-tag is required and must be fully qualified inside %s (got %q)", uatImageNamespacePrefix, *imageTag)
	}

	return CLIArgs{OutDir: *outDir, StepsDir: *stepsDir, ImageTag: *imageTag}, nil
}

// RunCLI is pix-host's entire `uat-env-matrix` entry point: parse argv, then
// either report CheckNames (pure, no host tools) or execute Run against the
// validated Inputs. It returns the process exit code so main.go's case arm
// only needs to call it and os.Exit the result — no logic of its own to
// drift from what this function actually does.
func RunCLI(args []string, stdout, stderr io.Writer) int {
	parsed, err := ParseArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if parsed.ListChecks {
		for _, name := range CheckNames() {
			fmt.Fprintln(stdout, name)
		}
		return 0
	}
	if err := Run(context.Background(), Inputs{OutDir: parsed.OutDir, StepsDir: parsed.StepsDir, ImageTag: parsed.ImageTag}); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
