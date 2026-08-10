// service.go — the trusted pack [[services]] UnitSpec.
//
// [[services]] is the SOLE way any pack may declare a long-running external
// service unit for the host supervisor — never a hand-edited `plugins.*` block.
// This file is declaration + validation only: the struct parses, normalizes,
// validates fail-closed at LoadPack, and enters the Tier-1 bill-of-materials
// and the host-exec fingerprint. A supervisor may consume ONLY a gate-passed
// service (unitview.go).
//
// Security posture (the trust boundary is the pack author → this host): runtime
// is a closed set (SHA-pinned "go-plugin" executable or digest-pinned
// "container" image); env carries REFERENCE NAMES only, so secret VALUES never
// live in a manifest; listeners are loopback-only and reserved pix-host ports
// unclaimable; mounts are repo-relative and network bare egress hostnames;
// license/source (SPDX + https) attribute what runs. EVERY field is
// fingerprinted, so any change re-gates.
package packinfo

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// ServiceRuntimeContainer is the one runtime value another file needs to name
// (trust.go renders container identity differently in the BoM).
const ServiceRuntimeContainer = "container"

// serviceRules is the whole [[services]] vocabulary — closed sets, reserved
// names/ports, value shapes — as ONE immutable package value, since they are
// only ever read together. container is a DECLARATION-only runtime (validated,
// consented, fingerprinted) with no consumer yet. reservedPorts are pix-host's
// own front doors and reservedNames its built-in slots: a pack service may
// never bind, impersonate or shadow one.
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
	runtimes:    map[string]bool{"go-plugin": true, ServiceRuntimeContainer: true},
	activations: map[string]bool{"always": true, "on-demand": true},
	reservedPorts: map[int]string{
		11435: "pix-host memory",
	},
	reservedNames: map[string]bool{
		"memory":    true,
		"knowledge": true,
		"broker":    true,
		"serve":     true,
	},
	envName:     regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`),
	spdx:        regexp.MustCompile(`^[A-Za-z0-9 .()+-]{1,128}$`),
	shaHex:      regexp.MustCompile(`^[0-9a-fA-F]{64}$`),
	networkHost: regexp.MustCompile(`^\*?[A-Za-z0-9][A-Za-z0-9.-]{0,252}(:[0-9]{1,5})?$`),
}

// Service is one [[services]] entry: a normalized long-running service
// declaration. Unexported — unitview.go's accepted view is the only way out of
// this package. EVERY field is part of the Tier-1 host-exec fingerprint, and
// the json tags below ARE that canonical encoding (see trust.go): their names,
// order and omitempty are load-bearing.
type Service struct {
	Name       string `toml:"name" json:"name"`
	Runtime    string `toml:"runtime" json:"runtime"`       // serviceRules.runtimes (closed set)
	Activation string `toml:"activation" json:"activation"` // serviceRules.activations (closed set)
	// go-plugin identity: repo-relative executable + pinned sha256 of its bytes,
	// verified at staging/launch. The file need not exist at declaration time —
	// the pin IS the identity.
	Path  string   `toml:"path,omitempty" json:"path,omitempty"`
	SHA   string   `toml:"sha,omitempty" json:"sha,omitempty"`
	Image string   `toml:"image,omitempty" json:"image,omitempty"` // container identity: digest-pinned OCI ref
	Argv  []string `toml:"argv,omitempty" json:"argv,omitempty"`   // launch arguments, uninterpreted
	// Env is the allowlist of environment REFERENCE NAMES the unit receives
	// (resolved elsewhere, e.g. op-refs.env). Names only, never values.
	Env []string `toml:"env,omitempty" json:"env,omitempty"`
	// Port + Listen + Health describe the unit's loopback front door.
	Port    int      `toml:"port,omitempty" json:"port,omitempty"`
	Listen  string   `toml:"listen,omitempty" json:"listen,omitempty"`   // loopback only; default 127.0.0.1
	Health  string   `toml:"health,omitempty" json:"health,omitempty"`   // "tcp" or an HTTP path ("/healthz")
	Mounts  []string `toml:"mounts,omitempty" json:"mounts,omitempty"`   // repo-relative paths only
	Network []string `toml:"network,omitempty" json:"network,omitempty"` // bare egress hostnames
	// Resources are declared ceilings (informational until a consumer exists).
	Resources *ServiceResources `toml:"resources,omitempty" json:"resources,omitempty"`
	// License (SPDX) and Source (https URL) attribute the code the user is
	// consenting to run. Both required.
	License string `toml:"license" json:"license"`
	Source  string `toml:"source" json:"source"`
}

// ServiceResources are declared resource ceilings.
type ServiceResources struct {
	MemoryMB   int `toml:"memory_mb,omitempty" json:"memory_mb"`
	CPUPercent int `toml:"cpu_percent,omitempty" json:"cpu_percent"`
}

// normalized returns a whitespace-trimmed, case-canonical copy: the SHAPE the
// BoM shows, the fingerprint hashes, and (later) the supervisor consumes.
func (s Service) Normalized() Service {
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
	out.Env, out.Mounts, out.Network = trimmedCopy(s.Env), trimmedCopy(s.Mounts), trimmedCopy(s.Network)
	if s.Resources != nil {
		r := *s.Resources
		out.Resources = &r
	}
	return out
}

// trimmedCopy is a whitespace-trimmed copy of in (nil stays nil).
func trimmedCopy(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = strings.TrimSpace(v)
	}
	return out
}

// serviceListenIsLoopback reports whether listen names a loopback interface
// (localhost, ::1, 127.0.0.0/8). A pack service is never LAN-reachable.
func serviceListenIsLoopback(listen string) bool {
	if listen == "localhost" {
		return true
	}
	ip := net.ParseIP(listen)
	return ip != nil && ip.IsLoopback()
}

// ValidateServices hardens the [[services]] facet at load time, the same
// fail-closed posture as the rest of validatePackFacets: every rejection here
// never reaches the BoM, the fingerprint, or an exec path.
func ValidateServices(root string, m *Manifest) error {
	seen := map[string]bool{}
	for i := range m.Services {
		s := m.Services[i].Normalized()
		bad := func(format string, args ...any) error {
			label := s.Name
			if label == "" {
				label = fmt.Sprintf("#%d", i+1)
			}
			return fmt.Errorf("pack %s: [[services]] %s: %s", root, label, fmt.Sprintf(format, args...))
		}
		if !SafeArtifactName(s.Name) {
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
			return bad("invalid runtime %q (want %q or %q)", m.Services[i].Runtime, "go-plugin", ServiceRuntimeContainer)
		}
		if s.Runtime == ServiceRuntimeContainer {
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
