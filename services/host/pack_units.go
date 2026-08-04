// pack_units.go — U07d: the supervisor-side half of the trusted pack
// [[services]] wiring. pack.AcceptedGoPluginServices (the pack-side half) is
// the ONLY source of these views, and it answers only after the Tier-1
// fingerprint/consent check passes — so by construction, admission strictly
// precedes everything this file does (staging, hashing, exec all happen
// inside the supervision tree, downstream of an already-accepted view).
//
// This file is the INTEGRATOR HOOK, deliberately not wired into runServe:
// the root serve composition is unchanged, and a future story decides where
// reconcilePackUnits is called from (serve startup, `pack use`, on-demand).
// With the direct config [plugins.*] declaration retired (inert, see
// config.applyDefaults), a pack-trust-admitted [[services]] entry is the SOLE
// way an external unit ever reaches the supervisor (AC-SUP-05).
package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"pix/host/plugin"
	"pix/host/supervise"
	"pix/host/workflow/pack"
)

// packUnitSpec is the constructor: ONE accepted pack service view → the
// validated supervise.UnitSpec the tree consumes. kind must name a registered
// go-plugin capability (plugin.PluginMap — the closed set; a pack can never
// introduce a dispense kind). Everything else fails closed inside
// supervise.NewExternalUnit: unpinned or relative paths, value-shaped env.
// The unit's EnvAllow is EXACTLY the pack-declared (and consented) reference
// names — a pack unit never inherits the built-in units' broader allowlist.
func packUnitSpec(v pack.AcceptedService, kind string) (supervise.UnitSpec, error) {
	if _, ok := plugin.PluginMap[kind]; !ok {
		kinds := make([]string, 0, len(plugin.PluginMap))
		for k := range plugin.PluginMap {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		return supervise.UnitSpec{}, fmt.Errorf("pack service %s: unknown plugin kind %q (dispensable kinds: %s)", v.Name, kind, strings.Join(kinds, ", "))
	}
	return supervise.NewExternalUnit(v.Name, kind, v.Path, v.SHA,
		append([]string(nil), v.Argv...), append([]string(nil), v.Env...))
}

// reconcilePackUnits is the reconciler hook the integrator calls with the
// accepted views: it supervises every activation=="always" view not already
// in the tree, as an external GoPluginService unit (staged copy, sha
// re-verified on every start, filtered env, health-probed).
//
// Reconcile semantics are ADD-ONLY and collision-safe:
//   - an already-supervised unit name is left untouched (a pack can never
//     replace, restart, or reconfigure a running unit — and the built-in slot
//     names were already unclaimable at pack validation);
//   - "on-demand" views are skipped (their activation path is a later story);
//   - a view that fails to convert or to become healthy is reported and does
//     NOT abort its siblings; the joined error carries every failure.
//
// kindFor maps a view to its dispense capability (the integrator owns that
// policy); nil or an unknown kind fails that view closed.
func (s *supervisor) reconcilePackUnits(selfPath string, views []pack.AcceptedService, kindFor func(pack.AcceptedService) string) (map[string]*pluginHolder, error) {
	holders := map[string]*pluginHolder{}
	var errs []error
	tree := s.ensure(selfPath)
	for _, v := range views {
		if v.Activation != "always" {
			continue
		}
		if _, exists := tree.Unit(v.Name); exists {
			continue
		}
		kind := ""
		if kindFor != nil {
			kind = kindFor(v)
		}
		spec, err := packUnitSpec(v, kind)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		h, err := tree.Add(spec, unitHealth(kind))
		if err != nil {
			errs = append(errs, fmt.Errorf("pack unit %s: %w", v.Name, err))
			continue
		}
		holders[v.Name] = h
	}
	return holders, errors.Join(errs...)
}
