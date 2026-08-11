// pack_units.go — the supervisor-side half of the trusted pack [[services]]
// wiring. pack.AcceptedGoPluginServices is the ONLY source of these views,
// answering only after the Tier-1 fingerprint/consent gate passes — admission
// strictly precedes staging, hashing and exec, all downstream in the tree.
// reconcilePackUnits is `serve`'s desired-state reconciler: it treats its
// WHOLE views argument as the full desired state, so it must be called
// EXACTLY ONCE per reconcile pass across every active pack, never once per
// pack (a second call would remove the first pack's units, since they are
// absent from the second pack's views). mergePackServices below is what
// flattens every active pack's views into that one call's argument.
package main

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"pix/host/packinfo"
	"pix/host/plugin"
	"pix/host/supervise"
	"pix/host/workflow/pack"
)

// packServiceSet pairs one active pack's accepted go-plugin [[services]]
// views with that pack's name, so a unit-name collision across two packs can
// be reported by IDENTITY (which pack declared what) instead of one pack's
// view silently replacing another's in a bare map keyed only by unit name.
type packServiceSet struct {
	packName string
	views    []pack.AcceptedService
}

// mergePackServices flattens every active pack's accepted views into the
// single wanted set reconcilePackUnits consumes in one call. A unit name
// declared by more than one pack is a hard, FAIL-CLOSED conflict: pix has no
// policy for picking a winner between two packs claiming the same unit name,
// so every view sharing that name is dropped from the merged result (neither
// runs) rather than the later pack silently overwriting the earlier one, and
// the returned error names every colliding pack so an operator can fix the
// collision. Non-conflicting units from every pack are unaffected and still
// returned, sorted by name for deterministic ordering.
func mergePackServices(sets []packServiceSet) ([]pack.AcceptedService, error) {
	owner := map[string]string{}    // unit name -> the pack that declared it first
	conflicted := map[string]bool{} // unit name -> seen from more than one pack
	out := map[string]pack.AcceptedService{}
	var errs []error
	for _, set := range sets {
		for _, v := range set.views {
			prevPack, dup := owner[v.Name]
			if !dup {
				owner[v.Name] = set.packName
				out[v.Name] = v
				continue
			}
			delete(out, v.Name)
			if !conflicted[v.Name] {
				conflicted[v.Name] = true
				errs = append(errs, fmt.Errorf("pack service %q declared by both pack %q and pack %q: refusing both (fail closed)", v.Name, prevPack, set.packName))
			}
		}
	}
	merged := make([]pack.AcceptedService, 0, len(out))
	for _, v := range out {
		merged = append(merged, v)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Name < merged[j].Name })
	return merged, errors.Join(errs...)
}

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

// reconcileDaemons starts every accepted `runtime = "daemon"` view the tree is
// not already supervising.
//
// It is ADD-only on purpose, unlike reconcilePackUnits' full desired-state diff.
// A daemon binds a port, so removing and re-adding one on any manifest change
// means a window where the sandbox's wrapper gets connection-refused — and the
// tree already restarts a daemon whose health fails, which covers the case that
// actually matters. Changing a daemon's declaration re-gates the pack (its
// [[services]] entry is fingerprinted), and takes effect on the next `serve`.
func (s *supervisor) reconcileDaemons(selfPath string, views []pack.AcceptedService) error {
	tree := s.ensure(selfPath)
	var errs []error
	for _, v := range views {
		if v.Runtime != packinfo.ServiceRuntimeDaemon || v.Activation != "always" {
			continue
		}
		if tree.Has(v.Name) {
			continue
		}
		unit, err := supervise.NewExternalUnit(v.Name, packinfo.ServiceRuntimeDaemon, v.Path, v.SHA,
			append([]string(nil), v.Argv...), append([]string(nil), v.Env...))
		if err != nil {
			// A PATH-resolved daemon has no path or pin, which NewExternalUnit
			// refuses by design (it exists to stop an unpinned EXECUTABLE path).
			// Build the spec directly for that case; the weaker identity is
			// disclosed on the consent screen rather than smuggled past a guard.
			if v.Command == "" {
				errs = append(errs, err)
				continue
			}
			unit = supervise.UnitSpec{
				Name: v.Name, Kind: packinfo.ServiceRuntimeDaemon, SelfExec: false,
				Argv: append([]string(nil), v.Argv...), EnvAllow: append([]string(nil), v.Env...),
			}
		}
		if derr := tree.AddDaemon(supervise.DaemonSpec{
			Unit: unit, Command: v.Command,
			Listen: v.Listen, Port: v.Port, Health: v.Health,
		}); derr != nil {
			errs = append(errs, fmt.Errorf("daemon %s: %w", v.Name, derr))
		}
	}
	return errors.Join(errs...)
}

// reconcilePackUnits diffs views against s.packUnits (units THIS supervisor
// previously admitted from a pack, never a built-in slot): ADD a new view,
// REMOVE one dropped or gone on-demand, RESTART (remove+add) a changed spec.
// views is treated as the COMPLETE desired state for every pack unit: a
// caller with more than one active pack MUST merge every pack's views (see
// mergePackServices) into one slice and call this exactly once per reconcile
// pass, never once per pack — a second call with only a second pack's views
// would read the first pack's units as dropped and remove them.
func (s *supervisor) reconcilePackUnits(selfPath string, views []pack.AcceptedService) (map[string]*pluginHolder, error) {
	tree := s.ensure(selfPath)
	wanted := map[string]supervise.UnitSpec{}
	var errs []error
	for _, v := range views {
		if v.Activation != "always" {
			continue
		}
		// A DAEMON is launched and probed, never dispensed, so it does not go
		// through packUnitSpec's plugin-kind check at all: there is no capability
		// to dispense. It is reconciled separately below.
		if v.Runtime == packinfo.ServiceRuntimeDaemon {
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

	// Clone the previous ledger under the lock so the diff/mutate work below
	// runs entirely on a private copy: s.packUnits itself is never touched
	// outside s.mu, and a concurrent reconcile pass never observes a
	// half-updated map. The clone is swapped back onto s.packUnits — one
	// final atomic assignment under the lock — only once this pass's full
	// result is known.
	s.mu.Lock()
	next := make(map[string]supervise.UnitSpec, len(s.packUnits))
	for name, spec := range s.packUnits {
		next[name] = spec
	}
	s.mu.Unlock()

	for name, spec := range next {
		if wantedSpec, ok := wanted[name]; ok && reflect.DeepEqual(spec, wantedSpec) {
			continue
		}
		if err := tree.Remove(name); err != nil {
			errs = append(errs, fmt.Errorf("pack unit %s: remove: %w", name, err))
		}
		delete(next, name)
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
		next[name] = spec
	}

	s.mu.Lock()
	s.packUnits = next
	s.mu.Unlock()

	return holders, errors.Join(errs...)
}
