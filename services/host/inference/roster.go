// roster.go composes the environment literal roster (docs/design/
// environments.md §5.2/§6.4/§7) into the generated inference.json manifest.
//
// Package inference is L1: it never imports envinfo or workflow/env to read
// a pix.toml sidecar itself. Instead, whichever caller selected an
// environment (a later story's workflow/env composition, today nothing)
// resolves that sidecar's typed facts on its own and hands them across the
// composition boundary as a RosterInput. That keeps environment authoring
// and CLI/workflow concerns fully decoupled from this package, and it is the
// public typed contract downstream stories build the actual wiring against.
package inference

import (
	"fmt"
	"sort"
	"strings"
)

// RosterInput is the typed composition-boundary input for the literal
// roster, and the ONLY roster-shaped type this package exports: an external
// caller resolves the environment's typed facts on its own and hands them
// across the boundary as a RosterInput to CompileInferenceRuntime or
// SynthesizeInferenceKit — composition against the generated model list
// (buildRoster, unexported below) is an internal step of those two
// functions, never a separate call an outside package makes itself. A zero
// value means "no environment roster is in effect": every existing caller
// that has not been taught to resolve one yet passes this, and
// buildRoster's result is nil with no error, so the manifest carries no
// "roster" key at all — the additive-field guarantee holds for every caller
// this story does not touch.
type RosterInput struct {
	// Main is the selected environment's `[models].main` model id.
	Main string
	// Agents is the selected environment's authored `[agents]` table
	// verbatim: agent name -> model id. An authored entry always wins over
	// the shipped-agent default below.
	Agents map[string]string
	// ShippedAgents names the shipped subagent roster (agents/*.md base
	// names), resolved by the caller — this package never reads that
	// directory itself. A shipped name absent from Agents defaults to Main
	// (§6.4). A name that is in neither ShippedAgents nor Agents gets no
	// roster entry at all: a custom project agent's own `model:` remains a
	// higher-precedence, reader-side concern (E3.2), untouched here.
	ShippedAgents []string
}

// RosterError is one roster composition/validation failure: the sidecar
// file and the exact offending key, in the design doc's own bracket-table
// spelling (`[models].main`, `[agents].<name>` — PRD §5.7) — never a line
// number, since composition works from already-typed facts, not sidecar
// source text. Exit semantics belong to the caller: this type only
// classifies the fault.
type RosterError struct {
	File   string
	Key    string
	Reason string
}

func (e *RosterError) Error() string {
	return fmt.Sprintf("%s: %s: %s", e.File, e.Key, e.Reason)
}

// rosterSourceFile is the one sidecar file every roster fact comes from.
const rosterSourceFile = "pix.toml"

// buildRoster composes and validates a manifest roster from in against
// models — the SAME generated model list the manifest is about to ship,
// never a second, divergent resolution path. Every model in models already
// references a backend the manifest generates a provider for
// (manifestModels only ever emits bound, backend-resolved entries; see
// TestManifestModelsAlwaysReferenceAGeneratedBackend), so checking a roster
// reference for membership in models IS checking "resolves to a model whose
// backend Pix generates a provider for" — one set, one check.
//
// A blank in.Main means no environment roster is selected: buildRoster
// returns (nil, nil), and the manifest omits "roster" entirely.
//
// Unexported on purpose: models and the *runtimeRoster result are this
// package's own runtime types, which an external caller can neither
// construct nor name. The only sanctioned composition boundary is
// RosterInput through CompileInferenceRuntime/SynthesizeInferenceKit (see
// RosterInput's doc); this is the one call site that resolves the SAME
// manifest.Models those functions are about to ship, so exporting a second
// entry point here would only invite a caller to validate against a
// divergent, hand-built model list. See TestPublicAPINeverExposesUnexported
// in public_api_test.go, which fails the build the moment any exported
// function parameter references an unexported package type again.
func buildRoster(in RosterInput, models []runtimeModel) (*runtimeRoster, error) {
	if strings.TrimSpace(in.Main) == "" {
		return nil, nil
	}
	known := make(map[string]bool, len(models))
	for _, m := range models {
		known[m.ID] = true
	}
	if !known[in.Main] {
		return nil, &RosterError{File: rosterSourceFile, Key: "[models].main", Reason: fmt.Sprintf("%q is not a generated model", in.Main)}
	}

	agents := make(map[string]string, len(in.ShippedAgents)+len(in.Agents))
	for _, name := range in.ShippedAgents {
		agents[name] = in.Main // shipped agent absent from [agents] maps to main, §6.4
	}

	names := make([]string, 0, len(in.Agents))
	for name := range in.Agents {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic validation order: first offending name, always the same one
	for _, name := range names {
		model := in.Agents[name]
		if !known[model] {
			return nil, &RosterError{File: rosterSourceFile, Key: "[agents]." + name, Reason: fmt.Sprintf("%q is not a generated model", model)}
		}
		agents[name] = model // authored entry wins over the shipped-agent default filled in above
	}

	return &runtimeRoster{Main: in.Main, Agents: agents}, nil
}

// runtimeRoster is the additive v1 manifest field. It is additive precisely
// because it is a NEW key on an existing struct with omitempty on the
// pointer: a manifest built with a zero-value RosterInput (Roster == nil)
// marshals with no "roster" key at all, so an older v1 reader — which
// checks only version/backends/models — sees byte-for-byte what it always
// has.
type runtimeRoster struct {
	Main   string            `json:"main"`
	Agents map[string]string `json:"agents"`
}
