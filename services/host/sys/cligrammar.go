package sys

import (
	"regexp"
	"strings"
)

// cligrammar.go classifies a FAILED CLI invocation's output as either a
// GRAMMAR mismatch — the invoked binary's own argument parser positively
// rejected this argv shape (an unknown flag, an unknown command, or the
// wrong number of arguments) — or anything else. This is the ONE signal
// that may license a bounded retry with a different, KNOWN alternate
// argv (see health.SbxProbe and mcp.RegisterServers/RunSbxGrammarFallback):
// an auth failure, a policy denial, a timeout, or any other operational
// error looks identical under either grammar, so retrying those with a
// different flag spelling cannot fix them — it would only hide the real
// cause behind a second, unrelated attempt. Callers must gate every
// grammar-fallback retry on IsUsageMismatch and nothing else.
var usageMismatchPatterns = []*regexp.Regexp{
	// cobra/clap-style unknown flag: "unknown flag: --url", "unknown
	// shorthand flag: 'v' in -v".
	regexp.MustCompile(`unknown (?:shorthand )?flag`),
	// Go's stdlib flag package: "flag provided but not defined: -version".
	regexp.MustCompile(`flag provided but not defined`),
	// A flag name the parser has never heard of, phrased as a flag rather
	// than a command.
	regexp.MustCompile(`no such flag`),
	// cobra unknown command: `unknown command "version" for "sbx"`.
	regexp.MustCompile(`unknown (?:sub)?command`),
	regexp.MustCompile(`unrecognized (?:sub)?command`),
	// Wrong arity — a positional URL rejected as an extra argument, or a
	// required positional missing, both phrased by the parser itself.
	regexp.MustCompile(`accepts (?:at most |at least )?\d+ arg\(s\), received \d+`),
	regexp.MustCompile(`unexpected argument`),
	regexp.MustCompile(`too many arguments`),
	// A flag the new grammar requires that the old invocation never passed.
	regexp.MustCompile(`required flag\(?s?\)? "?[^"\n]*"? not (?:set|registered)`),
}

// IsUsageMismatch reports whether output carries a RECOGNIZED grammar-mismatch
// signature — the invoked CLI's own parser saying it does not understand this
// argv, in the vocabulary cobra/kong/clap/the Go flag package use for that.
// It is deliberately narrow: unlike a generic "invalid"/"error" substring,
// every pattern here is anchored to the parser's own words for "this flag or
// command does not exist" or "wrong number of arguments", so a genuine
// operational, auth, or policy failure (which often also says "error:
// invalid ...") can never be mistaken for a grammar mismatch and trigger an
// unwarranted retry.
func IsUsageMismatch(output string) bool {
	hay := strings.ToLower(output)
	for _, re := range usageMismatchPatterns {
		if re.MatchString(hay) {
			return true
		}
	}
	return false
}
