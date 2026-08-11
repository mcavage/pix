// unitview.go — the trusted pack [[services]] → supervisor wiring, pack side.
//
// AcceptedGoPluginServices is the SINGLE seam through which anything outside
// this package may read a pack's [[services]], and it answers ONLY for a pack
// whose ENTIRE host-exec surface is accepted at this exact fingerprint —
// consent STRICTLY precedes any export. The view is deliberately MINIMAL: only
// the fields needed to construct one external go-plugin unit (mounts, network,
// resources, license and source are consent material, not launch material).
package pack

import (
	"fmt"
	"path/filepath"
	"pix/host/packinfo"
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
	// Runtime distinguishes how the supervisor must LAUNCH this unit:
	// "go-plugin" (handshake + dispense) or "daemon" (exec + probe its port).
	Runtime string
	// Command is a bare binary name on PATH, set only for a daemon identified
	// that way instead of by a pinned Path. Exactly one of Path/Command is set.
	Command string
}

// AcceptedGoPluginServices returns the SUPERVISABLE [[services]] views of p —
// go-plugin and daemon runtimes, each tagged with which it is — ONLY
// after proving its current host-exec surface is the exact one accepted at the
// Tier-1 gate. Every other answer fails closed: a re-validation failure, an
// unreadable store, an unaccepted pack, a stale fingerprint. Container services
// are declared/consented but have no consumer yet. It fingerprints the SAME
// surface VerifyPackLaunchTrust does, so acceptance recorded there is the
// acceptance checked here.
func AcceptedGoPluginServices(p *packinfo.Info) ([]AcceptedService, error) {
	if p == nil || len(p.Manifest.Services) == 0 {
		return nil, nil
	}
	// Belt and suspenders: re-run the full load-time validation so a caller
	// holding a mutated packinfo.Info can never export a shape packinfo.LoadPack would refuse.
	if err := packinfo.ValidateServices(p.Root, &p.Manifest); err != nil {
		return nil, err
	}
	bom := ComputeHostBoM(p)
	fp, _, err := ComputeHostExecFingerprint(p.Root, bom)
	if err != nil {
		return nil, fmt.Errorf("pack %s services trust surface: %w", p.Manifest.Name, err)
	}
	if err := requireAcceptedFingerprint(p, fp, "[[services]]"); err != nil {
		return nil, err
	}
	var out []AcceptedService
	for _, svc := range bom.Services { // normalized copies — the fingerprinted shape
		if svc.Runtime == packinfo.ServiceRuntimeContainer {
			continue // container: declaration-only, no runtime consumes it yet
		}
		// A daemon identified by a PATH command has no pack-relative file to
		// resolve, and UnitSpec forbids an unpinned path — so it carries an
		// empty Path and the supervisor resolves the command at launch.
		if svc.Command != "" {
			out = append(out, AcceptedService{
				Name: svc.Name, Activation: svc.Activation, Runtime: svc.Runtime,
				Command: svc.Command,
				Argv:    append([]string(nil), svc.Argv...),
				Env:     append([]string(nil), svc.Env...),
				Port:    svc.Port, Listen: svc.Listen, Health: svc.Health,
			})
			continue
		}
		// Validated above; resolved absolute because UnitSpec requires that.
		abs, aerr := filepath.Abs(filepath.Join(p.Root, svc.Path))
		if aerr != nil {
			return nil, fmt.Errorf("pack %s service %s: %v", p.Manifest.Name, svc.Name, aerr)
		}
		out = append(out, AcceptedService{
			Name:       svc.Name,
			Activation: svc.Activation,
			Runtime:    svc.Runtime,
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

// AcceptedGoPluginServicesForSelf is GONE. It existed only to fabricate a
// hostenv.Env so the bill of materials could run its local-MCP probe against
// pix-host's own executable. The BoM is pure data now, so `serve` calls
// AcceptedGoPluginServices directly and needs no stand-in resolver at all.
