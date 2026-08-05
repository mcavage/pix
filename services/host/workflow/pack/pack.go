// pack.go implements `pix pack` — the git-backed context bundle (skills,
// knowledge, mcp/proxy/config facets; docs/design/packs.md). All OS/git calls
// go through hostenv so the logic is testable with fakes.
package pack

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/launcher"
	"pix/host/routing"
	"pix/host/secret"
	"pix/host/service"
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
	Bins []packBin `toml:"bin,omitempty"`
	// Setup steps contribute resumable host onboarding to `pix setup --pack`: a
	// repo-relative executable with a read-only probe + idempotent apply, both
	// fingerprinted.
	Setup []packSetupStep `toml:"setup,omitempty"`
	// Inference declares an authenticated model gateway without putting its
	// endpoint or aliases in public Pix.
	Inference *Inference `toml:"inference,omitempty"`
	// Services are [[services]]: the SOLE declaration of a long-running external
	// service unit — fail-closed at load, Tier-1 gated, fingerprinted (service.go).
	Services []packService `toml:"services,omitempty"`
}

// ApplyPackInference projects a pack's inference contract into launcher config:
// public wiring metadata only (the schema cannot carry a secret); probe
// evidence starts false and is earned later by setup.
func ApplyPackInference(cfg *config.Config, inf *Inference, source string) error {
	if cfg == nil || inf == nil {
		return nil
	}
	for name := range inf.Backends {
		if existing, ok := cfg.Inference.Backends[name]; ok && existing.Source != source {
			owner := "user configuration"
			if existing.Source != "" {
				owner = existing.Source
			}
			return fmt.Errorf("pack inference backend %q conflicts with %s; backend names cannot replace another source", name, owner)
		}
	}
	// Reapplying an unchanged active pack must not erase the availability
	// evidence setup just earned: preserved only across an exact backend +
	// binding match, so any change starts unverified.
	type evidence struct{ available, verified bool }
	prior := map[string]evidence{}
	for _, binding := range cfg.Inference.Models {
		backend, ok := cfg.Inference.Backends[binding.Backend]
		if !ok || binding.Source != source {
			continue
		}
		key := inferenceEvidenceKey(binding, backend)
		prior[key] = evidence{binding.Available, binding.Verified}
	}
	ClearPackInference(cfg, source)
	if cfg.Inference.Backends == nil {
		cfg.Inference.Backends = map[string]config.InferenceBackend{}
	}
	for name, b := range inf.Backends {
		cfg.Inference.Backends[name] = config.InferenceBackend{
			Driver: b.Driver, Protocol: b.Protocol, BaseURL: b.BaseURL, Auth: b.Auth, KeyEnv: b.KeyEnv, Source: source,
			CredentialService: b.CredentialService, CredentialHeader: b.CredentialHeader, CredentialFormat: b.CredentialFormat,
		}
	}
	for _, b := range inf.Models {
		binding := config.InferenceModelBinding{
			Model: b.Model, Backend: b.Backend, Upstream: b.Upstream, Source: source,
		}
		if backend, ok := cfg.Inference.Backends[b.Backend]; ok {
			if ev, found := prior[inferenceEvidenceKey(binding, backend)]; found {
				binding.Available, binding.Verified = ev.available, ev.verified
			}
		}
		cfg.Inference.Models = append(cfg.Inference.Models, binding)
	}
	if inf.Exclusive {
		cfg.Inference.ExclusiveSource = source
	}
	return nil
}

func inferenceEvidenceKey(binding config.InferenceModelBinding, backend config.InferenceBackend) string {
	return strings.Join([]string{
		binding.Source, binding.Model, binding.Backend, binding.Upstream,
		backend.Driver, backend.Protocol, backend.BaseURL, backend.Auth,
		backend.KeyEnv, backend.Source, backend.CredentialService,
		backend.CredentialHeader, backend.CredentialFormat,
	}, "\x00")
}

// ClearPackInference removes only pack-owned inference. An empty source clears
// every pack contribution; setup-authored backends have Source="" and survive.
func ClearPackInference(cfg *config.Config, source string) {
	if cfg == nil {
		return
	}
	for name, backend := range cfg.Inference.Backends {
		if backend.Source != "" && (source == "" || backend.Source == source) {
			delete(cfg.Inference.Backends, name)
		}
	}
	kept := cfg.Inference.Models[:0]
	for _, binding := range cfg.Inference.Models {
		if binding.Source != "" && (source == "" || binding.Source == source) {
			continue
		}
		kept = append(kept, binding)
	}
	cfg.Inference.Models = kept
	if cfg.Inference.ExclusiveBackend != "" {
		if _, ok := cfg.Inference.Backends[cfg.Inference.ExclusiveBackend]; !ok {
			cfg.Inference.ExclusiveBackend = ""
		}
	}
	if cfg.Inference.ExclusiveSource != "" && (source == "" || cfg.Inference.ExclusiveSource == source) {
		cfg.Inference.ExclusiveSource = ""
	}
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

type packSetupStep struct {
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

// packBin is one [[bin]] entry: an external, SHA-pinned host binary (Tier-1).
// LoadPack fails closed on an empty SHA — never an exec path unpinned.
type packBin struct {
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
		if isSymlinkPath(d) {
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
		if isSymlinkPath(f) {
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
		if !safeArtifactName(p.Name) {
			return fmt.Errorf("pack %s: [[proxy]] name %q is invalid (letters, digits, -, _, . only; no path separators)", root, p.Name)
		}
	}
	for _, prerequisite := range m.Prerequisites {
		if strings.TrimSpace(prerequisite) == "" || strings.ContainsAny(prerequisite, "\x00\r\n") {
			return fmt.Errorf("pack %s: prerequisites must be non-empty single-line text", root)
		}
	}
	for _, b := range m.Bins {
		if !safeArtifactName(b.Name) {
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
		if !safeArtifactName(s.ID) {
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
	if err := validatePackServices(root, m); err != nil {
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
	if isSymlinkPath(clean) {
		return fmt.Errorf("path %q is a symlink; refusing to mount", rel)
	}
	return nil
}

// isSymlinkPath reports whether path itself is a symlink (Lstat, no follow).
func isSymlinkPath(path string) bool {
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
		return expandUser(strings.TrimSpace(override))
	}
	return expandUser(strings.TrimSpace(cfgPack))
}

// ActivePackRoots is the ordered pack STACK: the --pack override alone when
// given, else cfg.Packs in command order plus cfg.Pack (the last activation).
func ActivePackRoots(cfg *config.Config, override string) []string {
	if strings.TrimSpace(override) != "" {
		return []string{expandUser(strings.TrimSpace(override))}
	}
	if cfg == nil {
		return nil
	}
	var roots []string
	seen := map[string]bool{}
	for _, root := range append(append([]string{}, cfg.Packs...), cfg.Pack) {
		root = expandUser(strings.TrimSpace(root))
		if root != "" && !seen[root] {
			seen[root] = true
			roots = append(roots, root)
		}
	}
	return roots
}

// PersistPackStack composes every declared config facet after each pack has
// independently passed adoption and trust checks: collections union, scalars
// apply in command order (last wins), ownership is recorded PER PACK.
func PersistPackStack(roots []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	store, err := requireTrustStore()
	if err != nil {
		return err
	}
	var packs []*Info
	for _, root := range UniquePackRoots(roots) {
		p, perr := LoadPack(root)
		if perr != nil {
			return perr
		}
		packs = append(packs, p)
	}
	records, _, err := applyPackStack(cfg, store, packs)
	if err != nil {
		return err
	}
	return packTxn{records: records}.commit(cfg)
}

// requireTrustStore loads the launcher-owned trust store or fails closed. An
// unreadable store is FATAL wherever it is read: it is the reversibility AND
// acceptance backbone, so an empty stand-in would lose the previous removal set
// and clobber the store at the commit point.
func requireTrustStore() (*PackTrustStore, error) {
	store, err := loadPackTrustStore()
	if err != nil {
		return nil, fmt.Errorf("pack trust state unreadable: %v (fix or remove %s and re-run)", err, packTrustStorePath())
	}
	return store, nil
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

// applyPackFacets applies ONE pack's config facets and returns its ownership
// record: the MCP names it actually added (never one the user already had) plus
// each scalar's PRIOR value, so switching away restores exactly that.
func applyPackFacets(cfg *config.Config, p *Info) (packLock, error) {
	var lock packLock
	for _, name := range McpNames(p) {
		if cfg.AddMCP(name) {
			lock.MCP = append(lock.MCP, name)
		}
	}
	if v := strings.TrimSpace(p.Manifest.GogAccount); v != "" {
		lock.PriorGogAccount = cfg.GogAccount
		lock.GogAccount = v
		cfg.SetGogAccount(v)
	}
	if v := strings.TrimSpace(p.Manifest.OllamaBridgeModel); v != "" {
		lock.PriorOllamaBridgeModel = cfg.OllamaBridgeModel
		lock.OllamaBridgeModel = v
		cfg.OllamaBridgeModel = v
	}
	// Exclusive policy is an ordered scalar, not an additive facet: a later
	// non-exclusive declaration clears an earlier pack's exclusivity.
	if p.Manifest.Inference != nil && !p.Manifest.Inference.Exclusive {
		cfg.Inference.ExclusiveSource = ""
	}
	if err := ApplyPackInference(cfg, p.Manifest.Inference, p.Root); err != nil {
		return packLock{}, err
	}
	return lock, nil
}

// applyPackStack is the ONE compose path — `pack use` passes a single pack,
// `pix setup --pack ...` the whole stack. It rewinds the live activation to its
// pre-pack baseline (so the Prior* values each record captures are the real
// ones), applies the packs in command order, and returns one ownership record
// per pack plus every MCP name the rewind removed. Nothing is written here; the
// caller commits.
func applyPackStack(cfg *config.Config, store *PackTrustStore, packs []*Info) ([]packActivationRecord, []string, error) {
	removed := revertPackStack(cfg, store, ActivePackRoots(cfg, ""))
	ClearPackInference(cfg, "")
	cfg.Pack, cfg.Packs = "", nil
	var records []packActivationRecord
	for _, p := range packs {
		lock, err := applyPackFacets(cfg, p)
		if err != nil {
			return nil, nil, err
		}
		cfg.Pack = p.Root
		cfg.Packs = append(cfg.Packs, p.Root)
		records = append(records, store.newActivationRecord(p.Root, lock))
	}
	dedupeInferenceBindings(cfg)
	return records, removed, nil
}

// dedupeInferenceBindings keeps the LAST declaration of each (model,backend) in
// stack order, so a later pack can replace an earlier pack's upstream alias.
func dedupeInferenceBindings(cfg *config.Config) {
	if len(cfg.Inference.Models) == 0 {
		return
	}
	seen := map[string]bool{}
	kept := make([]config.InferenceModelBinding, 0, len(cfg.Inference.Models))
	for i := len(cfg.Inference.Models) - 1; i >= 0; i-- {
		b := cfg.Inference.Models[i]
		if key := b.Model + "\x00" + b.Backend; !seen[key] {
			seen[key] = true
			kept = append(kept, b)
		}
	}
	slices.Reverse(kept)
	cfg.Inference.Models = kept
}

// revertPackStack unwinds an activation stack in REVERSE command order — the
// only order where each scalar restore sees the value its predecessor set —
// reporting every MCP name actually removed.
func revertPackStack(cfg *config.Config, store *PackTrustStore, roots []string) []string {
	var removed []string
	for i := len(roots) - 1; i >= 0; i-- {
		removed = append(removed, revertPackPriorContribution(cfg, store.activationFor(roots[i]))...)
	}
	return removed
}

// expandUser expands a leading ~ to $HOME (git/toml don't do it for us).
func expandUser(p string) string {
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

// packContainerMCP returns {integration.mcp: config.MCPContainer} for a pack's
// CONTAINER/REMOTE integrations, which `pix mcp register` adds specially rather
// than as plain host subcommands. nil when there are none.
func packContainerMCP(p *Info) map[string]config.MCPContainer {
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

// ActiveContainerMCP resolves packContainerMCP for the active pack, or nil when
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
	return packContainerMCP(p)
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

// packKitDir is the PER-PACK KEY ephemeral mixin kits are synthesized under
// (<StateDir>/pix/pack-kits/<hash>): a naming PREFIX, not a live dir — each
// launch builds its own <hash>.kit-XXXX beside it.
func packKitDir(root string) string {
	sum := sha256.Sum256([]byte(root))
	dir, err := config.StateDir()
	if err != nil {
		dir = "pix-state"
	}
	return filepath.Join(dir, "pix", "pack-kits", hex.EncodeToString(sum[:])[:16])
}

// SynthesizePackKit builds the ephemeral mixin kit that mounts a pack's
// sandbox-side files: a `kind: mixin` spec.yaml plus files/home/.local/bin/
// <name> (0755) per non-host [[proxy]], capabilities.json and web-search.json.
// Returns (dir, nil), ("", nil) when there is nothing to mount (the caller must
// not stack an empty kit), or ("", err) when something IS declared but the kit
// cannot be built — the caller then fails the launch closed. Copies, never
// symlinks; each call builds its OWN MkdirTemp dir COMPLETELY before returning,
// so concurrent launches never clash and a partial kit is never mounted.
func SynthesizePackKit(p *Info) (string, error) {
	var sandboxProxies []PackProxy
	for _, pr := range p.Manifest.Proxies {
		if !pr.Host {
			sandboxProxies = append(sandboxProxies, pr)
		}
	}
	base := packKitDir(p.Root)
	parent := filepath.Dir(base)
	sweepStaleKitTemps(parent, filepath.Base(base))
	if len(sandboxProxies) == 0 && p.CapabilitiesFile == "" && p.WebSearchFile == "" {
		return "", nil // nothing to mount; the sweep above reaps any previous kit
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", fmt.Errorf("pack kit for %s: %v", p.Manifest.Name, err)
	}
	dir, err := os.MkdirTemp(parent, filepath.Base(base)+kitLaunchInfix)
	if err != nil {
		return "", fmt.Errorf("pack kit for %s: %v", p.Manifest.Name, err)
	}
	fail := func(format string, a ...any) (string, error) {
		_ = os.RemoveAll(dir) // never leave a half-built kit dir behind
		return "", fmt.Errorf(format, a...)
	}
	_ = os.Chmod(dir, 0o755) // MkdirTemp creates 0700; the kit is a mounted tree
	// A stacked kit needs a valid manifest: schemaVersion (required by the loader),
	// kind: mixin, and a name. Match the base kit's schemaVersion "2".
	spec := fmt.Sprintf("schemaVersion: \"2\"\nkind: mixin\nname: %s\n", p.Manifest.Name)
	// Fold each sandbox proxy's declared egress into permissions.network.allow so
	// the wrapper can reach its host endpoint — the sbx egress proxy blocks (403)
	// anything off the allowlist. Kit stacking unions this with the base kit's.
	var egress []string
	egSeen := map[string]bool{}
	addEgress := func(e string) {
		if e == "" || egSeen[e] {
			return
		}
		egSeen[e] = true
		egress = append(egress, e)
	}
	for _, pr := range sandboxProxies {
		for _, e := range pr.Egress {
			e = strings.TrimSpace(e)
			addEgress(e)
			// The sbx egress proxy matches host.docker.internal and localhost as
			// DISTINCT rules, so a host-loopback egress must allow BOTH forms.
			if h := strings.TrimPrefix(e, "host.docker.internal:"); h != e {
				addEgress("localhost:" + h)
			} else if l := strings.TrimPrefix(e, "localhost:"); l != e {
				addEgress("host.docker.internal:" + l)
			}
		}
	}
	if len(egress) > 0 {
		spec += "permissions:\n  network:\n    allow:\n"
		for _, e := range egress {
			spec += "      - " + e + "\n"
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "spec.yaml"), []byte(spec), 0o644); err != nil {
		return fail("pack kit for %s: %v", p.Manifest.Name, err)
	}
	// Everything else is a copy into files/home/** (the sbx mixin mount honors
	// files/home/** into $HOME but NOT files/usr/local/**, so a wrapper written
	// there never lands). Any declared-but-unreadable file fails the whole synth
	// — never launch with a partial kit.
	type kitFile struct {
		label, src string
		dest       []string
		mode       os.FileMode
	}
	var files []kitFile
	for _, pr := range sandboxProxies {
		files = append(files, kitFile{"pack proxy " + pr.Name, filepath.Join(p.Root, "bin", pr.Name),
			[]string{"files", "home", ".local", "bin", pr.Name}, 0o755})
	}
	if p.CapabilitiesFile != "" {
		files = append(files, kitFile{"pack capabilities.json", p.CapabilitiesFile,
			[]string{"files", "home", ".pi", "agent", "capabilities.json"}, 0o644})
	}
	if p.WebSearchFile != "" {
		files = append(files, kitFile{"pack web-search.json", p.WebSearchFile,
			[]string{"files", "home", ".pi", "web-search.json"}, 0o644})
	}
	for _, f := range files {
		dest := filepath.Join(append([]string{dir}, f.dest...)...)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fail("pack kit for %s: %v", p.Manifest.Name, err)
		}
		b, err := os.ReadFile(f.src)
		if err != nil {
			return fail("%s: %v (refusing to build the pack kit)", f.label, err)
		}
		if err := os.WriteFile(dest, b, f.mode); err != nil {
			return fail("%s: %v (refusing to build the pack kit)", f.label, err)
		}
	}
	return dir, nil
}

// kitLaunchInfix suffixes each per-launch kit dir onto the pack hash, so launch
// dirs sit beside their key and sweepStaleKitTemps finds them by prefix.
const kitLaunchInfix = ".kit-"

// sweepStaleKitTemps best-effort removes old per-launch kit dirs for THIS pack.
// Only entries older than an hour are touched, so a concurrent launch's kit is
// never yanked out from under it.
func sweepStaleKitTemps(parent, base string) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-time.Hour)
	for _, e := range entries {
		name := e.Name()
		// base+"." covers kitLaunchInfix and any other dotted debris beside the
		// key; the bare base is the stable kit path older builds synthesized into.
		if name != base && !strings.HasPrefix(name, base+".") {
			continue
		}
		if info, ierr := e.Info(); ierr != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(parent, name))
	}
}

// proxyShimTemplate is the scaffold `pack add proxy <name>` writes to bin/<name>
// (0755): a bash shim the pack author fills in.
func proxyShimTemplate(name string) string {
	return "#!/usr/bin/env bash\n" +
		"# " + name + " — pack proxy wrapper (scaffolded by `pix pack add proxy`).\n" +
		"# Runs IN THE SANDBOX, fenced by the net allowlist. Edit it to wrap the real\n" +
		"# CLI/API call, and declare any domains it needs in pack.toml's [[proxy]]\n" +
		"# egress = [...] so the sbx kit allowlist matches.\n" +
		"set -euo pipefail\n" +
		"echo \"" + name + ": TODO — implement this wrapper\" >&2\n" +
		"exit 1\n"
}

// packLock is <pack-root>/pack.lock: GENERATED activation provenance (what the
// last `pack use` contributed to cfg), git-ignored.
//
// TRUST: a LOCAL, HUMAN-READABLE HINT only. It sits inside the pack directory —
// attacker-writable for any cloned pack via a plain `git pull` — so nothing
// that drives a config mutation is read from it. The authoritative record is
// the launcher-owned trust store, written at the same commit point. The only
// field read back is Remote/Commit, a FAIL-SAFE adoption marker.
type packLock struct {
	MCP []string `toml:"mcp,omitempty"`
	// Remote/Commit are set ONLY by a `pack use <git-url>` adoption and kept
	// across re-activations, so adoption can't be laundered by pointing `pack
	// use` at the already-cloned local directory.
	Remote string `toml:"remote,omitempty"`
	Commit string `toml:"commit,omitempty"`
	// GogAccount/OllamaBridgeModel record the value THIS pack's last activation
	// set on cfg. Prior* is whatever cfg held immediately BEFORE this pack
	// overwrote it, so reverting on switch-away restores exactly that.
	GogAccount             string `toml:"gog_account,omitempty"`
	PriorGogAccount        string `toml:"prior_gog_account,omitempty"`
	OllamaBridgeModel      string `toml:"ollama_bridge_model,omitempty"`
	PriorOllamaBridgeModel string `toml:"prior_ollama_bridge_model,omitempty"`
}

const PackLockName = "pack.lock"

func PackLockPath(root string) string { return filepath.Join(root, PackLockName) }

// readPackLock reads root's pack.lock, best-effort: an absent OR UNPARSABLE
// file returns the zero value — never guess at (or half-decode) what an older
// activation contributed, since that mis-reports a removal set.
func readPackLock(root string) packLock {
	b, err := os.ReadFile(PackLockPath(root))
	if err != nil {
		return packLock{}
	}
	var l packLock
	if err := toml.Unmarshal(b, &l); err != nil {
		return packLock{}
	}
	return l
}

// writePackLock writes root's pack.lock (0644; NAMES and paths, never a
// credential).
func writePackLock(root string, l packLock) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(l); err != nil {
		return err
	}
	return writePackLockBytes(root, buf.Bytes())
}

// writePackLockBytes is the raw-bytes half of writePackLock (so a rollback can
// restore the prior lock byte-for-byte without a decode round-trip). It
// Lstat-REFUSES a symlinked destination — a malicious pack could redirect the
// write at any host file — and writes temp + rename.
func writePackLockBytes(root string, data []byte) error {
	dest := PackLockPath(root)
	if fi, err := os.Lstat(dest); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to write through it", dest)
	}
	return sys.AtomicWriteInDir(root, PackLockName, data, 0o644)
}

// packTxn is the ONE commit point every activation shares (`pack use`, the
// active-pack `pack add mcp`, and `pix setup --pack ...`). Three writes,
// ordered so the failure residue is always safe: (1) pack.lock, the local hint
// — unwritable aborts before anything commits; (2) the AUTHORITATIVE ownership
// ledger — a failure also aborts before cfg.Save, so config is never committed
// unattributed; (3) cfg.Save, whose ordinary failure rolls BOTH back. Only a
// hard kill between (2) and (3) leaves an over-claiming record (a no-op
// removal). Steps 2-3 and the rollback run under the trust lock against a
// FRESH store load.
type packTxn struct {
	records  []packActivationRecord
	lockRoot string   // pack whose pack.lock hint to write ("" writes none)
	lock     packLock // the hint itself; only meaningful with lockRoot
}

func (t packTxn) commit(cfg *config.Config) error {
	restoreLock, err := t.writeLockHint()
	if err != nil {
		return err
	}
	return withPackTrustLock(func() error {
		fresh, lerr := loadPackTrustStore()
		if lerr != nil {
			return t.abort(restoreLock, "pack trust state unreadable: %v", lerr)
		}
		prior := append([]packActivationRecord(nil), fresh.Activations...)
		records := append([]packActivationRecord(nil), t.records...)
		for i := range records {
			// Re-key against the FRESH store: identity may have been upgraded
			// path->remote by a clone recorded since the caller loaded its view.
			records[i].Owner = fresh.TrustKey(records[i].Path)
		}
		fresh.setActivationStack(records)
		if serr := fresh.Save(); serr != nil {
			return t.abort(restoreLock, "recording activation in pack trust state: %v", serr)
		}
		if cerr := cfg.Save(); cerr != nil {
			// Roll BOTH the ledger and the lock back so they match the
			// (unchanged) on-disk config.
			fresh.Activations = prior
			serr, rerr := fresh.Save(), restoreLock()
			if serr != nil || rerr != nil {
				return fmt.Errorf("saving config: %v (rollback incomplete — trust store: %v, pack.lock: %v — the activation record may over-claim this activation's contributions; harmless, but re-run `pack use` once the config is writable)", cerr, serr, rerr)
			}
			return fmt.Errorf("saving config: %v (activation record rolled back; nothing was committed)", cerr)
		}
		return nil
	})
}

// writeLockHint snapshots and writes pack.lock, returning the rollback that
// restores the prior bytes (or removes the file) byte-for-byte. Without a
// snapshot a Save-failure rollback would be impossible, so an unreadable prior
// lock aborts BEFORE anything is written.
func (t packTxn) writeLockHint() (func() error, error) {
	noop := func() error { return nil }
	if t.lockRoot == "" {
		return noop, nil
	}
	const abort = "aborting without saving config (nothing was committed; fix the pack directory and re-run)"
	prior, priorErr := os.ReadFile(PackLockPath(t.lockRoot))
	if priorErr != nil && !os.IsNotExist(priorErr) {
		return nil, fmt.Errorf("reading prior pack.lock for %s: %v — %s", t.lockRoot, priorErr, abort)
	}
	if err := writePackLock(t.lockRoot, t.lock); err != nil {
		return nil, fmt.Errorf("writing pack.lock for %s: %v — %s", t.lockRoot, err, abort)
	}
	existed := priorErr == nil
	return func() error {
		if existed {
			return writePackLockBytes(t.lockRoot, prior)
		}
		return os.Remove(PackLockPath(t.lockRoot))
	}, nil
}

// abort restores the pack.lock hint and reports that nothing was committed.
func (t packTxn) abort(restoreLock func() error, format string, a ...any) error {
	cause := fmt.Sprintf(format, a...)
	if rerr := restoreLock(); rerr != nil {
		return fmt.Errorf("%s (and restoring the prior pack.lock failed: %v) — aborting without saving config (nothing was committed)", cause, rerr)
	}
	return fmt.Errorf("%s — aborting without saving config (nothing was committed; fix %s and re-run)", cause, packTrustStorePath())
}

// commitPackActivation commits ONE pack's activation: the single-pack shape of
// packTxn, with that pack's pack.lock hint.
func commitPackActivation(cfg *config.Config, store *PackTrustStore, root string, lock packLock) error {
	return packTxn{
		records:  []packActivationRecord{store.newActivationRecord(root, lock)},
		lockRoot: root,
		lock:     lock,
	}.commit(cfg)
}

// gatePackHostSurface is the Tier-1 host-exec trust gate, shared by `pack use`
// and the active-pack `pack add mcp` attach so both gate on the SAME surface.
// Tier-0 returns ("", "", nil) and adopts silently; Tier-1 halts at the BoM
// screen unless HOST trust state already holds this identity's acceptance of
// the EXACT current surface. A non-nil error means the caller commits NOTHING.
func gatePackHostSurface(env hostenv.Env, out io.Writer, store *PackTrustStore, p *Info, root, cfgGogAccount string, yes bool) (fingerprint, key string, err error) {
	bom := ComputeHostBoM(p, cfgGogAccount, LocalMCPClassifier(env, env.HostBinary))
	if !bom.Tier1() {
		return "", "", nil
	}
	fp, _, ferr := ComputeHostExecFingerprint(root, bom)
	if ferr != nil {
		return "", "", fmt.Errorf("pack %s: %v", root, ferr)
	}
	key = store.TrustKey(root)
	if got, ok := store.acceptedFingerprint(key); !ok || got != fp {
		if gerr := packTrustGate(os.Stdin, out, cli.IsTTY(os.Stdin), yes, p.Manifest.Name, bom); gerr != nil {
			return "", "", gerr
		}
	}
	return fp, key, nil
}

// recordPackAcceptance persists an accepted Tier-1 host-exec fingerprint in
// HOST state; an empty fingerprint (Tier-0) records nothing. Provenance is
// HOST-recorded ONLY — never the pack-supplied lock, whose forged Remote could
// alias a legit pack and make RecordAcceptance's hygiene sweep DELETE its
// acceptance. Best-effort: a failed write only re-prompts — never opens.
func recordPackAcceptance(out io.Writer, key, root, fingerprint, remote, commit string) {
	if fingerprint == "" {
		return
	}
	rec := PackTrustRecord{Path: CanonicalizePackRoot(root), Fingerprint: fingerprint, Remote: remote, Commit: commit}
	if _, werr := mutatePackTrustStore(func(s *PackTrustStore) error {
		if rec.Remote == "" {
			if prov, ok := s.Adopted[rec.Path]; ok {
				rec.Remote, rec.Commit = prov.Remote, prov.Commit
			}
		}
		s.RecordAcceptance(key, rec)
		return nil
	}); werr != nil {
		fmt.Fprintf(out, "note: could not record the accepted host BoM: %v (the Tier-1 gate will re-prompt)\n", werr)
	}
}

// isAdoptedPack reports whether root was cloned from a remote via `pack use
// <git-url>` — attacker-controlled content, so its manifest never gets to point
// host reads at an arbitrary directory. Three fail-safe signals (a forged
// marker only RESTRICTS): the pack.lock Remote marker, the trust store's
// adoption provenance, and the clone LOCATION under PacksDir.
func isAdoptedPack(root string) bool {
	if strings.TrimSpace(readPackLock(root).Remote) != "" {
		return true
	}
	if store, err := loadPackTrustStore(); err == nil {
		if _, ok := store.Adopted[CanonicalizePackRoot(root)]; ok {
			return true
		}
	}
	return packRootInPacksDir(root)
}

// packRootInPacksDir reports whether root lives under config.PacksDir() — the
// directory only clonePack ever populates, so location alone proves adoption.
func packRootInPacksDir(root string) bool {
	packs := CanonicalizePackRoot(config.PacksDir())
	r := CanonicalizePackRoot(root)
	return packs != "" && strings.HasPrefix(r, packs+string(filepath.Separator))
}

// scrubUntrustedPackLock removes a pack-supplied pack.lock before adopting a
// pack that is NOT currently active: a downloaded pack can ship a forged one,
// or a symlink redirecting the fresh write (os.Remove never follows it).
func scrubUntrustedPackLock(root string) error {
	path := PackLockPath(root)
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing untrusted %s symlink: %w", PackLockName, err)
		}
		return nil
	}
	if fi.IsDir() {
		// A DIRECTORY named pack.lock carries no forged content (readPackLock
		// zero-values it); leave it for the commit point, which fails loudly.
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing untrusted %s: %w", PackLockName, err)
	}
	return nil
}

// revertPackPriorContribution undoes ONE previous activation: it removes
// exactly the MCP entries the ledger attributes to that pack (never one it
// doesn't mention) and restores the scalars to what cfg held before.
func revertPackPriorContribution(cfg *config.Config, prevLock packLock) (removedMCP []string) {
	for _, m := range prevLock.MCP {
		if cfg.RemoveMCP(m) {
			removedMCP = append(removedMCP, m)
		}
	}
	// Only revert if cfg still holds exactly what THIS pack set — never clobber
	// a value something else changed in the meantime.
	if prevLock.GogAccount != "" && cfg.GogAccount == prevLock.GogAccount {
		cfg.SetGogAccount(prevLock.PriorGogAccount)
	}
	if prevLock.OllamaBridgeModel != "" && cfg.OllamaBridgeModel == prevLock.OllamaBridgeModel {
		cfg.OllamaBridgeModel = prevLock.PriorOllamaBridgeModel
	}
	return removedMCP
}

// CanonicalizePackRoot normalizes a pack root path for identity comparison:
// expands ~, then Abs + Clean, so a relative CLI argument compares correctly
// against the absolute cfg.Pack (falling back to Clean if Abs fails).
func CanonicalizePackRoot(p string) string {
	p = expandUser(strings.TrimSpace(p))
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

// printPackRecreateLine is the "same breath" recreate instruction every
// sandbox-facet change MUST print, because --mcp/--kit are create-only.
func printPackRecreateLine(out io.Writer) {
	fmt.Fprintln(out, "MCP attach + sandbox bin/ wrappers + pack skills only take effect on a sandbox CREATE.")
	fmt.Fprintln(out, "Recreate to pick them up:  pix rm <box> && pix run  (`pix ls` names the box)")
}

// --- verb tree --------------------------------------------------------------
//
// Every verb RETURNS a typed error and writes only to the injected writer: the
// domain neither exits the process nor picks a stream. cli.UsageError is a BAD
// INVOCATION; anything else is a failed operation. cmd/pix (L4) is the ONE
// place either becomes an exit code, because that is the layer that owns the
// process.
func usagef(format string, a ...any) error { return cli.Usagef(format, a...) }

// packFlags is the ONE argv parse the verbs share (cmd/pix owns the typed
// grammar and renders it back to argv).
type packFlags struct {
	positionals []string
	host, yes   bool
	envVar      string
}

func parsePackFlags(rest []string) (packFlags, error) {
	var f packFlags
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "--host":
			f.host = true
		case a == "--yes", a == "-y":
			f.yes = true
		case strings.HasPrefix(a, "--env="):
			f.envVar = strings.TrimPrefix(a, "--env=")
		case a == "--env":
			if i+1 >= len(rest) {
				return f, fmt.Errorf("--env needs a value")
			}
			i++
			f.envVar = rest[i]
		case strings.HasPrefix(a, "-"):
			return f, fmt.Errorf("unknown flag %q", a)
		default:
			f.positionals = append(f.positionals, a)
		}
	}
	return f, nil
}

// packTarget resolves an optional positional PATH to a pack root, defaulting to
// the default pack root.
func packTarget(rest []string) string {
	if len(rest) > 0 && strings.TrimSpace(rest[0]) != "" {
		return expandUser(rest[0])
	}
	return DefaultPackRoot()
}

// isPackDir reports whether root already carries a pack.toml.
func isPackDir(root string) bool {
	_, err := os.Stat(filepath.Join(root, PackManifestName))
	return err == nil
}

// safeArtifactRune is the ONE artifact-name character class: a name built from
// it can never carry a path separator out of the pack root.
func safeArtifactRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.'
}

// safeArtifactName rejects a skill/knowledge name that could escape the pack root
// (path separators, `..`) or is empty.
func safeArtifactName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		if !safeArtifactRune(r) {
			return false
		}
	}
	return true
}

// activateDefaultPack points cfg.Pack at root when (and only when) root IS the
// resolved default pack AND cfg.Pack is empty, so a fresh default pack is
// usable without a manual `pack use`; an explicitly active alternate pack is
// NEVER overridden. The cfg.Save error is returned, never swallowed.
func activateDefaultPack(root string) error {
	if root != DefaultPackRoot() {
		return nil // not the default pack; nothing for this to do
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config to activate default pack: %w", err)
	}
	if cfg.Pack != "" {
		return nil // an explicitly active (possibly alternate) pack is never overridden
	}
	cfg.Pack = root
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("could not save config to activate default pack %s: %w", root, err)
	}
	return nil
}

// RunPackNew adopts a pre-existing repo (or one already carrying pack.toml) in
// place, else creates + git-inits a fresh pack. Never re-inits or clobbers.
func RunPackNew(env hostenv.Env, out io.Writer, rest []string) error {
	return packNew(env, out, packTarget(rest))
}

func packNew(env hostenv.Env, out io.Writer, root string) error {
	if isPackDir(root) {
		fmt.Fprintf(out, "already a pack: %s\n", root)
		return activateDefaultPack(root)
	}
	fi, statErr := os.Stat(root)
	existsDir := statErr == nil && fi.IsDir()
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	// git init only if it isn't already a repo (adopt an existing one in place).
	_, gitErr := os.Stat(filepath.Join(root, ".git"))
	isRepo := gitErr == nil
	if !isRepo {
		if _, err := env.Run("git", "-C", root, "init"); err != nil {
			fmt.Fprintf(out, "  note: `git init` failed (%v) — the pack still works; init it yourself\n", err)
		} else {
			isRepo = true
		}
	}
	name := filepath.Base(root)
	if err := WriteManifest(root, Manifest{Name: name, Schema: 1}); err != nil {
		return fmt.Errorf("could not write %s: %w", PackManifestName, err)
	}
	// pack.lock is GENERATED provenance: seed a .gitignore line so a fresh pack
	// never commits it. Best-effort, no-clobber.
	seedPackGitignore(root)
	switch {
	case existsDir && isRepo:
		fmt.Fprintf(out, "adopted existing repo as pack %q: %s\n", name, root)
	case isRepo:
		fmt.Fprintf(out, "created pack %q (git-initialized): %s\n", name, root)
	default:
		fmt.Fprintf(out, "created pack %q: %s (git init it to version it)\n", name, root)
	}
	// Auto-activate the default pack so it is immediately usable (no manual
	// `pack use` for the common case). A named/other pack still needs `pack use`.
	if root != DefaultPackRoot() {
		fmt.Fprintf(out, "use it:  pix pack use %s\n", root)
		return nil
	}
	if err := activateDefaultPack(root); err != nil {
		return err
	}
	fmt.Fprintln(out, "active pack -> this (default) pack")
	return nil
}

// RegisterFn registers the named servers with the sbx gateway: pack may not
// call mcp (both are capabilities), so the caller supplies this seam.
type RegisterFn func(cfg *config.Config, env hostenv.Env, out io.Writer, names []string,
	hostResolver func() (string, error), containers map[string]config.MCPContainer) error

func RunPackAdd(env hostenv.Env, out io.Writer, rest []string, register RegisterFn) error {
	return packAdd(env, out, rest, register)
}

// packAdd writes ONE artifact into a pack, implicit-creating the pack when it
// is absent. Each kind is its own operation below; only `mcp` touches the host.
func packAdd(env hostenv.Env, out io.Writer, rest []string, register RegisterFn) error {
	if len(rest) < 2 {
		return usagef("usage: pix pack add <skill|knowledge|proxy|mcp> <name> [PACK] [flags]")
	}
	kind, name := rest[0], rest[1]
	if !safeArtifactName(name) {
		return usagef("pix pack add: invalid name %q (letters, digits, -, _, . only; no path separators)", name)
	}
	flags, err := parsePackFlags(rest[2:])
	if err != nil {
		return usagef("pix pack add: %v", err)
	}
	root := DefaultPackRoot()
	if len(flags.positionals) >= 1 {
		root = expandUser(flags.positionals[0])
	}
	if !isPackDir(root) {
		RunPackNew(env, out, []string{root})
	}
	// Refuse to write through a symlinked skills/knowledge/bin dir (an adopted
	// pack could point it outside the root) — LoadPack's mount posture.
	for _, d := range []string{"skills", "knowledge", "bin"} {
		if isSymlinkPath(filepath.Join(root, d)) {
			return fmt.Errorf("%s has a symlinked %s/ dir; refusing to write through it", root, d)
		}
	}
	switch kind {
	case "skill":
		wrote, err := writePackArtifact(out, filepath.Join(root, "skills", name), "SKILL.md", skillTemplate(name), "skill", name)
		if wrote {
			fmt.Fprintln(out, "edit it, then commit it to your pack's git repo.")
		}
		return err
	case "knowledge":
		// Embed only: a literal knowledge/ doc, discovered by convention and INERT
		// (mounted like skills/, indexed by nothing).
		_, err := writePackArtifact(out, filepath.Join(root, "knowledge"), name+".md", knowledgeTemplate(name), "knowledge doc", name)
		return err
	case "proxy":
		return addPackProxy(out, root, name, flags.host)
	case "mcp":
		return addPackMCP(env, out, register, root, name, flags.envVar, flags.yes)
	}
	return usagef("pix pack add: unknown kind %q (want: skill, knowledge, proxy, mcp)", kind)
}

// writePackArtifact is the mkdir → no-clobber stat → write → report shape every
// `pack add` kind shares; it reports whether it wrote the file.
func writePackArtifact(out io.Writer, dir, file, body, label, name string) (bool, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	f := filepath.Join(dir, file)
	if _, err := os.Stat(f); err == nil {
		fmt.Fprintf(out, "%s already exists: %s\n", label, f)
		return false, nil
	}
	if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
		return false, err
	}
	fmt.Fprintf(out, "added %s %q: %s\n", label, name, f)
	return true, nil
}

// addPackProxy scaffolds bin/<name> and declares it in pack.toml. A host=true
// wrapper is a Tier-1 facet: it installs only once the BoM gate accepts it at
// `pack use`, and only for `pix host` (never the sandbox).
func addPackProxy(out io.Writer, root, name string, host bool) error {
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	f := filepath.Join(binDir, name)
	if _, err := os.Stat(f); err != nil {
		if err := os.WriteFile(f, []byte(proxyShimTemplate(name)), 0o755); err != nil {
			return err
		}
		fmt.Fprintf(out, "scaffolded proxy wrapper: %s\n", f)
	} else {
		fmt.Fprintf(out, "proxy wrapper already exists: %s\n", f)
	}
	p, err := LoadPack(root)
	if err != nil {
		return err
	}
	found := false
	for i, pr := range p.Manifest.Proxies {
		if pr.Name == name {
			p.Manifest.Proxies[i].Host, found = host, true
			break
		}
	}
	if !found {
		p.Manifest.Proxies = append(p.Manifest.Proxies, PackProxy{Name: name, Host: host})
	}
	if err := WriteManifest(root, p.Manifest); err != nil {
		return err
	}
	fmt.Fprintf(out, "added proxy %q to pack.toml (host=%v)\n", name, host)
	if !host {
		printPackRecreateLine(out)
		return nil
	}
	fmt.Fprintf(out, "host wrapper: review + accept it with `pix pack use %s` (Tier-1 host BoM gate);\n", root)
	fmt.Fprintln(out, "once accepted it installs for `pix host` sessions only (requires host.enabled).")
	return nil
}

// addPackMCP declares an mcp integration and, when this IS the active pack,
// attaches it now. Attaching means the gateway runs its command ON THE HOST, so
// it goes through the SAME Tier-1 gate and commit point as `pack use` — a
// mid-session add is never a cheaper path to host exec. On refusal the
// declaration stays in pack.toml (inert) but NOTHING attaches.
func addPackMCP(env hostenv.Env, out io.Writer, register RegisterFn, root, name, envVar string, yes bool) error {
	p, err := LoadPack(root)
	if err != nil {
		return err
	}
	found := false
	for i, ig := range p.Manifest.Integrations {
		if ig.MCP == name {
			p.Manifest.Integrations[i].Env, found = envVar, true
			break
		}
	}
	if !found {
		p.Manifest.Integrations = append(p.Manifest.Integrations, Integration{Name: name, MCP: name, Env: envVar})
	}
	if err := WriteManifest(root, p.Manifest); err != nil {
		return err
	}
	fmt.Fprintf(out, "added mcp integration %q to pack.toml\n", name)
	// Compare CANONICALIZED paths: root may be relative while cfg.Pack is
	// stored absolute.
	cfg, cerr := config.Load()
	if cerr != nil || CanonicalizePackRoot(cfg.Pack) != CanonicalizePackRoot(root) {
		fmt.Fprintf(out, "activate the pack to attach it to a sandbox:  pix pack use %s\n", root)
		return nil
	}
	store, err := requireTrustStore()
	if err != nil {
		return err
	}
	fingerprint, key, gerr := gatePackHostSurface(env, out, store, p, root, cfg.GogAccount, yes)
	if gerr != nil {
		return fmt.Errorf("%v (declared in pack.toml, but NOT attached)", gerr)
	}
	// Attribution is gated on the AddMCP result, so a pre-existing user-added
	// name is never claimed. Its BASE is the HOST-state record.
	added := cfg.AddMCP(name)
	if added {
		lock := store.activationFor(root)
		if !slices.Contains(lock.MCP, name) {
			lock.MCP = append(lock.MCP, name)
		}
		lock.Remote, lock.Commit = adoptionMarker(store, root, readPackLock(root))
		if err := commitPackActivation(cfg, store, root, lock); err != nil {
			return err
		}
	}
	// Persist the acceptance in HOST state so a later `pack use` of this pack
	// won't re-prompt for what was just accepted here.
	recordPackAcceptance(out, key, root, fingerprint, "", "")
	registerPackMCP(register, cfg, env, out, p, []string{name})
	solicitPackCredentials(env, os.Stdin, out, cli.IsTTY(os.Stdin), p)
	if added {
		printPackRecreateLine(out)
	}
	return nil
}

// registerPackMCP registers names with the sbx gateway. Idempotent, and it runs
// even for a name ALREADY in cfg.MCP: a retry after a failed registration must
// re-register, and a changed gog_account must re-resolve. Best-effort.
func registerPackMCP(register RegisterFn, cfg *config.Config, env hostenv.Env, out io.Writer, p *Info, names []string) {
	if len(names) == 0 {
		return
	}
	if err := register(cfg, env, out, names, launcher.FindHostBinary, packContainerMCP(p)); err != nil {
		fmt.Fprintf(out, "note: mcp registration: %v\n", err)
	}
}

func RunPackLs(out io.Writer) error {
	return packLs(out)
}

func packLs(out io.Writer) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	active := ActivePackRoot(cfg.Pack, "")
	if active == "" {
		fmt.Fprintln(out, "no active pack (`pix pack add skill <name>` to start one, or `pix pack use <path|git-url>`)")
		return nil
	}
	p, err := LoadPack(active)
	if err != nil {
		fmt.Fprintf(out, "pack %s: %v\n", active, err)
		return nil
	}
	fmt.Fprintf(out, "active pack: %s (%s)\n", p.Manifest.Name, p.Root)
	return nil
}

func RunPackShow(env hostenv.Env, out io.Writer, rest []string) error {
	return packShow(env, out, rest)
}

func packShow(env hostenv.Env, out io.Writer, rest []string) error {
	root := packTarget(rest)
	if len(rest) == 0 {
		if cfg, err := config.Load(); err == nil && ActivePackRoot(cfg.Pack, "") != "" {
			root = ActivePackRoot(cfg.Pack, "")
		}
	}
	p, err := LoadPack(root)
	if err != nil {
		return err
	}
	for _, row := range []struct{ label, value string }{
		{"pack", p.Manifest.Name},
		{"root", p.Root},
		{"skills", present(p.SkillsDir)},
		{"knowledge", present(p.KnowledgeDir) + " (inert; not indexed by any service)"},
		{"ollama", p.Manifest.OllamaBridgeModel},
		{"gog", p.Manifest.GogAccount},
		{"memory", p.Manifest.MemoryScope},
		{"capabilities", labelIf(p.CapabilitiesFile, "yes (mounts to ~/.pi/agent/capabilities.json)")},
		{"web search", labelIf(p.WebSearchFile, "yes (mounts to ~/.pi/web-search.json)")},
	} {
		if row.value != "" {
			fmt.Fprintf(out, "%-10s %s\n", row.label+":", row.value)
		}
	}
	if len(p.Manifest.Setup) > 0 {
		fmt.Fprintln(out, "setup:")
		for _, s := range p.Manifest.Setup {
			kind := "optional"
			if s.Required {
				kind = "required"
			}
			fmt.Fprintf(out, "  - %s (%s; %s)\n", s.ID, kind, s.Path)
		}
	}
	if len(p.Manifest.Proxies) > 0 {
		fmt.Fprintln(out, "proxies:")
		for _, pr := range p.Manifest.Proxies {
			kind := "sandbox bin/"
			if pr.Host {
				kind = "HOST (`pix host` only, Tier-1)"
			}
			fmt.Fprintf(out, "  - %s (%s)\n", pr.Name, kind)
		}
	}
	if len(p.Manifest.Integrations) > 0 {
		fmt.Fprintln(out, "integrations:")
		for _, ig := range p.Manifest.Integrations {
			showIntegration(env, out, p, ig)
		}
	}
	return nil
}

// labelIf renders text only when the facet it describes is present.
func labelIf(present, text string) string {
	if present == "" {
		return ""
	}
	return text
}

// showIntegration renders ONE integration line: its registration mode and,
// where the secret comes from op-refs (Manifest containers get theirs
// Docker-side), whether that ref is filled.
func showIntegration(env hostenv.Env, out io.Writer, p *Info, ig Integration) {
	fmt.Fprintf(out, "  - %s", ig.Name)
	if ig.MCP != "" {
		fmt.Fprintf(out, " (mcp: %s)", ig.MCP)
	}
	switch {
	case ig.Manifest != "":
		fmt.Fprintf(out, " — manifest: %s (creds Docker-side)", ig.Manifest)
	case ig.Image != "":
		fmt.Fprintf(out, " — image: %s", ig.Image)
	case ig.URL != "":
		fmt.Fprintf(out, " — url: %s (OAuth host-side)", ig.URL)
	}
	switch {
	case ig.Env == "" || ig.Manifest != "":
	case secret.OpRefFilled(env, ig.Env):
		fmt.Fprintf(out, " — %s ✓", ig.Env)
	case ig.Setup != "":
		fmt.Fprintf(out, "; later: pix setup --pack %s --with %s", sys.ShellQuote(p.Root), sys.ShellQuote(ig.Setup))
	default:
		fmt.Fprintf(out, " — %s ✗ (run: pix secret set %s op://vault/item/field)", ig.Env, ig.Env)
	}
	fmt.Fprintln(out)
}

// solicitPackCredentials, on a TTY, prompts for any pack integration whose op://
// credential ref is missing and writes each accepted ref. No-op off-TTY or
// without op. The pack ships no secret — only the user's own op:// reference.
func solicitPackCredentials(env hostenv.Env, in io.Reader, out io.Writer, tty bool, p *Info) {
	if !tty || in == nil || !secret.OpInstalled(env) {
		return
	}
	var missing []Integration
	for _, ig := range p.Manifest.Integrations {
		if ig.Env == "" || ig.Setup != "" {
			continue
		}
		if !secret.EnvVarNameRe.MatchString(ig.Env) {
			fmt.Fprintf(out, "  (skipping integration %q: invalid env var name %q)\n", ig.Name, ig.Env)
			continue
		}
		if !secret.OpRefFilled(env, ig.Env) {
			missing = append(missing, ig)
		}
	}
	if len(missing) == 0 {
		return
	}
	fmt.Fprintf(out, "\nThis pack uses %d integration(s) needing a 1Password credential.\n", len(missing))
	sc := bufio.NewScanner(in)
	for _, ig := range missing {
		fmt.Fprintf(out, "  %s -> op:// ref for %s (Enter to skip): ", ig.Name, ig.Env)
		if !sc.Scan() {
			return
		}
		ref := secret.NormalizeOpRef(sc.Text())
		if ref == "" {
			continue
		}
		if !strings.HasPrefix(ref, "op://") {
			fmt.Fprintf(out, "    skipped %s: not an op:// ref\n", ig.Env)
			continue
		}
		if err := secret.WriteOpRefQuiet(env, ig.Env, ref); err != nil {
			fmt.Fprintf(out, "    could not save %s: %v\n", ig.Env, err)
			continue
		}
		fmt.Fprintf(out, "    saved %s\n", ig.Env)
	}
}

func RunPackUse(env hostenv.Env, out io.Writer, rest []string, register RegisterFn) error {
	return packUse(env, out, rest, register)
}

// packUse switches the active pack in ONE transaction: resolve the target
// (cloning a git URL), verify its pins, gate its host surface, apply the facet
// set to an in-memory cfg, and commit. On any failure NOTHING is committed.
// Everything after the commit is a best-effort, idempotent side effect.
func packUse(env hostenv.Env, out io.Writer, rest []string, register RegisterFn) error {
	flags, err := parsePackFlags(rest)
	if err != nil {
		return usagef("pix pack use: %v", err)
	}
	if len(flags.positionals) == 0 {
		return usagef("usage: pix pack use [--yes] <path|git-url|default>")
	}
	root, remote, commit, err := resolveUseTarget(env, out, flags.positionals[0])
	if err != nil {
		return err
	}
	p, err := LoadPack(root)
	if err != nil {
		return err
	}
	// Re-hash every SHA-pinned [[bin]] BEFORE the gate even renders: a
	// mismatched pin is refused outright, so the BoM always shows the bytes on
	// disk.
	for _, bn := range p.Manifest.Bins {
		if verr := verifyPackBinSHA(root, bn); verr != nil {
			return fmt.Errorf("pack %s: %v", root, verr)
		}
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// The pack-supplied pack.lock is NEVER trusted for reversibility (see the
	// packLock doc); only its fail-safe adoption marker is read, and it is
	// SCRUBBED on a not-currently-active local-path adoption.
	hint := readPackLock(root)
	if cfg.Pack != root && remote == "" {
		if serr := scrubUntrustedPackLock(root); serr != nil {
			return fmt.Errorf("%v (refusing to adopt with an untrusted %s in place)", serr, PackLockName)
		}
	}
	// The Tier-1 gate runs against TRUSTED HOST STATE, never anything the pack
	// ships.
	store, err := requireTrustStore()
	if err != nil {
		return err
	}
	fingerprint, key, err := gatePackHostSurface(env, out, store, p, root, cfg.GogAccount, flags.yes)
	if err != nil {
		return err
	}
	// `pack use` remains a single-pack switch: multi-pack composition is an
	// explicit `pix setup --pack ... --pack ...` transaction, so applying a
	// one-pack stack is exactly right — it also reverts THIS pack's own prior
	// contribution first, without which a re-activation would claim NOTHING and
	// a field dropped from the manifest would stay live forever.
	records, removedMCP, err := applyPackStack(cfg, store, []*Info{p})
	if err != nil {
		return err
	}
	lock := records[0].lock()
	lock.Remote, lock.Commit = remote, commit
	if lock.Remote == "" {
		// Not cloned THIS activation: keep the marker this pack already carried
		// (a local-path re-activation must not un-adopt it).
		lock.Remote, lock.Commit = adoptionMarker(store, root, hint)
	}
	if err := (packTxn{records: records, lockRoot: root, lock: lock}).commit(cfg); err != nil {
		return err
	}
	recordPackAcceptance(out, key, root, fingerprint, remote, commit)

	// --- post-commit: best-effort side effects (each already idempotent). ---

	if !env.Quiet {
		reportPackActivation(out, cfg, root, removedMCP, lock.MCP)
	}
	registerPackMCP(register, cfg, env, out, p, McpNames(p))
	// Swap the host-exec wrappers NOW: clear the previous activation's, then
	// stage+verify+swap this pack's ACCEPTED set.
	if _, werr := refreshHostPackWrappers(quietly(out, env), cfg, false); werr != nil {
		fmt.Fprintf(out, "note: host wrappers not refreshed: %v\n", werr)
	}
	// Solicit any 1Password creds this pack's reference-only integrations need.
	solicitPackCredentials(env, os.Stdin, out, cli.IsTTY(os.Stdin), p)
	// A knowledge change is daemon-affecting: advise the running serve so the
	// new bundle is indexed.
	service.PropagateConfig(service.DefaultReloader(), quietly(out, env))
	return nil
}

// quietly resolves the stream a best-effort side effect may narrate on.
func quietly(out io.Writer, env hostenv.Env) io.Writer {
	if env.Quiet {
		return io.Discard
	}
	return out
}

// resolveUseTarget maps the `pack use` positional to a local pack root plus its
// clone provenance. "default" is a built-in alias for the default pack root
// (NOT $PWD/default) and "personal" a deprecated alias for it; only the EXACT
// bare token matches, so a real path or URL of that name still resolves as one.
func resolveUseTarget(env hostenv.Env, out io.Writer, arg string) (root, remote, commit string, err error) {
	switch arg = strings.TrimSpace(arg); arg {
	case "default":
		arg = DefaultPackRoot()
	case "personal":
		fmt.Fprintln(out, "pix pack use: \"personal\" is deprecated; use \"default\" instead.")
		arg = DefaultPackRoot()
	}
	if !isPackGitURL(arg) {
		root = expandUser(arg)
		if abs, aerr := filepath.Abs(root); aerr == nil {
			root = abs
		}
		return root, "", "", nil
	}
	if root, err = clonePack(env, out, arg); err != nil {
		return "", "", "", err
	}
	remote, _ = parsePackURL(arg)
	if sha, cerr := env.Run("git", "-C", root, "rev-parse", "HEAD"); cerr == nil {
		commit = strings.TrimSpace(sha)
	}
	return root, remote, commit, nil
}

// adoptionMarker resolves the fail-safe adoption marker to carry forward: HOST
// state first, the pack's own lock only as a hint (a forged one only RESTRICTS).
func adoptionMarker(store *PackTrustStore, root string, hint packLock) (remote, commit string) {
	if prov, ok := store.Adopted[CanonicalizePackRoot(root)]; ok {
		return prov.Remote, prov.Commit
	}
	return strings.TrimSpace(hint.Remote), strings.TrimSpace(hint.Commit)
}

// reportPackActivation summarizes the swap. Only MCP names that STAYED out are
// reported as detached: a reactivation removes and immediately re-adds every
// still-declared entry.
func reportPackActivation(out io.Writer, cfg *config.Config, root string, removedMCP, addedMCP []string) {
	fmt.Fprintf(out, "active pack -> %s\n", root)
	var detached []string
	for _, m := range removedMCP {
		if !slices.Contains(cfg.MCP, m) {
			detached = append(detached, m)
		}
	}
	if len(detached) > 0 {
		fmt.Fprintf(out, "detached mcp (previous activation): %s\n", strings.Join(detached, ", "))
	}
	if len(addedMCP) > 0 {
		fmt.Fprintf(out, "attached mcp: %s\n", strings.Join(addedMCP, ", "))
	}
	// --mcp/--kit are create-only, so the recreate line is UNCONDITIONAL: the
	// sandbox-facet-changing case is never silently skipped.
	printPackRecreateLine(out)
}

func RunPackRm(out io.Writer, rest []string) error {
	if len(rest) > 0 {
		return usagef("pix pack rm: unexpected argument %q (rm detaches the ACTIVE pack; it takes no name)", rest[0])
	}
	detached, err := packRm(out)
	if err != nil {
		return err // a failed detach reports nothing: the ledger is untouched
	}
	reportPackDetach(out, detached)
	return nil
}

// packDetach is what `pack rm` undid; nil means there was no active pack.
type packDetach struct {
	root     string
	wrappers []string
	mcp      []string
}

// packRm detaches the active pack. The ENTIRE detach — re-reading cfg and the
// store, clearing the wrappers, reverting the contribution set, cfg.Save,
// dropping the spent activation — runs under ONE hold of the trust lock:
// deciding from a PRE-lock snapshot let a concurrent refresh install AFTER
// rm reported "detached".
func packRm(out io.Writer) (*packDetach, error) {
	var detached *packDetach
	err := withPackTrustLock(func() error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		if cfg.Pack == "" {
			return nil
		}
		// `rm` must undo the active pack's contributions, not just clear
		// cfg.Pack, or "detached" is a lie.
		store, serr := requireTrustStore()
		if serr != nil {
			return serr
		}
		d := &packDetach{root: cfg.Pack}
		// "detached" includes the host wrappers: remove exactly what HOST state
		// attributes to hostPackBinDir() (works even when the pack dir is gone),
		// FIRST — a failed clear aborts BEFORE anything detaches.
		if store.Installed != nil && len(store.Installed.Wrappers) > 0 {
			d.wrappers = append([]string(nil), store.Installed.Wrappers...)
			if cerr := clearInstalledHostPackWrappersLocked(out, store); cerr != nil {
				return fmt.Errorf("stale host wrappers could not be removed: %v — nothing detached; fix that and re-run", cerr)
			}
		}
		d.mcp = revertPackStack(cfg, store, ActivePackRoots(cfg, ""))
		ClearPackInference(cfg, "")
		cfg.Pack, cfg.Packs = "", nil
		if err := cfg.Save(); err != nil {
			return err
		}
		detached = d
		// The ledger is spent (its contributions were just reverted). The lock
		// is HELD, so use the already-locked mutation — never nest
		// withPackTrustLock. A failed write merely over-claims (a no-op removal).
		if len(store.Activations) > 0 {
			if _, werr := mutatePackTrustStoreLocked(func(s *PackTrustStore) error {
				s.Activations = nil
				return nil
			}); werr != nil {
				fmt.Fprintf(out, "note: could not clear the activation record: %v (harmless over-claim; re-run `pack rm` once %s is writable)\n", werr, packTrustStorePath())
			}
		}
		return nil
	})
	return detached, err
}

func reportPackDetach(out io.Writer, d *packDetach) {
	if d == nil {
		fmt.Fprintln(out, "no active pack to detach")
		return
	}
	fmt.Fprintf(out, "detached active pack (%s). The files are untouched; re-attach with `pix pack use`.\n", d.root)
	if len(d.wrappers) > 0 {
		fmt.Fprintf(out, "removed host wrappers: %s\n", strings.Join(d.wrappers, ", "))
	}
	if len(d.mcp) > 0 {
		fmt.Fprintf(out, "detached mcp: %s\n", strings.Join(d.mcp, ", "))
		printPackRecreateLine(out)
	}
}

// --- git-URL adoption -------------------------------------------------------

// isPackGitURL classifies s as a git URL (cloneable) and additionally accepts
// the "git+" scheme prefix used by kit URLs.
func isPackGitURL(s string) bool {
	s = strings.TrimSpace(s)
	// A git transport-helper string (ext::, fd::, ...) is URL-SHAPED, not a path:
	// routing it here gets a clear "unsafe transport" rejection from safeGitURL
	// instead of a confusing "not a pack" error.
	if strings.Contains(s, "::") {
		return true
	}
	switch {
	case strings.HasPrefix(s, "git+"),
		strings.HasPrefix(s, "http://"),
		strings.HasPrefix(s, "https://"),
		strings.HasPrefix(s, "git://"),
		strings.HasPrefix(s, "ssh://"),
		strings.HasPrefix(s, "git@"):
		return true
	case strings.HasSuffix(s, ".git"):
		return true
	}
	return false
}

// parsePackURL splits an optional "#ref=<ref>" (or bare "#<ref>") pin off a git
// URL and strips a leading "git+" scheme prefix. Returns (url, ref).
func parsePackURL(raw string) (url, ref string) {
	url = strings.TrimPrefix(raw, "git+")
	if i := strings.IndexByte(url, '#'); i >= 0 {
		frag := url[i+1:]
		url = url[:i]
		ref = strings.TrimPrefix(frag, "ref=")
	}
	return url, ref
}

// packNameFromURL derives a SAFE, stable local dir name from a git URL: the
// basename (minus .git) sanitized to [A-Za-z0-9._-], plus a short hash of the
// FULL url so two remotes sharing a basename never collide on one dest.
func packNameFromURL(url string) string {
	u := strings.TrimSuffix(url, ".git")
	u = strings.TrimRight(u, "/")
	base := u
	if i := strings.LastIndexAny(u, "/:"); i >= 0 {
		base = u[i+1:]
	}
	safe := make([]rune, 0, len(base))
	for _, r := range base {
		if !safeArtifactRune(r) {
			r = '-'
		}
		safe = append(safe, r)
	}
	name := strings.Trim(string(safe), ".-")
	if name == "" || name == ".." {
		name = "pack"
	}
	sum := sha256.Sum256([]byte(url))
	return name + "-" + hex.EncodeToString(sum[:])[:16]
}

// clonePack clones (or updates) a remote pack into PacksDir/<name>, pinned to
// the optional ref, and returns the local path. The git remote is trusted for
// Tier-0 content; anything host-executing is gated separately at adoption.
func clonePack(env hostenv.Env, out io.Writer, raw string) (string, error) {
	url, ref := parsePackURL(raw)
	if !cli.SafeGitURL(url) {
		return "", fmt.Errorf("refusing unsafe git URL %q (only https/ssh/git remotes; no ext::/file:: transports)", url)
	}
	if ref != "" && strings.HasPrefix(ref, "-") {
		return "", fmt.Errorf("refusing ref %q (leading dash)", ref)
	}
	name := packNameFromURL(url)
	dest := filepath.Join(config.PacksDir(), name)
	if err := os.MkdirAll(config.PacksDir(), 0o755); err != nil {
		return "", err
	}
	freshClone := false
	if _, err := os.Stat(filepath.Join(dest, ".git")); err == nil {
		// A dir already exists at this URL-hash dest: verify its origin first — a
		// collision (or pre-planted dir) must never activate the wrong repo.
		if got, _ := env.Run("git", "-C", dest, "remote", "get-url", "origin"); strings.TrimSpace(got) != url {
			_ = os.RemoveAll(dest)
		} else {
			fmt.Fprintf(out, "updating pack %q...\n", name)
			if _, err := env.Run("git", "-C", dest, "fetch", "--tags", "--", "origin"); err != nil {
				return "", fmt.Errorf("git fetch %s: %w", url, err)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(dest, ".git")); err != nil {
		fmt.Fprintf(out, "cloning pack %q from %s...\n", name, url)
		if _, err := env.Run("git", "clone", "--", url, dest); err != nil {
			return "", fmt.Errorf("git clone %s: %w", url, err)
		}
		freshClone = true
	}
	if ref != "" {
		// No `--` before a ref: `git checkout -- <ref>` means path-checkout, not a
		// ref switch. ref is already validated (no leading dash), so this is safe.
		if _, err := env.Run("git", "-C", dest, "checkout", ref); err != nil {
			if freshClone {
				_ = os.RemoveAll(dest)
			}
			return "", fmt.Errorf("git checkout %s: %w", ref, err)
		}
		// Advance to the fetched tip when ref is a branch (no-op for a tag/sha).
		_, _ = env.Run("git", "-C", dest, "reset", "--hard", "origin/"+ref)
	} else if !freshClone {
		// Unpinned existing clone: advance to the remote default branch's tip.
		_, _ = env.Run("git", "-C", dest, "reset", "--hard", "@{upstream}")
	}
	// A clone that has no pack.toml is not a pack: clean up the fresh clone so a
	// retry starts clean, and fail with a clear message.
	if _, err := os.Stat(filepath.Join(dest, PackManifestName)); err != nil {
		if freshClone {
			_ = os.RemoveAll(dest)
		}
		return "", fmt.Errorf("cloned %s but it has no %s — not a pack", url, PackManifestName)
	}
	// pack.lock is LOCAL GENERATED state and must NEVER come from the remote.
	// Scrub AFTER every git op, BEFORE markPackAdopted; a failed scrub fails
	// the adoption.
	if err := scrubRemotePackLock(env, dest, freshClone); err != nil {
		if freshClone {
			_ = os.RemoveAll(dest)
		}
		return "", err
	}
	// Mark the clone ADOPTED durably before returning: an UNMARKED clone would be
	// treated as user-authored on retry, so an unwritable marker fails adoption.
	if err := markPackAdopted(env, dest, url); err != nil {
		if freshClone {
			_ = os.RemoveAll(dest)
		}
		return "", fmt.Errorf("recording adoption provenance for %s: %w", url, err)
	}
	return dest, nil
}

// scrubRemotePackLock deletes a pack.lock that came from the REMOTE — a
// checked-in symlink would redirect the adoption marker at an arbitrary host
// file, and a regular one could claim the user's own entries. On a fresh clone
// any lock did; on an update, one that is a symlink (never legitimate) or that
// git tracks. A legit LOCAL lock (untracked regular file) is preserved.
func scrubRemotePackLock(env hostenv.Env, dest string, freshClone bool) error {
	path := PackLockPath(dest)
	fi, err := os.Lstat(path)
	if err != nil {
		return nil // no pack.lock at all — nothing to scrub
	}
	fromRemote := freshClone || fi.Mode()&os.ModeSymlink != 0
	if !fromRemote {
		// Tracked by git => restored from the remote by checkout/reset above.
		if _, lerr := env.Run("git", "-C", dest, "ls-files", "--error-unmatch", "--", PackLockName); lerr == nil {
			fromRemote = true
		}
	}
	if !fromRemote {
		return nil
	}
	if rerr := os.Remove(path); rerr != nil && !os.IsNotExist(rerr) {
		return fmt.Errorf("removing checked-in %s: %w", PackLockName, rerr)
	}
	return nil
}

// markPackAdopted durably records adoption provenance (Remote + Commit),
// MERGING into any existing lock so a re-clone never sheds earlier attribution.
// The trust-store mirror is best-effort: the lock marker plus the PacksDir check
// keep the guard fail-safe.
func markPackAdopted(env hostenv.Env, root, remote string) error {
	lock := readPackLock(root)
	lock.Remote = remote
	lock.Commit = ""
	if sha, err := env.Run("git", "-C", root, "rev-parse", "HEAD"); err == nil {
		lock.Commit = strings.TrimSpace(sha)
	}
	_ = recordPackAdoptionInTrustStore(root, remote, lock.Commit)
	return writePackLock(root, lock)
}

// --- helpers ----------------------------------------------------------------

// WriteManifest writes root's pack.toml symlink-safe + atomically: the pack
// root is untrusted input, so a symlinked destination is REFUSED.
func WriteManifest(root string, m Manifest) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(m); err != nil {
		return err
	}
	dest := filepath.Join(root, PackManifestName)
	if fi, err := os.Lstat(dest); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to write through it", dest)
	}
	return sys.AtomicWriteInDir(root, PackManifestName, buf.Bytes(), 0o644)
}

// seedPackGitignore appends a `pack.lock` line to <root>/.gitignore so a fresh
// pack never commits its generated lockfile. Idempotent, best-effort, and
// symlink-safe: `pack new .` can run in an UNTRUSTED directory where .gitignore
// may point at e.g. ~/.bashrc.
func seedPackGitignore(root string) {
	path := filepath.Join(root, ".gitignore")
	const line = PackLockName
	if isSymlinkPath(path) {
		return // never read or write through a symlinked .gitignore
	}
	b, err := os.ReadFile(path)
	if err == nil && strings.Contains(string(b), line) {
		return // already present
	}
	content := string(b)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += line + "\n"
	_ = sys.AtomicWriteInDir(root, ".gitignore", []byte(content), 0o644)
}

func present(p string) string {
	if p == "" {
		return "(none)"
	}
	return p
}

func skillTemplate(name string) string {
	return "---\nname: " + name + "\ndescription: \"TODO: when should this skill fire? One sentence.\"\n---\n# " + name + "\n\nTODO: tight, opinionated steps.\n"
}

func knowledgeTemplate(name string) string {
	return "# " + name + "\n\nTODO: durable, shared domain knowledge (what is X and why), not a personal preference.\n"
}
