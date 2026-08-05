// service.go — the trusted pack [[services]] UnitSpec.
//
// [[services]] is the SOLE way any pack may declare a long-running external
// service unit for the host supervisor — never a hand-edited `plugins.*` config
// block, never any other manifest facet. This file is declaration + validation
// only: the struct parses, normalizes, validates fail-closed at LoadPack, and
// enters the Tier-1 bill-of-materials and the host-exec fingerprint. Nothing
// here launches anything; a supervisor may consume ONLY a gate-passed service
// (see unitview.go).
//
// Security posture (the trust boundary is the pack author → this host):
//   - runtime is a closed set: "go-plugin" (a repo-relative executable,
//     SHA-pinned like [[bin]]) or "container" (digest-pinned image, declared for
//     the future container path). The typed schema is the allowlist.
//   - env carries REFERENCE NAMES only. A value-shaped entry ("FOO=bar", an
//     op:// ref, a pasted token) is refused at load: secret VALUES never live in
//     a pack manifest, and the allowlist of names is exactly what the consent
//     screen shows and the fingerprint pins.
//   - listeners are loopback-only and the reserved pix-host ports can never be
//     claimed, so a pack service can neither expose the LAN nor squat a built-in
//     unit's front door.
//   - mounts are repo-relative (a pack cannot claim /etc or ~/.ssh), network is
//     bare egress hostnames, and license/source (SPDX + https URL) are required
//     so the BoM screen can attribute what the user is about to run.
//   - EVERY field is fingerprinted: any change re-gates through acceptance.
package pack

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// serviceRuntimeContainer is the one runtime value another file needs to name
// (trust.go renders container identity differently in the BoM).
const serviceRuntimeContainer = "container"

// serviceRules is the whole [[services]] vocabulary: the closed sets, the
// reserved names/ports, and the value shapes. ONE immutable package value
// rather than a name per rule — they are only ever read together.
//
//   - runtimes: go-plugin, plus container accepted as a DECLARATION (identity
//     validated, consented, fingerprinted) with no consumer yet.
//   - activations: start with serve ("always") or on first use ("on-demand").
//   - reservedPorts are pix-host's own front doors: a colliding pack service
//     would either fail to bind or win the race and impersonate a built-in unit.
//   - reservedNames are the built-in supervisor slots; shadowing one would make
//     `serve status` and the consent screen ambiguous about WHOSE code runs.
//   - envName is what an env REFERENCE NAME may look like; anything else is
//     value-shaped and refused. spdx covers SPDX identifiers and expressions,
//     shaHex a full sha256 digest, networkHost a bare egress hostname.
var serviceRules = struct {
	runtimes      map[string]bool
	activations   map[string]bool
	reservedPorts map[int]string
	reservedNames map[string]bool
	envName       *regexp.Regexp
	spdx          *regexp.Regexp
	shaHex        *regexp.Regexp
	networkHost   *regexp.Regexp
}{
	runtimes:    map[string]bool{"go-plugin": true, serviceRuntimeContainer: true},
	activations: map[string]bool{"always": true, "on-demand": true},
	reservedPorts: map[int]string{
		11435: "pix-host memory",
		11437: "pix monitor",
	},
	reservedNames: map[string]bool{
		"memory":    true,
		"knowledge": true,
		"broker":    true,
		"monitor":   true,
		"serve":     true,
	},
	envName:     regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`),
	spdx:        regexp.MustCompile(`^[A-Za-z0-9 .()+-]{1,128}$`),
	shaHex:      regexp.MustCompile(`^[0-9a-fA-F]{64}$`),
	networkHost: regexp.MustCompile(`^\*?[A-Za-z0-9][A-Za-z0-9.-]{0,252}(:[0-9]{1,5})?$`),
}

// packService is one [[services]] entry: a normalized long-running service
// declaration. All fields are part of the Tier-1 host-exec fingerprint.
// Unexported — unitview.go's accepted view is the only way out of this package.
type packService struct {
	Name       string `toml:"name"`
	Runtime    string `toml:"runtime"`    // serviceRules.runtimes (closed set)
	Activation string `toml:"activation"` // serviceRules.activations (closed set)
	// go-plugin identity: repo-relative executable + pinned sha256 of its bytes
	// (verified at staging/launch, exactly like [[bin]]). The file need not exist
	// at declaration time — the pin is the identity.
	Path string `toml:"path,omitempty"`
	SHA  string `toml:"sha,omitempty"`
	// container identity: a digest-pinned OCI image ref (future runtime).
	Image string `toml:"image,omitempty"`
	// Argv are the arguments the unit is launched with (no interpretation).
	Argv []string `toml:"argv,omitempty"`
	// Env is the explicit allowlist of environment REFERENCE NAMES the unit
	// receives (resolved elsewhere, e.g. op-refs.env). Names only, never values.
	Env []string `toml:"env,omitempty"`
	// Port + Listen + Health describe the unit's loopback front door.
	Port   int    `toml:"port,omitempty"`
	Listen string `toml:"listen,omitempty"` // loopback only; default 127.0.0.1
	Health string `toml:"health,omitempty"` // "tcp" or an HTTP path ("/healthz")
	// Mounts are repo-relative paths the unit may be given access to.
	Mounts []string `toml:"mounts,omitempty"`
	// Network are bare egress hostnames the unit declares it reaches.
	Network []string `toml:"network,omitempty"`
	// Resources are declared ceilings (informational until a consumer exists).
	Resources *packServiceResources `toml:"resources,omitempty"`
	// License (SPDX identifier/expression) and Source (https URL) attribute
	// the code the user is consenting to run. Both required.
	License string `toml:"license"`
	Source  string `toml:"source"`
}

// packServiceResources are declared resource ceilings.
type packServiceResources struct {
	MemoryMB   int `toml:"memory_mb,omitempty"`
	CPUPercent int `toml:"cpu_percent,omitempty"`
}

// normalized returns a whitespace-trimmed, case-canonical copy: the SHAPE the
// BoM shows, the fingerprint hashes, and (later) the supervisor consumes.
func (s packService) normalized() packService {
	out := s
	out.Name = strings.TrimSpace(s.Name)
	out.Runtime = strings.ToLower(strings.TrimSpace(s.Runtime))
	out.Activation = strings.ToLower(strings.TrimSpace(s.Activation))
	out.Path = strings.TrimSpace(s.Path)
	out.SHA = strings.ToLower(strings.TrimSpace(s.SHA))
	out.Image = strings.TrimSpace(s.Image)
	out.Listen = strings.TrimSpace(s.Listen)
	if out.Listen == "" && out.Port != 0 {
		out.Listen = "127.0.0.1"
	}
	out.Health = strings.TrimSpace(s.Health)
	out.License = strings.TrimSpace(s.License)
	out.Source = strings.TrimSpace(s.Source)
	out.Argv = append([]string(nil), s.Argv...)
	out.Env = append([]string(nil), s.Env...)
	for i, e := range out.Env {
		out.Env[i] = strings.TrimSpace(e)
	}
	out.Mounts = append([]string(nil), s.Mounts...)
	for i, m := range out.Mounts {
		out.Mounts[i] = strings.TrimSpace(m)
	}
	out.Network = append([]string(nil), s.Network...)
	for i, n := range out.Network {
		out.Network[i] = strings.TrimSpace(n)
	}
	if s.Resources != nil {
		r := *s.Resources
		out.Resources = &r
	}
	return out
}

// serviceListenIsLoopback reports whether listen names a loopback interface:
// localhost, ::1, or anything in 127.0.0.0/8. A pack service must never be
// reachable from the LAN.
func serviceListenIsLoopback(listen string) bool {
	switch listen {
	case "localhost", "::1":
		return true
	}
	if strings.HasPrefix(listen, "127.") {
		parts := strings.Split(listen, ".")
		if len(parts) != 4 {
			return false
		}
		for _, p := range parts {
			if p == "" || len(p) > 3 || strings.Trim(p, "0123456789") != "" {
				return false
			}
		}
		return true
	}
	return false
}

// validatePackServices hardens the [[services]] facet at load time — the same
// fail-closed posture as the rest of validatePackFacets. Every rejection here is
// a declaration that must never reach the BoM, the fingerprint, or an exec path.
func validatePackServices(root string, m *Manifest) error {
	seen := map[string]bool{}
	for i := range m.Services {
		s := m.Services[i].normalized()
		bad := func(format string, args ...any) error {
			label := s.Name
			if label == "" {
				label = fmt.Sprintf("#%d", i+1)
			}
			return fmt.Errorf("pack %s: [[services]] %s: %s", root, label, fmt.Sprintf(format, args...))
		}
		if !safeArtifactName(s.Name) {
			return bad("name %q is invalid (letters, digits, -, _, . only; no path separators)", m.Services[i].Name)
		}
		if serviceRules.reservedNames[strings.ToLower(s.Name)] {
			return bad("name %q is reserved for a built-in pix-host unit", s.Name)
		}
		if seen[s.Name] {
			return bad("duplicate service name %q; each service must be declared exactly once", s.Name)
		}
		seen[s.Name] = true
		if !serviceRules.runtimes[s.Runtime] {
			return bad("invalid runtime %q (want %q or %q)", m.Services[i].Runtime, "go-plugin", serviceRuntimeContainer)
		}
		if s.Runtime == serviceRuntimeContainer {
			if s.Path != "" || s.SHA != "" {
				return bad("runtime container must not set path/sha (identity is the digest-pinned image)")
			}
			at := strings.LastIndex(s.Image, "@sha256:")
			if s.Image == "" || at < 1 || !serviceRules.shaHex.MatchString(s.Image[at+len("@sha256:"):]) {
				return bad("runtime container requires a digest-pinned image (repo@sha256:<64 hex>), got %q", s.Image)
			}
		} else { // go-plugin: the only other member of the closed set
			if s.Image != "" {
				return bad("runtime go-plugin must not set image (image identifies a container runtime)")
			}
			if s.Path == "" {
				return bad("runtime go-plugin requires a repo-relative executable path")
			}
			if err := validateRepoRelativePath(root, s.Path); err != nil {
				return bad("%v", err)
			}
			if !serviceRules.shaHex.MatchString(s.SHA) {
				return bad("runtime go-plugin requires a full sha256 hex pin in sha (external service binaries are never admitted unpinned; fail closed)")
			}
		}
		if !serviceRules.activations[s.Activation] {
			return bad("invalid activation %q (want %q or %q)", m.Services[i].Activation, "always", "on-demand")
		}
		for _, arg := range s.Argv {
			if strings.ContainsAny(arg, "\x00\r\n") {
				return bad("argv contains a control character")
			}
		}
		for _, e := range s.Env {
			if !serviceRules.envName.MatchString(e) {
				return bad("env entry %q is value-shaped; [[services]] env carries reference NAMES only (the value stays in 1Password / op-refs.env)", e)
			}
		}
		if s.Port < 0 || s.Port > 65535 {
			return bad("port %d is out of range", s.Port)
		}
		if owner, taken := serviceRules.reservedPorts[s.Port]; taken {
			return bad("port %d is reserved for %s; a pack service can never claim a built-in pix-host port", s.Port, owner)
		}
		if s.Listen != "" && !serviceListenIsLoopback(s.Listen) {
			return bad("listen %q is not loopback; pack services must never listen beyond this host (127.0.0.0/8, ::1, localhost)", s.Listen)
		}
		if (s.Listen != "" || s.Health != "") && s.Port == 0 {
			return bad("listen/health declared without a port")
		}
		if s.Health != "" && s.Health != "tcp" && (!strings.HasPrefix(s.Health, "/") || strings.ContainsAny(s.Health, "\x00\r\n ")) {
			return bad("health %q must be \"tcp\" or an HTTP path starting with /", s.Health)
		}
		for _, mount := range s.Mounts {
			if err := validateRepoRelativePath(root, mount); err != nil {
				return bad("mount: %v (mounts are repo-relative; a pack never claims host paths)", err)
			}
		}
		for _, host := range s.Network {
			if !serviceRules.networkHost.MatchString(host) {
				return bad("network entry %q is not a bare egress hostname", host)
			}
		}
		if r := s.Resources; r != nil && (r.MemoryMB < 0 || r.CPUPercent < 0) {
			return bad("resources must be non-negative")
		}
		if !serviceRules.spdx.MatchString(s.License) {
			return bad("license %q must be a non-empty SPDX identifier/expression", m.Services[i].License)
		}
		u, err := url.Parse(s.Source)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
			return bad("source %q must be an https URL (no credentials) attributing where this service's code lives", m.Services[i].Source)
		}
	}
	return nil
}
