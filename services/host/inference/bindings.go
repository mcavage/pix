// Package inference answers ONE question for every binary in the stack: which
// catalog models can this host actually call right now?
//
// The shipped catalog (routing.LoadRegistry) says what a model IS — its prices,
// its limits, and whether Pix still routes to it at all (`available: false` is
// RETIRED, an editorial fact about the catalog). It says nothing about whether
// YOU can call it. That second fact lives in config: a backend binding, probed
// and marked Verified, filtered by the topology (an exclusive pack) and by the
// personal roster.
//
// Those two facts were conflated once already, and the bug was invisible in the
// worst way: `pix models show` rendered the catalog's `available` under a column
// named AVAIL and resolved every intent against it, so a host with no OpenAI key
// was told its default intent routed to openai/gpt-5.6-sol — and `pix models
// route` then WROTE that route into routing.json, which host-mode subagents
// read. The binding-aware resolve existed the whole time; it just lived in the
// launcher where the host binary could not reach it. This package is that logic
// with the import cycle removed, so there is one implementation of "callable"
// and both binaries answer the question the same way.
//
// Layering: routing stays config-free (see routing.Dir) and config stays
// routing-free. This package sits above both and is the only place allowed to
// know about the two together.
package inference

import (
	"strings"

	"pix/host/config"
	"pix/host/routing"
)

// Bindings returns the callable bindings in cfg, in config order, as the
// routing package's Binding type. A binding that is not Callable is omitted
// entirely rather than emitted with Available:false — downstream consumers read
// presence, and an unavailable entry has fooled that reading before.
func Bindings(cfg *config.Config) []routing.Binding {
	if cfg == nil {
		return nil
	}
	out := make([]routing.Binding, 0, len(cfg.Inference.Models))
	for _, b := range cfg.Inference.Models {
		if !Callable(cfg, b) {
			continue
		}
		out = append(out, routing.Binding{Model: b.Model, Backend: b.Backend, UpstreamID: b.Upstream, Available: true})
	}
	return out
}

// Configured reports whether this host has made any inference decision at all.
// It is the guard that keeps a fresh, unconfigured box honest in the other
// direction: with no bindings, filtering the catalog by bindings would render
// EVERY model unavailable and resolve every intent to a fallback, which
// describes nothing useful. Bindings are the availability authority only once
// they exist.
func Configured(cfg *config.Config) bool {
	return cfg != nil && len(cfg.Inference.Models) > 0
}

// BoundRegistry narrows catalog to what this host can call, and reports whether
// the narrowing actually happened. bound == false means the caller is looking at
// the raw catalog (no bindings configured yet) and MUST say so rather than let a
// catalog row read as a callable model.
//
// The returned registry keeps every catalog row — a retired or unbound model
// stays present with Available:false — so a caller can still show the full
// catalog and mark it, which is what makes `pix models ls` useful on a host
// that has only wired one provider.
func BoundRegistry(cfg *config.Config, catalog *routing.Registry) (reg *routing.Registry, bound bool) {
	if catalog == nil || !Configured(cfg) {
		return catalog, false
	}
	// No exclusiveBackend argument: Bindings has already applied the topology
	// filter (topologyAllowed), so passing it again would be a second, redundant
	// spelling of the same rule — and two spellings of one rule is how they drift.
	return routing.RegistryForBindings(catalog, Bindings(cfg), ""), true
}

// Callable is the single definition of "this host can call this model now":
// probed available, allowed by topology and roster, pointing at a backend that
// exists, and — where Pix is able to prove it from the host — actually proven.
func Callable(cfg *config.Config, b config.InferenceModelBinding) bool {
	if cfg == nil || !b.Available || !Allowed(cfg, b) {
		return false
	}
	if _, ok := cfg.Inference.Backends[b.Backend]; !ok {
		return false
	}
	return !needsHostProof(cfg, b) || b.Verified
}

// Allowed applies the two policy filters that are independent of any probe: the
// topology (an exclusive pack owns the whole surface) and the personal roster.
func Allowed(cfg *config.Config, b config.InferenceModelBinding) bool {
	if !TopologyAllowed(cfg, b) {
		return false
	}
	// A mandatory pack owns the whole inference surface while active. Preserve
	// the personal roster underneath so detaching the pack restores it.
	if cfg.Inference.ExclusiveSource != "" || len(cfg.Inference.AllowedModels) == 0 {
		return true
	}
	for _, id := range cfg.Inference.AllowedModels {
		if id == b.Model {
			return true
		}
	}
	return false
}

// TopologyAllowed is Allowed without the personal roster: it answers only "is
// this binding part of the surface this host's topology permits". Callers that
// are BUILDING the roster need this — asking Allowed there would filter the
// candidate list by the very answer they are about to compute.
func TopologyAllowed(cfg *config.Config, b config.InferenceModelBinding) bool {
	if cfg.Inference.ExclusiveSource != "" {
		return b.Source == cfg.Inference.ExclusiveSource
	}
	return cfg.Inference.ExclusiveBackend == "" || b.Backend == cfg.Inference.ExclusiveBackend
}

// BackendAllowed is Allowed's counterpart for a whole backend: an exclusive
// pack or an exclusive backend makes every other backend dormant.
func BackendAllowed(cfg *config.Config, b config.InferenceBackend, name string) bool {
	if cfg.Inference.ExclusiveSource != "" {
		return b.Source == cfg.Inference.ExclusiveSource
	}
	return cfg.Inference.ExclusiveBackend == "" || name == cfg.Inference.ExclusiveBackend
}

// needsHostProof reports whether Pix CAN — and therefore MUST — prove this
// binding from the host before calling it callable. It replaces an inline
// `backend.Auth != "1password"` shortcut that made honest Ollama verification
// cosmetic: the ollama backend is written with Auth "none", so an ollama binding
// used to be callable regardless of Verified. That is the hole the gated-cloud-
// model incident came through.
//
// Pack-declared bindings are exempt ONLY where the exemption is earned: a pack's
// authority is the sandbox smoke test, because sbx-session auth cannot be
// faithfully replayed by a host HTTP probe.
//
// That reasoning does NOT extend to a pack's 1Password-backed native backend,
// which packs may legally declare. Host proof for those is not merely possible,
// it already happens: verification probes every 1password binding with no Source
// check, and demotes the ones that fail. Exempting them by source would let a
// binding whose probe was DISPATCHED AND REFUSED stay callable, and flow on into
// the compiled manifest, the sandbox kit, and doctor's "N callable model(s)" — a
// success word behind a failed probe. So the exemption is scoped to the auth Pix
// cannot verify from here.
func needsHostProof(cfg *config.Config, b config.InferenceModelBinding) bool {
	backend, ok := cfg.Inference.Backends[b.Backend]
	if !ok {
		return false
	}
	if b.Source != "" && backend.Auth != "1password" {
		return false
	}
	return backend.Auth == "1password" || backend.Driver == "ollama"
}

// RuntimeID is the id a runtime provider calls a bound model by: the upstream id
// qualified by its backend, without double-qualifying one that already is.
func RuntimeID(b routing.Binding) string {
	if routing.IsQualifiedID(b.UpstreamID) && strings.HasPrefix(b.UpstreamID, b.Backend+"/") {
		return b.UpstreamID
	}
	return b.Backend + "/" + b.UpstreamID
}

// BoundNativeProviders is the set of providers that already had a native
// binding. Callers capture it BEFORE configureDirectInference mutates the
// bindings; that pre-mutation snapshot is the whole mechanism behind widening.
func BoundNativeProviders(cfg *config.Config) map[string]bool {
	out := map[string]bool{}
	if cfg == nil {
		return out
	}
	for _, b := range cfg.Inference.Models {
		if cfg.Inference.Backends[b.Backend].Driver == "native" {
			out[b.Backend] = true
		}
	}
	return out
}
