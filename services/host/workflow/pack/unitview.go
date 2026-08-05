// unitview.go — the trusted pack [[services]] → supervisor wiring, pack side.
//
// AcceptedGoPluginServices is the SINGLE seam through which anything outside
// this package may read a pack's [[services]] declarations, and it answers ONLY
// for a pack whose ENTIRE host-exec surface is accepted in the launcher-owned
// trust store at this exact fingerprint. The order is the security property:
// consent STRICTLY precedes any export, and therefore precedes any staging,
// hashing or exec the supervisor performs on what is exported.
//
// The exported view is deliberately MINIMAL: exactly the fields a supervisor
// needs to construct one external go-plugin unit. Mounts, network, resources,
// license and source stay inside the package — consent-screen material, not
// launch material. The view carries NO supervisor state and no way to reach
// any: a pack cannot name a staging dir, a state dir, a reattach record or
// another unit's slot. Supervisor state remains launcher-owned.
package pack

import (
	"fmt"
	"path/filepath"

	"pix/host/hostenv"
)

// AcceptedService is the minimal accepted, normalized view of ONE go-plugin
// [[services]] entry. Every field is already normalized and covered by the
// accepted fingerprint; the supervisor re-validates through
// supervise.NewExternalUnit and re-hashes the staged bytes on every start.
type AcceptedService struct {
	Name       string   // unit name (never a reserved built-in slot)
	Activation string   // "always" | "on-demand"
	Path       string   // ABSOLUTE source path, resolved under the pack root
	SHA        string   // full sha256 pin of the executable bytes (lowercase)
	Argv       []string // launch arguments, uninterpreted
	Env        []string // env reference NAMES only — never values
	Port       int      // 0 = no listener declared
	Listen     string   // loopback only; "" when Port is 0
	Health     string   // "tcp", an HTTP path, or ""
}

// AcceptedGoPluginServices returns the go-plugin [[services]] views of p, and
// ONLY after proving the pack's current host-exec surface is the exact one
// accepted at the Tier-1 gate. It fails closed on every other answer:
//
//   - re-validation failure (a manifest mutated in memory or on disk into an
//     invalid shape never reaches the trust check, let alone a caller);
//   - an unreadable trust store, an unaccepted pack, or a fingerprint that no
//     longer matches → an error naming `pix pack use` as the re-review path.
//
// Container-runtime services are declared/consented but have no consumer yet.
// cfgGogAccount and env mirror VerifyPackInferenceTrust: the fingerprint must be
// computed over the SAME resolved surface the acceptance was recorded over.
func AcceptedGoPluginServices(p *Info, cfgGogAccount string, env hostenv.Env) ([]AcceptedService, error) {
	if p == nil || len(p.Manifest.Services) == 0 {
		return nil, nil
	}
	// Belt and suspenders: re-run the full load-time validation (reserved
	// ports/names, loopback-only listeners, env reference names, repo-relative
	// paths, pinned identity) so a caller holding a mutated Info can never
	// export a shape LoadPack would have refused.
	if err := validatePackServices(p.Root, &p.Manifest); err != nil {
		return nil, err
	}
	bom := ComputeHostBoM(p, cfgGogAccount, LocalMCPClassifier(env, env.HostBinary))
	fp, _, err := ComputeHostExecFingerprint(p.Root, bom)
	if err != nil {
		return nil, fmt.Errorf("pack %s services trust surface: %w", p.Manifest.Name, err)
	}
	if err := withPackTrustLock(func() error {
		store, lerr := loadPackTrustStore()
		if lerr != nil {
			return fmt.Errorf("pack trust state unreadable: %w", lerr)
		}
		key := store.TrustKey(p.Root)
		if got, ok := store.acceptedFingerprint(key); !ok || got != fp {
			return fmt.Errorf("pack %s [[services]] are not accepted (or changed since acceptance) — run `pix pack use %s` to review them", p.Manifest.Name, p.Root)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	var out []AcceptedService
	for _, svc := range bom.Services { // normalized copies — the fingerprinted shape
		if svc.Runtime != "go-plugin" {
			continue // container: declaration-only, no runtime consumes it
		}
		// The relative path was validated above; resolve it under the pack
		// root because supervise.UnitSpec requires an absolute source path.
		abs, aerr := filepath.Abs(filepath.Join(p.Root, svc.Path))
		if aerr != nil {
			return nil, fmt.Errorf("pack %s service %s: %v", p.Manifest.Name, svc.Name, aerr)
		}
		out = append(out, AcceptedService{
			Name:       svc.Name,
			Activation: svc.Activation,
			Path:       abs,
			SHA:        svc.SHA,
			Argv:       append([]string(nil), svc.Argv...),
			Env:        append([]string(nil), svc.Env...),
			Port:       svc.Port,
			Listen:     svc.Listen,
			Health:     svc.Health,
		})
	}
	return out, nil
}
