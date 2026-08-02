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
	"sort"
	"strings"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/secret"
)

// addKeyedProvider and addOllamaProvider are the two shapes a provider comes
// in. They RETURN errors: the command contract owns the exit code, so neither
// calls os.Exit, and both are testable against a bytes.Buffer.
//
// They replaced runModelsAdd, which hand-parsed argv, validated the provider
// name against a list it maintained separately from providerNames(), and exited
// the process from nine places.
func addKeyedProvider(d *cli.Deps, cfg *config.Config, env shellEnv, provider string) error {
	p, ok := providerByName(provider)
	if !ok {
		return cli.Usagef("unknown provider %q (want one of: %s)", provider, strings.Join(providerNames(), ", "))
	}
	// Refuse under a mandatory pack BEFORE touching anything. configureDirect-
	// Inference would happily write bindings that the topology filter then drops
	// silently, so "added" would be a success word with nothing behind it.
	if cfg.Inference.ExclusiveSource != "" {
		return fmt.Errorf("the active pack (%s) owns inference on this host, so a provider key cannot be wired in.\n"+
			"  The key itself is still worth storing now: pix secret set %s op://vault/item/field\n"+
			"  It gets wired the moment the pack stops being the exclusive source (`pix pack rm`, or a pack that does not claim inference).",
			cfg.Inference.ExclusiveSource, p.EnvVar)
	}
	if _, hasRef := secret.CurrentOpRef(env, p.EnvVar); !hasRef {
		if !d.Interactive {
			return fmt.Errorf("no 1Password ref for %s yet, and there is no terminal to ask on.\n"+
				"  pix secret set %s op://vault/item/field && pix models add %s", p.Name, p.EnvVar, p.Name)
		}
		if err := ensureSetupPrereqsFor(env, d.In, d.Out, d.Interactive, true); err != nil {
			return err
		}
		ref, _, ok := promptProviderRef(env, bufio.NewScanner(d.In), d.Out, p)
		if !ok {
			return cli.SilentError{Code: 1}
		}
		if err := secret.RunSecretSet(env, d.Out, p.EnvVar, ref); err != nil {
			return cli.SilentError{Code: 1}
		}
	}
	res, err := reconcileDirectInference(cfg, env, d.In, d.Out, d.Interactive, "", p.Name)
	if err != nil {
		return err
	}
	renderModelsAdd(d.Out, p.Name, res)
	// The sandbox reads the credential from sbx, not from this host's refs file,
	// so a key that never reaches sbx is wired for host mode only.
	secret.RunSecretSync(env, d.Out)
	return nil
}

// addOllamaProvider is the keyless half. Ollama needs no credential, so there
// is no ref to prompt for and nothing to sync into sbx — the whole job is: is
// the daemon up, what does it list, which of those can this machine/plan
// actually run, and put the survivors in the roster.
//
// With neither --local nor --cloud it does BOTH. They are separate products (a
// `:cloud` row appears on every signed-in machine and says nothing about what
// this box can run), but a user typing `pix models add ollama` means "take
// everything you can prove", and making them guess which flag they needed would
// be the discoverability failure this command was written to end.
func addOllamaProvider(d *cli.Deps, cfg *config.Config, env shellEnv, sel ollamaSelection) error {
	if !sel.Local && !sel.Cloud {
		sel = ollamaSelection{Local: true, Cloud: true}
	}
	if cfg.Inference.ExclusiveSource != "" {
		return fmt.Errorf("the active pack (%s) owns inference on this host, so Ollama cannot be wired in.\n"+
			"  It gets wired the moment the pack stops being the exclusive source (`pix pack rm`, or a pack that does not claim inference).",
			cfg.Inference.ExclusiveSource)
	}
	res, plan, err := reconcileOllamaInference(cfg, env, d.In, d.Out, d.Interactive, sel)
	if err != nil {
		return err
	}
	renderModelsAddOllama(d.Out, res, plan)
	return nil
}

// renderModelsAddOllama reports proof, then the two things a user can act on:
// a rung worth pulling, and rungs this machine is too small for. Both are said
// out loud because the alternative — binding nothing and reporting a bare count
// — is what made the local flow feel broken.
func renderModelsAddOllama(out io.Writer, res reconcileResult, plan ollamaPlan) {
	if len(res.Added) == 0 {
		fmt.Fprintln(out, "ollama was already wired; re-checked it.")
	}
	fmt.Fprintf(out, "%d Ollama model(s) answered a live generate at %s.\n", res.Verified, plan.Endpoint)
	if len(res.Failures) > 0 {
		fmt.Fprintf(out, "%d candidate(s) did not answer: %s\n", len(res.Failures), strings.Join(res.Failures, "; "))
	}
	// Name the pullable rung in BOTH cases, or the offer line configureOllama-
	// Inference already printed ("offering qwen3.5:35b") is left hanging.
	switch {
	case plan.WantPull != "":
		fmt.Fprintf(out, "\nNot downloaded: %s is the largest local model that fits this machine, but it is not pulled.\n", plan.WantPull)
		fmt.Fprintf(out, "  ollama pull %s && pix models add ollama --local\n", plan.WantPull)
	case plan.BestFit != "" && !containsString(plan.LocalBoundTags(), plan.BestFit):
		fmt.Fprintf(out, "\nA larger local model fits this machine but is not pulled: %s\n", plan.BestFit)
		fmt.Fprintf(out, "  ollama pull %s && pix models add ollama --local\n", plan.BestFit)
	}
	if len(plan.SkippedRAM) > 0 {
		fmt.Fprintf(out, "Too large for this machine (%0.f GB usable): %s\n", plan.Memory.UsableGB, strings.Join(plan.SkippedRAM, ", "))
	}
	fmt.Fprintln(out, "Next: pix models        (see the roster)")
	fmt.Fprintln(out, "      pix models route  (re-resolve intents onto it)")
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

// providerNames is what `models add` accepts, which is NOT the same list as
// secret.ProviderKeyRefOrder: ollama is a provider you can add and has no key ref, and
// leaving it out of the error message is how a user concludes it cannot be
// added at all.
func providerNames() []string {
	names := make([]string, 0, len(secret.ProviderKeyRefOrder)+1)
	for _, p := range secret.ProviderKeyRefOrder {
		names = append(names, p.Name)
	}
	names = append(names, "ollama")
	sort.Strings(names)
	return names
}

// providerByName accepts the provider name, its env var, and the obvious alias
// (gemini for google), so a user who read the key name in a doc is not told
// their own credential's name is wrong.
func providerByName(raw string) (secret.ProviderKeyRef, bool) {
	want := strings.ToLower(strings.TrimSpace(raw))
	if want == "gemini" {
		want = "google"
	}
	for _, p := range secret.ProviderKeyRefOrder {
		if want == p.Name || strings.EqualFold(want, p.EnvVar) {
			return p, true
		}
	}
	return secret.ProviderKeyRef{}, false
}

func modelsAddUsage() string {
	return `usage: pix models add <provider> [--local] [--cloud]

Wire a provider into callable models, end to end: rebuild the model bindings,
prove each one with a live request, widen the roster to include it, and leave
nothing claimed that was not proven.

providers: ` + strings.Join(providerNames(), ", ") + `

  anthropic | openai | google    keyed. Stores the provider's 1Password ref if
                                 it has none yet (prompts on a terminal), then
                                 reconciles the key into sbx so the sandbox can
                                 use it too.
  ollama                         keyless. Reads what your local daemon lists and
                                 proves each one with a real generate. Does both
                                 local and cloud models unless you narrow it:
                                   --local   models that run on this machine
                                   --cloud   models on your ollama.com plan
                                 Downloads nothing; it names a tag worth pulling
                                 and leaves the decision to you.

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
	names, err := secret.HostModeProviderKeys(env)
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
