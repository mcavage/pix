// Package inference answers ONE question for every binary in the stack: which
// catalog models can this host actually call right now?
//
// The catalog says what a model IS (limits, local-or-not, still-offered); config
// says what this host may CALL (a probed binding, filtered by topology and
// roster). Conflating them once routed a keyless host to openai and wrote that
// bound a keyless host to a provider it could not call, so "callable" has
// exactly one implementation and both binaries read it here. catalog.go holds
// the shipped facts; config holds this host's decisions; this file is the only
// place allowed to know both.
package inference

import (
	"maps"
	"slices"
	"strings"

	"pix/host/config"
)

// Bindings returns the callable bindings in cfg, in config order. An uncallable
// binding is OMITTED rather than emitted with Available:false: consumers read
// presence, and an unavailable entry has fooled that reading before.
func Bindings(cfg *config.Config) []Binding {
	if cfg == nil {
		return nil
	}
	out := make([]Binding, 0, len(cfg.Inference.Models))
	for _, b := range cfg.Inference.Models {
		if Callable(cfg, b) {
			out = append(out, Binding{Model: b.Model, Backend: b.Backend, UpstreamID: b.Upstream, Available: true})
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
	return HostProofRequired(backend, b.Source)
}

// HostProofRequired is needsHostProof's decision, taken on a bare backend +
// source pair rather than a binding already sitting in cfg. Pack projection
// (workflow/pack.ApplyPackInference) calls this at the moment a binding is
// CREATED, before it exists in cfg.Inference.Models, to decide whether
// Available may be asserted immediately or must wait on an actual probe: the
// same two backends (1Password, and Ollama's local/cloud daemon) that
// needsHostProof gates are the only two this reports true for.
func HostProofRequired(backend config.InferenceBackend, source string) bool {
	if source != "" && backend.Auth != "1password" {
		return false
	}
	return backend.Auth == "1password" || backend.Driver == "ollama"
}

// RuntimeID is the id a runtime provider calls a bound model by: the upstream
// id qualified by its backend, without double-qualifying one that already is.
func RuntimeID(b Binding) string {
	if IsQualifiedID(b.UpstreamID) && strings.HasPrefix(b.UpstreamID, b.Backend+"/") {
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

// CallableProviders returns the distinct providers this host has at least one
// CALLABLE binding for, in first-seen config order, or nil when this host has
// made no inference decision at all.
//
// nil is load-bearing and is not the same as an empty slice. A host that never
// ran `pix models add` has no bindings, routes through the image's baked map,
// and works: reporting "no provider is callable" there would be a fabricated
// fault. Only once bindings EXIST does their absence for a given vendor mean
// that vendor is unrouted. Callers must treat nil as "unknown", not "none".
// KeylessBackends names the backends this host reaches a model through WITHOUT
// a provider key — e.g. "docker-anthropic, docker-openai (auth: sbx-session)"
// — and is "" when a 1Password-held API key IS the credential.
//
// Non-empty is the launch gate's own condition (ConfiguredKeylessInference), so
// a surface that must SHOW why a key is not missing and a surface that merely
// decides cannot drift apart. Also "" when no allowed binding resolves to a
// backend the config defines: that is a host with no inference, not a host that
// needs no key, and a caller must fall back to the key store rather than
// green-light an axis on the empty set.
func KeylessBackends(cfg *config.Config) string {
	if !Configured(cfg) || InferenceNeedsOnePassword(cfg) {
		return ""
	}
	backends, auths := map[string]bool{}, map[string]bool{}
	for _, b := range cfg.Inference.Models {
		backend, ok := cfg.Inference.Backends[b.Backend]
		if !ok || !Allowed(cfg, b) || !BackendAllowed(cfg, backend, b.Backend) {
			continue
		}
		auth := backend.Auth
		if auth == "" {
			auth = "none"
		}
		backends[b.Backend], auths[auth] = true, true
	}
	if len(backends) == 0 {
		return ""
	}
	return strings.Join(slices.Sorted(maps.Keys(backends)), ", ") +
		" (auth: " + strings.Join(slices.Sorted(maps.Keys(auths)), ", ") + ")"
}

func CallableProviders(cfg *config.Config) []string {
	if !Configured(cfg) {
		return nil
	}
	seen := map[string]bool{}
	out := []string{}
	for _, b := range Bindings(cfg) {
		provider, _, ok := strings.Cut(b.Model, "/")
		if !ok || seen[provider] {
			continue
		}
		seen[provider] = true
		out = append(out, provider)
	}
	return out
}
