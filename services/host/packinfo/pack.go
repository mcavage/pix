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

// SetupStep is one resumable onboarding step. It has two forms, and the
// DECLARATIVE one is the form to write:
//
//	[[setup.require]]  what must be true, in a closed vocabulary pix implements
//	[[setup.apply]]    what to run when it is not
//
// The declarative form exists because the executable form could not be kept
// honest. A `path` hook is opaque shell that pix hands control to, and every
// one ever written called back into the pix CLI — so a deleted verb turned a
// pack's setup into a step that could never pass, with the failure surfacing
// as a shell exit code nobody could act on. A pack that only DECLARES what it
// needs cannot name a command that does not exist, because it names no
// commands at all: pix owns every verb in the vocabulary.
//
// Path/CheckArgs/ApplyArgs remain for genuinely bespoke work no vocabulary
// will cover (installing a vendor daemon, say). They are fingerprinted by
// CONTENT hash exactly as before, so an existing pack's acceptance is
// unaffected by the new fields being available.
type SetupStep struct {
	ID          string   `toml:"id"`
	Description string   `toml:"description,omitempty"`
	Path        string   `toml:"path,omitempty"`
	CheckArgs   []string `toml:"check_args,omitempty"`
	ApplyArgs   []string `toml:"apply_args,omitempty"`
	Required    bool     `toml:"required,omitempty"`

	Require []SetupRequire `toml:"require,omitempty"`
	Apply   []SetupApply   `toml:"apply,omitempty"`
}

// SetupRequire is one condition, in a CLOSED vocabulary. Pix implements every
// kind; a pack chooses among them and supplies data. Unknown kinds are refused
// at load, so a typo is a startup error rather than a step that silently never
// passes.
type SetupRequire struct {
	// Kind is one of:
	//   bin     — Name must resolve on PATH; Install is the hint shown when it
	//             does not (the pack knows how its dependency is installed;
	//             pix must never guess a package manager).
	//   op-ref  — Env must be a FILLED op:// reference in op-refs.env.
	//   probe   — Argv must exit 0. This is the only kind that proves a thing
	//             WORKS rather than merely exists, so it is the one a pack
	//             should reach for last and rely on most.
	Kind    string   `toml:"kind"`
	Name    string   `toml:"name,omitempty"`
	Install string   `toml:"install,omitempty"`
	Env     string   `toml:"env,omitempty"`
	Argv    []string `toml:"argv,omitempty"`
}

// SetupApply is one remediation. Kind is "interactive" (inherits the terminal:
// browser grants, prompts, anything a user must answer) or "exec" (bounded,
// no terminal). Explain is shown BEFORE it runs, because a user about to be
// sent to a browser deserves to know what for.
//
// Apply steps MUST be idempotent. All of a step's applies run whenever ANY of
// its requirements is unmet, because pix cannot know which remediation maps to
// which condition — so re-installing an already-installed tool, or re-running
// an already-satisfied configuration, has to be safe and quiet.
//
// One exception is built in: an unmet `op-ref` requirement never triggers
// applies at all, because no command pix could run will put a secret in a
// user's 1Password vault.
type SetupApply struct {
	Kind    string   `toml:"kind"`
	Argv    []string `toml:"argv"`
	Explain string   `toml:"explain,omitempty"`
}

// Setup vocabularies, closed on purpose. See SetupStep.
var (
	setupRequireKinds = map[string]bool{"bin": true, "op-ref": true, "probe": true}
	setupApplyKinds   = map[string]bool{"interactive": true, "exec": true}
)

// Declarative reports whether this step is written in the require/apply form.
func (s SetupStep) Declarative() bool { return len(s.Require) > 0 }

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

// Integration is REFERENCE-ONLY: the pack says "I use <mcp>, start it like
// THIS, and it needs the credential <env>", shipping NO executable code of its
// own — the binary or image is something the user installs and the credential
// is an op:// ref the user owns. Command, Image, Manifest and URL are the four
// MUTUALLY EXCLUSIVE registration modes (validatePackFacets).
type Integration struct {
	Name string `toml:"name"`          // human label
	Env  string `toml:"env,omitempty"` // op-refs.env ENV VAR the credential lives under
	MCP  string `toml:"mcp,omitempty"` // MCP server name to attach (host-provided)

	// Command + Args run a HOST BINARY over stdio: the pack names a command
	// that must be on PATH (its setup hook installs it) and the LITERAL argv to
	// start it with. Pix adds no flags of its own, so hardening a server —
	// read-only, no-send, whatever it supports — is stated here where a
	// reviewer sees it in the pack's bill of materials and re-consents when it
	// changes. Nothing here is templated; per-user values travel as env_keys.
	Command string   `toml:"command,omitempty"`
	Args    []string `toml:"args,omitempty"`

	// Probe is the argv that answers "can this server actually do its job",
	// which is NOT the same question as "is it registered". `pix doctor` runs
	// it and shows the output. A pack that declares no probe gets reported as
	// unverifiable rather than healthy — silence is not evidence.
	Probe []string `toml:"probe,omitempty"`
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
		for _, value := range []string{ig.Command, ig.Manifest, ig.Image, ig.URL} {
			if strings.TrimSpace(value) != "" {
				kinds++
			}
		}
		if kinds > 1 {
			return fmt.Errorf("pack %s: integration %q sets more than one of command, manifest, image, and url; choose exactly one", root, name)
		}
		if kinds == 0 {
			// A reference-only integration cannot be registered by anything:
			// pix ships no built-in servers, so a name with no transport is a
			// server nobody can start. Caught here, at load, rather than as a
			// mystery "not declared" at registration time.
			return fmt.Errorf("pack %s: integration %q declares no transport; set exactly one of command, image, manifest or url", root, name)
		}
		if (strings.TrimSpace(ig.Manifest) != "" || strings.TrimSpace(ig.URL) != "") &&
			(strings.TrimSpace(ig.Env) != "" || len(ig.EnvKeys) > 0 || len(ig.EnvValues) > 0) {
			return fmt.Errorf("pack %s: integration %q cannot use env/env_keys with manifest or url; those registration modes do not forward pack environment variables", root, name)
		}
		// A host command gets its environment from op-refs.env, which is the
		// only channel `op run --env-file` has. There is no `-e` to carry a
		// literal, so honouring env_values here is impossible — and a consent
		// screen that shows a value which never reaches the server is worse
		// than not supporting it. Non-secret literals belong in op-refs.env,
		// declared via env_keys.
		if strings.TrimSpace(ig.Command) != "" && len(ig.EnvValues) > 0 {
			return fmt.Errorf("pack %s: integration %q sets env_values with command; a host command receives its "+
				"environment from op-refs.env only — put the value there and list it in env_keys", root, name)
		}
		if strings.TrimSpace(ig.Command) == "" && len(ig.Args) > 0 {
			return fmt.Errorf("pack %s: integration %q sets args without command; args are the argv of a host command", root, name)
		}
		// A command must be a BARE binary name, resolved on PATH at
		// registration. An absolute path in a manifest would pin one machine's
		// filesystem layout into a git-shared pack, and a relative path would
		// resolve against whatever directory the gateway happened to start in.
		if c := strings.TrimSpace(ig.Command); c != "" && !SafeArtifactName(c) {
			return fmt.Errorf("pack %s: integration %q command %q must be a bare binary name resolved on PATH (letters, digits, -, _, . only; no path separators)", root, name, c)
		}
		for _, argv := range [][]string{ig.Args, ig.Probe} {
			for _, a := range argv {
				if strings.ContainsAny(a, "\x00\r\n") {
					return fmt.Errorf("pack %s: integration %q has an argv entry containing a control character", root, name)
				}
			}
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
		if s.Declarative() {
			if strings.TrimSpace(s.Path) != "" {
				return fmt.Errorf("pack %s: [[setup]] %q sets both a path hook and declarative require/apply; choose one", root, s.ID)
			}
			if err := validateDeclarativeSetup(root, s); err != nil {
				return err
			}
			continue
		}
		if strings.TrimSpace(s.Path) == "" {
			return fmt.Errorf("pack %s: [[setup]] %q declares nothing: add [[setup.require]] conditions "+
				"(kind = bin | op-ref | probe), or a path hook for bespoke work", root, s.ID)
		}
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

// ServerMCP returns {integration.mcp: config.MCPServer} for a pack's
// CONTAINER/REMOTE integrations, which `pix mcp register` adds specially rather
// than as plain host subcommands. nil when there are none.
func ServerMCP(p *Info) map[string]config.MCPServer {
	out := map[string]config.MCPServer{}
	for _, ig := range p.Manifest.Integrations {
		if ig.MCP == "" {
			continue
		}
		// envKeys is the set of variable NAMES forwarded into the server, the
		// declared secret first. Order is stable (secret, then the pack's own
		// list) because it lands in a registered argv that a reviewer compares
		// against the bill of materials they approved.
		envKeys := func() []string {
			var keys []string
			if ig.Env != "" {
				keys = append(keys, ig.Env) // the op-refs secret, forwarded too
			}
			return append(keys, ig.EnvKeys...)
		}
		envValues := func() map[string]string {
			values := make(map[string]string, len(ig.EnvValues))
			for key, value := range ig.EnvValues {
				values[key] = value
			}
			return values
		}
		switch {
		case strings.TrimSpace(ig.Command) != "":
			// No EnvValues: a host command has no channel for a literal (see
			// the load-time refusal), so carrying one here would be dead state
			// that reads like a supported feature.
			out[ig.MCP] = config.MCPServer{
				Command: strings.TrimSpace(ig.Command),
				Args:    append([]string(nil), ig.Args...),
				EnvKeys: envKeys(),
				Probe:   append([]string(nil), ig.Probe...),
			}
		case strings.TrimSpace(ig.Manifest) != "":
			out[ig.MCP] = config.MCPServer{Manifest: strings.TrimSpace(ig.Manifest), Probe: append([]string(nil), ig.Probe...)}
		case strings.TrimSpace(ig.Image) != "":
			out[ig.MCP] = config.MCPServer{
				Image:     strings.TrimSpace(ig.Image),
				EnvKeys:   envKeys(),
				EnvValues: envValues(),
				Probe:     append([]string(nil), ig.Probe...),
			}
		case strings.TrimSpace(ig.URL) != "":
			out[ig.MCP] = config.MCPServer{RemoteURL: strings.TrimSpace(ig.URL), Probe: append([]string(nil), ig.Probe...)}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// NonSecretEnvNames is the union of every declared integration's env_keys: the
// variable names this pack authorizes to carry a LITERAL value in op-refs.env.
// The integration's own `env` secret is deliberately NOT in this set — a pack
// can never allowlist its own credential into plaintext.
func NonSecretEnvNames(p *Info) map[string]bool {
	if p == nil {
		return nil
	}
	out := map[string]bool{}
	for _, ig := range p.Manifest.Integrations {
		for _, k := range ig.EnvKeys {
			if k = strings.TrimSpace(k); k != "" && k != strings.TrimSpace(ig.Env) {
				out[k] = true
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ActiveNonSecretEnvNames resolves NonSecretEnvNames for the active pack.
func ActiveNonSecretEnvNames(cfg *config.Config) map[string]bool {
	root := ActivePackRoot(cfg.Pack, "")
	if root == "" {
		return nil
	}
	p, err := LoadPack(root)
	if err != nil {
		return nil
	}
	return NonSecretEnvNames(p)
}

// ActiveServerMCP resolves ServerMCP for the active pack, or nil when
// there is none or it won't load (other registrations proceed regardless).
func ActiveServerMCP(cfg *config.Config) map[string]config.MCPServer {
	root := ActivePackRoot(cfg.Pack, "")
	if root == "" {
		return nil
	}
	p, err := LoadPack(root)
	if err != nil {
		return nil
	}
	return ServerMCP(p)
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

// validateDeclarativeSetup fails closed on the require/apply vocabulary. Every
// rejection here is a pack that would otherwise have shipped a step which
// cannot pass — the exact failure the declarative form exists to make
// impossible, so it is caught at LOAD rather than at 2am during onboarding.
func validateDeclarativeSetup(root string, s SetupStep) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("pack %s: [[setup]] %q: "+format, append([]any{root, s.ID}, a...)...)
	}
	noCtrl := func(argv []string) bool {
		for _, a := range argv {
			if strings.ContainsAny(a, "\x00\r\n") {
				return false
			}
		}
		return true
	}
	for _, r := range s.Require {
		if !setupRequireKinds[r.Kind] {
			return bad("require kind %q is not one of bin, op-ref, probe", r.Kind)
		}
		switch r.Kind {
		case "bin":
			if !SafeArtifactName(r.Name) {
				return bad("require bin needs a bare binary name (got %q)", r.Name)
			}
			if strings.TrimSpace(r.Install) == "" {
				// A missing binary with no install hint is a dead end for the
				// user: pix cannot guess a package manager, and the pack is the
				// only thing that knows how its own dependency is obtained.
				return bad("require bin %q needs an install hint (how a user obtains it)", r.Name)
			}
		case "op-ref":
			if !EnvVarName(r.Env) {
				return bad("require op-ref needs an env var name (got %q)", r.Env)
			}
		case "probe":
			if len(r.Argv) == 0 {
				return bad("require probe needs an argv")
			}
			if !SafeArtifactName(r.Argv[0]) {
				return bad("require probe argv[0] must be a bare binary name (got %q)", r.Argv[0])
			}
			if !noCtrl(r.Argv) {
				return bad("require probe argv contains a control character")
			}
		}
	}
	for _, a := range s.Apply {
		if !setupApplyKinds[a.Kind] {
			return bad("apply kind %q is not one of interactive, exec", a.Kind)
		}
		if len(a.Argv) == 0 {
			return bad("apply needs an argv")
		}
		if !SafeArtifactName(a.Argv[0]) {
			return bad("apply argv[0] must be a bare binary name (got %q)", a.Argv[0])
		}
		if !noCtrl(a.Argv) {
			return bad("apply argv contains a control character")
		}
	}
	return nil
}

// EnvVarName reports whether s is a plausible environment variable name. It is
// here, beside the manifest schema, because a pack declares env var NAMES in
// three places and all three must agree on what one looks like.
func EnvVarName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r == '_':
		case r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}
