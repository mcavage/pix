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
	cat, err := inference.LoadCatalog()
	if err != nil {
		return res, fmt.Errorf("verify ollama inference: %w", err)
	}
	endpoint := strings.TrimRight(inference.OllamaEndpointFor(env).URL, "/")
	// A pack's authority is the sandbox smoke test; a host probe must not demote
	// what it cannot replay.
	cands := eligible(cfg, func(b config.InferenceModelBinding, backend config.InferenceBackend) bool {
		return backend.Driver == "ollama" && b.Source == ""
	})
	// A candidate with no catalog row (an unknown pulled tag — see
	// ConfigureOllamaInference's second pass) carries NO stored local/cloud
	// classification: reg.Get finds nothing for it. Without re-deriving that
	// classification here, every such candidate fell into the CLOUD bucket below
	// unconditionally — including ones ConfigureOllamaInference itself just
	// classified and bound as LOCAL — so a big local model got probed
	// CONCURRENTLY with N others at num_ctx 0 under a 20s cloud timeout: the exact
	// RAM-exhaustion hazard the serialized local loop below exists to prevent,
	// and a model that cannot cold-load that fast in that company never became
	// callable. Re-reading the SAME signals (remote_host / :cloud-suffix / size)
	// against a fresh listing carries the classification through. Best-effort: a
	// listing failure here (the daemon answered during configure but not now) is
	// this function's PRE-FIX behavior for that one edge case — fall through to
	// the cloud bucket — not a new regression.
	unknownTagInfo, _ := ollamaListing(env)
	var local, cloud []candidate
	for _, c := range cands {
		// num_ctx is the rung's DECLARED context, so the probe allocates the same
		// KV cache the RAM gate priced: a rung that cannot hold its own context
		// fails here, which is exactly when we want to find out.
		m, found := cat.Get(c.label)
		if found && m.Local {
			// RAM comes from the rung table (E4.3), the one home for local
			// hardware facts; the catalog declares the context to probe with.
			rung, _ := inference.LocalOllamaRungFor(m.ID)
			c.numCtx, c.minRAM = m.ContextWindow, rung.MinRAMGB
			local = append(local, c)
			continue
		}
		if !found {
			if info, ok := unknownTagInfo[inference.OllamaTagFor(c.label)]; ok {
				if cloudTag, classified := classifyOllamaTag(inference.OllamaTagFor(c.label), info); classified && !cloudTag {
					// Unknown-but-classified-LOCAL: no declared context (nothing to price
					// a KV cache from), so numCtx stays 0 — the same shape a cloud probe
					// would send — but this candidate joins the SERIALIZED, budgeted local
					// loop, never the concurrent cloud one.
					local = append(local, c)
					continue
				}
			}
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

func orDiscard(out io.Writer) io.Writer {
	if out == nil {
		return io.Discard
	}
	return out
}
