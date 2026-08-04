// retired.go — the one table of retired CLI surfaces, and the one way a
// retired surface answers.
//
// Retirement is not deletion-by-silence. A verb removed from the dispatch
// switch would fall to the unknown-command path, which guesses by edit
// distance and cannot possibly guess `mcp register` from `slack`. So a retired
// surface keeps exactly one behaviour: print a machine-greppable PIX_RETIRED
// line naming the replacement, exit 2, and do NOTHING else — no config load,
// no daemon, no sandbox, no file. That is what makes hitting one from a stale
// script or a shell history safe.
//
// Every entry here has an approved, append-only record in
// corpus/retirement.jsonl (retired_test.go proves the two agree), so the
// recovery path for a name survives long after the code that implemented it.
package main

import (
	"fmt"
	"os"
	"strings"
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
// value is a full command line (not a bare verb) because a replacement is not
// always a pix verb — `upgrade` is now the package manager's job.
//
// A value may itself name a retired verb: `gog` was retired IN FAVOUR of
// `gworkspace`, which W1 retired in turn. The recorded history stays as it was
// written (the manifest is append-only); terminalReplacement follows the chain
// so the message a user sees always names a surface that still exists.
func retiredSurfaces() map[string]string {
	return map[string]string{
		// W1 U01a: the host-integration, distribution, and state-management
		// surfaces the launcher no longer owns.
		"backup":                       "pix-host backup",
		"evals":                        "pix models route",
		"gworkspace":                   "pix mcp register",
		"host":                         "pix run",
		"kb":                           "pix pack use",
		"knowledge":                    "pix pack use",
		"man":                          "pix help --all",
		"restore":                      "pix-host restore",
		"slack":                        "pix mcp register",
		"upgrade":                      "brew upgrade pix",
		retiredKey("pix", "--man"):     "pix help --all",
		retiredKey("state", "backup"):  "pix-host backup",
		retiredKey("state", "restore"): "pix-host restore",
		retiredKey("task", "gc"):       "pix task rm",
		retiredKey("task", "harvest"):  "pix task path",
		// Earlier retirements, previously answered only by a did-you-mean hint on
		// the unknown-verb path. They answer with the marker now, same as the rest.
		"gog":     "pix gworkspace",
		"onboard": "pix setup --no-agent",
		"route":   "pix models",
	}
}

// terminalReplacement resolves a retirement chain: it follows a replacement
// whose own verb was retired later, and stops at the first surface that still
// exists (or after the chain is exhausted, which a cycle cannot outlive).
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

// retiredExit prints the notice on stderr and exits 2. stdout stays clean so a
// script that pipes it gets no output to misparse.
func retiredExit(key string) {
	fmt.Fprint(os.Stderr, retiredMessage(key))
	os.Exit(2)
}

// retiredIfRetired exits with the notice when key names a retired surface, and
// returns otherwise. It is the single check every dispatch site calls.
func retiredIfRetired(verb, flag string) {
	if _, ok := retiredSurfaces()[retiredKey(verb, flag)]; ok {
		retiredExit(retiredKey(verb, flag))
	}
}

// hasGlobalManFlag reports whether the retired global `--man` appears before a
// `--` terminator (everything after `--` is pi passthrough). It replaces the
// call into the man renderer: the flag no longer renders anything, so the
// launcher no longer composes that capability at all.
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
