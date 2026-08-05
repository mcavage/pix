// retired.go — the one table of retired CLI surfaces, and the one way a
// retired surface answers.
//
// Retirement is not deletion-by-silence: an unknown verb guesses by edit
// distance and cannot possibly guess `mcp register` from `slack`. So a retired
// surface keeps exactly one behaviour — print a machine-greppable PIX_RETIRED
// line naming the replacement, exit 2, and do NOTHING else: no config load, no
// daemon, no sandbox, no file. That is what makes hitting one from a stale
// script or a shell history safe.
//
// Every entry here has an approved, append-only record in
// corpus/retirement.jsonl (retired_test.go proves the two agree), so the
// recovery path for a name survives long after the code that implemented it.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"pix/host/cli"
)

// retiredSep separates a verb from its retired subcommand/flag in a table key.
// A NUL cannot occur in an argv token, so it can never collide with a real one.
const retiredSep = "\x00"

// retiredKey builds the table key for a retired surface. flag is empty for a
// whole verb, or the subcommand/flag token for a narrower retirement
// (`state backup`, `task harvest`, the global `pix --man`).
func retiredKey(verb, flag string) string {
	if flag == "" {
		return verb
	}
	return verb + retiredSep + flag
}

// retiredSplit is retiredKey's inverse.
func retiredSplit(key string) (verb, flag string) {
	if i := strings.Index(key, retiredSep); i >= 0 {
		return key[:i], key[i+len(retiredSep):]
	}
	return key, ""
}

// retiredSurfaces maps a retired surface to the command that replaced it. The
// value is a full command line, not a bare verb, because a replacement is not
// always a pix verb (`upgrade` is the package manager's job).
//
// A value may itself name a retired surface: the manifest is append-only, so
// history stays as written and terminalReplacement chains the hops.
func retiredSurfaces() map[string]string {
	return map[string]string{
		// W1 U01a: the host-integration, distribution, and state-management
		// surfaces the launcher no longer owns.
		"backup":                   "pix-host backup",
		"evals":                    "pix models route",
		"gworkspace":               "pix mcp register",
		"host":                     "pix run",
		"kb":                       "pix pack use",
		"knowledge":                "pix pack use",
		"man":                      "pix help --all",
		"restore":                  "pix-host restore",
		"slack":                    "pix mcp register",
		"upgrade":                  "brew upgrade pix",
		retiredKey("pix", "--man"): "pix help --all",
		// U04e: --replace was a forced `sbx rm -f` before a create, issued with
		// no zero-holder proof — it could destroy a sandbox another shell was
		// live in. Removal is explicit and proof-gated now.
		retiredKey("run", "--replace"):   "pix rm BOX",
		retiredKey("setup", "--replace"): "pix rm BOX",
		retiredKey("state", "backup"):    "pix-host backup",
		retiredKey("state", "restore"):   "pix-host restore",
		retiredKey("task", "gc"):         "pix task rm",
		retiredKey("task", "harvest"):    "pix task path",
		// Earlier retirements, previously answered only by a did-you-mean hint on
		// the unknown-verb path. They answer with the marker now, same as the rest.
		"gog":     "pix gworkspace",
		"onboard": "pix setup --no-agent",
		"route":   "pix models",
	}
}

// terminalReplacement follows a replacement whose own verb was retired later,
// stopping at the first surface that still exists (or when the chain is
// exhausted, which a cycle cannot outlive).
func terminalReplacement(key string) string {
	table := retiredSurfaces()
	replacement := table[key]
	seen := map[string]bool{key: true}
	for range table {
		fields := strings.Fields(replacement)
		if len(fields) < 2 || fields[0] != "pix" {
			return replacement
		}
		next := retiredKey(fields[1], "")
		if seen[next] {
			return replacement
		}
		onward, ok := table[next]
		if !ok {
			return replacement
		}
		seen[next] = true
		replacement = onward
	}
	return replacement
}

// retiredMessage renders the notice for a retired surface. The PIX_RETIRED
// prefix is a contract: it is what a wrapper script greps for.
func retiredMessage(key string) string {
	verb, flag := retiredSplit(key)
	typed := verb
	if flag != "" {
		typed = verb + " " + flag
	}
	if verb != "pix" {
		typed = "pix " + typed
	}
	return fmt.Sprintf("PIX_RETIRED: `%s` was retired. Use `%s` instead.\n", typed, terminalReplacement(key))
}

// retiredExit prints the notice on stderr and exits 2; stdout stays clean, so
// a script that pipes it has nothing to misparse.
func retiredExit(key string) {
	fmt.Fprint(os.Stderr, retiredMessage(key))
	os.Exit(2)
}

// retiredFlag is the answer for a retired FLAG of a live verb: the same
// PIX_RETIRED notice on the caller's stderr and the same exit 2, but returned
// through the root's error mapping instead of os.Exit, because the verb's
// parser has already run by the time a flag value is visible. It stays inert —
// the caller must consult it before any probe, config read or mutation.
func retiredFlag(errOut io.Writer, verb, flag string) error {
	key := retiredKey(verb, flag)
	if _, ok := retiredSurfaces()[key]; !ok {
		// A flag routed here without a table entry would exit 2 saying nothing
		// useful. Fail loudly instead: the table and the call site are one
		// decision (retired_test.go proves the table matches the manifest).
		return fmt.Errorf("internal: %s %s is not a retired surface", verb, flag)
	}
	fmt.Fprint(errOut, retiredMessage(key))
	return cli.SilentError{Code: 2}
}

// retiredIfRetired exits with the notice when key names a retired surface, and
// returns otherwise. It is the single check every dispatch site calls.
func retiredIfRetired(verb, flag string) {
	if _, ok := retiredSurfaces()[retiredKey(verb, flag)]; ok {
		retiredExit(retiredKey(verb, flag))
	}
}

// hasGlobalManFlag reports whether the retired global `--man` appears before a
// `--` terminator (everything after `--` is pi passthrough).
func hasGlobalManFlag(argv []string) bool {
	for _, a := range argv {
		if a == "--" {
			return false
		}
		if a == "--man" {
			return true
		}
	}
	return false
}
