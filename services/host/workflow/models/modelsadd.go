package models

// modelsadd.go implements `pix models add <provider>`: the answer to "setup
// told me I could add the others later, but I could not find where." The only
// prior path, `pix secret set`, wrote the ref and stopped — nothing rebuilt the
// bindings, probed them, or widened the roster, so a second key was present,
// correct, and completely inert.

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strings"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/secret"
)

// AddKeyedProvider and AddOllamaProvider are the two shapes a provider comes
// in. They RETURN errors: the command contract owns the exit code.
func AddKeyedProvider(d *cli.Deps, cfg *config.Config, env hostenv.Env, provider string) error {
	p, ok := ProviderByName(provider)
	if !ok {
		return cli.Usagef("unknown provider %q (want one of: %s)", provider, strings.Join(ProviderNames(), ", "))
	}
	// Refuse BEFORE touching anything; the key is still worth storing, so say so.
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
		if err := requireOnePassword(env); err != nil {
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
	res, err := ReconcileDirectInference(cfg, env, d.In, d.Out, d.Interactive, "", p.Name)
	if err != nil {
		return err
	}
	renderAdd(d.Out, p.Name, res, nil)
	// The sandbox reads the credential from sbx, not from this host's refs file,
	// so a key that never reaches sbx is wired for host mode only.
	secret.RunSecretSync(env, d.Out)
	return nil
}

// AddOllamaProvider is the keyless half: no ref to prompt for, nothing to sync
// into sbx. The job is — is the daemon up, what does it list, which of those
// can this machine/plan actually run, and put the survivors in the roster.
// With neither --local nor --cloud it does BOTH: a user typing `pix models add
// ollama` means "take everything you can prove", and making them guess the flag
// is the discoverability failure this command exists to end.
func AddOllamaProvider(d *cli.Deps, cfg *config.Config, env hostenv.Env, sel OllamaSelection) error {
	if !sel.Local && !sel.Cloud {
		sel = OllamaSelection{Local: true, Cloud: true}
	}
	if cfg.Inference.ExclusiveSource != "" {
		return fmt.Errorf("the active pack (%s) owns inference on this host, so Ollama cannot be wired in.\n"+
			"  It gets wired the moment the pack stops being the exclusive source (`pix pack rm`, or a pack that does not claim inference).",
			cfg.Inference.ExclusiveSource)
	}
	res, plan, err := ReconcileOllamaInference(cfg, env, d.In, d.Out, d.Interactive, sel)
	if err != nil {
		return err
	}
	renderAdd(d.Out, "ollama", res, &plan)
	return nil
}

// renderAdd reports what was PROVEN, never what was merely written: the counts
// come from live per-model probes, so a provider whose models all failed reads
// as a shortfall even though its key resolved fine. plan is Ollama's extra
// evidence (nil for a keyed provider): a rung worth pulling, and rungs this
// machine is too small for.
func renderAdd(out io.Writer, provider string, res reconcileResult, plan *ollamaPlan) {
	if len(res.Added) == 0 {
		fmt.Fprintf(out, "%s was already wired; re-checked it.\n", provider)
	}
	if plan != nil {
		fmt.Fprintf(out, "%d Ollama model(s) answered a live generate at %s.\n", res.Verified, plan.Endpoint)
	} else {
		fmt.Fprintf(out, "%d model(s) answered a live request across %d provider(s).\n", res.Verified, len(res.Providers))
	}
	if len(res.Failures) > 0 {
		fmt.Fprintf(out, "%d candidate(s) did not answer: %s\n", len(res.Failures), strings.Join(res.Failures, "; "))
	}
	if plan != nil {
		// Name the pullable rung in BOTH cases, or the offer line already printed
		// is left hanging.
		switch {
		case plan.WantPull != "":
			fmt.Fprintf(out, "\nNot downloaded: %s is the largest local model that fits this machine, but it is not pulled.\n", plan.WantPull)
			fmt.Fprintf(out, "  ollama pull %s && pix models add ollama --local\n", plan.WantPull)
		case plan.BestFit != "" && !ContainsString(plan.LocalBoundTags(), plan.BestFit):
			fmt.Fprintf(out, "\nA larger local model fits this machine but is not pulled: %s\n", plan.BestFit)
			fmt.Fprintf(out, "  ollama pull %s && pix models add ollama --local\n", plan.BestFit)
		}
		if len(plan.SkippedRAM) > 0 {
			fmt.Fprintf(out, "Too large for this machine (%0.f GB usable): %s\n", plan.Memory.UsableGB, strings.Join(plan.SkippedRAM, ", "))
		}
	}
	fmt.Fprintln(out, "Next: pix models        (see the roster)")
	fmt.Fprintln(out, "      pix models route  (re-resolve intents onto it)")
}

// ProviderNames is what `models add` accepts — NOT secret.ProviderKeyRefOrder:
// ollama is addable and keyless, and leaving it out of the error message is how
// a user concludes it cannot be added at all.
func ProviderNames() []string {
	names := make([]string, 0, len(secret.ProviderKeyRefOrder)+1)
	for _, p := range secret.ProviderKeyRefOrder {
		names = append(names, p.Name)
	}
	names = append(names, "ollama")
	sort.Strings(names)
	return names
}

// ProviderByName accepts the name, the env var and the gemini/google alias, so
// a user who read the key name in a doc is not told it is wrong. Ollama is
// absent on purpose: keyless, and a zero ref would send `pix secret set ""`.
func ProviderByName(raw string) (secret.ProviderKeyRef, bool) {
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
