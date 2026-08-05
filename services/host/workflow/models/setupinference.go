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

// setupinference.go — the PROBE half of `pix models`: choose a runtime, prove
// every binding with a real request, then reconcile (bind -> widen -> probe ->
// save -> judge -> roster). Direct keys and Ollama share this file because they
// share that sequence; only the evidence differs.

// probeOutcome is what a verification pass ESTABLISHED, as one value instead of
// four positional returns a call site could silently drop a field from.
type probeOutcome struct {
	Attempted int
	Verified  int
	Failures  []string
	// NotProbed is neither verified nor failed: the local probe budget ran out
	// before this candidate got a turn. Reporting it as a failure would blame a
	// model for a clock.
	NotProbed []string
}

// ErrNoProbeSeam is returned when a verify function is handed a hostenv.Env
// with no probe. That is a PROGRAMMING error, and it must never be reported as
// `0 attempted, 0 verified, no failures` — indistinguishable from a clean pass.
var ErrNoProbeSeam = fmt.Errorf("no inference probe is configured on this hostenv.Env (use defaultShellEnv, or inject a probe in tests)")

// reconcileResult is what a reconcile actually did and proved.
type reconcileResult struct {
	Providers []string // every provider with a resolvable key, sorted
	Added     []string // providers that had no native binding before this run
	probeOutcome
}

// candidate is one binding a verify pass may probe. It carries the index, not a
// pointer, so promotion writes through cfg — which is what gets saved.
type candidate struct {
	index          int
	label, backend string
	tag            string  // the id the backend itself speaks
	numCtx         int     // a local rung's declared context, 0 elsewhere
	minRAM         float64 // sorts the local set largest-rung-first
}

// eligible walks the bindings once, DEMOTING every claim it is about to re-earn
// and returning the ones accept() admits. Demote-then-probe is the invariant: a
// stale claim must never survive a run that could not re-earn it.
func eligible(cfg *config.Config, accept func(config.InferenceModelBinding, config.InferenceBackend) bool) []candidate {
	var out []candidate
	for i := range cfg.Inference.Models {
		b := &cfg.Inference.Models[i]
		backend, ok := cfg.Inference.Backends[b.Backend]
		if !ok || !b.Available || !inference.Allowed(cfg, *b) || !accept(*b, backend) {
			continue
		}
		b.Verified, b.VerifiedBy, b.VerifiedAt = false, "", ""
		out = append(out, candidate{index: i, label: b.Model, backend: b.Backend, tag: b.Upstream})
	}
	return out
}

// promote writes the claim and its provenance in the SAME assignment (and
// eligible clears both together), so provenance never outlives its claim.
func (res *probeOutcome) promote(cfg *config.Config, index int) {
	b := &cfg.Inference.Models[index]
	b.Verified, b.VerifiedBy, b.VerifiedAt = true, config.VerifiedByProbe, time.Now().UTC().Format(time.RFC3339)
	res.Verified++
}

// dispatch starts one goroutine per candidate and returns the collector. Split
// in two so the serialized local set can run while network probes are in
// flight. These probes hold nothing local, so N of them cost one timeout.
func (res *probeOutcome) dispatch(cands []candidate, probe func(candidate) error) func(*config.Config) {
	type result struct {
		c   candidate
		err error
	}
	done := make(chan result, len(cands))
	for _, c := range cands {
		res.Attempted++
		go func(c candidate) { done <- result{c, probe(c)} }(c)
	}
	return func(cfg *config.Config) {
		for range cands {
			r := <-done
			if r.err != nil {
				res.Failures = append(res.Failures, r.c.label+": "+r.err.Error())
				continue
			}
			res.promote(cfg, r.c.index)
		}
	}
}

// VerifyDirectInference earns Verified with a model-specific inference request
// per binding, concurrently. Resolved key bytes stay in process memory: never
// logged, never persisted.
func VerifyDirectInference(cfg *config.Config, env hostenv.Env) (res probeOutcome, err error) {
	if cfg == nil {
		return res, fmt.Errorf("verify direct inference: no config")
	}
	if env.DirectInference == nil {
		return res, ErrNoProbeSeam
	}
	cands := eligible(cfg, func(_ config.InferenceModelBinding, backend config.InferenceBackend) bool {
		return backend.Auth == "1password"
	})
	// One resolution and one failure line per provider: N bindings behind one
	// missing ref is one problem, not N.
	keys, failed := map[string]string{}, map[string]bool{}
	var probeable []candidate
	for _, c := range cands {
		if _, ok := keys[c.backend]; !ok {
			if failed[c.backend] {
				continue
			}
			key, reason := resolveProviderKey(env, cfg.Inference.Backends[c.backend])
			if reason != "" {
				failed[c.backend] = true
				res.Failures = append(res.Failures, c.backend+": "+reason)
				continue
			}
			keys[c.backend] = key
		}
		probeable = append(probeable, c)
	}
	collect := res.dispatch(probeable, func(c candidate) error {
		return env.DirectInference(c.backend, strings.TrimPrefix(c.tag, c.backend+"/"), keys[c.backend])
	})
	collect(cfg)
	sort.Strings(res.Failures)
	return res, nil
}

// resolveProviderKey returns the resolved key, or a reason naming which step
// failed — a missing ref and an unresolvable one have different fixes.
func resolveProviderKey(env hostenv.Env, backend config.InferenceBackend) (key, reason string) {
	ref, ok := secret.CurrentOpRef(env, backend.KeyEnv)
	if !ok {
		return "", "credential ref missing"
	}
	key, ok = secret.OpReadNonEmpty(env, ref)
	if !ok {
		return "", "credential could not be resolved"
	}
	return key, ""
}

// VerifyOllamaInference earns Verified through the RESOLVED endpoint. CLOUD
// probes run concurrently; LOCAL probes are SERIALIZED and unload after
// themselves, because two concurrent generates co-load two sets of weights and
// the second then reports a timeout it never got a turn to spend — un-binding a
// model that works.
//
// The local set runs LARGEST RUNG FIRST under a wall budget, and a probe is
// never STARTED unless its full timeout fits what remains, so the budget can
// never manufacture a timeout. A candidate it never reached is `not probed` — a
// THIRD state, excluded from attempted, never a rejection.
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
	// A pack's authority is the sandbox smoke test; a host probe must not demote
	// what it cannot replay.
	cands := eligible(cfg, func(b config.InferenceModelBinding, backend config.InferenceBackend) bool {
		return backend.Driver == "ollama" && b.Source == ""
	})
	var local, cloud []candidate
	for _, c := range cands {
		// num_ctx is the rung's DECLARED context, so the probe allocates the same
		// KV cache the RAM gate priced: a rung that cannot hold its own context
		// fails here, which is exactly when we want to find out.
		if m, found := reg.Get(c.label); found && m.Local {
			c.numCtx, c.minRAM = m.ContextWindow, m.MinRAMGB
			local = append(local, c)
			continue
		}
		cloud = append(cloud, c)
	}

	collectCloud := res.dispatch(cloud, func(c candidate) error {
		return env.OllamaInference(endpoint, c.tag, 0, ollamaCloudProbeTimeout)
	})

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
			// NOT a failure: it never got a turn, and a budget must not un-bind a
			// healthy model.
			res.NotProbed = append(res.NotProbed, c.label)
			fmt.Fprintf(out, "    %-14s not probed — %.0fs left of the %.0fs local budget, less than one probe's %.0fs\n",
				c.tag, remaining.Seconds(), OllamaLocalProbeBudget.Seconds(), OllamaLocalProbeTimeout.Seconds())
			continue
		}
		res.Attempted++
		start := time.Now()
		probeErr := env.OllamaInference(endpoint, c.tag, c.numCtx, OllamaLocalProbeTimeout)
		elapsed := time.Since(start)
		if remaining -= elapsed; remaining < 0 {
			remaining = 0
		}
		if probeErr != nil {
			res.Failures = append(res.Failures, c.label+": "+probeErr.Error())
			fmt.Fprintf(out, "    %-14s failed (%.0fs): %v\n", c.tag, elapsed.Seconds(), probeErr)
			continue
		}
		res.promote(cfg, c.index)
		fmt.Fprintf(out, "    %-14s ok (%.0fs)\n", c.tag, elapsed.Seconds())
	}

	collectCloud(cfg)
	sort.Strings(res.Failures)
	sort.Strings(res.NotProbed)
	return res, nil
}

// ReconcileDirectInference turns the provider keys on this host into callable
// bindings. Every path that adds a key routes through it, so a key written by
// `pix secret set` is rebuilt, probed and widened like setup's own.
//
// requestedProvider is the provider the USER named, or "" for setup's own
// reconcile; it alone overrides the roster's already-offered stamp.
func ReconcileDirectInference(cfg *config.Config, env hostenv.Env, in io.Reader, out io.Writer, interactive bool, requestedModels, requestedProvider string) (reconcileResult, error) {
	var res reconcileResult
	if err := reconcilable(cfg); err != nil {
		return res, err
	}
	// Captured BEFORE binding, or widening cannot see what is new.
	prior := inference.BoundNativeProviders(cfg)
	providers, err := secret.ProviderKeyNames(env)
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
	widenRosterForNewProviders(cfg, prior)
	WidenRosterForProvider(cfg, requestedProvider)
	outcome, verr := VerifyDirectInference(cfg, env)
	if verr != nil {
		return res, fmt.Errorf("verifying provider keys: %w", verr)
	}
	res.probeOutcome = outcome
	err = res.finish(cfg, in, orDiscard(out), interactive, requestedModels,
		"provider keys resolved, but live inference verification failed", "no provider accepted a model-specific request")
	return res, err
}

// ReconcileOllamaInference is ReconcileDirectInference's counterpart for the
// one backend with no key to store: same order, same honesty rules, but the
// evidence is `ollama list` plus a generate through the resolved endpoint. It
// exists because `models add` was built around secret.ProviderKeyRefOrder, so
// the one keyless backend had no post-setup path at all.
//
// Downloads nothing: the plan may NAME a rung worth pulling and leave the
// decision to the user, because `models add` is a wiring command.
func ReconcileOllamaInference(cfg *config.Config, env hostenv.Env, in io.Reader, out io.Writer, interactive bool, sel OllamaSelection) (reconcileResult, ollamaPlan, error) {
	var res reconcileResult
	if err := reconcilable(cfg); err != nil {
		return res, ollamaPlan{}, err
	}
	out = orDiscard(out)
	if err := RequireOllamaReady(env); err != nil {
		return res, ollamaPlan{}, err
	}
	res.Providers = []string{"ollama"}
	if _, existed := cfg.Inference.Backends["ollama"]; !existed {
		res.Added = []string{"ollama"}
	}
	plan, err := ConfigureOllamaInference(cfg, env, sel, out)
	if err != nil {
		return res, plan, err
	}
	WidenRosterForProvider(cfg, "ollama")
	outcome, verr := VerifyOllamaInference(cfg, env, out)
	if verr != nil {
		return res, plan, fmt.Errorf("verifying ollama models: %w", verr)
	}
	res.probeOutcome = outcome
	err = res.finish(cfg, in, out, interactive, "",
		"Ollama is reachable, but no model proved callable", "no Ollama model answered a generate request")
	return res, plan, err
}

// finish is the tail both reconciles share, and its ORDER is load-bearing:
// Save BEFORE the verdict (a partial success must survive the error path);
// verified == 0 with something attempted is a hard error while verified > 0
// with some failures is not; the roster comes LAST because it prunes whatever
// failed its probe.
func (res *reconcileResult) finish(cfg *config.Config, in io.Reader, out io.Writer, interactive bool, requestedModels, verdict, noDetail string) error {
	if err := cfg.Save(); err != nil {
		return err
	}
	if res.Verified == 0 && (res.Attempted > 0 || len(res.Failures) > 0) {
		detail := strings.Join(res.Failures, "; ")
		if detail == "" {
			detail = noDetail
		}
		return fmt.Errorf("%s: %s", verdict, detail)
	}
	if len(res.NotProbed) > 0 {
		fmt.Fprintf(out, "%d candidate(s) were not probed within the time budget: %s\n", len(res.NotProbed), strings.Join(res.NotProbed, ", "))
	}
	if callable, _ := inference.ConfiguredSummary(cfg); callable > 0 || strings.TrimSpace(requestedModels) != "" {
		if err := ConfigureModelRoster(cfg, in, out, interactive, requestedModels); err != nil {
			return fmt.Errorf("choosing models: %w", err)
		}
	}
	return cfg.Save()
}

// reconcilable refuses BEFORE any mutation: see ErrInferenceExclusive.
func reconcilable(cfg *config.Config) error {
	switch {
	case cfg == nil:
		return fmt.Errorf("no config")
	case cfg.Inference.ExclusiveSource != "":
		return ErrInferenceExclusive
	}
	return nil
}

func orDiscard(out io.Writer) io.Writer {
	if out == nil {
		return io.Discard
	}
	return out
}
