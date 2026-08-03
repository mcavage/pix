// man_sync_test.go — the man page must document exactly the verbs and config
// keys this binary actually has. The subject is cmd/pix's verb table, not the
// man renderer, so these live here while the renderer lives in workflow/man.
package main

import (
	"pix/host/service"
	"pix/host/workflow/man"
	"pix/host/workflow/setup"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestManPageDocumentsEveryKnownVerb is the anti-drift guardrail: a new verb
// added to knownVerbs with no man-page entry fails here (part a), and a verb
// documented in the man page but absent from knownVerbs fails too (part b). This
// keeps the authored page and the dispatch table from silently diverging. The
// page is NOT generated from usage consts — it stays hand-authored, and this
// test is the gate.
func TestManPageDocumentsEveryKnownVerb(t *testing.T) {
	documented := documentedManVerbs(t)

	// (a) every known verb is documented.
	var missing []string
	for v := range knownVerbs {
		if !documented[v] {
			missing = append(missing, v)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("knownVerbs not documented in man page (add a `pix <verb>` entry): %v", missing)
	}

	// (b) the page documents no verb that isn't a known verb.
	var stale []string
	for v := range documented {
		if !knownVerbs[v] {
			stale = append(stale, v)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("man page documents verbs absent from knownVerbs (removed/renamed?): %v", stale)
	}
}

// TestManPageDocumentsEveryConfigKey is the config-key anti-drift guardrail, the
// sibling of the verb check: every key the CLI's own help advertises MUST appear
// in the embedded man page. This is exactly the gap that let ollama_bridge_model
// and host.* drift out of the man page while the CLI help stayed complete.
func TestManPageDocumentsEveryConfigKey(t *testing.T) {
	page := string(man.Source())
	var missing []string
	for _, k := range configKeysFromHelp(t) {
		if !strings.Contains(page, k) {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("config keys in the CLI help but NOT in the man page (document them in the .SS config section): %v", missing)
	}
}

// TestManPageDocumentsServeSubverbs is the SUBVERB anti-drift guardrail (M3),
// the sibling of TestManPageDocumentsEveryConfigKey: the verb-level check only
// guards `pix serve`, so `serve install`/`serve uninstall` could silently
// drift out of the man page while the CLI help stayed complete. Every subverb
// the CLI's own service.Usage advertises MUST appear in the man page as an
// invocable `pix serve <sub>` form.
func TestManPageDocumentsServeSubverbs(t *testing.T) {
	page := string(man.Source())
	var missing []string
	for _, sub := range serveSubverbsFromUsage(t) {
		if !strings.Contains(page, "pix serve "+sub) {
			missing = append(missing, sub)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("serve subverbs in the CLI help but NOT in the man page (add `pix serve <sub>` entries): %v", missing)
	}
}

// documentedManVerbs extracts the set of verbs the embedded man page documents
// as INVOCABLE commands: every `"pix <verb>"` quoted command form (used in
// the .BR/.B synopsis lines under each .SS). This deliberately ignores prose
// mentions ("pix binaries", "pix checkout") and reserved stubs that
// carry no invocation form (models/upgrade), so it maps 1:1 onto knownVerbs.
func documentedManVerbs(t *testing.T) map[string]bool {
	t.Helper()
	re := regexp.MustCompile(`"pix ([a-z]+)`)
	got := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(man.Source()), -1) {
		got[m[1]] = true
	}
	if len(got) == 0 {
		t.Fatal("no `\"pix <verb>` command forms found in embedded man page")
	}
	return got
}

// configKeysFromHelp parses the canonical settable key names out of the CLI's own
// setup.ConfigKeysHelp constant (the single source of truth the `config set/get/unset`
// help prints). Key lines are indented exactly two spaces followed by the key
// token; wrapped description lines are indented further, so they are ignored.
func configKeysFromHelp(t *testing.T) []string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^  ([a-z][a-z_.]+) `)
	seen := map[string]bool{}
	var keys []string
	for _, m := range re.FindAllStringSubmatch(setup.ConfigKeysHelp, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			keys = append(keys, m[1])
		}
	}
	if len(keys) == 0 {
		t.Fatal("no keys parsed from setup.ConfigKeysHelp")
	}
	return keys
}

// serveSubverbsFromUsage parses the subverb names out of service.Usage's
// `subcommands:` block (two-space-indented leading token), the same
// single-source-of-truth pattern configKeysFromHelp uses.
func serveSubverbsFromUsage(t *testing.T) []string {
	t.Helper()
	block := service.Usage
	if i := strings.Index(block, "subcommands:"); i >= 0 {
		block = block[i:]
	}
	re := regexp.MustCompile(`(?m)^  ([a-z]+) `)
	seen := map[string]bool{}
	var subs []string
	for _, m := range re.FindAllStringSubmatch(block, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			subs = append(subs, m[1])
		}
	}
	if len(subs) == 0 {
		t.Fatal("no subverbs parsed from service.Usage")
	}
	return subs
}
