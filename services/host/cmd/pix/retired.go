// retired.go — the one table of retired CLI surfaces, and the one way a retired
// surface answers.
//
// Retirement is not deletion-by-silence: an unknown verb guesses by edit distance
// and cannot guess `mcp register` from `slack`. So a retired surface keeps exactly
// one behaviour — print a machine-greppable PIX_RETIRED line naming the
// replacement, exit 2, and do NOTHING else: no config load, no daemon, no sandbox,
// no file. That is what makes hitting one from a stale script safe.
//
// Every entry has an approved, append-only record in corpus/retirement.jsonl
// (retired_test.go proves the two agree), so a name's recovery path survives long
// after the code that implemented it.
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

// retiredKey builds the table key for a retired surface. flag is empty for a whole
// verb, or the subcommand/flag token for a narrower retirement (`state backup`,
// `task harvest`, the global `pix --man`).
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

// retiredSurfaces maps a retired surface to the command that replaced it. The value
// is a full command line, not a bare verb, because a replacement is not always a
// pix verb (`upgrade` is the package manager's job).
//
// A value may itself name a retired surface: the manifest is append-only, so
// history stays as written and terminalReplacement chains the hops.
func retiredSurfaces() map[string]string {
	return map[string]string{
		// The host-integration, distribution and state-management surfaces the
		// launcher no longer owns.
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
		// --replace was a forced `sbx rm -f` before a create, issued with no
		// zero-holder proof: it could destroy a sandbox another shell was live in.
		// Removal is explicit and proof-gated now.
		retiredKey("run", "--replace"):   "pix rm BOX",
		retiredKey("setup", "--replace"): "pix rm BOX",
		retiredKey("state", "backup"):    "pix-host backup",
		retiredKey("state", "restore"):   "pix-host restore",
		retiredKey("task", "gc"):         "pix task rm",
		retiredKey("task", "harvest"):    "pix task path",
		// An agent is a hand-edited agents/*.md file plus scorecard.json now, not a
		// CLI mutation surface — see docs/design/routing.md.
		retiredKey("agent", "new"):      "edit agents/*.md, then pix models route",
		retiredKey("agent", "edit"):     "edit agents/*.md, then pix models route",
		retiredKey("agent", "rm"):       "delete agents/*.md, then pix models route",
		retiredKey("agent", "remove"):   "delete agents/*.md, then pix models route",
		retiredKey("agent", "reassess"): "pix models route",
		// The pack authoring surface: create/edit pack.toml and skills/*/SKILL.md
		// files directly (a plain text edit, then `pack use` to activate).
		retiredKey("pack", "new"): "editing pack.toml directly",
		retiredKey("pack", "add"): "editing pack.toml and skills/*/SKILL.md directly",
		// Earlier retirements, once answered only by a did-you-mean hint: they answer
		// with the marker now, like the rest.
		"gog":     "pix gworkspace",
		"onboard": "pix setup --no-agent",
		"route":   "pix models",
	}
}

// terminalReplacement follows a replacement whose own verb was retired later,
// stopping at the first surface that still exists (or when the chain is exhausted,
// which a cycle cannot outlive).
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

// retiredMessage renders the notice. The PIX_RETIRED prefix is a contract: it is
// what a wrapper script greps for.
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

// retiredExit prints the notice on stderr and exits 2; stdout stays clean, so a
// script that pipes it has nothing to misparse.
func retiredExit(key string) {
	fmt.Fprint(os.Stderr, retiredMessage(key))
	os.Exit(2)
}

// retiredFlag is the answer for a retired FLAG of a live verb: the same PIX_RETIRED
// notice and exit 2, returned through the root's error mapping instead of os.Exit,
// because the verb's parser has already run by the time a flag value is visible. It
// stays inert — the caller must consult it before any probe, config read or
// mutation.
func retiredFlag(errOut io.Writer, verb, flag string) error {
	key := retiredKey(verb, flag)
	if _, ok := retiredSurfaces()[key]; !ok {
		// A flag routed here with no table entry would exit 2 saying nothing useful:
		// the table and the call site are one decision.
		return fmt.Errorf("internal: %s %s is not a retired surface", verb, flag)
	}
	fmt.Fprint(errOut, retiredMessage(key))
	return cli.SilentError{Code: 2}
}

// retiredIfRetired exits with the notice when key names a retired surface, and
// returns otherwise: the single check every dispatch site calls.
func retiredIfRetired(verb, flag string) {
	if _, ok := retiredSurfaces()[retiredKey(verb, flag)]; ok {
		retiredExit(retiredKey(verb, flag))
	}
}

// hasGlobalManFlag reports whether the retired global `--man` appears before a `--`
// terminator (everything after `--` is pi passthrough).
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
