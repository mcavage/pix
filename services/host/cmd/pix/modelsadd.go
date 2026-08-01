package main

// modelsadd.go implements `pix models add <provider>`: the answer to "setup
// told me I could add the others later, but I could not find where."
//
// Before this, the only later path was `pix secret set <P>_API_KEY op://...`,
// which writes the credential ref and mirrors it, and stops there. Nothing
// rebuilt cfg.Inference.Models, nothing probed the new provider, and the roster
// only ever pruned — so a second key was present, correct, and completely
// inert, with no command that meant "take this into account".

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"pix/host/config"
)

// runModelsAdd wires one provider end to end: make sure a 1Password ref exists
// for it (prompting on a TTY), then run the SAME reconcile setup runs, so the
// key becomes callable models and a routable roster in one command.
func runModelsAdd(argv []string) {
	if wantsHelp(argv) {
		fmt.Print(modelsAddUsage())
		return
	}
	if len(argv) != 1 {
		fmt.Fprintf(os.Stderr, "pix models add: want exactly one provider (%s)\n", strings.Join(providerNames(), ", "))
		os.Exit(2)
	}
	p, ok := providerByName(argv[0])
	if !ok {
		fmt.Fprintf(os.Stderr, "pix models add: unknown provider %q (want one of: %s)\n", argv[0], strings.Join(providerNames(), ", "))
		os.Exit(2)
	}

	env := defaultShellEnv()
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix models add: %v\n", err)
		os.Exit(1)
	}
	// Refuse under a mandatory pack BEFORE touching anything. configureDirect-
	// Inference would happily write bindings that the topology filter then drops
	// silently, so "added" would be a success word with nothing behind it.
	if cfg.Inference.ExclusiveSource != "" {
		fmt.Fprintf(os.Stderr, "pix models add: the active pack (%s) owns inference on this host, so a provider key cannot be wired in.\n", cfg.Inference.ExclusiveSource)
		fmt.Fprintf(os.Stderr, "  The key itself is still worth storing now: pix secret set %s op://vault/item/field\n", p.envVar)
		fmt.Fprintln(os.Stderr, "  It gets wired the moment the pack stops being the exclusive source (`pix pack rm`, or a pack that does not claim inference).")
		os.Exit(2)
	}

	interactive := isTTY(os.Stdin)
	sc := bufio.NewScanner(os.Stdin)
	if _, hasRef := currentOpRef(env, p.envVar); !hasRef {
		if !interactive {
			fmt.Fprintf(os.Stderr, "pix models add: no 1Password ref for %s yet, and there is no terminal to ask on.\n", p.name)
			fmt.Fprintf(os.Stderr, "  pix secret set %s op://vault/item/field && pix models add %s\n", p.envVar, p.name)
			os.Exit(2)
		}
		if err := ensureSetupPrereqsFor(env, os.Stdin, os.Stdout, interactive, true); err != nil {
			fmt.Fprintf(os.Stderr, "pix models add: %v\n", err)
			os.Exit(1)
		}
		ref, _, ok := promptProviderRef(env, sc, os.Stdout, p)
		if !ok {
			os.Exit(1)
		}
		if err := runSecretSet(env, os.Stdout, p.envVar, ref); err != nil {
			os.Exit(1)
		}
	}

	res, err := reconcileDirectInference(cfg, env, os.Stdin, os.Stdout, interactive, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix models add: %v\n", err)
		os.Exit(1)
	}
	renderModelsAdd(os.Stdout, p.name, res)

	// The sandbox reads the credential from sbx, not from this host's refs file,
	// so a key that never reaches sbx is wired for host mode only. Reconcile it
	// the same way setup does rather than quietly leaving half the job done.
	runSecretSync(env, os.Stdout)
}

// renderModelsAdd reports what was PROVEN, never what was merely written. The
// verified count comes from verifyDirectInference's live per-model requests, so
// "callable" here is probe-backed; a provider whose models all failed their
// probe is reported as a shortfall even though its key resolved fine.
func renderModelsAdd(out io.Writer, provider string, res reconcileResult) {
	if len(res.Added) == 0 {
		fmt.Fprintf(out, "%s was already wired; re-checked it.\n", provider)
	}
	fmt.Fprintf(out, "%d model(s) answered a live request across %d provider(s).\n", res.Verified, len(res.Providers))
	if len(res.Failures) > 0 {
		fmt.Fprintf(out, "%d candidate(s) did not answer: %s\n", len(res.Failures), strings.Join(res.Failures, "; "))
	}
	fmt.Fprintln(out, "Next: pix models        (see the roster)")
	fmt.Fprintln(out, "      pix models route  (re-resolve intents onto it)")
}

func providerNames() []string {
	names := make([]string, 0, len(providerKeyRefOrder))
	for _, p := range providerKeyRefOrder {
		names = append(names, p.name)
	}
	sort.Strings(names)
	return names
}

// providerByName accepts the provider name, its env var, and the obvious alias
// (gemini for google), so a user who read the key name in a doc is not told
// their own credential's name is wrong.
func providerByName(raw string) (struct{ envVar, name string }, bool) {
	want := strings.ToLower(strings.TrimSpace(raw))
	if want == "gemini" {
		want = "google"
	}
	for _, p := range providerKeyRefOrder {
		if want == p.name || strings.EqualFold(want, p.envVar) {
			return p, true
		}
	}
	return struct{ envVar, name string }{}, false
}

func modelsAddUsage() string {
	return `usage: pix models add <provider>

Wire a model provider key into callable models, end to end: store its
1Password ref if it has none yet, rebuild the model bindings, prove each one
with a live request, widen the roster, and reconcile the key into sbx.

providers: ` + strings.Join(providerNames(), ", ") + `

This is the command setup means by "you can add others later". ` + "`pix secret set`" + `
stores a credential ref; it deliberately does not make network calls, so it
alone leaves a key unwired.
`
}

// unwiredProviderKeys is the gap this whole feature closes, as a fact both the
// status screen and doctor can read: a provider whose key RESOLVES on this host
// but which has no native binding in config, i.e. a key that is present,
// correct, and doing nothing.
//
// It reports absence of wiring, never a verdict about the key's validity. A
// binding that exists but failed its probe is NOT reported here — that is a
// different problem with a different fix, and conflating them would send a user
// to `models add` for a credential their provider rejected.
//
// Silent when a pack owns inference (its bindings are the pack's business) and
// when the key list is unreadable, since an unreadable list is not evidence of
// a gap.
func unwiredProviderKeys(cfg *config.Config, env shellEnv) []string {
	if cfg == nil || cfg.Inference.ExclusiveSource != "" {
		return nil
	}
	names, err := hostModeProviderKeys(env)
	if err != nil || len(names) == 0 {
		return nil
	}
	bound := boundNativeProviders(cfg)
	var gaps []string
	for _, n := range names {
		if !bound[n] {
			gaps = append(gaps, n)
		}
	}
	sort.Strings(gaps)
	return gaps
}
