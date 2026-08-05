// pack_units.go — the supervisor-side half of the trusted pack [[services]]
// wiring. pack.AcceptedGoPluginServices is the ONLY source of these views,
// answering only after the Tier-1 fingerprint/consent gate passes — admission
// strictly precedes staging, hashing and exec, all downstream in the tree.
// reconcilePackUnits is `serve`'s desired-state reconciler, run per pack.
package main

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"pix/host/plugin"
	"pix/host/supervise"
	"pix/host/workflow/pack"
)

// packUnitSpec converts one accepted view into the validated supervise.UnitSpec
// the tree consumes. kind must be a registered plugin.PluginMap capability —
// the closed set, so a pack can never introduce a dispense kind.
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

// reconcilePackUnits diffs views against s.packUnits (units THIS supervisor
// previously admitted from a pack, never a built-in slot): ADD a new view,
// REMOVE one dropped or gone on-demand, RESTART (remove+add) a changed spec.
func (s *supervisor) reconcilePackUnits(selfPath string, views []pack.AcceptedService) (map[string]*pluginHolder, error) {
	tree := s.ensure(selfPath)
	wanted := map[string]supervise.UnitSpec{}
	var errs []error
	for _, v := range views {
		if v.Activation != "always" {
			continue
		}
		// "memory" is the only dispensable capability a pack unit can be today.
		spec, err := packUnitSpec(v, "memory")
		if err != nil {
			errs = append(errs, err)
			continue
		}
		wanted[v.Name] = spec
	}

	s.mu.Lock()
	if s.packUnits == nil {
		s.packUnits = map[string]supervise.UnitSpec{}
	}
	prev := s.packUnits
	s.mu.Unlock()

	for name := range prev {
		if next, ok := wanted[name]; ok && reflect.DeepEqual(prev[name], next) {
			continue
		}
		if err := tree.Remove(name); err != nil {
			errs = append(errs, fmt.Errorf("pack unit %s: remove: %w", name, err))
		}
		delete(prev, name)
	}

	holders := map[string]*pluginHolder{}
	for name, spec := range wanted {
		if _, exists := tree.Unit(name); exists {
			continue
		}
		h, err := tree.Add(spec, unitHealth(spec.Kind))
		if err != nil {
			errs = append(errs, fmt.Errorf("pack unit %s: %w", name, err))
			continue
		}
		holders[name] = h
		prev[name] = spec
	}
	return holders, errors.Join(errs...)
}
