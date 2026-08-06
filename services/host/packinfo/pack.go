// Package packinfo is the READ-ONLY model of a pack on disk: pack.toml's typed
// schema, the fail-closed loader that validates it, active-root resolution, and
// the handful of facts derived from those (a pack's container MCP entries, its
// declared MCP names, its memory scope).
//
// It exists because three workflows — launch, doctor and provision — need to
// answer "what pack is active and what does it declare", and none of them may
// import workflow/pack to ask: L3 packages are siblings, and a sibling edge is
// the shape docs/design/architecture.md exists to forbid. So the FACTS live
// here, at L1, where anything may read them.
//
// What is deliberately NOT here is every decision: adoption, the Tier-1 bill of
// materials, the trust store, host-exec fingerprints, wrapper installation and
// the config transaction all stay in workflow/pack. This package reads and
// validates; it never decides that a pack may run something on this host, and
// it holds no state. Consistent with L1, it imports no sibling capability —
// only config, routing, sys and workspace.
package packinfo

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"pix/host/config"
	"pix/host/routing"
	"pix/host/sys"
	"pix/host/workspace"

	"github.com/BurntSushi/toml"
)

// Manifest is pack.toml: identity + model prefs, integrations, proxy/bin
// wrappers, services and config layering. Skills and knowledge are discovered
// by convention (skills/, knowledge/); knowledge/ is INERT (mounted only).
type Manifest struct {
	Name              string `toml:"name"`
	Schema            int    `toml:"schema"`
	OllamaBridgeModel string `toml:"ollama_bridge_model,omitempty"`
	GogAccount        string `toml:"gog_account,omitempty"` // layered into cfg.GogAccount on `pack use`
	// MemoryScope tags in-VM memory recall/capture; "" or "default" is shared.
	MemoryScope string `toml:"memory_scope,omitempty"`
	// Prerequisites are external state the user must bring, shown on the
	// adoption screen before any setup hook runs.
	Prerequisites []string      `toml:"prerequisites,omitempty"`
	Integrations  []Integration `toml:"integrations,omitempty"`
	Proxies       []PackProxy   `toml:"proxy,omitempty"` // bin/ wrappers: sandbox, or host-mode (Tier-1)
	// Bins are [[bin]] external host binaries (Tier-1, SHA-pinned; fail-closed
	// on a missing sha at load, re-hashed before every activation).
	Bins []Bin `toml:"bin,omitempty"`
	// Setup steps contribute resumable host onboarding to `pix setup --pack`: a
	// repo-relative executable with a read-only probe + idempotent apply, both
	// fingerprinted.
	Setup []SetupStep `toml:"setup,omitempty"`
	// Inference declares an authenticated model gateway without putting its
	// endpoint or aliases in public Pix.
	Inference *Inference `toml:"inference,omitempty"`
	// Services are [[services]]: the SOLE declaration of a long-running external
	// service unit — fail-closed at load, Tier-1 gated, fingerprinted (service.go).
	Services []Service `toml:"services,omitempty"`
}

type Inference struct {
	Exclusive       bool                     `toml:"exclusive,omitempty"`
	RequiredBackend string                   `toml:"required_backend,omitempty"`
	Backends        map[string]InferenceBack `toml:"backends,omitempty"`
	Models          []InferenceModel         `toml:"models,omitempty"`
}

type InferenceBack struct {
	Driver            string `toml:"driver"`
	Protocol          string `toml:"protocol,omitempty"`
	BaseURL           string `toml:"base_url,omitempty"`
	Auth              string `toml:"auth"`
	KeyEnv            string `toml:"key_env,omitempty"`
	CredentialService string `toml:"credential_service,omitempty"`
	CredentialHeader  string `toml:"credential_header,omitempty"`
	CredentialFormat  string `toml:"credential_format,omitempty"`
}

type InferenceModel struct {
	Model    string `toml:"model"`
	Backend  string `toml:"backend"`
	Upstream string `toml:"upstream_id"`
}

type SetupStep struct {
	ID          string   `toml:"id"`
	Description string   `toml:"description,omitempty"`
	Path        string   `toml:"path"`
	CheckArgs   []string `toml:"check_args,omitempty"`
	ApplyArgs   []string `toml:"apply_args,omitempty"`
	Required    bool     `toml:"required,omitempty"`
}

// PackProxy is one [[proxy]] entry: a bin/<name> wrapper script. Host=false is
// an in-sandbox wrapper (mounted via SynthesizePackKit); Host=true is host-mode.
type PackProxy struct {
	Name   string   `toml:"name"`
	Host   bool     `toml:"host,omitempty"`
	Egress []string `toml:"egress,omitempty"`
}

// Bin is one [[bin]] entry: an external, SHA-pinned host binary (Tier-1).
// LoadPack fails closed on an empty SHA — never an exec path unpinned.
type Bin struct {
	Name string `toml:"name"`
	Path string `toml:"path"`
	SHA  string `toml:"sha"`
	Host bool   `toml:"host,omitempty"`
}

// Integration is REFERENCE-ONLY: the pack says "I use <mcp> and need the
// credential <env>", shipping NO executable code — the server is host-provided
// and the credential an op:// ref the user owns. Manifest, Image and URL are
// the three MUTUALLY EXCLUSIVE registration modes (validatePackFacets).
type Integration struct {
	Name string `toml:"name"`          // human label
	Env  string `toml:"env,omitempty"` // op-refs.env ENV VAR the credential lives under
	MCP  string `toml:"mcp,omitempty"` // MCP server name to attach (host-provided)
	// Manifest runs a CONTAINER by server-manifest URL (`sbx mcp add --local
	// --url`); its creds are Docker-side, never op-refs.
	Manifest string `toml:"manifest,omitempty"`
	// Image runs a CONTAINER by DIRECT image ref, op-run wrapped so creds
	// resolve from 1Password at gateway spawn.
	Image string `toml:"image,omitempty"`
	// EnvKeys are ADDITIONAL (typically non-secret) env var names forwarded into
	// an Image container via `-e <KEY>`; the op-refs-backed secret is Env.
	EnvKeys []string `toml:"env_keys,omitempty"`
	// EnvValues are non-secret literal env values baked into the container
	// command. Secret-shaped entries are refused — use Env/op:// instead.
	EnvValues map[string]string `toml:"env_values,omitempty"`
	URL       string            `toml:"url,omitempty"` // REMOTE endpoint; the gateway OAuths host-side
	// Setup links this integration to an optional [[setup]] hook: activation
	// registers it but does not solicit its credential up front.
	Setup string `toml:"setup,omitempty"`
}

// Info is a resolved pack on disk.
type Info struct {
	Root             string
	Manifest         Manifest
	SkillsDir        string // <root>/skills if it exists, else ""
	KnowledgeDir     string // <root>/knowledge if it exists, else ""
	CapabilitiesFile string // mounted at ~/.pi/agent/capabilities.json
	WebSearchFile    string // mounted at ~/.pi/web-search.json
}

const PackManifestName = "pack.toml"

// ErrNotAPack is the sentinel LoadPack wraps when root has no pack.toml at all,
// separating that "genuinely absent" class — safe to degrade on — from a pack
// that EXISTS but is broken.
var ErrNotAPack = errors.New("not a pack")

// LoadPack reads a pack from a directory; pack.toml's presence IS the "is this
// a pack" test, and its absence errors around ErrNotAPack.
func LoadPack(root string) (*Info, error) {
	root = filepath.Clean(root)
	mf := filepath.Join(root, PackManifestName)
	b, err := os.ReadFile(mf)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%s is %w (no %s)", root, ErrNotAPack, PackManifestName)
		}
		return nil, err
	}
	var m Manifest
	if err := toml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", mf, err)
	}
	p := &Info{Root: root, Manifest: m}
	// skills/, knowledge/ and bin/ are discovered by convention and mounted, so
	// each is refused on ANY symlink (the dir itself or anything beneath it).
	// bin/ is validated but not recorded: its wrappers are resolved per [[proxy]].
	for _, mount := range []struct {
		name string
		dest *string
	}{{"skills", &p.SkillsDir}, {"knowledge", &p.KnowledgeDir}, {"bin", nil}} {
		d := filepath.Join(root, mount.name)
		if !sys.DirHasEntries(d) {
			continue
		}
		if IsSymlinkPath(d) {
			return nil, fmt.Errorf("pack %s: %s/ is a symlink; refusing to mount", root, mount.name)
		}
		if has, bad := dirHasSymlink(d); has {
			return nil, fmt.Errorf("pack %s: %s/ contains a symlink (%s); packs must not use symlinks, refusing to mount", root, mount.name, bad)
		}
		if mount.dest != nil {
			*mount.dest = d
		}
	}
	// Mounted config files: same symlink posture, and web-search.json must also
	// be a bounded JSON object (it is loaded by the sandbox as-is).
	for _, file := range []struct {
		name  string
		dest  *string
		check func([]byte) error
	}{{"capabilities.json", &p.CapabilitiesFile, nil}, {"web-search.json", &p.WebSearchFile, validateWebSearchJSON}} {
		f := filepath.Join(root, file.name)
		if !sys.IsRegularFile(f) {
			continue
		}
		if IsSymlinkPath(f) {
			return nil, fmt.Errorf("pack %s: %s is a symlink; refusing to mount", root, file.name)
		}
		if file.check != nil {
			b, readErr := os.ReadFile(f)
			if readErr != nil {
				return nil, fmt.Errorf("pack %s: reading %s: %w", root, file.name, readErr)
			}
			if err := file.check(b); err != nil {
				return nil, fmt.Errorf("pack %s: %w", root, err)
			}
		}
		*file.dest = f
	}
	if err := validatePackFacets(root, &m); err != nil {
		return nil, err
	}
	return p, nil
}

// validateWebSearchJSON accepts only a bounded JSON object.
func validateWebSearchJSON(b []byte) error {
	var value any
	if len(b) > 64*1024 || json.Unmarshal(b, &value) != nil {
		return fmt.Errorf("web-search.json must be valid JSON no larger than 64 KiB")
	}
	if _, ok := value.(map[string]any); !ok {
		return fmt.Errorf("web-search.json must contain a JSON object")
	}
	return nil
}

// validatePackFacets hardens the typed facets at load time, fail closed: safe
// artifact names, inference backends/models against the catalog, one
// registration mode per integration, and every [[bin]] repo-relative,
// non-symlinked and SHA-pinned.
func validatePackFacets(root string, m *Manifest) error {
	if inf := m.Inference; inf != nil {
		catalog, err := routing.LoadRegistry()
		if err != nil {
			return fmt.Errorf("pack %s: loading model catalog: %w", root, err)
		}
		if inf.RequiredBackend != "" {
			if _, ok := inf.Backends[inf.RequiredBackend]; !ok {
				return fmt.Errorf("pack %s: inference.required_backend %q is not declared in inference.backends", root, inf.RequiredBackend)
			}
		}
		for name, b := range inf.Backends {
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("pack %s: inference backend name is empty", root)
			}
			switch b.Driver {
			case "native", "openai-compatible", "ollama":
			default:
				return fmt.Errorf("pack %s: inference backend %q has unsupported driver %q", root, name, b.Driver)
			}
			if b.Protocol != "" && b.Protocol != "openai-completions" && b.Protocol != "openai-responses" && b.Protocol != "anthropic-messages" && b.Protocol != "google-generative-ai" {
				return fmt.Errorf("pack %s: inference backend %q has unsupported protocol %q", root, name, b.Protocol)
			}
			switch b.Auth {
			case "1password":
				if strings.TrimSpace(b.KeyEnv) == "" {
					return fmt.Errorf("pack %s: inference backend %q uses 1password but has no key_env", root, name)
				}
			case "sbx-session", "none":
			default:
				return fmt.Errorf("pack %s: inference backend %q has unsupported auth %q", root, name, b.Auth)
			}
			if b.Auth == "sbx-session" && (strings.TrimSpace(b.CredentialService) == "" || strings.TrimSpace(b.KeyEnv) == "") {
				return fmt.Errorf("pack %s: inference backend %q uses sbx-session but has no credential_service/key_env", root, name)
			}
			if b.Auth == "sbx-session" && strings.TrimSpace(b.CredentialService) != "sbx-login" {
				return fmt.Errorf("pack %s: inference backend %q uses sbx-session but credential_service is %q (want reserved service sbx-login)", root, name, b.CredentialService)
			}
			if b.Driver != "native" && b.Driver != "ollama" {
				u, err := url.Parse(strings.TrimSpace(b.BaseURL))
				if err != nil || u.Hostname() == "" || u.User != nil {
					return fmt.Errorf("pack %s: inference backend %q has invalid base_url %q", root, name, b.BaseURL)
				}
				loopback := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"
				if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
					return fmt.Errorf("pack %s: inference backend %q base_url must use https (or loopback http)", root, name)
				}
			}
		}
		for _, binding := range inf.Models {
			if !routing.IsQualifiedID(binding.Model) {
				return fmt.Errorf("pack %s: inference model %q is not a canonical lab/model id", root, binding.Model)
			}
			if _, ok := catalog.Get(binding.Model); !ok {
				return fmt.Errorf("pack %s: inference model %q is not in the Pix model catalog", root, binding.Model)
			}
			if _, ok := inf.Backends[binding.Backend]; !ok {
				return fmt.Errorf("pack %s: inference model %q references unknown backend %q", root, binding.Model, binding.Backend)
			}
			if strings.TrimSpace(binding.Upstream) == "" {
				return fmt.Errorf("pack %s: inference model %q has no upstream_id", root, binding.Model)
			}
		}
	}
	seenMCP := map[string]bool{}
	for _, ig := range m.Integrations {
		name := strings.TrimSpace(ig.MCP)
		if name == "" {
			continue
		}
		if seenMCP[name] {
			return fmt.Errorf("pack %s: duplicate [[integrations]] mcp %q; each server name must be declared exactly once", root, name)
		}
		seenMCP[name] = true
		kinds := 0
		for _, value := range []string{ig.Manifest, ig.Image, ig.URL} {
			if strings.TrimSpace(value) != "" {
				kinds++
			}
		}
		if kinds > 1 {
			return fmt.Errorf("pack %s: integration %q sets more than one of manifest, image, and url; choose exactly one", root, name)
		}
		if (strings.TrimSpace(ig.Manifest) != "" || strings.TrimSpace(ig.URL) != "") &&
			(strings.TrimSpace(ig.Env) != "" || len(ig.EnvKeys) > 0 || len(ig.EnvValues) > 0) {
			return fmt.Errorf("pack %s: integration %q cannot use env/env_keys with manifest or url; those registration modes do not forward pack environment variables", root, name)
		}
		for key, value := range ig.EnvValues {
			if strings.TrimSpace(key) == "" || strings.ContainsAny(key+value, "\x00\r\n") {
				return fmt.Errorf("pack %s: integration %q has an invalid env_values entry", root, name)
			}
			if config.LooksSecretShaped(key, value) {
				return fmt.Errorf("pack %s: integration %q env_values[%s] looks secret-shaped; use an op:// reference via env instead", root, name, key)
			}
		}
	}
	for _, p := range m.Proxies {
		if !SafeArtifactName(p.Name) {
			return fmt.Errorf("pack %s: [[proxy]] name %q is invalid (letters, digits, -, _, . only; no path separators)", root, p.Name)
		}
	}
	for _, prerequisite := range m.Prerequisites {
		if strings.TrimSpace(prerequisite) == "" || strings.ContainsAny(prerequisite, "\x00\r\n") {
			return fmt.Errorf("pack %s: prerequisites must be non-empty single-line text", root)
		}
	}
	for _, b := range m.Bins {
		if !SafeArtifactName(b.Name) {
			return fmt.Errorf("pack %s: [[bin]] name %q is invalid (letters, digits, -, _, . only; no path separators)", root, b.Name)
		}
		if strings.TrimSpace(b.SHA) == "" {
			return fmt.Errorf("pack %s: [[bin]] %q has no sha — external binaries must be SHA-pinned (fail closed)", root, b.Name)
		}
		if err := validateRepoRelativePath(root, b.Path); err != nil {
			return fmt.Errorf("pack %s: [[bin]] %q: %w", root, b.Name, err)
		}
	}
	seenSetup := map[string]bool{}
	for _, s := range m.Setup {
		if !SafeArtifactName(s.ID) {
			return fmt.Errorf("pack %s: [[setup]] id %q is invalid (letters, digits, -, _, . only)", root, s.ID)
		}
		if seenSetup[s.ID] {
			return fmt.Errorf("pack %s: duplicate [[setup]] id %q", root, s.ID)
		}
		seenSetup[s.ID] = true
		if err := validateRepoRelativePath(root, s.Path); err != nil {
			return fmt.Errorf("pack %s: [[setup]] %q: %w", root, s.ID, err)
		}
		if err := validateNoSymlinkComponents(root, s.Path); err != nil {
			return fmt.Errorf("pack %s: [[setup]] %q: %w", root, s.ID, err)
		}
		fi, err := os.Stat(filepath.Join(root, s.Path))
		if err != nil {
			return fmt.Errorf("pack %s: [[setup]] %q: %v", root, s.ID, err)
		}
		if !fi.Mode().IsRegular() || fi.Mode()&0o111 == 0 {
			return fmt.Errorf("pack %s: [[setup]] %q path %q must be a regular executable file", root, s.ID, s.Path)
		}
		for _, arg := range append(append([]string{}, s.CheckArgs...), s.ApplyArgs...) {
			if strings.ContainsAny(arg, "\x00\r\n") {
				return fmt.Errorf("pack %s: [[setup]] %q contains a control character in argv", root, s.ID)
			}
		}
	}
	for _, ig := range m.Integrations {
		if ig.Setup != "" && !seenSetup[ig.Setup] {
			return fmt.Errorf("pack %s: integration %q references unknown setup hook %q", root, ig.Name, ig.Setup)
		}
	}
	if err := ValidateServices(root, m); err != nil {
		return err
	}
	return nil
}

// validateNoSymlinkComponents rejects a symlink at ANY component beneath root:
// Lstat on the leaf alone misses an intermediate directory symlink.
func validateNoSymlinkComponents(root, rel string) error {
	cur := filepath.Clean(root)
	for _, part := range strings.Split(filepath.Clean(rel), string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path %q contains symlink component %q; refusing host execution", rel, cur)
		}
	}
	return nil
}

// validateRepoRelativePath rejects a path that is empty, absolute, escapes root
// via `..`, or is a symlink — the skills/knowledge posture, for declared paths.
func validateRepoRelativePath(root, rel string) error {
	if strings.TrimSpace(rel) == "" {
		return fmt.Errorf("path is empty")
	}
	if filepath.IsAbs(rel) {
		return fmt.Errorf("path %q must be repo-relative, not absolute", rel)
	}
	clean := filepath.Join(root, rel)
	if !strings.HasPrefix(clean, filepath.Clean(root)+string(filepath.Separator)) {
		return fmt.Errorf("path %q escapes the pack root", rel)
	}
	if IsSymlinkPath(clean) {
		return fmt.Errorf("path %q is a symlink; refusing to mount", rel)
	}
	return nil
}

// IsSymlinkPath reports whether path itself is a symlink (Lstat, no follow).
func IsSymlinkPath(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

// dirHasSymlink walks dir and reports the first symlink of ANY kind: WalkDir
// does not descend into a symlinked DIRECTORY, so only blanket rejection is
// a complete defense.
func dirHasSymlink(dir string) (bool, string) {
	bad := ""
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d == nil {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			bad = path
			return filepath.SkipAll
		}
		return nil
	})
	return bad != "", bad
}

// ActivePackRoot resolves the active pack path: the --pack override wins, else
// config's `pack`. "" means no active pack.
func ActivePackRoot(cfgPack, override string) string {
	if strings.TrimSpace(override) != "" {
		return ExpandUser(strings.TrimSpace(override))
	}
	return ExpandUser(strings.TrimSpace(cfgPack))
}

// ActivePackRoots is the ordered pack STACK: the --pack override alone when
// given, else cfg.Packs in command order plus cfg.Pack (the last activation).
func ActivePackRoots(cfg *config.Config, override string) []string {
	if strings.TrimSpace(override) != "" {
		return []string{ExpandUser(strings.TrimSpace(override))}
	}
	if cfg == nil {
		return nil
	}
	var roots []string
	seen := map[string]bool{}
	for _, root := range append(append([]string{}, cfg.Packs...), cfg.Pack) {
		root = ExpandUser(strings.TrimSpace(root))
		if root != "" && !seen[root] {
			seen[root] = true
			roots = append(roots, root)
		}
	}
	return roots
}

func UniquePackRoots(roots []string) []string {
	seen := map[string]bool{}
	unique := make([]string, 0, len(roots))
	for _, root := range roots {
		key := CanonicalizePackRoot(root)
		if key != "" && !seen[key] {
			seen[key] = true
			unique = append(unique, root)
		}
	}
	return unique
}

// ExpandUser expands a leading ~ to $HOME (git/toml don't do it for us).
func ExpandUser(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// DefaultPackRoot is the default-pack location under the pix data dir.
// Resolution only: it creates nothing and never rewrites cfg.Pack.
func DefaultPackRoot() string { return config.PackDir() }

// ContainerMCP returns {integration.mcp: config.MCPContainer} for a pack's
// CONTAINER/REMOTE integrations, which `pix mcp register` adds specially rather
// than as plain host subcommands. nil when there are none.
func ContainerMCP(p *Info) map[string]config.MCPContainer {
	out := map[string]config.MCPContainer{}
	for _, ig := range p.Manifest.Integrations {
		if ig.MCP == "" {
			continue
		}
		switch {
		case strings.TrimSpace(ig.Manifest) != "":
			out[ig.MCP] = config.MCPContainer{Manifest: strings.TrimSpace(ig.Manifest)}
		case strings.TrimSpace(ig.Image) != "":
			var keys []string
			if ig.Env != "" {
				keys = append(keys, ig.Env) // the op-refs secret, forwarded too
			}
			keys = append(keys, ig.EnvKeys...)
			values := make(map[string]string, len(ig.EnvValues))
			for key, value := range ig.EnvValues {
				values[key] = value
			}
			out[ig.MCP] = config.MCPContainer{Image: strings.TrimSpace(ig.Image), EnvKeys: keys, EnvValues: values}
		case strings.TrimSpace(ig.URL) != "":
			out[ig.MCP] = config.MCPContainer{RemoteURL: strings.TrimSpace(ig.URL)}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ActiveContainerMCP resolves ContainerMCP for the active pack, or nil when
// there is none or it won't load (other registrations proceed regardless).
func ActiveContainerMCP(cfg *config.Config) map[string]config.MCPContainer {
	root := ActivePackRoot(cfg.Pack, "")
	if root == "" {
		return nil
	}
	p, err := LoadPack(root)
	if err != nil {
		return nil
	}
	return ContainerMCP(p)
}

// McpNames returns the de-duplicated `integration.mcp` names a pack declares,
// in manifest order.
func McpNames(p *Info) []string {
	var names []string
	seen := map[string]bool{}
	for _, ig := range p.Manifest.Integrations {
		if ig.MCP != "" && !seen[ig.MCP] {
			seen[ig.MCP] = true
			names = append(names, ig.MCP)
		}
	}
	return names
}

// CanonicalizePackRoot normalizes a pack root path for identity comparison:
// expands ~, then Abs + Clean, so a relative CLI argument compares correctly
// against the absolute cfg.Pack (falling back to Clean if Abs fails).
func CanonicalizePackRoot(p string) string {
	p = ExpandUser(strings.TrimSpace(p))
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(p)
}

// WriteMemoryScope writes (or removes) <workspace>/.pix/profile: the memory
// scope tag the in-VM recall/capture extensions read. p is the active pack (nil
// when none). Symlink-safe — a hostile repo can commit .pix as a symlink.
func WriteMemoryScope(ws string, p *Info) {
	if p == nil {
		_ = workspace.RemoveStateFile(ws, "profile")
		return
	}
	// Memory is a single SHARED store by default; ONLY an explicit
	// `memory_scope` isolates a pack. The pack NAME must NOT become a scope —
	// that hides every capture from the default recall view.
	scope := strings.TrimSpace(p.Manifest.MemoryScope)
	if scope == "" || scope == "default" {
		_ = workspace.RemoveStateFile(ws, "profile")
		return
	}
	_ = workspace.WriteStateFile(ws, "profile", []byte(scope+"\n"), 0o644)
}

// SafeArtifactRune is the ONE artifact-name character class: a name built from
// it can never carry a path separator out of the pack root.
func SafeArtifactRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.'
}

// SafeArtifactName rejects a skill/knowledge name that could escape the pack root
// (path separators, `..`) or is empty.
func SafeArtifactName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		if !SafeArtifactRune(r) {
			return false
		}
	}
	return true
}
