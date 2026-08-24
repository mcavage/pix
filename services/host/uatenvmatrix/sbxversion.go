package uatenvmatrix

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// sbxversion.go is a small, LOCAL equivalent of health.SbxProbe's version
// probe (health/probes.go), duplicated rather than imported: this package
// is a sibling L1 capability that must never import health or the future
// envinfo capability (matrix.go's package doc, arch_test.go's
// TestArchitecture_UatenvmatrixNeverImportsEnvinfo). It exists so
// checkEnvironmentCreateThenExecInvocation's own bounded artifact can carry
// AC-7 / the E0.7 unit's required "observed sbx version" evidence, captured
// by THIS check's own probe rather than assumed from a separate, untracked
// invocation.

// sbxVersionPattern extracts the exact digit.digit run a real `sbx
// --version`/`sbx version` banner carries — the same shape health's own
// versionish regex looks for.
var sbxVersionPattern = regexp.MustCompile(`[0-9]+\.[0-9]+`)

// sbxVersionUnknownFlagPatterns is the ONE narrow signal that may license
// probeSbxVersion's bounded, one-shot grammar fallback: sbx's own output
// positively saying it does not understand the `--version` flag itself, in
// the vocabulary cobra/kong/clap/the stdlib flag package use for that. This
// duplicates health/sys's narrow unknown-flag posture (sys.IsUsageMismatch,
// health.SbxProbe.Check) rather than importing either. An unrelated primary
// failure — auth, policy, a hung process, a generic non-zero exit — must
// never trigger this fallback: retrying with a different flag spelling
// cannot fix an operational error, and would only hide the real cause
// behind a second, unrelated attempt.
var sbxVersionUnknownFlagPatterns = []*regexp.Regexp{
	// cobra/clap-style unknown flag: "unknown flag: --version", "unknown
	// shorthand flag: 'v' in -v".
	regexp.MustCompile(`unknown (?:shorthand )?flag`),
	// Go's stdlib flag package: "flag provided but not defined: -version".
	regexp.MustCompile(`flag provided but not defined`),
	// A flag name the parser has never heard of, phrased as a flag rather
	// than a command.
	regexp.MustCompile(`no such flag`),
}

// isSbxVersionUnknownFlag reports whether output carries one of the narrow
// unknown-flag signatures above. It is deliberately narrower than a generic
// "error"/"invalid" substring match, for the same reason
// sys.IsUsageMismatch's doc comment gives: every pattern is anchored to the
// parser's own words for "this flag does not exist", so a genuine
// operational failure that also happens to say "error: invalid ..." can
// never be mistaken for a grammar mismatch and trigger an unwarranted
// retry.
func isSbxVersionUnknownFlag(output string) bool {
	hay := strings.ToLower(output)
	for _, re := range sbxVersionUnknownFlagPatterns {
		if re.MatchString(hay) {
			return true
		}
	}
	return false
}

// probeSbxVersion runs the bounded, at-most-two-attempt version probe AC-7
// and the E0.7 unit require this check's own bounded artifact to carry:
// `sbx --version` first (this host's default; health.SbxProbe's own
// default), and ONLY when that exact invocation is refused as an unknown
// flag, a single fallback to `sbx version` (the one known alternate
// grammar). Every other primary failure — auth, policy, a timeout, a
// generic non-zero exit — returns immediately without ever attempting the
// fallback (isSbxVersionUnknownFlag's whole point). A fallback that also
// fails, or output from either attempt carrying no recognizable version
// string, fails closed: this probe answers "what version is running",
// never "assume it's fine".
//
// Every byte of both attempts (exact command, stdout, stderr, error) is
// logged to lw, plus a normalized "observed sbx version: <value>" line on
// success.
func probeSbxVersion(ctx context.Context, lw io.Writer, executor Executor, env []string, dir string) (string, error) {
	primary := []string{"--version"}
	fmt.Fprintf(lw, "$ sbx %s\n", strings.Join(primary, " "))
	out, errOut, err := executor.Run(ctx, "sbx", primary, env, dir)
	fmt.Fprintf(lw, "stdout: %s\nstderr: %s\nerr: %v\n", out, errOut, err)

	usedFallback := false
	if err != nil {
		if !isSbxVersionUnknownFlag(out + "\n" + errOut) {
			return "", fmt.Errorf("sbx --version: %w", err)
		}
		fallback := []string{"version"}
		fmt.Fprintf(lw, "sbx --version was refused as an unknown flag; falling back: $ sbx %s\n", strings.Join(fallback, " "))
		out, errOut, err = executor.Run(ctx, "sbx", fallback, env, dir)
		fmt.Fprintf(lw, "stdout: %s\nstderr: %s\nerr: %v\n", out, errOut, err)
		if err != nil {
			return "", fmt.Errorf("sbx version (fallback): %w", err)
		}
		usedFallback = true
	}

	version := sbxVersionPattern.FindString(out)
	if version == "" {
		what := "sbx --version"
		if usedFallback {
			what = "sbx version (fallback)"
		}
		return "", fmt.Errorf("%s: no recognizable version in output (stdout=%q, stderr=%q)", what, out, errOut)
	}
	fmt.Fprintf(lw, "observed sbx version: %s\n", version)
	return version, nil
}
