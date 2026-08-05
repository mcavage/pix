// Package inference answers ONE question for every binary in the stack: which
// catalog models can this host actually call right now?
//
// The catalog says what a model IS (prices, limits, retired-or-not); config
// says what this host may CALL (a probed binding, filtered by topology and
// roster). Conflating them once routed a keyless host to openai and wrote that
// route into routing.json, so "callable" has exactly one implementation and
// both binaries read it here. Layering: routing and config stay independent;
// this package is the only place allowed to know both.
package inference

import (
	"slices"
	"strings"

	"pix/host/config"
	"pix/host/routing"
)

// Bindings returns the callable bindings in cfg, in config order. An uncallable
// binding is OMITTED rather than emitted with Available:false: consumers read
// presence, and an unavailable entry has fooled that reading before.
func Bindings(cfg *config.Config) []routing.Binding {
	if cfg == nil {
		return nil
	}
	out := make([]routing.Binding, 0, len(cfg.Inference.Models))
	for _, b := range cfg.Inference.Models {
		if Callable(cfg, b) {
			out = append(out, routing.Binding{Model: b.Model, Backend: b.Backend, UpstreamID: b.Upstream, Available: true})
		}
	}
	return out
}

// Configured reports whether this host made any inference decision at all.
// Bindings are the availability authority only once they exist: filtering an
// empty set marks every catalog model unavailable and describes nothing.
func Configured(cfg *config.Config) bool {
	return cfg != nil && len(cfg.Inference.Models) > 0
}

// BoundRegistry narrows catalog to what this host can call and reports whether
// the narrowing happened; bound == false means the caller is showing the raw
// catalog and MUST say so. Every row survives (unbound => Available:false), so
// `pix models ls` still describes the whole catalog.
func BoundRegistry(cfg *config.Config, catalog *routing.Registry) (reg *routing.Registry, bound bool) {
	if catalog == nil || !Configured(cfg) {
		return catalog, false
	}
	// No exclusiveBackend argument: Bindings already applied the topology filter.
	return routing.RegistryForBindings(catalog, Bindings(cfg), ""), true
}

// Callable is the single definition of "this host can call this model now":
// available, allowed by topology and roster, backed by a backend that exists,
// and — where Pix can prove it from the host — proven.
func Callable(cfg *config.Config, b config.InferenceModelBinding) bool {
	if cfg == nil || !b.Available || !Allowed(cfg, b) {
		return false
	}
	if _, ok := cfg.Inference.Backends[b.Backend]; !ok {
		return false
	}
	return !needsHostProof(cfg, b) || b.Verified
}

// Allowed applies the two probe-independent filters: topology (an exclusive
// pack owns the whole surface) and the personal roster.
func Allowed(cfg *config.Config, b config.InferenceModelBinding) bool {
	if !TopologyAllowed(cfg, b) {
		return false
	}
	// A pack owns the surface while active; the roster is preserved underneath.
	if cfg.Inference.ExclusiveSource != "" || len(cfg.Inference.AllowedModels) == 0 {
		return true
	}
	return slices.Contains(cfg.Inference.AllowedModels, b.Model)
}

// TopologyAllowed is Allowed without the roster: a caller BUILDING the roster
// would otherwise filter candidates by the answer it is about to compute.
func TopologyAllowed(cfg *config.Config, b config.InferenceModelBinding) bool {
	if cfg.Inference.ExclusiveSource != "" {
		return b.Source == cfg.Inference.ExclusiveSource
	}
	return cfg.Inference.ExclusiveBackend == "" || b.Backend == cfg.Inference.ExclusiveBackend
}

// BackendAllowed is TopologyAllowed for a whole backend.
func BackendAllowed(cfg *config.Config, b config.InferenceBackend, name string) bool {
	if cfg.Inference.ExclusiveSource != "" {
		return b.Source == cfg.Inference.ExclusiveSource
	}
	return cfg.Inference.ExclusiveBackend == "" || name == cfg.Inference.ExclusiveBackend
}

// needsHostProof reports whether Pix CAN — and therefore MUST — prove this
// binding from the host. Ollama's backend has Auth "none", so gating on
// `Auth == "1password"` alone made its verification cosmetic (the hole the
// gated-cloud-model incident came through). A pack binding is exempt only where
// the exemption is earned — sbx-session auth cannot be replayed from the host —
// never for a pack's 1Password backend, whose probe is dispatched and must be
// obeyed.
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

// RuntimeID is the id a runtime provider calls a bound model by: the upstream
// id qualified by its backend, without double-qualifying one that already is.
func RuntimeID(b routing.Binding) string {
	if routing.IsQualifiedID(b.UpstreamID) && strings.HasPrefix(b.UpstreamID, b.Backend+"/") {
		return b.UpstreamID
	}
	return b.Backend + "/" + b.UpstreamID
}

// BoundNativeProviders is the set of providers that already had a native
// binding. Captured BEFORE rebuilding bindings, it is the whole mechanism
// behind roster widening.
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
