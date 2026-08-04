// pack.go implements `pix pack` — the git-backed context bundle (skills +
// knowledge + later mcp/proxies/routing/config). See docs/design/packs.md.
//
// v1 (this file): the local, Tier-0 slice — pack.toml manifest, new (adopt an
// existing repo or git-init a fresh one), add skill|knowledge, ls, show, use, rm.
// No host execution, no git-URL adoption yet, no profile deletion; those are
// later increments. All OS/git calls go through defaultShellEnv so the logic is
// testable with fakes.
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
	"pix/host/knowledge"
	"pix/host/launcher"
	"pix/host/routing"
	"pix/host/secret"
	"pix/host/service"
	"pix/host/sys"
	"pix/host/workspace"

	"github.com/BurntSushi/toml"
)

// Manifest is pack.toml. Identity + model prefs (v1), plus the v2 facets:
// F1 integrations attach (unchanged shape, now enabling), F2/F3 proxy wrappers,
// the (struct-only, P2) external-binary facet, F4 config layering (gog_account/
// memory_scope/routing), and F6 knowledge references. Skills and embedded
// knowledge are still discovered by convention (skills/, knowledge/), so a pack
// does not have to enumerate them.
type Manifest struct {
	Name              string `toml:"name"`
	Schema            int    `toml:"schema"`
	OllamaBridgeModel string `toml:"ollama_bridge_model,omitempty"`
	// GogAccount, when set, is layered into cfg.GogAccount on `pack use` (F4).
	GogAccount string `toml:"gog_account,omitempty"`
	// MemoryScope tags in-VM memory recall/capture (F4); default = the pack Name.
	// "default" (or the pack's own default) selects the shared/unscoped tag.
	MemoryScope string `toml:"memory_scope,omitempty"`
	// Prerequisites are pack-authored, human-readable conditions shown on the
	// adoption screen before any setup hook runs. They describe external state
	// the user must bring; executable checks remain in [[setup]].
	Prerequisites []string `toml:"prerequisites,omitempty"`
	// Routing is a STRUCT-ONLY placeholder for a pack-level routing override
	// (packs-v2-impl.md §2: "optional/stretch"). Nothing reads it in Phase 1.
	Routing      *packRouting  `toml:"routing,omitempty"`
	Integrations []Integration `toml:"integrations,omitempty"`
	// Proxies are [[proxy]] entries: F2 in-sandbox bin/ wrappers (Host unset/
	// false) and F3 host-mode wrappers (Host true, struct carried but NOT
	// installed/PATH-wired until Phase 2 — no host exec in this build).
	Proxies []PackProxy `toml:"proxy,omitempty"`
	// Bins are [[bin]] external host binaries (Tier-1, SHA-pinned). STRUCT ONLY
	// in Phase 1: LoadPack validates the shape (fail-closed on a missing sha) but
	// nothing executes one — that is the P2 trust-gated host-exec path.
	Bins []packBin `toml:"bin,omitempty"`
	// Knowledge are [[knowledge]] references (F6): shared=true travels (a git
	// URL an adopter pulls), shared=false does not (a local path, standalone).
	Knowledge []packKnowledge `toml:"knowledge,omitempty"`
	// Setup steps let a pack contribute resumable host onboarding to `pix
	// setup --pack`. Each step is a repo-relative executable with a read-only
	// probe and an idempotent apply action. The script bytes and argv are part of
	// the Tier-1 host-exec fingerprint.
	Setup []packSetupStep `toml:"setup,omitempty"`
	// Inference lets a pack participate declaratively in setup before any user
	// prompt. A private pack can provide an authenticated gateway and make it
	// exclusive without adding its endpoint or aliases to public Pix.
	Inference *Inference `toml:"inference,omitempty"`
	// Services are [[services]] entries (U08a, AC-PACK-02/AC-SUP-05): the SOLE
	// declaration of a long-running external service unit. Declaration-only in
	// this build — validated fail-closed at load, Tier-1 gated and fully
	// fingerprinted, consumed by no supervisor yet. See service.go.
	Services []packService `toml:"services,omitempty"`
}

// ApplyPackInference projects a pack's declarative inference contract into
// launcher config. It intentionally copies only public wiring metadata; secret
// values are impossible in this schema. Probe evidence starts false and is
// earned later by setup.
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
	// Reapplying an unchanged active pack at launch must not erase the
	// availability evidence setup just earned. Preserve it only across an exact
	// backend + binding match; any endpoint/protocol/auth/upstream change starts
	// unverified again.
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

// packRouting is the struct-only pack-level routing override placeholder
// (packs-v2-impl.md §2). Repo-relative paths; nothing wires them in Phase 1.
type packRouting struct {
	Policy    string `toml:"policy,omitempty"`
	Scorecard string `toml:"scorecard,omitempty"`
}

// PackProxy is one [[proxy]] entry: a bin/<name> wrapper script. Host=false
// (default) is an F2 in-sandbox wrapper, synthesized into an ephemeral mixin
// kit at launch (SynthesizePackKit). Host=true is an F3 host-mode wrapper —
// carried by the schema in Phase 1, but installation/PATH-wiring is P2.
type PackProxy struct {
	Name   string   `toml:"name"`
	Host   bool     `toml:"host,omitempty"`
	Egress []string `toml:"egress,omitempty"`
}

// packBin is one [[bin]] entry: an external, SHA-pinned host binary (Tier-1,
// rare, P2). LoadPack fails closed on an empty SHA (never reaches an exec path
// unpinned) even though Phase 1 never executes one.
type packBin struct {
	Name string `toml:"name"`
	Path string `toml:"path"`
	SHA  string `toml:"sha"`
	Host bool   `toml:"host,omitempty"`
}

// packKnowledge is one [[knowledge]] entry (F6): a reference to a bundle beyond
// the pack's own embedded knowledge/ dir. Shared=true travels with the pack (a
// git URL adopters pull); Shared=false does not (a local path, standalone —
// deliberately NOT repo-root-scoped, since pointing outside the pack is the
// entire point of a private reference).
type packKnowledge struct {
	Name   string `toml:"name"`
	Source string `toml:"source"`
	Shared bool   `toml:"shared,omitempty"`
}

// Integration is a REFERENCE-ONLY integration (v1): the pack says "I use
// <mcp> and need the credential <env>". It ships NO executable code — the MCP
// server is host-provided (gog, a gateway-catalog server), and the credential is
// solicited as an op:// ref the user owns. Pack-SHIPPED executables are v2.
type Integration struct {
	Name string `toml:"name"`          // human label
	Env  string `toml:"env,omitempty"` // op-refs.env ENV VAR the credential lives under
	MCP  string `toml:"mcp,omitempty"` // MCP server name to attach (host-provided)
	// Manifest, when set, makes this a CONTAINER integration: an OCI-packaged
	// stdio MCP server the sbx gateway runs on the host via Docker. The value is a
	// server-manifest URL (server.json/server.yaml, e.g. a GitHub raw or internal
	// HTTP URL) or a registry ref that `sbx mcp add --local --url <manifest>`
	// accepts. Registration uses that form (NOT the host `--command` path), so no
	// pix-host recompile or private build is needed; the container's
	// credentials are provided Docker-side (declared in its server.json), not via
	// the op-run wrapper. Leave empty for a host-provided server (slack, gog) or a
	// remote gateway-catalog server.
	Manifest string `toml:"manifest,omitempty"`
	// Image, when set, is a CONTAINER integration registered by DIRECT image ref:
	// pix registers it as `docker run -i --rm -e <KEY>… <image>`, op-run
	// wrapped exactly like slack (so its creds resolve from 1Password at gateway
	// spawn and forward into the container via `-e`). Simpler than Manifest — no
	// server.json to host: a locally-built image tag works, and a registry only
	// matters when you share it. Mutually exclusive with Manifest.
	Image string `toml:"image,omitempty"`
	// EnvKeys are ADDITIONAL (typically non-secret) env var names forwarded into an
	// Image container via `-e <KEY>` (e.g. HR_TENANT). The primary
	// op-refs-backed secret goes in Env (also forwarded, and warned about if unset).
	EnvKeys []string `toml:"env_keys,omitempty"`
	// EnvValues are non-secret literal environment values baked into a pack's
	// container command (for example, a company-wide tenant name). Secrets must
	// use Env/op:// instead and are rejected here when they look secret-shaped.
	EnvValues map[string]string `toml:"env_values,omitempty"`
	// URL, when set, makes this a REMOTE integration the pack registers ITSELF:
	// `pack use` runs `sbx mcp add <mcp> --url <url>` so the pack's remote
	// gateway-catalog servers are wired without
	// a manual `pix mcp bundle` + `sbx mcp add`. The URL is a remote MCP
	// endpoint (https://host/mcp); OAuth is discovered + handled host-side by the
	// gateway on first use (no credential in the pack). Mutually exclusive with
	// Manifest/Image. Leave empty for a server the user registers out-of-band.
	URL string `toml:"url,omitempty"`
	// Setup links this integration to an optional [[setup]] hook. Pack
	// activation registers it but does not solicit its credential up front.
	Setup string `toml:"setup,omitempty"`
}

// Info is a resolved pack on disk.
type Info struct {
	Root         string
	Manifest     Manifest
	SkillsDir    string // <root>/skills if it exists, else ""
	KnowledgeDir string // <root>/knowledge if it exists, else ""
	BinDir       string // <root>/bin if it exists, else "" (F2/F3 proxy wrapper scripts)
	// CapabilitiesFile is <root>/capabilities.json if it exists (a regular file),
	// else "". Mounted into the sandbox at ~/.pi/agent/capabilities.json via the
	// synthesized mixin kit so a pack carries its own capability->provider routing.
	CapabilitiesFile string
	// WebSearchFile is <root>/web-search.json when present. It is mounted at
	// ~/.pi/web-search.json (pi-web-access's sandbox config path) so a pack can
	// route discovery through its private inference gateway without putting that
	// endpoint in Pix.
	WebSearchFile string
}

const PackManifestName = "pack.toml"

// ErrNotAPack is the sentinel LoadPack wraps when root is not a pack AT ALL
// (no pack.toml, including the root directory itself being gone). Callers use
// errors.Is to distinguish this "genuinely absent" class — safe to degrade on
// (e.g. a stale cfg.Pack pointing at a deleted dir) — from every OTHER load
// error (symlink rejection, facet validation, parse failure), which means a
// pack that EXISTS but is broken or tampered and must fail closed.
var ErrNotAPack = errors.New("not a pack")

// LoadPack reads a pack from a directory. A missing pack.toml is an error (the
// presence of pack.toml is the entire "is this a pack" test), wrapped around
// ErrNotAPack so callers can tell "absent" from "broken".
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
	if d := filepath.Join(root, "skills"); sys.DirHasEntries(d) {
		if isSymlinkPath(d) {
			return nil, fmt.Errorf("pack %s: skills/ is a symlink; refusing to mount", root)
		}
		if has, bad := dirHasSymlink(d); has {
			return nil, fmt.Errorf("pack %s: skills/ contains a symlink (%s); packs must not use symlinks, refusing to mount", root, bad)
		}
		p.SkillsDir = d
	}
	if d := filepath.Join(root, "knowledge"); sys.DirHasEntries(d) {
		if isSymlinkPath(d) {
			return nil, fmt.Errorf("pack %s: knowledge/ is a symlink; refusing to mount", root)
		}
		if has, bad := dirHasSymlink(d); has {
			return nil, fmt.Errorf("pack %s: knowledge/ contains a symlink (%s); packs must not use symlinks, refusing to mount", root, bad)
		}
		p.KnowledgeDir = d
	}
	if d := filepath.Join(root, "bin"); sys.DirHasEntries(d) {
		if isSymlinkPath(d) {
			return nil, fmt.Errorf("pack %s: bin/ is a symlink; refusing to mount", root)
		}
		if has, bad := dirHasSymlink(d); has {
			return nil, fmt.Errorf("pack %s: bin/ contains a symlink (%s); packs must not use symlinks, refusing to mount", root, bad)
		}
		p.BinDir = d
	}
	if f := filepath.Join(root, "capabilities.json"); sys.IsRegularFile(f) {
		if isSymlinkPath(f) {
			return nil, fmt.Errorf("pack %s: capabilities.json is a symlink; refusing to mount", root)
		}
		p.CapabilitiesFile = f
	}
	if f := filepath.Join(root, "web-search.json"); sys.IsRegularFile(f) {
		if isSymlinkPath(f) {
			return nil, fmt.Errorf("pack %s: web-search.json is a symlink; refusing to mount", root)
		}
		b, readErr := os.ReadFile(f)
		if readErr != nil {
			return nil, fmt.Errorf("pack %s: reading web-search.json: %w", root, readErr)
		}
		var value any
		if len(b) > 64*1024 || json.Unmarshal(b, &value) != nil {
			return nil, fmt.Errorf("pack %s: web-search.json must be valid JSON no larger than 64 KiB", root)
		}
		if _, ok := value.(map[string]any); !ok {
			return nil, fmt.Errorf("pack %s: web-search.json must contain a JSON object", root)
		}
		p.WebSearchFile = f
	}
	if err := validatePackFacets(root, &m); err != nil {
		return nil, err
	}
	return p, nil
}

// validatePackFacets hardens the v2 typed facets at load time (fail closed,
// same posture as the existing skills/knowledge symlink checks): every
// proxy/bin/knowledge Name must be a safe artifact name (reusing
// safeArtifactName); every [[bin]].Path must be a repo-relative path that does
// not escape the pack root and is not a symlink; every [[bin]] MUST carry a
// non-empty SHA (an external binary is never registered unpinned — P2 never
// reaches an exec path for one that failed to load). [[knowledge]].Source is
// deliberately NOT root-scoped: a shared=false reference pointing OUTSIDE the
// pack (e.g. ~/notes/okf) is the entire point of a private reference (F6).
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
	for _, k := range m.Knowledge {
		if !safeArtifactName(k.Name) {
			return fmt.Errorf("pack %s: [[knowledge]] name %q is invalid (letters, digits, -, _, . only; no path separators)", root, k.Name)
		}
		if strings.TrimSpace(k.Source) == "" {
			return fmt.Errorf("pack %s: [[knowledge]] %q has no source", root, k.Name)
		}
		// CRITICAL (finding A): the shared flag MUST match the source's CLASS.
		// shared=true ("travels") REQUIRES a git URL — resolved through the
		// safeGitURL-gated clone path; shared=false ("private") REQUIRES a
		// local path. Without this, an adopted pack could declare shared=true
		// with a LOCAL path (e.g. "/etc" or ~/.ssh) and bypass the adopted-pack
		// guard in resolvePackKnowledgeRef, which the flag alone used to key —
		// host-file disclosure via the knowledge index. Fail closed at load.
		if k.Shared && !knowledgeSourceIsGitURL(k.Source) {
			return fmt.Errorf("pack %s: [[knowledge]] %q: shared=true requires a git URL source (got local path %q); use shared=false for a local path", root, k.Name, k.Source)
		}
		if !k.Shared && knowledgeSourceIsGitURL(k.Source) {
			return fmt.Errorf("pack %s: [[knowledge]] %q: shared=false (private) requires a local path source (got URL %q); use shared=true for a git URL", root, k.Name, k.Source)
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

// validateNoSymlinkComponents rejects a symlink at any component beneath root.
// Lstat on only the leaf is insufficient because the OS follows intermediate
// directory symlinks before inspecting the final file.
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

// knowledgeSourceIsGitURL classifies a [[knowledge]].Source as git-URL-shaped
// (cloneable — including transport-helper strings like ext::/fd:: that must
// route through the safeGitURL rejection rather than be mistaken for a local
// path) vs a local path. The finding-A security guards key on this CLASS,
// never on the manifest's shared flag, which an attacker controls.
func knowledgeSourceIsGitURL(source string) bool {
	source = strings.TrimSpace(source)
	return knowledge.IsGitURL(source) || strings.Contains(source, "::")
}

// validateRepoRelativePath rejects a [[bin]].Path that is empty, absolute, that
// escapes root via `..`, or that resolves to a symlink — mirroring the
// skills/knowledge symlink posture. rel MUST be repo-relative (packs-v2-impl.md
// §2: "path = bin/fastmail-mcp").
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

// dirHasSymlink walks dir and reports the first symlink of ANY kind. Adopted
// packs have no legitimate need for symlinks, and rejecting all of them is the
// only complete defense: WalkDir does NOT descend into a symlinked DIRECTORY, so
// an "escaping vs not" test can be masked (skills/sub -> ../masked, then
// masked/x -> /etc). Rejecting the link entry itself (which WalkDir DOES visit)
// closes that bypass. Returns (true, path) on the first symlink found.
// isSymlinkPath reports whether path itself is a symlink (Lstat, no follow).
func isSymlinkPath(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

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

func ActivePackRoots(cfg *config.Config, override string) []string {
	if strings.TrimSpace(override) != "" {
		return []string{expandUser(strings.TrimSpace(override))}
	}
	var roots []string
	seen := map[string]bool{}
	if cfg != nil {
		for _, root := range cfg.Packs {
			root = expandUser(strings.TrimSpace(root))
			if root != "" && !seen[root] {
				seen[root] = true
				roots = append(roots, root)
			}
		}
		root := expandUser(strings.TrimSpace(cfg.Pack))
		if root != "" && !seen[root] {
			roots = append(roots, root)
		}
	}
	return roots
}

// PersistPackStack composes every declared config facet after each pack has
// independently passed adoption and trust checks. Collections are unions;
// scalar declarations are applied in command order (last declaration wins).
// Ownership is recorded per pack in host state so a later switch/rm removes
// only entries the stack actually added and restores scalar values in reverse.
func PersistPackStack(roots []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	store, err := loadPackTrustStore()
	if err != nil {
		return fmt.Errorf("pack trust state unreadable: %w", err)
	}
	records, err := composePackStack(cfg, store, roots)
	if err != nil {
		return err
	}
	return commitPackStack(cfg, records)
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

func composePackStack(cfg *config.Config, store *PackTrustStore, roots []string) ([]packActivationRecord, error) {
	// Repeating the same pack does not create a second ownership layer. Without
	// this normalization both records have the same identity, making a later
	// reverse lookup unable to distinguish their scalar restore chain.
	roots = UniquePackRoots(roots)

	// The setup adoption loop ends with the last pack active. Return to the
	// pre-stack baseline, then apply the whole ordered stack once. Reversing the
	// ownership ledger is what makes scalar Prior* chains unwind correctly.
	priorRoots := ActivePackRoots(cfg, "")
	for i := len(priorRoots) - 1; i >= 0; i-- {
		revertPackPriorContribution(cfg, store.activationFor(priorRoots[i]))
	}
	ClearPackInference(cfg, "")

	cfg.Packs = append([]string(nil), roots...)
	if len(roots) > 0 {
		cfg.Pack = roots[len(roots)-1]
	} else {
		cfg.Pack = ""
	}
	var records []packActivationRecord
	for _, root := range roots {
		p, err := LoadPack(root)
		if err != nil {
			return nil, err
		}
		lock := packLock{}
		for _, name := range McpNames(p) {
			if cfg.AddMCP(name) {
				lock.MCP = append(lock.MCP, name)
			}
		}
		if p.KnowledgeDir != "" {
			if cfg.AddKnowledgeBundle(p.KnowledgeDir) {
				lock.Knowledge = append(lock.Knowledge, knowledge.CanonicalizeKnowledgeBundle(p.KnowledgeDir))
			}
			cfg.AddService("knowledge")
		}
		adopted := isAdoptedPack(root)
		if store != nil {
			_, adopted = store.Adopted[CanonicalizePackRoot(root)]
			adopted = adopted || isAdoptedPack(root)
		}
		for _, k := range p.Manifest.Knowledge {
			resolved, rerr := resolvePackKnowledgeRef(io.Discard, root, adopted, k)
			if rerr != nil {
				if errors.Is(rerr, errPrivateRefSkippedAdopted) {
					continue
				}
				// Match ordinary pack activation: one bad optional ref does not
				// discard the rest of an otherwise usable pack.
				continue
			}
			if cfg.AddKnowledgeBundle(resolved) {
				lock.Knowledge = append(lock.Knowledge, knowledge.CanonicalizeKnowledgeBundle(resolved))
			}
			cfg.AddService("knowledge")
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
		// Exclusive policy is an ordered scalar, not an additive facet. A
		// later pack that explicitly declares non-exclusive inference clears
		// an earlier pack's exclusivity (last writer wins).
		if p.Manifest.Inference != nil && !p.Manifest.Inference.Exclusive {
			cfg.Inference.ExclusiveSource = ""
		}
		if err := ApplyPackInference(cfg, p.Manifest.Inference, root); err != nil {
			return nil, err
		}
		records = append(records, store.newActivationRecord(root, lock))
	}
	// De-duplicate bindings by (model,backend), preserving the last declaration
	// in stack order so a later pack can replace an upstream alias.
	seen := map[string]bool{}
	var bindings []config.InferenceModelBinding
	for i := len(cfg.Inference.Models) - 1; i >= 0; i-- {
		b := cfg.Inference.Models[i]
		key := b.Model + "\x00" + b.Backend
		if seen[key] {
			continue
		}
		seen[key] = true
		bindings = append(bindings, b)
	}
	for i, j := 0, len(bindings)-1; i < j; i, j = i+1, j-1 {
		bindings[i], bindings[j] = bindings[j], bindings[i]
	}
	cfg.Inference.Models = bindings
	return records, nil
}

// commitPackStack keeps the host-owned ownership ledger and config on the same
// safe side of a two-file commit. A normal config-save error restores the prior
// ledger; only a hard kill can leave a harmless over-claim.
func commitPackStack(cfg *config.Config, records []packActivationRecord) error {
	return withPackTrustLock(func() error {
		store, err := loadPackTrustStore()
		if err != nil {
			return err
		}
		priorSingle := store.Activation
		priorStack := append([]packActivationRecord(nil), store.Activations...)
		store.setActivationStack(records)
		if err := store.Save(); err != nil {
			return err
		}
		if err := cfg.Save(); err != nil {
			store.Activation = priorSingle
			store.Activations = priorStack
			if rollbackErr := store.Save(); rollbackErr != nil {
				return fmt.Errorf("saving config: %v (ownership rollback failed: %v)", err, rollbackErr)
			}
			return fmt.Errorf("saving config: %w", err)
		}
		return nil
	})
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

// DefaultPackRoot is the default-pack location: the "default" directory under
// the pix data dir. Resolution only — it creates nothing, migrates nothing and
// never rewrites cfg.Pack. There is no legacy-path discovery here: the 0.1.0
// rename was a clean pre-launch cutover, so the pre-public "pack"/"personal"
// directory names were never written by any released build. (The BARE token
// `pix pack use personal` remains a deprecated alias for this root; that is a
// CLI spelling, not a path probe.)
func DefaultPackRoot() string { return config.PackDir() }

// packContainerMCP returns {integration.mcp: config.MCPContainer} for a pack's
// CONTAINER/REMOTE integrations — Manifest servers (`sbx mcp add <name> --local
// --url`), Image servers (`docker run <image>`, op-run wrapped), and remote URL
// servers (`sbx mcp add <name> --url`, OAuth'd host-side). These are what
// `pix mcp register` adds specially rather than as a plain host subcommand.
// Returns nil when the pack declares none.
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

// ActiveContainerMCP resolves packContainerMCP for the active pack. Returns nil
// when there is no active pack or it won't load (registration of the other
// servers proceeds regardless).
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

// McpNames returns the de-duplicated `integration.mcp` names a pack
// declares, in manifest order. Used by RunPackUse to compute what F1 attaches.
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

// packKitDir resolves the PER-PACK KEY under which a pack's ephemeral mixin
// kits are synthesized: <StateDir>/pix/pack-kits/<hash>, keyed by a hash
// of the pack root. Since round-3 R2 this is a naming PREFIX, not a live dir:
// every launch synthesizes into its own unique <hash>.kit-XXXX dir beside it
// (see SynthesizePackKit), and sweepStaleKitTemps age-gates the cleanup of old
// ones (a `pix state reset` sweep is the backstop).
func packKitDir(root string) string {
	sum := sha256.Sum256([]byte(root))
	dir, err := config.StateDir()
	if err != nil {
		dir = "pix-state"
	}
	return filepath.Join(dir, "pix", "pack-kits", hex.EncodeToString(sum[:])[:16])
}

// SynthesizePackKit builds the ephemeral mixin kit that puts a pack's non-host
// bin/ wrappers on the sandbox PATH (F2/ADR-2): a minimal `kind: mixin`
// spec.yaml, plus files/home/.local/bin/<name> (0755) for each [[proxy]] with
// Host unset/false — ~/.local/bin is on the sandbox PATH and (unlike /usr/local)
// is reached by the runtime mixin-kit mount. Returns (dir, nil) on success, ("", nil) when
// the pack has no sandbox proxies (nothing to mount — the caller must not
// stack an empty kit), and ("", err) when the pack DECLARES a sandbox proxy
// but the kit can't be built — the caller must fail the launch closed
// (round-4 F2), never proceed to a kitless create.
// Copies (never symlinks): LoadPack already refuses a symlinked bin/, and sbx
// mounts a real tree.
//
// PER-LAUNCH UNIQUE DIR (round-3 R2): every call synthesizes into its OWN
// os.MkdirTemp dir (keyed by the pack hash as a name prefix) and returns THAT
// path for --kit. There is no stable shared path any more, so there is no
// replace-in-place window where the live kit is briefly absent, and two
// concurrent launches of the same pack can never clash on a shared mutable
// dir — each builds its kit COMPLETELY before returning it, then never touches
// it again. A proxy removed from pack.toml can't resurrect either: the fresh
// dir only ever holds what THIS synth wrote (the old finding-#6 guarantee,
// now structural). Old launch dirs are age-gate swept (sweepStaleKitTemps).
// And it FAILS CLOSED: if any declared wrapper can't be read or copied, the
// whole synth is refused with an error — never a partial kit with that one
// wrapper silently missing.
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
		// No sandbox proxies and no capabilities.json: nothing to mount. A previous
		// launch's kit dir is inert (nothing references it) and the sweep above
		// cleans it up.
		return "", nil
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
	// Fold each sandbox proxy's declared egress into permissions.network.allow so the
	// wrapper can actually reach its host endpoint — the sbx egress proxy blocks
	// (403) any destination not on the allowlist, even host.docker.internal. Kit
	// stacking unions this with the base kit's allowlist.
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
			// DISTINCT rules (it resolves the former to the latter), so a
			// host-loopback egress must allow BOTH forms — mirrors the base kit,
			// which lists host.docker.internal:PORT and localhost:PORT together.
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
	if len(sandboxProxies) > 0 {
		// Proxy wrappers go under files/home/.local/bin (→ ~/.local/bin, on PATH):
		// the sbx runtime mixin-kit mount honors files/home/** (into $HOME) but NOT
		// files/usr/local/**, so a wrapper written to usr/local/bin never lands.
		binOut := filepath.Join(dir, "files", "home", ".local", "bin")
		if err := os.MkdirAll(binOut, 0o755); err != nil {
			return fail("pack kit for %s: %v", p.Manifest.Name, err)
		}
		for _, pr := range sandboxProxies {
			src := filepath.Join(p.Root, "bin", pr.Name)
			b, err := os.ReadFile(src)
			if err != nil {
				// Fail closed: never launch with a partial kit because one declared
				// wrapper couldn't be read.
				return fail("pack proxy %q: %v (refusing to build the pack kit)", pr.Name, err)
			}
			if err := os.WriteFile(filepath.Join(binOut, pr.Name), b, 0o755); err != nil {
				return fail("pack proxy %q: %v (refusing to build the pack kit)", pr.Name, err)
			}
		}
	}
	// A pack's capabilities.json travels into ~/.pi/agent so its capability
	// routing overrides the base image's generic one. Fail closed if it's
	// declared but unreadable.
	if p.CapabilitiesFile != "" {
		agentOut := filepath.Join(dir, "files", "home", ".pi", "agent")
		if err := os.MkdirAll(agentOut, 0o755); err != nil {
			return fail("pack kit for %s: %v", p.Manifest.Name, err)
		}
		b, err := os.ReadFile(p.CapabilitiesFile)
		if err != nil {
			return fail("pack capabilities.json: %v (refusing to build the pack kit)", err)
		}
		if err := os.WriteFile(filepath.Join(agentOut, "capabilities.json"), b, 0o644); err != nil {
			return fail("pack capabilities.json: %v (refusing to build the pack kit)", err)
		}
	}
	if p.WebSearchFile != "" {
		configOut := filepath.Join(dir, "files", "home", ".pi")
		if err := os.MkdirAll(configOut, 0o755); err != nil {
			return fail("pack web-search.json: %v (refusing to build the pack kit)", err)
		}
		b, err := os.ReadFile(p.WebSearchFile)
		if err != nil {
			return fail("pack web-search.json: %v (refusing to build the pack kit)", err)
		}
		if err := os.WriteFile(filepath.Join(configOut, "web-search.json"), b, 0o644); err != nil {
			return fail("pack web-search.json: %v (refusing to build the pack kit)", err)
		}
	}
	return dir, nil
}

// kitLaunchInfix names each per-launch unique kit dir as a suffix on the pack
// hash (so launch dirs sit beside their key under pack-kits/ and
// sweepStaleKitTemps finds them by prefix).
const kitLaunchInfix = ".kit-"

// sweepStaleKitTemps best-effort removes old per-launch kit dirs (and legacy
// temp/aside/stable-path debris) for THIS pack. Only entries older than an
// hour are touched, so a concurrent launch's freshly-built kit — which sbx may
// still be reading at create time — is never yanked out from under it.
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
		"#\n" +
		"# Runs IN THE SANDBOX, fenced by the net allowlist (F2: in-sandbox, safe by\n" +
		"# default). Edit this to wrap the real CLI/API call — e.g. curl a REST\n" +
		"# endpoint, or exec a real binary already on PATH under a different name.\n" +
		"# Declare any domains it needs in pack.toml's [[proxy]] egress = [...] so\n" +
		"# the sbx kit allowlist can be updated to match.\n" +
		"set -euo pipefail\n" +
		"echo \"" + name + ": TODO — implement this wrapper\" >&2\n" +
		"exit 1\n"
}

// packLock is <pack-root>/pack.lock: GENERATED activation provenance, not a
// resolver lockfile (packs-v2-impl.md §3/ADR-1). It records exactly what the
// LAST `pack use` of this pack contributed to cfg.MCP / cfg.KnowledgeBundles /
// cfg.GogAccount / cfg.OllamaBridgeModel, so switching AWAY removes/restores
// exactly that contribution — never a user's own manually-added entry, and
// never leaking one pack's config into the next (finding #5). Git-ignored by
// default (RunPackNew seeds a pack-local .gitignore line for it).
//
// TRUST (round-2 A): pack.lock is a LOCAL, HUMAN-READABLE HINT only. It sits
// inside the pack directory — attacker-writable for any cloned pack via a
// plain `git pull` (or a zip update), even for a pack that is ALREADY active
// — so NOTHING that drives a config mutation is ever read from it. The
// authoritative activation provenance lives in the launcher-owned trust
// store (packtruststore.go, Activation), written at the same commit point.
// The only field ever read back from pack.lock is Remote/Commit, used purely
// as a FAIL-SAFE adoption marker (a forged marker only RESTRICTS what a pack
// may do — isAdoptedPack).
type packLock struct {
	MCP       []string `toml:"mcp,omitempty"`
	Knowledge []string `toml:"knowledge,omitempty"`
	// Remote/Commit are set ONLY when this pack was adopted via `pack use
	// <git-url>` (clonePack) — a non-empty Remote is the provenance marker
	// isAdoptedPack reads (finding #1, CRITICAL). Once set they are preserved
	// across every later re-activation of this same pack (never re-derived from
	// a local-path `pack use`), so adoption status can't be laundered by
	// pointing `pack use` at the already-cloned local directory.
	Remote string `toml:"remote,omitempty"`
	Commit string `toml:"commit,omitempty"`
	// GogAccount/OllamaBridgeModel record the value THIS pack's last activation
	// set on cfg (only present when the pack's manifest declares the field).
	// Prior* is whatever cfg held immediately BEFORE this pack overwrote it, so
	// reverting on switch-away restores exactly that (or empty, if there was
	// none) instead of leaking this pack's value into the next one (finding #5).
	GogAccount             string `toml:"gog_account,omitempty"`
	PriorGogAccount        string `toml:"prior_gog_account,omitempty"`
	OllamaBridgeModel      string `toml:"ollama_bridge_model,omitempty"`
	PriorOllamaBridgeModel string `toml:"prior_ollama_bridge_model,omitempty"`
	// NOTHING security-relevant lives here. Trust acceptance and installed
	// host-wrapper attribution used to (Accepted*/HostWrappers) — but pack.lock
	// sits INSIDE the pack directory, i.e. inside the attacker-controlled
	// payload for any downloaded/unzipped pack, so a pre-filled lock could
	// pre-accept its own host-exec surface. Both moved to the launcher-owned
	// trust store (<config-dir>/pack-trust.json — packtruststore.go). pack.lock
	// is ONLY Phase-1 activation provenance for reversibility, and it is
	// scrubbed/ignored whenever `pack use` targets a pack that is not already
	// active (scrubUntrustedPackLock).
}

const PackLockName = "pack.lock"

func PackLockPath(root string) string { return filepath.Join(root, PackLockName) }

// readPackLock reads root's pack.lock, best-effort: an absent OR UNPARSABLE
// file returns the zero value (no recorded contribution — the caller's removal
// set is then empty, which is the safe default: never guess at what an older
// activation contributed). A corrupt file is deliberately NOT trusted for a
// partial decode (finding #3): toml.Unmarshal can populate some fields before
// hitting a parse error, and treating that partial result as authoritative
// could silently under- or over-report a removal set. On any parse error this
// returns a clean zero value instead of whatever the decoder half-filled.
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

// writePackLock writes root's pack.lock (0644; not a secret — it holds server
// NAMES and canonical bundle PATHS, never a credential value).
//
// Hardened two ways (round-3 S1 CRITICAL + R1):
//   - Lstat-REFUSES a symlinked destination. os.WriteFile FOLLOWS a symlink,
//     so a malicious cloned pack committing pack.lock as a symlink (-> /dev/null
//     or a host file) could both swallow the adoption marker (the pack then
//     reads as AUTHORED, bypassing the local-path knowledge guard) and
//     overwrite an arbitrary host file. clonePack scrubs any checked-in
//     pack.lock right after clone, so a symlink here is always hostile or
//     corrupt — never legitimate local state.
//   - Writes ATOMICALLY via a same-dir temp file + rename: an interrupted
//     write can never truncate/corrupt an existing lock, and rename REPLACES a
//     symlink rather than following it (a second layer under the Lstat check).
func writePackLock(root string, l packLock) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(l); err != nil {
		return err
	}
	return writePackLockBytes(root, buf.Bytes())
}

// writePackLockBytes is the raw-bytes half of writePackLock (same Lstat
// symlink refusal, same atomic same-dir temp + rename). Split out so
// commitPackActivation can restore a SNAPSHOT of the prior lock byte-for-byte
// on a cfg.Save failure without round-tripping it through the decoder (which
// would silently normalize — or, for an unparsable lock, erase — it).
func writePackLockBytes(root string, data []byte) error {
	dest := PackLockPath(root)
	if fi, err := os.Lstat(dest); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to write through it", dest)
	}
	return sys.AtomicWriteInDir(root, PackLockName, data, 0o644)
}

// commitPackActivation is the commit point shared by `pack use` and the
// active-pack `pack add mcp` path. It writes THREE things, in an order whose
// failure residue is always safe:
//
//  1. pack.lock (the local HINT — round-4 abort-on-unwritable contract kept:
//     a lock that cannot be written, e.g. a dir-shaped pack.lock, ABORTS
//     before anything commits).
//  2. The AUTHORITATIVE activation record in the launcher-owned trust store
//     (round-2 A) — a store-write failure also aborts before cfg.Save, so
//     the config is never committed without host-state attribution.
//  3. cfg.Save. An ordinary Save failure ROLLS BACK both the store record
//     and the prior pack.lock bytes, so on-disk state stays mutually
//     consistent (the rollback contract the round-4 tests pin, now anchored
//     on the HOST-STATE record).
//
// A true hard kill between the store write and the config rename leaves an
// activation record that OVER-claims — harmless, because removing an absent
// MCP/bundle is a no-op (see the commit-ordering comment in RunPackUse).
//
// CONCURRENCY (round-3 #1): the store write, cfg.Save, and the rollback all
// run under the cross-process trust lock against a FRESH load of the store —
// saving the caller's (possibly stale) in-memory object here was the
// last-writer-wins clobber: a concurrent `pix host` wrapper refresh
// could overwrite the activation this commit just wrote (or vice versa). The
// caller's store view is synced on success.
func commitPackActivation(cfg *config.Config, store *PackTrustStore, root string, lock packLock) error {
	priorLock, priorErr := os.ReadFile(PackLockPath(root))
	priorExists := priorErr == nil
	if priorErr != nil && !os.IsNotExist(priorErr) {
		// Can't snapshot the prior lock, so a Save-failure rollback would be
		// impossible: abort BEFORE writing anything (nothing is committed).
		return fmt.Errorf("reading prior pack.lock for %s: %v — aborting without saving config (nothing was committed; fix the pack directory and re-run)", root, priorErr)
	}
	if err := writePackLock(root, lock); err != nil {
		return fmt.Errorf("writing pack.lock for %s: %v — aborting without saving config (nothing was committed; fix the pack directory and re-run)", root, err)
	}
	restoreLock := func() error {
		if priorExists {
			return writePackLockBytes(root, priorLock)
		}
		return os.Remove(PackLockPath(root))
	}
	return withPackTrustLock(func() error {
		fresh, lerr := loadPackTrustStore()
		if lerr != nil {
			if rerr := restoreLock(); rerr != nil {
				return fmt.Errorf("pack trust state unreadable: %v (and restoring the prior pack.lock failed: %v) — aborting without saving config (nothing was committed)", lerr, rerr)
			}
			return fmt.Errorf("pack trust state unreadable: %v — aborting without saving config (nothing was committed; fix %s and re-run)", lerr, packTrustStorePath())
		}
		priorActivation := fresh.Activation
		priorActivations := append([]packActivationRecord(nil), fresh.Activations...)
		fresh.setActivation(root, lock)
		if err := fresh.Save(); err != nil {
			if rerr := restoreLock(); rerr != nil {
				return fmt.Errorf("recording activation in pack trust state: %v (and restoring the prior pack.lock failed: %v) — aborting without saving config (nothing was committed)", err, rerr)
			}
			return fmt.Errorf("recording activation in pack trust state: %v — aborting without saving config (nothing was committed; fix %s and re-run)", err, packTrustStorePath())
		}
		if err := cfg.Save(); err != nil {
			// Roll BOTH the store record and the lock back so they match the
			// (unchanged) on-disk config.
			fresh.Activation = priorActivation
			fresh.Activations = priorActivations
			serr := fresh.Save()
			rerr := restoreLock()
			if serr != nil || rerr != nil {
				return fmt.Errorf("saving config: %v (rollback incomplete — trust store: %v, pack.lock: %v — the activation record may over-claim this activation's contributions; harmless, but re-run `pack use` once the config is writable)", err, serr, rerr)
			}
			return fmt.Errorf("saving config: %v (activation record rolled back; nothing was committed)", err)
		}
		if store != nil {
			store.Activation = fresh.Activation // keep the caller's view coherent
			store.Activations = append([]packActivationRecord(nil), fresh.Activations...)
		}
		return nil
	})
}

// migratePhase1Activation is the ONE-TIME Phase-1 → Phase-2 migration
// (round-3 #2). A pack activated by a Phase-1 build recorded its activation
// attribution ONLY in pack.lock; Phase 2 reads reversibility exclusively from
// the host-state store, which treats "no record" as "remove nothing" — so
// without migration the FIRST Phase-2 switch/reactivation/rm would silently
// lose the Phase-1 pack's attribution (its MCP/knowledge contributions
// over-retained forever, its gog/model overrides never restored).
//
// It fires exactly when the store has NO activation record attributed to the
// pack, and splits on adoption:
//   - LOCAL (authored) pack: pack.lock is the user's OWN Phase-1 state — safe
//     to trust ONCE — so its attribution is lifted into the store record the
//     caller's revert reads.
//   - ADOPTED pack: pack.lock is attacker-writable payload (`git pull` / zip
//     update), so it is NEVER trusted — nothing migrates, nothing reverts
//     (safe over-retention; the exact posture of a missing lock).
//
// The migrated record is IN-MEMORY only: on a completed switch the commit
// point immediately replaces it with the new pack's record, and on an abort
// (gate refusal, commit failure) the next run simply re-migrates — nothing
// needs persisting here, and never persisting means the migration can never
// clobber a concurrent writer either.
func migratePhase1Activation(store *PackTrustStore, root string) {
	if store == nil || strings.TrimSpace(root) == "" || store.hasActivationFor(root) {
		return
	}
	if isAdoptedPack(root) {
		return // adopted payload lock: never trusted (revert nothing)
	}
	hint := readPackLock(root)
	if len(hint.MCP) == 0 && len(hint.Knowledge) == 0 && hint.GogAccount == "" && hint.OllamaBridgeModel == "" {
		return // no Phase-1 attribution to migrate
	}
	store.setActivation(root, hint)
}

// isAdoptedPack reports whether root carries adoption provenance, i.e. this
// pack was cloned from a remote via `pack use <git-url>` at some point. Used
// by the finding-#1 CRITICAL guard: a shared=false local-path [[knowledge]]
// reference is NEVER honored for an adopted pack, because pack.toml there is
// attacker-controlled input from the remote — honoring it would let a
// malicious pack.toml point AddKnowledgeBundle at an arbitrary host directory
// (e.g. ~/.ssh) that the sandbox can then read via the knowledge service.
//
// Three signals, ANY of which marks the pack adopted (all fail-safe: a forged
// marker only ever RESTRICTS what a pack may do): the pack.lock Remote marker
// (clonePack/markPackAdopted write it), the launcher-owned trust store's
// adoption provenance (host state — survives even a scrubbed/forged lock),
// and the clone LOCATION itself (everything under PacksDir was put there by
// clonePack, never authored by the user).
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

// scrubUntrustedPackLock removes a pack-supplied pack.lock before a pack that
// is NOT currently active is adopted (item 4 of the trust-model rework): a
// downloaded/unzipped pack can ship a forged pack.lock — clonePack scrubs it
// for remote clones, but a local-path adoption used to bypass that. The forged
// file could claim the user's OWN mcp/knowledge entries as the pack's
// contribution (corrupting Phase-1 reversibility: a later switch-away would
// remove them) or be a symlink that blocks/redirects the fresh lock write.
// os.Remove removes a symlink itself, never its target. The caller must also
// IGNORE the lock's decoded content (treat as zero value) — scrubbing plus a
// fresh regenerate at commit is the whole contract.
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
		// A DIRECTORY named pack.lock cannot carry forged lock content
		// (readPackLock zero-values it) — leave it for the commit point, which
		// fails loudly with the established abort-without-commit message.
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing untrusted %s: %w", PackLockName, err)
	}
	return nil
}

// errPrivateRefSkippedAdopted is the sentinel resolvePackKnowledgeRef returns
// when it refuses to honor a shared=false local-path reference because the
// pack is adopted (finding #1). Callers use errors.Is to distinguish this from
// an ordinary resolution failure so they can batch it into one aggregate
// notice instead of per-ref noise.
var errPrivateRefSkippedAdopted = errors.New("private knowledge ref skipped: pack is adopted from a remote")

// revertPackPriorContribution undoes a previous activation's contribution
// (F4/finding #5): removes exactly the MCP + knowledge entries prevLock
// attributes to that pack (never a value the lock doesn't mention — the
// finding #3 reversibility guarantee), and restores gog_account /
// ollama_bridge_model to whatever cfg held immediately before that pack
// overwrote them (or empty, if there was none). Shared by RunPackUse
// (switching to a different pack, or re-activating the SAME pack — finding D)
// and RunPackRm (detaching), so all are equally honest about what
// "detached"/"switched away" means.
func revertPackPriorContribution(cfg *config.Config, prevLock packLock) (removedMCP, removedKnowledge []string) {
	for _, m := range prevLock.MCP {
		if cfg.RemoveMCP(m) {
			removedMCP = append(removedMCP, m)
		}
	}
	for _, id := range prevPackKnowledgeIDs(prevLock) {
		if cfg.RemoveKnowledgeBundle(id) {
			removedKnowledge = append(removedKnowledge, id)
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
	return removedMCP, removedKnowledge
}

// CanonicalizePackRoot normalizes a pack root path for identity comparison
// (finding #7): expands ~, then filepath.Abs + Clean, so e.g. `pack add mcp
// fastmail ./work` compares correctly against cfg.Pack even when cfg.Pack is
// stored absolute and the CLI argument is a relative path (or vice versa).
// Best-effort: a path that can't be made absolute falls back to
// expandUser+Clean rather than failing.
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

// prevPackKnowledgeIDs computes the canonical bundle ids the PREVIOUS active
// pack contributed, for removal on switch (F4). STRICTLY lock-attributed
// (finding C): only entries pack.lock records as that activation's own
// contribution are ever removed. An empty/missing/corrupt lock removes
// NOTHING — possible stale-bundle accumulation is accepted over the
// alternative of guessing from the manifest, which could delete a bundle the
// USER added independently (the old embedded-knowledge/ fallback did exactly
// that when the lock was lost).
func prevPackKnowledgeIDs(lock packLock) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range lock.Knowledge {
		id = knowledge.CanonicalizeKnowledgeBundle(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// resolvePackKnowledgeRef resolves one [[knowledge]] entry to an absolute local
// bundle path (F6). The guards here key on the source's CLASS (git URL vs
// local path — knowledgeSourceIsGitURL), NEVER on the manifest's shared flag,
// which an attacker authors (finding A, CRITICAL): keying the skip-guard on
// shared=false alone let an adopted pack declare shared=true with a LOCAL path
// and walk straight past it into AddKnowledgeBundle. LoadPack additionally
// enforces shared↔class agreement (shared=true ⇔ git URL), so a mismatched
// entry never even loads; the class check here keeps this function safe for
// any caller regardless.
//
// A git URL TRAVELS: resolved via knowledge.ResolveBundleRef, which clones/pulls it into
// the shared knowledge cache through the safeGitURL gate (no ext::/fd::/file::
// transports, no local-as-remote) — an adopter who shares the pack pulls the
// SAME team bundle.
//
// A LOCAL path is AUTHORED-ONLY: it is deliberately NOT root-scoped (pointing
// outside the pack at the owner's own machine is the entire point of a private
// reference), but pack.toml for an ADOPTED pack (cloned from a remote,
// adopted==true) is attacker-controlled input, so a local path there is NEVER
// honored — whatever the shared flag claims — or `pack use <attacker-git-url>`
// could point AddKnowledgeBundle at an arbitrary host directory (e.g. ~/.ssh)
// that the knowledge service then indexes and the sandbox can read. The caller
// aggregates these into one notice (see errPrivateRefSkippedAdopted).
//
// For a pack the user authored locally (adopted==false), two more guards apply
// before AddKnowledgeBundle ever sees the path: (a) it must resolve to an
// existing, readable directory — a typo'd/nonexistent path is skipped rather
// than indexed, so the knowledge service is never pointed at a dangling entry
// (no knowledge-service poisoning); (b) it must resolve OUTSIDE the pack's own
// tree — a private reference that actually lives inside root should be embedded
// under knowledge/ instead (that travels honestly), not declared "private" while
// silently living in the repo.
func resolvePackKnowledgeRef(out io.Writer, root string, adopted bool, k packKnowledge) (string, error) {
	source := strings.TrimSpace(k.Source)
	if source == "" {
		return "", fmt.Errorf("[[knowledge]] %q has no source", k.Name)
	}
	if knowledgeSourceIsGitURL(source) {
		return knowledge.ResolveBundleRef(source, knowledge.KnowledgeCacheDir(), out)
	}
	// LOCAL path: authored-only, regardless of the shared flag (finding A).
	if adopted {
		return "", errPrivateRefSkippedAdopted
	}
	abs, err := filepath.Abs(expandUser(source))
	if err != nil {
		return "", fmt.Errorf("resolving private knowledge %q: %w", k.Name, err)
	}
	resolved := filepath.Clean(abs)
	if r, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		resolved = r
	}
	// (b) reject a source resolving inside the pack tree.
	rootResolved := filepath.Clean(root)
	if r, rerr := filepath.EvalSymlinks(root); rerr == nil {
		rootResolved = r
	}
	if resolved == rootResolved || strings.HasPrefix(resolved, rootResolved+string(filepath.Separator)) {
		return "", fmt.Errorf("private knowledge %q (%s) resolves INSIDE the pack tree; embed it under knowledge/ instead of referencing it as private", k.Name, source)
	}
	// (a) validate it exists and is a readable directory before AddKnowledgeBundle.
	fi, statErr := os.Stat(resolved)
	if statErr != nil {
		return "", fmt.Errorf("private knowledge %q: %s: %w", k.Name, resolved, statErr)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("private knowledge %q: %s is not a directory", k.Name, resolved)
	}
	f, openErr := os.Open(resolved)
	if openErr != nil {
		return "", fmt.Errorf("private knowledge %q: %s is not readable: %w", k.Name, resolved, openErr)
	}
	f.Close()
	return resolved, nil
}

// WriteMemoryScope writes (or removes) <workspace>/.pix/profile: the
// memory scope tag the in-VM recall/capture extensions already read
// (memory-recall.ts, memory-capture.ts — no extension change for F4). p is the
// active pack (nil when none). The scope is p.Manifest.MemoryScope, defaulting
// to the pack's Name; an empty result or the literal "default" selects the
// shared/unscoped tag, matching "default" == the shared scope from the schema
// doc. No pack (or an unscoped pack) removes any stale file — this REPLACES the
// old unconditional profile-delete in run.go. Symlink-safe via
// workspace.WriteStateFile (a hostile repo can commit .pix/profile as a
// symlink) and workspace.RemoveStateFile (a hostile repo can commit .pix
// ITSELF as a symlink to another repo's .pix, which a plain os.Remove
// would traverse and delete through).
func WriteMemoryScope(ws string, p *Info) {
	if p == nil {
		_ = workspace.RemoveStateFile(ws, "profile")
		return
	}
	// Memory is a single SHARED store by default (AGENTS: the in-store scope column
	// is dormant). ONLY an explicit `memory_scope` in the manifest isolates a pack.
	// The pack NAME must NOT become a scope: doing so stamped every conversational
	// capture with the pack's own name, which hid it from the default recall view
	// (recall sees {scope}∪{default}; host-side recall queries default), so
	// captured preferences looked lost. Empty/"default" => shared, no scope file.
	// (The default pack's own Name IS literally "default", so this guard is what
	// keeps its captures shared rather than accidentally scoped to itself.)
	scope := strings.TrimSpace(p.Manifest.MemoryScope)
	if scope == "" || scope == "default" {
		_ = workspace.RemoveStateFile(ws, "profile")
		return
	}
	_ = workspace.WriteStateFile(ws, "profile", []byte(scope+"\n"), 0o644)
}

// packRecreateLine is the ADR-3 "same breath" recreate instruction: any
// operation that changes the sandbox facet set (MCP attach, sandbox bin/
// wrappers) MUST print this, because --mcp/--kit are create-only — a running
// sandbox cannot pick either up without a recreate (packs.md §13 must-fix).
func printPackRecreateLine(out io.Writer) {
	fmt.Fprintln(out, "MCP attach + sandbox bin/ wrappers + pack skills only take effect on a sandbox CREATE.")
	fmt.Fprintln(out, "Recreate to pick them up:  pix run --replace")
}

// --- verb tree --------------------------------------------------------------

// packTarget resolves an optional positional PATH to a pack root, defaulting to
// the default pack root.
func packTarget(rest []string) string {
	if len(rest) > 0 && strings.TrimSpace(rest[0]) != "" {
		return expandUser(rest[0])
	}
	return DefaultPackRoot()
}

// safeArtifactName rejects a skill/knowledge name that could escape the pack root
// (path separators, `..`) or is empty.
func safeArtifactName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// activateDefaultPack sets the default pack as active (config `pack`) if no
// pack is active yet, so implicit-create makes it immediately usable (no manual
// `pack use`). Best-effort. Only for the default pack root.
// activateDefaultPack points cfg.Pack at root when (and only when) root IS the
// resolved default pack AND cfg.Pack is currently empty — it must NEVER
// override an explicitly active alternate pack (cfg.Pack != ""), which is a
// no-op, not an error. Returns an error instead of swallowing one: a caller
// that reports "active pack -> this (default) pack" (or, worse, propagates
// setup success) after a cfg.Save failure would be lying — the config on
// disk still has no active pack. Every caller MUST check the returned error
// before claiming activation succeeded.
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
func RunPackNew(env hostenv.Env, out io.Writer, rest []string) {
	root := packTarget(rest)
	// Already a pack? Nothing to do (but ensure the default one is active).
	if _, err := os.Stat(filepath.Join(root, PackManifestName)); err == nil {
		fmt.Fprintf(out, "already a pack: %s\n", root)
		if aerr := activateDefaultPack(root); aerr != nil {
			fmt.Fprintf(out, "pix pack new: %v\n", aerr)
			os.Exit(1)
		}
		return
	}
	existsDir := false
	if fi, err := os.Stat(root); err == nil && fi.IsDir() {
		existsDir = true
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		fmt.Fprintf(out, "pix pack new: %v\n", err)
		os.Exit(1)
	}
	// git init only if it isn't already a repo (adopt an existing one in place).
	isRepo := false
	if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
		isRepo = true
	}
	if !isRepo {
		if _, err := env.Run("git", "-C", root, "init"); err != nil {
			fmt.Fprintf(out, "  note: `git init` failed (%v) — the pack still works; init it yourself\n", err)
		} else {
			isRepo = true
		}
	}
	name := filepath.Base(root)
	if err := WriteManifest(root, Manifest{Name: name, Schema: 1}); err != nil {
		fmt.Fprintf(out, "pix pack new: could not write %s: %v\n", PackManifestName, err)
		os.Exit(1)
	}
	// pack.lock is GENERATED activation provenance (ADR-1), never hand-authored;
	// seed a pack-local .gitignore line for it so a fresh pack never accidentally
	// commits it. Best-effort, no-clobber (never touches an existing .gitignore
	// beyond appending the line once).
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
	if root == DefaultPackRoot() {
		if aerr := activateDefaultPack(root); aerr != nil {
			fmt.Fprintf(out, "pix pack new: %v\n", aerr)
			os.Exit(1)
		}
		fmt.Fprintln(out, "active pack -> this (default) pack")
	} else {
		fmt.Fprintf(out, "use it:  pix pack use %s\n", root)
	}
}

// RunPackAdd writes one artifact into a pack (implicit-create), then registers
// it by presence (skills/knowledge are discovered by convention).
// RegisterFn registers the named servers with the sbx gateway. Activating a
// pack registers the MCP servers it declares, but pack may not call mcp —
// both are capabilities — so the caller supplies this. Same seam, same reason,
// as slack.RegisterFn.
type RegisterFn func(cfg *config.Config, env hostenv.Env, out io.Writer, names []string,
	hostResolver func() (string, error), containers map[string]config.MCPContainer) error

func RunPackAdd(env hostenv.Env, out io.Writer, rest []string, register RegisterFn) {
	if len(rest) < 2 {
		fmt.Fprintln(os.Stderr, "usage: pix pack add <skill|knowledge|proxy|mcp> <name> [PACK] [flags]")
		os.Exit(2)
	}
	kind, name := rest[0], rest[1]
	if !safeArtifactName(name) {
		fmt.Fprintf(os.Stderr, "pix pack add: invalid name %q (letters, digits, -, _, . only; no path separators)\n", name)
		os.Exit(2)
	}
	// Parse the tail: flags (--host, --private, --ref VALUE, --env VALUE) plus an
	// optional trailing PACK positional. Flags are shared across kinds; each kind
	// below reads only the ones it understands.
	var host, private, yes bool
	var ref, envVar string
	var positionals []string
	tail := rest[2:]
	for i := 0; i < len(tail); i++ {
		a := tail[i]
		switch {
		case a == "--host":
			host = true
		case a == "--private":
			private = true
		case a == "--yes" || a == "-y":
			yes = true
		case a == "--ref":
			if i+1 >= len(tail) {
				fmt.Fprintln(os.Stderr, "pix pack add: --ref needs a value")
				os.Exit(2)
			}
			i++
			ref = tail[i]
		case strings.HasPrefix(a, "--ref="):
			ref = strings.TrimPrefix(a, "--ref=")
		case a == "--env":
			if i+1 >= len(tail) {
				fmt.Fprintln(os.Stderr, "pix pack add: --env needs a value")
				os.Exit(2)
			}
			i++
			envVar = tail[i]
		case strings.HasPrefix(a, "--env="):
			envVar = strings.TrimPrefix(a, "--env=")
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "pix pack add: unknown flag %q\n", a)
			os.Exit(2)
		default:
			positionals = append(positionals, a)
		}
	}
	root := DefaultPackRoot()
	if len(positionals) >= 1 {
		root = expandUser(positionals[0])
	}
	// Implicit-create the pack if absent.
	if _, err := os.Stat(filepath.Join(root, PackManifestName)); err != nil {
		RunPackNew(env, out, []string{root})
	}
	// Refuse to write through a symlinked skills/knowledge/bin dir (an adopted
	// pack could point it outside the pack root); same posture as LoadPack's
	// mount check.
	for _, d := range []string{"skills", "knowledge", "bin"} {
		if isSymlinkPath(filepath.Join(root, d)) {
			fmt.Fprintf(os.Stderr, "pix pack add: %s has a symlinked %s/ dir; refusing to write through it\n", root, d)
			os.Exit(1)
		}
	}
	switch kind {
	case "skill":
		dir := filepath.Join(root, "skills", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(out, "pix pack add: %v\n", err)
			os.Exit(1)
		}
		f := filepath.Join(dir, "SKILL.md")
		if _, err := os.Stat(f); err == nil {
			fmt.Fprintf(out, "skill already exists: %s\n", f)
			return
		}
		if err := os.WriteFile(f, []byte(skillTemplate(name)), 0o644); err != nil {
			fmt.Fprintf(out, "pix pack add: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(out, "added skill %q: %s\n", name, f)
		fmt.Fprintln(out, "edit it, then commit it to your pack's git repo.")
	case "knowledge":
		if strings.TrimSpace(ref) == "" {
			// Embed (v1 behavior): a literal knowledge/ doc, discovered by convention.
			dir := filepath.Join(root, "knowledge")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				fmt.Fprintf(out, "pix pack add: %v\n", err)
				os.Exit(1)
			}
			f := filepath.Join(dir, name+".md")
			if _, err := os.Stat(f); err == nil {
				fmt.Fprintf(out, "knowledge doc already exists: %s\n", f)
				return
			}
			if err := os.WriteFile(f, []byte(knowledgeTemplate(name)), 0o644); err != nil {
				fmt.Fprintf(out, "pix pack add: %v\n", err)
				os.Exit(1)
			}
			fmt.Fprintf(out, "added knowledge doc %q: %s\n", name, f)
			return
		}
		// F6 reference: [[knowledge]] name/source/shared. --private (shared=false)
		// does NOT travel with the pack — the source is a local path that stays on
		// this machine; the default (shared=true) is meant for a git URL an adopter
		// pulls.
		p, err := LoadPack(root)
		if err != nil {
			fmt.Fprintf(out, "pix pack add: %v\n", err)
			os.Exit(1)
		}
		entry := packKnowledge{Name: name, Source: ref, Shared: !private}
		replaced := false
		for i, k := range p.Manifest.Knowledge {
			if k.Name == name {
				p.Manifest.Knowledge[i] = entry
				replaced = true
				break
			}
		}
		if !replaced {
			p.Manifest.Knowledge = append(p.Manifest.Knowledge, entry)
		}
		if err := WriteManifest(root, p.Manifest); err != nil {
			fmt.Fprintf(out, "pix pack add: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(out, "added knowledge reference %q (shared=%v) to pack.toml\n", name, entry.Shared)
		if private {
			fmt.Fprintln(out, "private: this reference will NOT travel if you share the pack.")
		}
		fmt.Fprintln(out, "run `pix pack use` on this pack to index it.")
	case "proxy":
		binDir := filepath.Join(root, "bin")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			fmt.Fprintf(out, "pix pack add: %v\n", err)
			os.Exit(1)
		}
		f := filepath.Join(binDir, name)
		if _, err := os.Stat(f); err != nil {
			if err := os.WriteFile(f, []byte(proxyShimTemplate(name)), 0o755); err != nil {
				fmt.Fprintf(out, "pix pack add: %v\n", err)
				os.Exit(1)
			}
			fmt.Fprintf(out, "scaffolded proxy wrapper: %s\n", f)
		} else {
			fmt.Fprintf(out, "proxy wrapper already exists: %s\n", f)
		}
		p, err := LoadPack(root)
		if err != nil {
			fmt.Fprintf(out, "pix pack add: %v\n", err)
			os.Exit(1)
		}
		exists := false
		for i, pr := range p.Manifest.Proxies {
			if pr.Name == name {
				p.Manifest.Proxies[i].Host = host
				exists = true
				break
			}
		}
		if !exists {
			p.Manifest.Proxies = append(p.Manifest.Proxies, PackProxy{Name: name, Host: host})
		}
		if err := WriteManifest(root, p.Manifest); err != nil {
			fmt.Fprintf(out, "pix pack add: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(out, "added proxy %q to pack.toml (host=%v)\n", name, host)
		if host {
			// F3: a host wrapper is a Tier-1 facet — it installs only after the
			// F5 BoM gate accepts it at `pack use`, and it is on PATH for
			// `pix host` sessions ONLY (never the sandbox), behind the
			// host.enabled machine gate.
			fmt.Fprintf(out, "host wrapper: review + accept it with `pix pack use %s` (Tier-1 host BoM gate);\n", root)
			fmt.Fprintln(out, "once accepted it installs for `pix host` sessions only (requires host.enabled).")
		} else {
			// Edit it, then a sandbox recreate is needed to mount it (F2/ADR-3).
			printPackRecreateLine(out)
		}
	case "mcp":
		p, err := LoadPack(root)
		if err != nil {
			fmt.Fprintf(out, "pix pack add: %v\n", err)
			os.Exit(1)
		}
		exists := false
		for i, ig := range p.Manifest.Integrations {
			if ig.MCP == name {
				p.Manifest.Integrations[i].Env = envVar
				exists = true
				break
			}
		}
		if !exists {
			p.Manifest.Integrations = append(p.Manifest.Integrations, Integration{Name: name, MCP: name, Env: envVar})
		}
		if err := WriteManifest(root, p.Manifest); err != nil {
			fmt.Fprintf(out, "pix pack add: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(out, "added mcp integration %q to pack.toml\n", name)
		// F1: if this IS the active pack, attach it right now (cfg.MCP + gateway
		// registration + credential solicit), same mechanism as `pack use` —
		// otherwise nothing has changed in the sandbox facet set yet, so no
		// recreate line is owed until the pack is actually activated.
		cfg, cerr := config.Load()
		// finding #7: compare CANONICALIZED paths — root here may be a raw
		// (possibly relative) CLI argument, while cfg.Pack is stored resolved
		// absolute by `pack use`. A raw-string compare made an active pack look
		// inactive whenever the two spellings differed (e.g. `pack add mcp
		// fastmail ./work` while cfg.Pack is the absolute form).
		if cerr == nil && CanonicalizePackRoot(cfg.Pack) == CanonicalizePackRoot(root) {
			// F5: attaching an MCP means the gateway runs its command ON THE
			// HOST — a Tier-1 fact. Same gate as `pack use`: an exact
			// fingerprint match in the HOST trust store skips the prompt,
			// non-TTY fails closed unless --yes. On refusal the declaration
			// stays in pack.toml (inert) but NOTHING attaches.
			trustStore, tserr := loadPackTrustStore()
			if tserr != nil {
				// FATAL (round-2 A): the store is both the acceptance source and
				// the activation-attribution commit target; an empty stand-in
				// would clobber it at the commit point.
				fmt.Fprintf(out, "pix pack add: pack trust state unreadable: %v (fix or remove %s and re-run)\n", tserr, packTrustStorePath())
				os.Exit(1)
			}
			// One-time Phase-1 → Phase-2 migration (round-3 #2): without it, a
			// Phase-1 active pack's lock attribution would be overwritten at the
			// commit below with ONLY the newly-added name.
			migratePhase1Activation(trustStore, root)
			bom := ComputeHostBoM(p, cfg.GogAccount, LocalMCPClassifier(env, env.HostBinary))
			var bomFingerprint, packKey string
			if bom.Tier1() {
				fp, _, ferr := ComputeHostExecFingerprint(root, bom)
				if ferr != nil {
					fmt.Fprintf(out, "pix pack add: pack %s: %v\n", root, ferr)
					os.Exit(1)
				}
				bomFingerprint = fp
				packKey = trustStore.TrustKey(root)
				if got, ok := trustStore.acceptedFingerprint(packKey); !ok || got != fp {
					if gerr := packTrustGate(os.Stdin, out, cli.IsTTY(os.Stdin), yes, p.Manifest.Name, bom); gerr != nil {
						fmt.Fprintf(out, "pix pack add: %v (declared in pack.toml, but NOT attached)\n", gerr)
						os.Exit(1)
					}
				}
			}
			added := cfg.AddMCP(name)
			if added {
				// Attribution stays gated on the AddMCP result (finding #2): a
				// pre-existing, user-added name is never claimed as this pack's.
				// Lock BEFORE Save, ABORT on lock failure (round-3 R1 + round-4 F1,
				// same commit point as RunPackUse): the config is never committed
				// without its attribution, so a later `pack use`/`pack rm` can
				// always clean up what this command added. The attribution BASE is
				// the HOST-state activation record (round-2 A), never the payload
				// lock; the adoption marker for the hint comes from host-recorded
				// provenance first, else the (fail-safe) payload marker.
				lock := trustStore.activationFor(root)
				if !slices.Contains(lock.MCP, name) {
					lock.MCP = append(lock.MCP, name)
				}
				if prov, ok := trustStore.Adopted[CanonicalizePackRoot(root)]; ok {
					lock.Remote, lock.Commit = prov.Remote, prov.Commit
				} else if hint := readPackLock(root); strings.TrimSpace(hint.Remote) != "" {
					lock.Remote, lock.Commit = strings.TrimSpace(hint.Remote), strings.TrimSpace(hint.Commit)
				}
				if err := commitPackActivation(cfg, trustStore, root, lock); err != nil {
					fmt.Fprintf(out, "pix pack add: %v\n", err)
					os.Exit(1)
				}
			}
			// F5: persist the acceptance in HOST state (the gate above passed,
			// or the stored fingerprint already covered this surface), so a
			// later `pack use` of this pack won't re-prompt for what was just
			// accepted here. Best-effort: a failed write only re-prompts.
			if bom.Tier1() {
				// Lock-serialized fresh-load mutation (round-3 #1); commit is
				// provenance metadata only (round-3 #5).
				rec := PackTrustRecord{Path: CanonicalizePackRoot(root), Fingerprint: bomFingerprint}
				if _, werr := mutatePackTrustStore(func(s *PackTrustStore) error {
					if prov, ok := s.Adopted[rec.Path]; ok {
						rec.Remote, rec.Commit = prov.Remote, prov.Commit
					}
					s.RecordAcceptance(packKey, rec)
					return nil
				}); werr != nil {
					fmt.Fprintf(out, "note: could not record the accepted host BoM: %v (the Tier-1 gate will re-prompt)\n", werr)
				}
			}
			// finding E: registration runs even when the name was ALREADY in
			// cfg.MCP (added == false) — it is idempotent, and a retry after a
			// failed gateway registration must actually re-register instead of
			// silently doing nothing.
			if err := register(cfg, env, out, []string{name}, launcher.FindHostBinary, packContainerMCP(p)); err != nil {
				fmt.Fprintf(out, "note: mcp registration: %v\n", err)
			}
			solicitPackCredentials(env, os.Stdin, out, cli.IsTTY(os.Stdin), p)
			if added {
				printPackRecreateLine(out)
			}
		} else {
			fmt.Fprintf(out, "activate the pack to attach it to a sandbox:  pix pack use %s\n", root)
		}
	default:
		fmt.Fprintf(os.Stderr, "pix pack add: unknown kind %q (want: skill, knowledge, proxy, mcp)\n", kind)
		os.Exit(2)
	}
}

func RunPackLs(out io.Writer) {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(out, "pix pack ls: %v\n", err)
		os.Exit(1)
	}
	active := ActivePackRoot(cfg.Pack, "")
	if active == "" {
		fmt.Fprintln(out, "no active pack (`pix pack add skill <name>` to start one, or `pix pack use <path|git-url>`)")
		return
	}
	p, err := LoadPack(active)
	if err != nil {
		fmt.Fprintf(out, "pack %s: %v\n", active, err)
		return
	}
	fmt.Fprintf(out, "active pack: %s (%s)\n", p.Manifest.Name, p.Root)
}

func RunPackShow(env hostenv.Env, out io.Writer, rest []string) {
	root := packTarget(rest)
	if len(rest) == 0 {
		cfg, err := config.Load()
		if err == nil && ActivePackRoot(cfg.Pack, "") != "" {
			root = ActivePackRoot(cfg.Pack, "")
		}
	}
	p, err := LoadPack(root)
	if err != nil {
		fmt.Fprintf(out, "pix pack show: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(out, "pack:      %s\n", p.Manifest.Name)
	fmt.Fprintf(out, "root:      %s\n", p.Root)
	fmt.Fprintf(out, "skills:    %s\n", present(p.SkillsDir))
	fmt.Fprintf(out, "knowledge: %s\n", present(p.KnowledgeDir))
	if p.CapabilitiesFile != "" {
		fmt.Fprintln(out, "capabilities: yes (mounts to ~/.pi/agent/capabilities.json)")
	}
	if p.WebSearchFile != "" {
		fmt.Fprintln(out, "web search: yes (mounts to ~/.pi/web-search.json)")
	}
	if p.Manifest.OllamaBridgeModel != "" {
		fmt.Fprintf(out, "ollama:    %s\n", p.Manifest.OllamaBridgeModel)
	}
	if p.Manifest.GogAccount != "" {
		fmt.Fprintf(out, "gog:       %s\n", p.Manifest.GogAccount)
	}
	if p.Manifest.MemoryScope != "" {
		fmt.Fprintf(out, "memory:    %s\n", p.Manifest.MemoryScope)
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
				kind = "HOST (Phase 2)"
			}
			fmt.Fprintf(out, "  - %s (%s)\n", pr.Name, kind)
		}
	}
	if len(p.Manifest.Knowledge) > 0 {
		fmt.Fprintln(out, "knowledge refs:")
		for _, k := range p.Manifest.Knowledge {
			shared := "private (does not travel)"
			if k.Shared {
				shared = "shared (travels)"
			}
			fmt.Fprintf(out, "  - %s -> %s [%s]\n", k.Name, k.Source, shared)
		}
	}
	if len(p.Manifest.Integrations) > 0 {
		fmt.Fprintln(out, "integrations:")
		for _, ig := range p.Manifest.Integrations {
			fmt.Fprintf(out, "  - %s", ig.Name)
			if ig.MCP != "" {
				fmt.Fprintf(out, " (mcp: %s)", ig.MCP)
			}
			switch {
			case ig.Manifest != "":
				// Manifest container: creds are Docker-side, not op-refs.
				fmt.Fprintf(out, " — manifest: %s (creds Docker-side)", ig.Manifest)
			case ig.Image != "":
				fmt.Fprintf(out, " — image: %s", ig.Image)
			case ig.URL != "":
				// Remote endpoint: OAuth'd host-side by the gateway, no op-refs.
				fmt.Fprintf(out, " — url: %s (OAuth host-side)", ig.URL)
			}
			// Image + host/remote integrations resolve their secret from op-refs
			// (Manifest containers don't — those are Docker-side).
			if ig.Env != "" && ig.Manifest == "" {
				if secret.OpRefFilled(env, ig.Env) {
					fmt.Fprintf(out, " — %s ✓", ig.Env)
				} else if ig.Setup != "" {
					fmt.Fprintf(out, "; later: pix setup --pack %s --with %s", sys.ShellQuote(p.Root), sys.ShellQuote(ig.Setup))
				} else {
					fmt.Fprintf(out, " — %s ✗ (run: pix secret set %s op://vault/item/field)", ig.Env, ig.Env)
				}
			}
			fmt.Fprintln(out)
		}
	}
}

// solicitPackCredentials, on a TTY, prompts for any pack integration whose op://
// credential ref is missing, writing each accepted ref. No-op off-TTY or when op
// isn't installed; missing refs then just surface as warnings at run time. The
// pack ships no secret — only the user's own op:// reference is stored.
func solicitPackCredentials(env hostenv.Env, in io.Reader, out io.Writer, tty bool, p *Info) {
	if !tty || in == nil || !secret.OpInstalled(env) {
		return
	}
	var missing []Integration
	for _, ig := range p.Manifest.Integrations {
		if ig.Env == "" {
			continue
		}
		if ig.Setup != "" {
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

func RunPackUse(env hostenv.Env, out io.Writer, rest []string, register RegisterFn) {
	// --yes / -y accepts the F5 Tier-1 host BoM without prompting (the ONLY way
	// a non-TTY adoption of a host-exec pack can proceed — it fails closed
	// otherwise).
	var yes bool
	var args []string
	for _, a := range rest {
		switch a {
		case "--yes", "-y":
			yes = true
		default:
			args = append(args, a)
		}
	}
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: pix pack use [--yes] <path|git-url|default>")
		os.Exit(2)
	}
	arg := strings.TrimSpace(args[0])
	// "default" is a real built-in alias for the default pack root (NOT
	// $PWD/default): resolves through DefaultPackRoot() exactly like every other
	// call site. "personal" is a DEPRECATED
	// alias kept for backward compatibility with a deprecation warning; only the
	// EXACT bare token matches — a git URL or a real path/dir literally named
	// "personal" (e.g. `./personal`, `../personal`, a full path, or a git URL
	// ending in personal.git) is unaffected and still resolves as a path/URL.
	switch arg {
	case "default":
		arg = DefaultPackRoot()
	case "personal":
		fmt.Fprintln(out, "pix pack use: \"personal\" is deprecated; use \"default\" instead.")
		arg = DefaultPackRoot()
	}
	var root, remoteURL, remoteCommit string
	if isPackGitURL(arg) {
		r, err := clonePack(env, out, arg)
		if err != nil {
			fmt.Fprintf(out, "pix pack use: %v\n", err)
			os.Exit(1)
		}
		root = r
		remoteURL, _ = parsePackURL(arg)
		if sha, cerr := env.Run("git", "-C", root, "rev-parse", "HEAD"); cerr == nil {
			remoteCommit = strings.TrimSpace(sha)
		}
	} else {
		root = expandUser(arg)
		if abs, err := filepath.Abs(root); err == nil {
			root = abs
		}
	}
	p, err := LoadPack(root)
	if err != nil {
		fmt.Fprintf(out, "pix pack use: %v\n", err)
		os.Exit(1)
	}
	// F5: re-hash every SHA-pinned [[bin]] BEFORE anything commits or the gate
	// even renders — activating a pack whose pinned binary does not match its
	// declared sha is refused outright (tampered binary or stale pin), so the
	// sha the BoM screen shows is always the sha of the actual bytes on disk.
	for _, bn := range p.Manifest.Bins {
		if verr := verifyPackBinSHA(root, bn); verr != nil {
			fmt.Fprintf(out, "pix pack use: pack %s: %v\n", root, verr)
			os.Exit(1)
		}
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(out, "pix pack use: %v\n", err)
		os.Exit(1)
	}

	// --- F4: the atomic swap. Everything below mutates the in-memory cfg; the
	// ONE cfg.Save() further down is the single commit point for every host-side/
	// config facet (ADR-3). Nothing is half-written to config if Save fails: the
	// pre-Save cfg was never persisted. ---

	prevRoot := cfg.Pack
	prevRoots := ActivePackRoots(cfg, "")
	switching := prevRoot != "" && (prevRoot != root || len(prevRoots) > 1)
	// The pack-supplied pack.lock is NEVER trusted for reversibility (round-2
	// A) — not even when this pack is already active: a plain `git pull` (or a
	// zip update) rewrites files under an already-active pack root, so a forged
	// lock claiming the user's own mcp/knowledge entries as the pack's
	// contribution would make the revert below DELETE them. The ONLY thing
	// read off the lock is the fail-safe adoption marker (Remote/Commit — a
	// forged marker only RESTRICTS what a pack may do). Reversibility reads
	// come exclusively from the launcher-owned trust store's activation
	// record. The payload lock is additionally SCRUBBED on a not-currently-
	// active local-path adoption (a symlinked lock must not redirect the fresh
	// hint write); a URL adoption's lock was just written by
	// clonePack/markPackAdopted — host-authored — so it is kept.
	hint := readPackLock(root)
	hintRemote, hintCommit := strings.TrimSpace(hint.Remote), strings.TrimSpace(hint.Commit)
	if prevRoot != root && remoteURL == "" {
		if serr := scrubUntrustedPackLock(root); serr != nil {
			fmt.Fprintf(out, "pix pack use: %v (refusing to adopt with an untrusted %s in place)\n", serr, PackLockName)
			os.Exit(1)
		}
	}

	// Adoption provenance (finding #1, CRITICAL): a pack cloned via a git URL
	// THIS activation, one whose lock carried a Remote marker (fail-safe: a
	// forged marker only RESTRICTS), or one host state / its location under
	// PacksDir proves was cloned (isAdoptedPack), is "adopted" — pack.toml
	// there is attacker-controlled, so shared=false local knowledge refs are
	// never honored (enforced inside resolvePackKnowledgeRef below).
	adopted := remoteURL != "" || hintRemote != "" || isAdoptedPack(root)

	// F5: the Tier-1 trust gate — against TRUSTED HOST STATE (packtruststore.go),
	// never anything the pack ships. Tier-0 (no host-exec facet) adopts
	// silently, exactly as Phase 1 did. Tier-1 halts at the BoM screen unless
	// the trust store holds this pack identity's acceptance of the EXACT
	// current host-exec surface (fingerprint match: MCP argv, host proxy script
	// content, bin pins, egress, creds). Switching between accepted packs never
	// re-prompts; ANY surface change does. Refusal aborts here: nothing
	// registered, installed, or committed.
	//
	// An UNREADABLE trust store is now FATAL (it is the reversibility AND
	// acceptance backbone): proceeding with an empty stand-in would both lose
	// the previous activation's removal set and — at the commit point — clobber
	// the store file with the stand-in. Fail closed with a pointer at the file.
	trustStore, tserr := loadPackTrustStore()
	if tserr != nil {
		fmt.Fprintf(out, "pix pack use: pack trust state unreadable: %v (fix or remove %s and re-run)\n", tserr, packTrustStorePath())
		os.Exit(1)
	}
	// One-time Phase-1 → Phase-2 migration (round-3 #2): a pre-Phase-2 active
	// pack has its activation attribution only in pack.lock — lift it into the
	// (in-memory) store record BEFORE computing the switch below, so its
	// contributions revert correctly. Adopted packs never migrate (their lock
	// is payload); see migratePhase1Activation.
	if prevRoot != "" && len(prevRoots) <= 1 {
		migratePhase1Activation(trustStore, prevRoot)
	}
	bom := ComputeHostBoM(p, cfg.GogAccount, LocalMCPClassifier(env, env.HostBinary))
	var bomFingerprint, packKey string
	if bom.Tier1() {
		fp, _, ferr := ComputeHostExecFingerprint(root, bom)
		if ferr != nil {
			fmt.Fprintf(out, "pix pack use: pack %s: %v\n", root, ferr)
			os.Exit(1)
		}
		bomFingerprint = fp
		packKey = trustStore.TrustKey(root)
		if got, ok := trustStore.acceptedFingerprint(packKey); !ok || got != fp {
			if gerr := packTrustGate(os.Stdin, out, cli.IsTTY(os.Stdin), yes, p.Manifest.Name, bom); gerr != nil {
				fmt.Fprintf(out, "pix pack use: %v\n", gerr)
				os.Exit(1)
			}
		}
	}

	// MCP set (F1 + ADR-1): remove exactly what the PREVIOUS pack's last
	// activation ACTUALLY ADDED (never a user's own manually-added MCP the
	// pack merely re-declares — finding #2), then add what the NEW pack
	// declares. Reversible: pack-use(A) -> pack-use(B) -> pack-use(A) restores
	// cfg.MCP to what it was after the first pack-use(A).
	var removedMCP, removedKnowledge []string
	switch {
	case switching:
		// A composed stack unwinds in reverse command order so each scalar
		// restoration sees the value its predecessor set. Collections remain
		// scoped to the exact per-pack ownership records.
		for i := len(prevRoots) - 1; i >= 0; i-- {
			mcp, knowledge := revertPackPriorContribution(cfg, trustStore.activationFor(prevRoots[i]))
			removedMCP = append(removedMCP, mcp...)
			removedKnowledge = append(removedKnowledge, knowledge...)
		}
	case prevRoot == root:
		// SAME-pack reactivation (finding D): revert THIS pack's own prior
		// contribution first, then re-apply the manifest fresh below. Without
		// the revert, every Add* returns false (the entries are already live),
		// the new lock overwrites the attribution with EMPTY slices — so a
		// later switch/rm could never remove this pack's contributions — and a
		// field REMOVED from the manifest since the last activation
		// (gog_account, ollama_bridge_model, an mcp, a knowledge ref) would
		// stay live forever. Revert-then-reapply reconciles both: facets still
		// declared re-add just below (regaining attribution), dropped ones
		// stay reverted. The removal set comes from HOST state (round-2 A),
		// never the pack-payload lock — a same-pack `git pull` forgery buys
		// nothing.
		removedMCP, removedKnowledge = revertPackPriorContribution(cfg, trustStore.activationFor(root))
	}
	var addedMCP []string
	for _, m := range McpNames(p) {
		if cfg.AddMCP(m) {
			addedMCP = append(addedMCP, m)
		}
	}

	// Knowledge (F4 + F6): add the NEW pack's embedded dir + resolved
	// [[knowledge]] refs (shared travels via knowledge.ResolveBundleRef; private resolves
	// to a local path that never entered the pack's git tree — and is skipped
	// entirely for an adopted pack, finding #1). Only what cfg.AddKnowledgeBundle
	// ACTUALLY ADDED is recorded for the next switch's removal set (finding #2):
	// a bundle already present (added by the user, or by another mechanism) is
	// never claimed as this pack's own contribution.
	var addedKnowledge, newKnowledgeIDs []string
	if p.KnowledgeDir != "" {
		if cfg.AddKnowledgeBundle(p.KnowledgeDir) {
			addedKnowledge = append(addedKnowledge, p.KnowledgeDir)
			newKnowledgeIDs = append(newKnowledgeIDs, knowledge.CanonicalizeKnowledgeBundle(p.KnowledgeDir))
		}
		cfg.AddService("knowledge")
	}
	var skippedPrivate int
	for _, k := range p.Manifest.Knowledge {
		resolved, rerr := resolvePackKnowledgeRef(out, root, adopted, k)
		if rerr != nil {
			if errors.Is(rerr, errPrivateRefSkippedAdopted) {
				skippedPrivate++
				continue
			}
			fmt.Fprintf(out, "note: knowledge %q: %v (skipping)\n", k.Name, rerr)
			continue
		}
		if cfg.AddKnowledgeBundle(resolved) {
			addedKnowledge = append(addedKnowledge, resolved)
			newKnowledgeIDs = append(newKnowledgeIDs, knowledge.CanonicalizeKnowledgeBundle(resolved))
		}
		cfg.AddService("knowledge")
	}

	// Config layering (F4/finding #5): a value the pack declares overwrites, but
	// remembers what cfg held immediately BEFORE the overwrite so switching away
	// restores exactly that (or empty) — never leaking one pack's value into the
	// next. On a SAME-pack reactivation the revert above already restored cfg to
	// the true pre-pack baseline (finding D), so capturing cfg's current value
	// is correct on every path (first use, switch, re-activation) — a chain of
	// re-activations never loses the baseline, and a field the manifest DROPPED
	// stays reverted instead of sticking around.
	var lockGogAccount, lockPriorGogAccount, lockOllamaModel, lockPriorOllamaModel string
	if p.Manifest.GogAccount != "" {
		lockPriorGogAccount = cfg.GogAccount
		lockGogAccount = p.Manifest.GogAccount
		cfg.SetGogAccount(lockGogAccount)
	}
	if m := strings.TrimSpace(p.Manifest.OllamaBridgeModel); m != "" {
		lockPriorOllamaModel = cfg.OllamaBridgeModel
		lockOllamaModel = m
		cfg.OllamaBridgeModel = lockOllamaModel
	}
	ClearPackInference(cfg, "")
	if err := ApplyPackInference(cfg, p.Manifest.Inference, root); err != nil {
		fmt.Fprintf(out, "pix pack use: %v\n", err)
		os.Exit(1)
	}

	cfg.Pack = root
	// `pack use` remains a single-pack switch. Multi-pack composition is an
	// explicit `pix setup --pack ... --pack ...` transaction; do not retain a
	// stale prior stack when a user later switches contexts.
	cfg.Packs = []string{root}

	// COMMIT ORDERING (round-3 R1 + round-4 F1): the lock is written BEFORE
	// cfg.Save, it records the INTENDED contribution set computed above, and a
	// lock-write FAILURE aborts before Save (commitPackActivation) — the config
	// is never committed without its attribution. The two writes can't be one
	// atomic transaction (two files), so pick the safe failure residue: a true
	// crash (SIGKILL/power loss) in the window between lock-write and Save
	// leaves a lock that OVER-claims (it names contributions the config never
	// committed) — harmless, because removal of an absent MCP/bundle is a no-op
	// (config.removeValue tolerates missing entries). The reverse order left
	// the fatal residue: an ACTIVE pack whose config-committed contributions
	// had NO lock attribution, so no later switch/rm could ever remove them.
	//
	// KNOWN RESIDUAL (deliberate, now crash-only): an ORDINARY cfg.Save
	// failure is fully consistent — commitPackActivation snapshots the prior
	// pack.lock and restores it atomically before returning the error, so lock
	// and config never diverge on a plain error (read-only config dir, disk
	// full). The only window left is a TRUE hard kill (SIGKILL/power loss) in
	// the milliseconds between the atomic lock rename and the atomic config
	// rename during a switch/reactivation: the new (narrower) lock lands
	// beside the old config, leaving a dropped MCP/bundle in config with no
	// lock attribution — over-retained until removed by hand (`pix
	// config`), since `pack use`/`pack rm` deliberately remove ONLY what the
	// lock attributes. That scoping is the chosen safe side of the
	// lock-only-removal design: it can never remove a user's manually-added
	// entry (the worse bug fixed in finding #2). Manifest-based reconciliation
	// would reopen that. Over-retention is safe (an extra entry, never a lost
	// one); do NOT "fix" it with manifest-driven removal.
	lock := packLock{
		MCP:                    addedMCP,
		Knowledge:              newKnowledgeIDs,
		Remote:                 remoteURL,
		Commit:                 remoteCommit,
		GogAccount:             lockGogAccount,
		PriorGogAccount:        lockPriorGogAccount,
		OllamaBridgeModel:      lockOllamaModel,
		PriorOllamaBridgeModel: lockPriorOllamaModel,
	}
	if lock.Remote == "" {
		// Not cloned THIS activation: keep whatever adoption marker this pack
		// already carried (a re-activation via local path must not un-adopt it) —
		// the launcher's own host-state provenance first, else the (fail-safe)
		// marker captured off the pack-supplied lock before the scrub. This
		// lands only in the pack.lock HINT (fail-safe marker), never in the
		// host-state activation or acceptance records.
		if prov, ok := trustStore.Adopted[CanonicalizePackRoot(root)]; ok {
			lock.Remote, lock.Commit = prov.Remote, prov.Commit
		} else {
			lock.Remote, lock.Commit = hintRemote, hintCommit
		}
	}
	if err := commitPackActivation(cfg, trustStore, root, lock); err != nil {
		// The lock IS part of this switch's committed state (finding #3 +
		// round-4 F1): if it can't be written, NOTHING is committed — the
		// in-memory cfg mutations above are discarded, the on-disk config (and
		// the active pack) stay exactly as they were.
		fmt.Fprintf(out, "pix pack use: %v\n", err)
		os.Exit(1)
	}

	// F5: persist the acceptance in HOST state (the gate above passed, or the
	// stored fingerprint already covered this exact surface — recording is
	// idempotent and re-normalizes provenance). A failed write just means the
	// gate re-prompts next time: fail closed, never open.
	if bom.Tier1() {
		// Provenance on the acceptance record is HOST-recorded ONLY (round-2 E):
		// this activation's own clone, or the launcher's adoption record — never
		// the pack-supplied pack.lock, whose forged Remote could alias a legit
		// pack and make recordAcceptance's hygiene sweep DELETE its acceptance.
		// Written via the lock-serialized fresh-load mutation (round-3 #1) so
		// this save can never clobber a concurrent writer's record. The commit
		// stored on the record is provenance METADATA only (round-3 #5): the
		// key is commit-stable, so a new commit with an unchanged host-exec
		// fingerprint never re-prompts.
		rec := PackTrustRecord{Path: CanonicalizePackRoot(root), Fingerprint: bomFingerprint}
		rec.Remote, rec.Commit = remoteURL, remoteCommit
		if _, werr := mutatePackTrustStore(func(s *PackTrustStore) error {
			if rec.Remote == "" {
				if prov, ok := s.Adopted[rec.Path]; ok {
					rec.Remote, rec.Commit = prov.Remote, prov.Commit
				}
			}
			s.RecordAcceptance(packKey, rec)
			return nil
		}); werr != nil {
			fmt.Fprintf(out, "note: could not record the accepted host BoM: %v (the Tier-1 gate will re-prompt)\n", werr)
		}
	}

	// --- post-Save: best-effort side effects (each already idempotent). ---

	if !env.Quiet {
		fmt.Fprintf(out, "active pack -> %s\n", root)
	}
	// On a same-pack reactivation the revert-then-reapply (finding D) removes
	// and immediately re-adds every still-declared entry; report as detached
	// only what actually STAYED out (a facet dropped from the manifest).
	detachedMCP, detachedKnowledge := removedMCP, removedKnowledge
	if !switching {
		detachedMCP, detachedKnowledge = nil, nil
		for _, m := range removedMCP {
			if !slices.Contains(cfg.MCP, m) {
				detachedMCP = append(detachedMCP, m)
			}
		}
		for _, id := range removedKnowledge {
			if !slices.Contains(cfg.KnowledgeBundles, id) {
				detachedKnowledge = append(detachedKnowledge, id)
			}
		}
	}
	if len(detachedMCP) > 0 && !env.Quiet {
		fmt.Fprintf(out, "detached mcp (previous activation): %s\n", strings.Join(detachedMCP, ", "))
	}
	if len(addedMCP) > 0 && !env.Quiet {
		fmt.Fprintf(out, "attached mcp: %s\n", strings.Join(addedMCP, ", "))
	}
	// finding E: register ALL of this pack's MCPs post-Save (registration is
	// idempotent), never just the newly-added ones — a retry after a failed
	// gateway registration finds the names already in cfg.MCP (AddMCP returned
	// false) and must still re-register, and a pack changing gog_account while
	// redeclaring an existing `gog` server must re-register the new account.
	if all := McpNames(p); len(all) > 0 {
		if err := register(cfg, env, out, all, launcher.FindHostBinary, packContainerMCP(p)); err != nil {
			fmt.Fprintf(out, "note: mcp registration: %v\n", err)
		}
	}
	if !env.Quiet {
		for _, id := range detachedKnowledge {
			fmt.Fprintf(out, "knowledge bundle detached (previous activation): %s\n", id)
		}
		for _, id := range addedKnowledge {
			fmt.Fprintf(out, "knowledge bundle registered: %s\n", id)
		}
	}
	if skippedPrivate > 0 && !env.Quiet {
		fmt.Fprintf(out, "skipped %d private knowledge ref(s) from an adopted pack (shared=false local paths are never honored for a pack cloned from a remote)\n", skippedPrivate)
	}

	// F3: swap the pack's host-exec wrappers NOW (core pack host-exec, not
	// `pix host` mode): RefreshHostPackWrappers clears what host state
	// attributes to the previous activation and stages+verifies+swaps in this
	// pack's ACCEPTED set (the acceptance recorded just above). Best-effort
	// here, like every other post-Save side effect.
	refreshOut := out
	if env.Quiet {
		refreshOut = io.Discard
	}
	if _, werr := RefreshHostPackWrappers(refreshOut, cfg, false); werr != nil {
		fmt.Fprintf(out, "note: host wrappers not refreshed: %v\n", werr)
	}

	// Solicit any 1Password creds this pack's reference-only integrations need.
	solicitPackCredentials(env, os.Stdin, out, cli.IsTTY(os.Stdin), p)

	// (The activation lock — this switch's removal set for the NEXT switch — was
	// already written just BEFORE cfg.Save; see the R1 commit-ordering comment.)

	// A knowledge change is daemon-affecting: restart/advise the running serve so
	// the new bundle is indexed (mirrors `knowledge use`). Best-effort.
	serveOut := out
	if env.Quiet {
		serveOut = io.Discard
	}
	service.PropagateConfig(service.DefaultReloader(), serveOut)

	// ADR-3: --mcp/--kit are create-only. Print the recreate line UNCONDITIONALLY
	// (this is "the change" for the purposes of packs.md §13's must-fix), so the
	// sandbox-facet-changing case is never silently skipped.
	if !env.Quiet {
		printPackRecreateLine(out)
	}
}

func RunPackRm(out io.Writer, rest []string) {
	if len(rest) > 0 {
		fmt.Fprintf(os.Stderr, "pix pack rm: unexpected argument %q (rm detaches the ACTIVE pack; it takes no name)\n", rest[0])
		os.Exit(2)
	}
	// The ENTIRE detach — re-reading cfg (the active pack) AND the trust
	// store, clearing the wrappers, reverting the contribution set, cfg.Save,
	// and dropping the spent activation record — runs under ONE hold of the
	// cross-process trust lock (concurrency review): `rm` used to decide what
	// to clear from a PRE-lock snapshot, so a concurrent `pix host`
	// wrapper refresh could install + attribute AFTER rm reported "detached",
	// or interleave into a live dir the store attributed to nobody. Under the
	// one lock the refresh and the detach serialize: either rm wins (nothing
	// installed, nothing attributed) or the refresh wins (installed AND
	// attributed) — never installed-but-unattributed. os.Exit stays OUTSIDE
	// the locked fn (withFlock contract); failures return and exit after the
	// lock is released.
	var (
		noActive         bool
		old              string
		removedWrappers  []string
		removedMCP       []string
		removedKnowledge []string
	)
	rmErr := withPackTrustLock(func() error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		if cfg.Pack == "" {
			noActive = true
			return nil
		}
		old = cfg.Pack
		// finding #5: `rm` must undo the active pack's contributions (mcp,
		// knowledge, gog/model overrides) too — not just clear cfg.Pack — or
		// "detached" is a lie about what actually happened. The contribution set
		// comes from HOST state (round-2 A), never the pack-payload lock — and an
		// unreadable trust store is FATAL: without it neither the removal set nor
		// the wrapper attribution can be honored.
		store, serr := loadPackTrustStore()
		if serr != nil {
			return fmt.Errorf("pack trust state unreadable: %v (fix or remove %s and re-run)", serr, packTrustStorePath())
		}
		// One-time Phase-1 → Phase-2 migration (round-3 #2), same as `pack use`:
		// a Phase-1 active pack's attribution lives only in its (local) pack.lock.
		migratePhase1Activation(store, old)
		// round-3 #4: "detached" includes the host wrappers — remove exactly what
		// HOST state attributes to HostPackBinDir() (reliable even when the pack
		// directory itself is gone; attribution never lived in the pack), and do
		// it FIRST: a failed clear aborts with a non-zero exit BEFORE anything
		// detaches, so `pack rm` never claims success while host executables
		// remain, and a plain re-run retries the whole detach cleanly. The
		// attribution is discarded only on CONFIRMED removal. Acceptance is kept:
		// trust was granted at adoption and re-attaching must not re-prompt.
		if store.Installed != nil && len(store.Installed.Wrappers) > 0 {
			removedWrappers = append([]string(nil), store.Installed.Wrappers...)
			if cerr := clearInstalledHostPackWrappersLocked(out, store); cerr != nil {
				removedWrappers = nil
				return fmt.Errorf("stale host wrappers could not be removed: %v — nothing detached; fix that and re-run", cerr)
			}
		}
		roots := ActivePackRoots(cfg, "")
		for i := len(roots) - 1; i >= 0; i-- {
			mcp, knowledge := revertPackPriorContribution(cfg, store.activationFor(roots[i]))
			removedMCP = append(removedMCP, mcp...)
			removedKnowledge = append(removedKnowledge, knowledge...)
		}
		ClearPackInference(cfg, "")
		cfg.Pack = ""
		cfg.Packs = nil
		if err := cfg.Save(); err != nil {
			return err
		}
		// The activation record is spent (its contributions were just reverted).
		// Dropped only when it was attributed to THIS pack, via the fresh-load
		// already-locked mutation (round-3 #1; the lock is held — never nest
		// withPackTrustLock); a failed store write merely over-claims (removals
		// of absent entries are no-ops).
		if len(store.Activations) > 0 || store.hasActivationFor(old) {
			if _, werr := mutatePackTrustStoreLocked(func(s *PackTrustStore) error {
				s.clearActivations()
				return nil
			}); werr != nil {
				fmt.Fprintf(out, "note: could not clear the activation record: %v (harmless over-claim; re-run `pack rm` once %s is writable)\n", werr, packTrustStorePath())
			}
		}
		return nil
	})
	if rmErr != nil {
		fmt.Fprintf(out, "pix pack rm: %v\n", rmErr)
		os.Exit(1)
	}
	if noActive {
		fmt.Fprintln(out, "no active pack to detach")
		return
	}
	fmt.Fprintf(out, "detached active pack (%s). The files are untouched; re-attach with `pix pack use`.\n", old)
	if len(removedWrappers) > 0 {
		fmt.Fprintf(out, "removed host wrappers: %s\n", strings.Join(removedWrappers, ", "))
	}
	if len(removedMCP) > 0 {
		fmt.Fprintf(out, "detached mcp: %s\n", strings.Join(removedMCP, ", "))
	}
	for _, id := range removedKnowledge {
		fmt.Fprintf(out, "knowledge bundle detached: %s\n", id)
	}
	if len(removedKnowledge) > 0 {
		service.PropagateConfig(service.DefaultReloader(), out)
	}
	if len(removedMCP) > 0 {
		printPackRecreateLine(out)
	}
}

// --- git-URL adoption -------------------------------------------------------

// isPackGitURL reuses knowledge.go's knowledge.IsGitURL and additionally accepts the
// "git+" scheme prefix used by kit URLs.
func isPackGitURL(s string) bool {
	s = strings.TrimSpace(s)
	// A git transport-helper string (ext::, fd::, ...) is URL-SHAPED, not a local
	// path: route it here so clonePack's safeGitURL rejects it with a clear
	// "unsafe transport" message instead of a confusing "not a pack" path error.
	if strings.Contains(s, "::") {
		return true
	}
	return strings.HasPrefix(s, "git+") || knowledge.IsGitURL(s)
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
// basename (minus .git), sanitized to [A-Za-z0-9._-] with any path-traversal
// (`.`/`..`/empty) neutralized, plus a short hash of the FULL url so two remotes
// with the same basename (org-a/tools vs org-b/tools) never collide on one dest.
func packNameFromURL(url string) string {
	u := strings.TrimSuffix(url, ".git")
	u = strings.TrimRight(u, "/")
	base := u
	if i := strings.LastIndexAny(u, "/:"); i >= 0 {
		base = u[i+1:]
	}
	safe := make([]rune, 0, len(base))
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			safe = append(safe, r)
		default:
			safe = append(safe, '-')
		}
	}
	name := strings.Trim(string(safe), ".-")
	if name == "" || name == ".." {
		name = "pack"
	}
	sum := sha256.Sum256([]byte(url))
	return name + "-" + hex.EncodeToString(sum[:])[:16]
}

// clonePack clones (or updates) a remote pack into PacksDir/<name>, pinned to the
// optional ref, and returns the local path. SHA-pin/provenance is a v2 concern;
// v1 trusts the git remote (Tier 0: skills/knowledge/config, no host execution).
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
		// A dir already exists at this URL-hash dest. Verify its origin actually
		// matches the requested URL before trusting it — a 64-bit hash collision (or
		// a pre-planted dir) must NOT let us fetch/activate the wrong repo. On
		// mismatch, wipe and re-clone.
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
	// pack.lock is LOCAL GENERATED activation state (ADR-1) and must NEVER come
	// from the remote (round-3 S1, CRITICAL): a malicious pack could commit it
	// as a SYMLINK (-> /dev/null, or a host file) so the adoption marker written
	// below would land on the symlink's TARGET — un-adopting the pack (bypassing
	// the local-path knowledge guard) and/or overwriting an arbitrary host file.
	// A checked-in REGULAR pack.lock is just as hostile (its attribution fields
	// would be merged by markPackAdopted and could claim the user's own MCP
	// entries for removal on switch-away). Scrub it AFTER every git operation
	// above (checkout/reset --hard restore tracked files) and BEFORE
	// markPackAdopted writes the real one. Failing the scrub fails the adoption.
	if err := scrubRemotePackLock(env, dest, freshClone); err != nil {
		if freshClone {
			_ = os.RemoveAll(dest)
		}
		return "", err
	}
	// Provenance durability (finding B): mark the clone ADOPTED here — durably,
	// before returning — never leaving it to the caller's post-Save lock
	// rewrite. A cfg.Save()/lock-write failure after this return must not leave
	// an UNMARKED adopted clone on disk that a retry would treat as user-
	// authored (and so honor its private/local knowledge refs — the finding-A
	// guard keys on this marker). If the marker itself cannot be written, fail
	// the whole adoption: an unmarked adopted clone is exactly the state this
	// guard exists to prevent.
	if err := markPackAdopted(env, dest, url); err != nil {
		if freshClone {
			_ = os.RemoveAll(dest)
		}
		return "", fmt.Errorf("recording adoption provenance for %s: %w", url, err)
	}
	return dest, nil
}

// scrubRemotePackLock deletes a pack.lock that came from the REMOTE in a
// cloned pack tree (round-3 S1): on a fresh clone ANY pack.lock came from the
// remote; on an update, one that is a symlink (never legitimate — writePackLock
// only ever creates regular files) or that git tracks (checkout/reset restore
// it from the remote) is remote-authored. A legit LOCAL lock (untracked regular
// file carrying prior activation attribution) is preserved. os.Remove removes a
// symlink itself, never its target.
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

// markPackAdopted durably records adoption provenance (pack.lock Remote +
// Commit) on a cloned/updated pack (finding B). It MERGES into any existing
// lock so a re-clone or update never sheds earlier activation attribution.
// The same provenance is mirrored into the launcher-owned trust store (host
// state — the trusted source for pack identity and the adopted-pack guard);
// that mirror is best-effort because the lock marker plus the under-PacksDir
// location check already keep the guard fail-safe without it.
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

// WriteManifest writes root's pack.toml. LEAF-symlink-safe (mirrors
// writePackLock/writePackLockBytes): the pack root is untrusted input — an
// adopted or migrated pack could have pack.toml replaced with a symlink (e.g.
// pointing outside the pack root) — so this Lstat-REFUSES a symlinked
// destination outright, then writes ATOMICALLY via a same-dir temp file +
// rename (an interrupted write can never truncate/corrupt an existing
// manifest).
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

// seedPackGitignore appends a `pack.lock` line to <root>/.gitignore, creating
// the file if absent, so a fresh pack never accidentally commits its generated
// activation-provenance lockfile (ADR-1). Idempotent (checked by substring) and
// best-effort: a write failure is silent — it must never block `pack new`.
//
// Symlink-safe (mirrors writePackLockBytes): `pack new .` can run in an
// UNTRUSTED directory, and os.ReadFile/os.WriteFile FOLLOW symlinks — a
// .gitignore symlinked at e.g. ~/.bashrc would have pack.lock appended to the
// TARGET. Lstat-REFUSE a symlinked .gitignore outright, and write via the
// same-dir atomic temp+rename (rename replaces a symlink, never follows one)
// so there is no check-then-write window either.
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

const Usage = `usage: pix pack <new|add|ls|show|use|rm>

A pack is a git-backed bundle of skills + knowledge + mcp integrations + proxy
wrappers + config that defines your context. See docs/design/packs.md.

  new [PATH]              adopt an existing repo (or one with pack.toml) as a
                          pack, else git-init a fresh one. Default PATH is the
                          default pack root (~/.local/share/pix/default).
  add skill <name> [P]              add a skill doc
  add knowledge <name> [P]          add an embedded knowledge doc
  add knowledge <name> [P] --ref <git-url|path> [--private]
                                     add a knowledge REFERENCE instead of
                                     embedding: shared (default) travels with
                                     the pack; --private does not
  add proxy <name> [P] [--host]     scaffold bin/<name> (an in-sandbox CLI
                                     wrapper on PATH); --host makes it a
                                     HOST-mode wrapper instead: Tier-1, gated
                                     by the "pack use" BoM review, on PATH for
                                     "pix host" only
  add mcp <name> [P] [--env VAR]    declare an MCP server this pack needs +
                                     the op-refs.env credential var name
                                     (attaching to the ACTIVE pack is Tier-1:
                                     the host BoM gate fires; --yes accepts)
                          (all "add" forms implicit-create pack P; default P
                          is the default pack)
  ls                      show the active pack
  show [PATH]             inspect a pack (default: the active pack)
  use [--yes] <path|git-url|default>
                          set the active pack: swaps mcp/knowledge/config in
                          ONE transaction (pack.lock tracks what to remove on
                          the next switch); a git URL is cloned to
                          ~/.local/share/pix/packs/<name> (optional #ref pin).
                          "default" is a built-in alias for the default pack
                          root (not $PWD/default); "personal" also works as a
                          deprecated alias (prints a deprecation warning).
                          A pack with HOST-exec facets (mcp, host wrappers,
                          [[bin]]) is Tier-1: adoption halts at a host
                          bill-of-materials review ([y/N], default No);
                          non-TTY fails closed unless --yes. MCP attach +
                          sandbox bin/ wrappers need a recreate
                          (pix run --replace) to take effect.
  rm                      detach the active pack (files untouched)
`
