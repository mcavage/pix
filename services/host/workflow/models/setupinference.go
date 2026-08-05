// setupinference.go — the inference-selection, live-verification and roster
// half of `pix models`. It moved here wholesale from the deleted setup phase
// machine: `pix models add` was always its real caller, and setup now only
// PROBES inference rather than interviewing the user about it.
package models

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/inference"
	"pix/host/routing"
	"pix/host/secret"
)

// is skipped when a pack or prior setup already supplied a backend. Ollama is
// shown only after both binary and daemon probes succeed.
func SetupChooseInference(cfg *config.Config, env hostenv.Env, in io.Reader, out io.Writer, interactive bool) (bool, error) {
	if cfg == nil {
		return false, nil
	}
	if len(cfg.Inference.Backends) > 0 {
		if inference.InferenceNeedsOnePassword(cfg) {
			return false, nil
		}
		if err := EnableDeclaredInferenceBindings(cfg); err != nil {
			return false, err
		}
		return true, nil
	}
	if !interactive {
		return false, nil
	}
	ollamaReady := false
	if _, err := env.LookPath("ollama"); err == nil {
		_, timedOut, runErr := env.RunTimed("ollama", "list")
		ollamaReady = runErr == nil && !timedOut
	}
	fmt.Fprintln(out, "How should Pix run models? (choose one or more, comma-separated)")
	fmt.Fprintln(out, "  1. API key (default)     Anthropic / OpenAI / Google keys, resolved from 1Password")
	if ollamaReady {
		fmt.Fprintln(out, "  2. Ollama local          models that run on this machine")
	}
	fmt.Fprintln(out, "  3. Custom gateway        an OpenAI-compatible endpoint you host")
	if ollamaReady {
		fmt.Fprintln(out, "  4. Ollama Cloud          large models on your ollama.com subscription")
		// A HINT, not entitlement. A `:cloud` row in the listing appears on every
		// signed-in machine and proves nothing about what the plan may call — that
		// inference is exactly how a gated model got bound and 401'd at call time.
		if n := listedCloudTagCount(env); n > 0 {
			fmt.Fprintf(out, "  (this machine lists %d cloud model(s); Pix proves which ones your plan can call)\n", n)
		}
	}
	fmt.Fprint(out, "Choose [1]: ")
	choice, ok := readSetupLine(in)
	if !ok {
		return false, fmt.Errorf("no inference choice; setup cannot continue")
	}
	if strings.TrimSpace(choice) == "" {
		return false, nil
	}
	selected := map[string]bool{}
	for _, raw := range strings.FieldsFunc(strings.ToLower(choice), func(r rune) bool { return r == ',' || r == ' ' }) {
		switch raw {
		case "1", "api":
			selected["api"] = true
		case "2", "ollama", "ollama-local", "local":
			selected["ollama"] = true
		case "3", "gateway":
			selected["gateway"] = true
		case "4", "ollama-cloud", "cloud":
			selected["ollama-cloud"] = true
		default:
			return false, fmt.Errorf("unknown inference choice %q", raw)
		}
	}
	if selected["ollama"] || selected["ollama-cloud"] {
		if !ollamaReady {
			return false, fmt.Errorf("Ollama is not installed and healthy, so it is not an available inference choice")
		}
		sel := OllamaSelection{Local: selected["ollama"], Cloud: selected["ollama-cloud"]}
		if _, err := ConfigureOllamaInference(cfg, env, sel, out); err != nil {
			return false, err
		}
	}
	if selected["gateway"] {
		if _, err := configureCustomGateway(cfg, in, out); err != nil {
			return false, err
		}
	}
	// false means the caller must continue through the direct-key transaction.
	return !selected["api"], nil
}

// VerifyOllamaInference earns Verified for ollama bindings with an actual
// model-specific request through the RESOLVED endpoint. Every binding is
// checked independently. CLOUD probes run concurrently (they are network round
// trips and hold no local resource). LOCAL probes are SERIALIZED and unload
// after themselves: two concurrent generates make Ollama co-load two sets of
// weights, which either exhausts the memory budget readiness_hardware.go just
// computed or serializes the loads anyway behind timers that started at
// dispatch — so the second reports a timeout it never got a turn to spend, and
// un-binds a model that works. Mirrors VerifyDirectInference in structure, not
// in concurrency.
//
// The local set runs LARGEST RUNG FIRST under its own wall budget, and a probe
// is never STARTED unless its full timeout fits what remains: the budget can
// therefore never manufacture a timeout. A candidate the budget never reached
// is `not probed` — a THIRD state that is neither verified nor failed, excluded
// from attempted, and never rendered as a rejection.
//
// Deviation from the design's signature: it takes an io.Writer (the design's
// own output spec prints a live line per local probe, which cannot be done
// without one) and returns the notProbed set (the third state has to be
// observable to be assertable).
func VerifyOllamaInference(cfg *config.Config, env hostenv.Env, out io.Writer) (res probeOutcome, err error) {
	if cfg == nil {
		return res, fmt.Errorf("verify ollama inference: no config")
	}
	if env.OllamaInference == nil {
		return res, ErrNoProbeSeam
	}
	if out == nil {
		out = io.Discard
	}
	reg, err := routing.LoadRegistry()
	if err != nil {
		return res, fmt.Errorf("verify ollama inference: %w", err)
	}
	endpoint := strings.TrimRight(inference.OllamaEndpointFor(env).URL, "/")
	type candidate struct {
		index  int
		label  string
		Tag    string
		numCtx int
		minRAM float64
	}
	var local, cloud []candidate
	for i := range cfg.Inference.Models {
		binding := &cfg.Inference.Models[i]
		backend, ok := cfg.Inference.Backends[binding.Backend]
		if !ok || backend.Driver != "ollama" || !binding.Available || !inference.Allowed(cfg, *binding) {
			continue
		}
		if binding.Source != "" {
			// A pack's authority is the sandbox smoke test; a host probe must not
			// demote what it cannot faithfully replay.
			continue
		}
		// Demote first: a stale claim (including a pre-provenance listing-derived
		// one) must never survive a run that could not re-earn it.
		binding.Verified, binding.VerifiedBy, binding.VerifiedAt = false, "", ""
		c := candidate{index: i, label: binding.Model, Tag: binding.Upstream}
		m, found := reg.Get(binding.Model)
		if found && m.Local {
			// num_ctx is the rung's DECLARED context budget, so the probe allocates
			// the same KV cache the RAM gate priced. A rung that cannot hold its own
			// declared context fails here, which is exactly when we want to find out.
			c.numCtx, c.minRAM = m.ContextWindow, m.MinRAMGB
			local = append(local, c)
			continue
		}
		cloud = append(cloud, c)
	}

	promote := func(index int) {
		cfg.Inference.Models[index].Verified = true
		cfg.Inference.Models[index].VerifiedBy = config.VerifiedByProbe
		cfg.Inference.Models[index].VerifiedAt = time.Now().UTC().Format(time.RFC3339)
		res.Verified++
	}

	// Cloud: concurrent. Nothing local is held, so N probes cost one timeout.
	type result struct {
		index int
		label string
		err   error
	}
	results := make(chan result, len(cloud))
	for _, c := range cloud {
		res.Attempted++
		go func(c candidate) {
			results <- result{index: c.index, label: c.label, err: env.OllamaInference(endpoint, c.Tag, 0, ollamaCloudProbeTimeout)}
		}(c)
	}

	// Local: strictly serial, largest rung first, each unloading after itself.
	sort.Slice(local, func(i, j int) bool {
		if local[i].minRAM != local[j].minRAM {
			return local[i].minRAM > local[j].minRAM
		}
		return local[i].label < local[j].label
	})
	if len(local) > 0 {
		fmt.Fprintf(out, "  verifying %d local ollama model(s), one at a time (each is loaded and unloaded) ...\n", len(local))
	}
	remaining := OllamaLocalProbeBudget
	for _, c := range local {
		if remaining < OllamaLocalProbeTimeout {
			// NOT a failure: this candidate never got a turn. Reporting it as broken
			// would let a budget un-bind a healthy model.
			res.NotProbed = append(res.NotProbed, c.label)
			fmt.Fprintf(out, "    %-14s not probed — %.0fs left of the %.0fs local budget, less than one probe's %.0fs\n",
				c.Tag, remaining.Seconds(), OllamaLocalProbeBudget.Seconds(), OllamaLocalProbeTimeout.Seconds())
			continue
		}
		res.Attempted++
		start := time.Now()
		err := env.OllamaInference(endpoint, c.Tag, c.numCtx, OllamaLocalProbeTimeout)
		elapsed := time.Since(start)
		if remaining -= elapsed; remaining < 0 {
			remaining = 0
		}
		if err != nil {
			res.Failures = append(res.Failures, c.label+": "+err.Error())
			fmt.Fprintf(out, "    %-14s failed (%.0fs): %v\n", c.Tag, elapsed.Seconds(), err)
			continue
		}
		promote(c.index)
		fmt.Fprintf(out, "    %-14s ok (%.0fs)\n", c.Tag, elapsed.Seconds())
	}

	for range cloud {
		r := <-results
		if r.err != nil {
			res.Failures = append(res.Failures, r.label+": "+r.err.Error())
			continue
		}
		promote(r.index)
	}
	sort.Strings(res.Failures)
	sort.Strings(res.NotProbed)
	return res, nil
}

// ConfigureModelRoster turns the broad set of backend bindings into the small,
// explicit catalog-model surface agents may use. The router continues to pick
// by intent, but it can never escape this roster. A mandatory pack is already
// an explicit policy decision and therefore skips the personal roster prompt.
func ConfigureModelRoster(cfg *config.Config, in io.Reader, out io.Writer, interactive bool, requested string) error {
	return configureModelRosterFrom(cfg, in, out, interactive, requested, nil)
}

// ReconcileDirectInference turns the provider keys that exist on this host into
// callable model bindings. It is the sequence that used to live only inside
// setup's keys step, which is why adding a key any other way left it inert:
// `pix secret set` wrote the ref, and nothing ever rebuilt the bindings, probed
// them, or widened the roster.
//
// Order is load-bearing and matches the step it was extracted from:
//
//		capture prior providers -> bind -> verify -> save -> judge -> roster
//
//	  - The prior set is captured BEFORE binding, or widening cannot see what is
//	    new (see rosterSeenProviders).
//	  - cfg.Save() happens BEFORE the verified==0 verdict, so a partial success is
//	    never thrown away by the error path.
//	  - verified == 0 with something attempted is a hard error; verified > 0 with
//	    some failures is not.
//
// requestedProvider is the provider the USER named on the command line
// (`pix models add google`), or "" for setup's own reconcile. It is the only
// thing that can override the roster's already-offered stamp — see
// WidenRosterForProvider.
func ReconcileDirectInference(cfg *config.Config, env hostenv.Env, in io.Reader, out io.Writer, interactive bool, requestedModels, requestedProvider string) (reconcileResult, error) {
	var res reconcileResult
	if cfg == nil {
		return res, fmt.Errorf("no config")
	}
	if cfg.Inference.ExclusiveSource != "" {
		return res, ErrInferenceExclusive
	}
	if out == nil {
		out = io.Discard
	}
	prior := inference.BoundNativeProviders(cfg)

	providers, err := secret.HostModeProviderKeys(env)
	if err != nil {
		return res, fmt.Errorf("reading configured providers: %w", err)
	}
	res.Providers = providers
	for _, p := range providers {
		if !prior[p] {
			res.Added = append(res.Added, p)
		}
	}
	sort.Strings(res.Added)

	if err := ConfigureDirectInference(cfg, providers); err != nil {
		return res, fmt.Errorf("configuring direct inference: %w", err)
	}
	// Widen BEFORE probing. VerifyDirectInference only probes bindings the roster
	// allows (inferenceBindingAllowed), so on a config with a non-empty roster
	// the newly added provider would otherwise never be probed, never become
	// callable, and be pruned straight back out of the roster for not being
	// callable — the key stays inert and the command still reports success.
	widenRosterForNewProviders(cfg, prior)
	WidenRosterForProvider(cfg, requestedProvider)
	outcome, verr := VerifyDirectInference(cfg, env)
	if verr != nil {
		return res, fmt.Errorf("verifying provider keys: %w", verr)
	}
	res.probeOutcome = outcome
	if err := cfg.Save(); err != nil {
		return res, err
	}
	if res.Verified == 0 && (res.Attempted > 0 || len(res.Failures) > 0) {
		detail := strings.Join(res.Failures, "; ")
		if detail == "" {
			detail = "no provider accepted a model-specific request"
		}
		return res, fmt.Errorf("provider keys resolved, but live inference verification failed: %s", detail)
	}
	if callable, _ := inference.ConfiguredSummary(cfg); callable > 0 || strings.TrimSpace(requestedModels) != "" {
		if err := configureModelRosterFrom(cfg, in, out, interactive, requestedModels, prior); err != nil {
			return res, fmt.Errorf("choosing models: %w", err)
		}
	}
	return res, cfg.Save()
}
