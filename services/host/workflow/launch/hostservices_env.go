// hostservices_env.go — docs/design/environments.md §10.4's desired-set
// union: which environment-declared [[host.services]] entries (E1.3's
// envinfo.HostService) this host's `pix-host serve` must keep running.
//
//	services from the machine default environment
//	UNION
//	services from every positively live environment holder
//
// "Positively live" mirrors EnvironmentHolders' own fail-closed contract
// (this same file's package): an environment whose holder state could not
// be proven — an untrusted `sbx ls`, in EnvironmentServiceSnapshot terms,
// Unknown — keeps every service it declares running rather than tearing one
// down on a guess. A unit stops only once no default and no live holder
// references it; that is a property of what this file returns, computed
// fresh on every reconcile pass, never a property of state this file keeps
// between calls.
//
// This is deliberately independent of `services/host/pack_units.go`
// (architect correction C8): E5.2 deletes that file outright once packs are
// gone, and this mechanism must survive that deletion untouched. Nothing
// here imports workflow/pack or packinfo's service-runtime vocabulary;
// nothing in pack_units.go imports this file. `serve.go` is the ONLY other
// file that may compose this union into the live supervision tree (see its
// own reconcileEnvironmentServices).
package launch

import (
	"fmt"
	"sort"

	"pix/host/envinfo"
)

// EnvironmentServiceSnapshot is one registered environment's liveness plus
// its declared [[host.services]] — the only two facts DesiredHostServices
// needs about it. Live is meaningless when Unknown is true: an unknown
// holder state preserves the service regardless of Live's zero value (fail
// closed, matching EnvironmentHolders' own contract).
type EnvironmentServiceSnapshot struct {
	// Name is the registered environment name.
	Name string
	// Live reports at least one positively identified, schema-verified
	// running holder for this environment (EnvironmentHolders' own
	// definition). Ignored when Unknown is true.
	Live bool
	// Unknown reports the holder state could not be proven at all (an
	// untrusted `sbx ls`, an unreadable lease root): the environment is
	// treated as live so its services are never torn down on a guess.
	Unknown bool
	// Services is this environment's declared [[host.services]] entries.
	Services []envinfo.HostService
}

// HostServiceWant is one desired host service, tagged with WHICH
// environment declared it, so a unit-name or port collision can be
// reported by identity — which environment claims what — rather than one
// silently shadowing the other.
type HostServiceWant struct {
	Environment string
	Service     envinfo.HostService
}

// DesiredHostServices computes §10.4's union over defaultEnv (the machine
// default environment's registered name, "" when none is selected) and
// envs (every registered environment's current liveness + declared
// services, including the default's own entry if it is registered).
//
// The default environment's services are unioned UNCONDITIONALLY — the
// machine default runs whether or not a sandbox is currently attached to
// it, matching §10.4's literal wording ("services from the machine default
// environment", no liveness clause attached). Every OTHER environment's
// services are unioned only when Live or Unknown; a proven-not-live,
// non-default environment contributes nothing.
//
// It also runs the synchronous unit-name/port collision check over the
// WHOLE unioned set (checkHostServiceCollisions) before returning, so two
// environments briefly both wanting the same name or port refuse here,
// before either is ever handed to the supervisor — never a race where one
// silently wins.
func DesiredHostServices(defaultEnv string, envs []EnvironmentServiceSnapshot) ([]HostServiceWant, error) {
	seen := map[string]bool{} // "environment\x00service name" — de-dupes a repeated call's identical entries
	var out []HostServiceWant
	add := func(envName string, svc envinfo.HostService) {
		key := envName + "\x00" + svc.Name
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, HostServiceWant{Environment: envName, Service: svc})
	}
	for _, e := range envs {
		if e.Name == defaultEnv {
			for _, svc := range e.Services {
				add(e.Name, svc)
			}
			continue
		}
		if !e.Live && !e.Unknown {
			continue // proven not live, and not the default: not in the desired set
		}
		for _, svc := range e.Services {
			add(e.Name, svc)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Environment != out[j].Environment {
			return out[i].Environment < out[j].Environment
		}
		return out[i].Service.Name < out[j].Service.Name
	})
	if err := checkHostServiceCollisions(out); err != nil {
		return nil, err
	}
	return out, nil
}

// checkHostServiceCollisions refuses synchronously, naming both claimants,
// when two DIFFERENT environments declare the same unit name or the same
// port. The SAME environment repeating its own name or port across two of
// its own entries is a single-environment authoring error that belongs to
// whatever validates one environment's own [[host.services]] table, not
// this cross-environment adjudicator, so it is not checked here.
func checkHostServiceCollisions(wanted []HostServiceWant) error {
	byName := map[string]HostServiceWant{}
	byPort := map[int64]HostServiceWant{}
	for _, w := range wanted {
		if prev, ok := byName[w.Service.Name]; ok && prev.Environment != w.Environment {
			return fmt.Errorf("launch: host service %q claimed by both environment %q and environment %q: refusing to start either (fail closed)",
				w.Service.Name, prev.Environment, w.Environment)
		}
		byName[w.Service.Name] = w
		if w.Service.Port == 0 {
			continue
		}
		if prev, ok := byPort[w.Service.Port]; ok && prev.Environment != w.Environment {
			return fmt.Errorf("launch: host service port %d claimed by both %q (environment %q) and %q (environment %q): refusing to start either (fail closed)",
				w.Service.Port, prev.Service.Name, prev.Environment, w.Service.Name, w.Environment)
		}
		byPort[w.Service.Port] = w
	}
	return nil
}
