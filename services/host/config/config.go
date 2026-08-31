// Package config is the pix config schema + loader, shared by the host binary
// (pix-host) AND the launcher binary. Deliberately dependency-light.
package config

import (
	"os"
	"path/filepath"
	"sort"

	"github.com/BurntSushi/toml"

	"pix/host/pixhome"
)

// Defaults applied when a config file is absent or a field is unset.
const (
	// A small, fast, extraction-grade local model DEDICATED to fact capture: it
	// must reliably emit STRUCTURED JSON (facts/events/corrections).
	DefaultMemoryWatcherModel = "qwen3.5:9b"
	DefaultMemoryEmbedModel   = "nomic-embed-text"
	// DefaultMemoryCapture: no automatic observation until a user opts in.
	DefaultMemoryCapture = MemoryCaptureExplicit
	// DefaultOllamaBridgeModel is the local model the sandbox's ollama-bridge
	// exposes to pi (the interactive Alt+P cycle) AND the router's local option.
	DefaultOllamaBridgeModel = "qwen3.5:9b"
	// BuiltinImpl is the default plugin impl: compiled into the host binary
	// rather than run as an external sub-process.
	BuiltinImpl = "builtin"
)

// The two memory_capture admission modes. Explicit is the default; there is
// no review/staging mode.
const (
	MemoryCaptureExplicit         = "explicit"
	MemoryCaptureExperimentalAuto = "experimental-auto"
)

// MemoryCaptureModes is the closed vocabulary for memory_capture, in the
// order help text should list them.
var MemoryCaptureModes = []string{MemoryCaptureExplicit, MemoryCaptureExperimentalAuto}

// ValidMemoryCapture reports whether s is one of MemoryCaptureModes.
func ValidMemoryCapture(s string) bool {
	for _, m := range MemoryCaptureModes {
		if s == m {
			return true
		}
	}
	return false
}

// PluginSpec configures one plugin slot: how it is implemented and, for external
// impls, where the binary lives and how it is verified/reached.
type PluginSpec struct {
	Impl     string   `toml:"impl"`      // "builtin" (default) or an external impl name
	Path     string   `toml:"path"`      // path to an external plugin binary
	SHA      string   `toml:"sha"`       // expected checksum of the external binary
	Port     int      `toml:"port"`      // port an external plugin listens on
	ExtraEnv []string `toml:"extra_env"` // additional env vars granted to this plugin subprocess
}

// HostMode configures the `[host]` table. The unsandboxed escape hatch it once
// gated is RETIRED: the sandbox is the only supported execution boundary.
type HostMode struct {
	// Autoserve gates the launcher's LAZY AUTO-START of `pix-host serve`
	// (docs/design/serve-lifecycle.md §1). nil = default TRUE (auto-start on).
	Autoserve *bool `toml:"autoserve,omitempty"`
}

// AutoserveEnabled reports whether lazy auto-start is enabled (default true).
func (c *Config) AutoserveEnabled() bool {
	return c.Host.Autoserve == nil || *c.Host.Autoserve
}

// Config is the pix configuration, decoded from TOML.
type Config struct {
	VersionPin string `toml:"version_pin"`

	MemoryWatcherModel string `toml:"memory_watcher_model,omitempty"`
	MemoryEmbedModel   string `toml:"memory_embed_model,omitempty"`
	OllamaBridgeModel  string `toml:"ollama_bridge_model,omitempty"`
	// MemoryCapture: explicit (default) or experimental-auto. See
	// config.MemoryCaptureModes; a garbled value resolves to explicit.
	MemoryCapture string `toml:"memory_capture,omitempty"`

	// Environment is the machine default environment NAME (Story 1, native
	// sandbox environments — docs/design/environments.md §5.3), resolved
	// against Environments. Empty = no registered default. This is the v1
	// registry field: v2's `pix env default NAME` writes the machine default
	// through pixhome.SetDefaultEnvironment instead (a plain directory under
	// ~/.pix/envs, no registration database — AGENTS.md's command-surface
	// invariants), so nothing in the current CLI writes this field any more.
	// There is no generic config-mutation verb that could reach it either
	// (see provision.environmentKeyRefusal).
	Environment string `toml:"environment,omitempty"`

	// Environments is the registered name -> CANONICAL ABSOLUTE local
	// directory path index from the same v1 registry. Nothing writes it any
	// more (v2 has no add/forget mutation path — an environment is created,
	// moved, and removed with ordinary filesystem and Git tools instead);
	// AddEnvironment validates the name (validEnvironmentName) and canonicalizes
	// the path (expanding a leading ~, then making the result absolute and
	// clean) before either is ever assigned here, so Save() never persists
	// anything else. A hand-edited entry with an unsafe NAME or a noncanonical
	// PATH is dropped on Load, fail closed (see applyDefaults).
	Environments map[string]string `toml:"environments,omitempty"`

	// Inference describes WHERE catalog models can be called: user/pack-owned
	// backend wiring and model-id bindings only, never model identity or secrets.
	Inference InferenceConfig `toml:"inference,omitempty"`

	Kits struct {
		Stack []string `toml:"stack"`
	} `toml:"kits"`

	Skills struct {
		Paths []string `toml:"paths"`
	} `toml:"skills"`

	Plugins map[string]PluginSpec `toml:"plugins"`

	// Host is the `[host]` table (see HostMode's own doc comment: the
	// unsandboxed escape hatch it once gated is retired; only the launcher's
	// lazy `pix-host serve` autoserve toggle lives here now). GLOBAL, never
	// per-profile: how pix-host starts is a machine-level decision.
	Host HostMode `toml:"host,omitempty"`

	// unknownKeys are the keys in the file that this binary does nothing with:
	// everything BurntSushi reports as undecoded (MetaData.Undecoded()), plus
	// any [plugins.*] slot, which decodes into a real field and is then
	// deliberately discarded (see applyDefaults).
	//
	// This used to be a curated allowlist of keys that "once meant something",
	// reported so a caller could say "this no longer does anything". Nothing
	// ever called the reporter, and an allowlist by construction says nothing
	// about the one case that actually costs time: a TYPO. `memory_watchr_model`
	// was silently ignored, doing nothing and saying nothing. Every unrecognized
	// key is now recorded, so a caller can surface all of them.
	unknownKeys []string
}

// InferenceConfig is deliberately small: setup and packs author it. A non-empty
// ExclusiveSource is a pack-contributed enforcement boundary.
type InferenceConfig struct {
	Backends map[string]InferenceBackend `toml:"backends,omitempty"`
	Models   []InferenceModelBinding     `toml:"models,omitempty"`
	// AllowedModels is the user's canonical catalog-model roster; empty means no
	// user restriction. Exclusive pack inference bypasses this preference.
	AllowedModels []string `toml:"allowed_models,omitempty"`
	// RosterProviders records which providers the roster was already offered for,
	// so a NEWLY added provider can widen AllowedModels.
	RosterProviders  []string `toml:"roster_providers,omitempty"`
	ExclusiveBackend string   `toml:"exclusive_backend,omitempty"`
	ExclusiveSource  string   `toml:"exclusive_source,omitempty"`
}

type InferenceBackend struct {
	Driver            string `toml:"driver"`             // native | openai-compatible | ollama
	Protocol          string `toml:"protocol,omitempty"` // openai-completions | openai-responses
	BaseURL           string `toml:"base_url,omitempty"` // public endpoint; never credentials
	Auth              string `toml:"auth"`               // 1password | sbx-session | none
	KeyEnv            string `toml:"key_env,omitempty"`  // only for auth=1password
	Source            string `toml:"source,omitempty"`   // contributing pack root; empty = user/setup
	CredentialService string `toml:"credential_service,omitempty"`
	CredentialHeader  string `toml:"credential_header,omitempty"`
	CredentialFormat  string `toml:"credential_format,omitempty"`
}

// InferenceModelBinding maps a canonical catalog model to the model id exposed
// by one backend. Available is observed evidence written by setup/probe, not a
// theoretical claim from the catalog.
type InferenceModelBinding struct {
	Model     string `toml:"model"`
	Backend   string `toml:"backend"`
	Upstream  string `toml:"upstream_id"`
	Available bool   `toml:"available,omitempty"`
	Verified  bool   `toml:"verified,omitempty"` // successful backend-specific probe, not declaration
	Source    string `toml:"source,omitempty"`   // contributing pack root; empty = user/setup

	// VerifiedBy records HOW Verified was earned. "probe" is the only value this
	// codebase writes; empty on a binding written before provenance existed,
	// which is exactly the legacy listing-derived claim doctor must flag.
	VerifiedBy string `toml:"verified_by,omitempty"`
	// VerifiedAt is RFC3339 EVIDENCE TEXT for the doctor/summary line ("verified
	// 2026-07-14"). NEVER read for a decision: no staleness expiry, no re-probe
	// trigger. It exists so a row can cite a date instead of asserting one.
	VerifiedAt string `toml:"verified_at,omitempty"`
}

// VerifiedByProbe is the only provenance value this codebase writes. Promotion
// sets it in the same assignment as Verified; demotion clears both, so the
// provenance can never outlive the claim it describes.
const VerifiedByProbe = "probe"

// UnknownKeys returns every key in the loaded file this binary does nothing
// with, sorted and deduplicated — a copy, so callers cannot mutate the stored
// slice. A caller should SURFACE these: silently ignoring a key the user
// deliberately wrote is how a typo turns into an afternoon.
//
// Deliberately not an error. A config that names something unrecognized is
// still a usable config, and failing the load would turn a cosmetic mistake
// into an unusable pix.
func (c *Config) UnknownKeys() []string { return append([]string(nil), c.unknownKeys...) }

// unknownIn renders BurntSushi's undecoded keys, sorted + deduplicated. A
// toml.Key's String() joins its path with dots, so a nested unknown key renders
// dotted ("inference.typo") and reads the way a user would write it.
func unknownIn(keys []toml.Key) []string {
	seen := map[string]bool{}
	var unknown []string
	for _, k := range keys {
		if s := k.String(); !seen[s] {
			seen[s] = true
			unknown = append(unknown, s)
		}
	}
	sort.Strings(unknown)
	return unknown
}

// configDir resolves the directory that holds config.toml and the broker
// token: PIX_HOME itself (pixhome.Dir). There is NO $PIX_CONFIG, no
// $XDG_CONFIG_HOME, and no ~/.config/pix fallback in production — PIX_HOME
// is the single root every Pix-owned file lives under (AGENTS.md safety
// invariant 1; QA F5 closed the last XDG/PIX_CONFIG escape here).
func configDir() (string, error) {
	return pixhome.Dir()
}

// Path resolves the config file path: <PIX_HOME>/config.toml. No override
// env var of any kind — set $PIX_HOME itself to redirect it (tests do this).
func Path() string {
	dir, err := configDir()
	if err != nil {
		// Fall back to a relative path; Load() will treat a missing file as
		// "use defaults", so this never hard-fails.
		return "config.toml"
	}
	return filepath.Join(dir, "config.toml")
}

// StateDir resolves the per-user state dir: <PIX_HOME>/state. Runtime state
// lives here, never config. No $XDG_STATE_HOME fallback in production.
func StateDir() (string, error) {
	home, err := pixhome.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "state"), nil
}

// DataDir resolves the per-user DATA dir: PIX_HOME itself — the durable root
// for anything that is not runtime state (a hand-authored model catalog
// override, personal context). No $XDG_DATA_HOME fallback in production.
func DataDir() (string, error) {
	return pixhome.Dir()
}

// ContextDir is the always-on, user-authored context layer: <PIX_HOME>/context
// (durable AGENTS.md + skills), personal — team context belongs in a pack.
func ContextDir() string { return filepath.Join(dataDirOr(), "context") }

// dataDirOr returns DataDir() or, if HOME cannot be resolved, a relative
// "pix" so path builders never panic on an empty base.
func dataDirOr() string {
	d, err := DataDir()
	if err != nil {
		return "pix"
	}
	return d
}

// defaults returns a Config with the sane defaults applied to any unset field.
func (c *Config) applyDefaults() {
	if c.Inference.Backends == nil {
		c.Inference.Backends = map[string]InferenceBackend{}
	}
	if c.MemoryWatcherModel == "" {
		c.MemoryWatcherModel = DefaultMemoryWatcherModel
	}
	if c.OllamaBridgeModel == "" {
		c.OllamaBridgeModel = DefaultOllamaBridgeModel
	}
	// Fail closed: absent or garbled both resolve to the default, never to the
	// opt-in mode nobody actually chose.
	if !ValidMemoryCapture(c.MemoryCapture) {
		c.MemoryCapture = DefaultMemoryCapture
	}
	if c.MemoryEmbedModel == "" {
		c.MemoryEmbedModel = DefaultMemoryEmbedModel
	}
	if c.Plugins == nil {
		c.Plugins = map[string]PluginSpec{}
	}
	// The ENTIRE [plugins.*] table is INERT BY DESIGN: a config file can no
	// longer name an executable for the supervisor to run. This is a security
	// boundary, not a deprecation courtesy — it stays whatever else is trimmed.
	// The slots decode into a real field, so they are not "undecoded"; they are
	// recorded here explicitly because a silently-discarded request to run a
	// binary is exactly the thing a user must be told about.
	if len(c.Plugins) > 0 {
		seen := map[string]bool{}
		for _, k := range c.unknownKeys {
			seen[k] = true
		}
		for slot := range c.Plugins {
			if key := "plugins." + slot; !seen[key] {
				c.unknownKeys = append(c.unknownKeys, key)
				seen[key] = true
			}
		}
		c.Plugins = map[string]PluginSpec{}
		sort.Strings(c.unknownKeys)
	}
	c.dropNoncanonicalEnvironments()
}

// dropNoncanonicalEnvironments fails closed on a hand-edited (or otherwise
// corrupted) `[environments]` entry: AddEnvironment is the only writer Save()
// ever exercises, and it never persists a name validEnvironmentName would
// have refused, nor anything but a canonical absolute path — so an unsafe
// NAME (a slash, a traversal segment, a control character, over 128 bytes)
// or a `~`-bearing/relative PATH can only have reached the file by hand.
// Either defect is dropped rather than trusted as a registration, and
// recorded in unknownKeys — the same "tell them" contract a retired
// [plugins.*] slot gets — so the user sees why their edit had no effect. A
// default naming a dropped (or never-registered) name resolves to no default
// rather than a dangling selection.
func (c *Config) dropNoncanonicalEnvironments() {
	if len(c.Environments) == 0 {
		if c.Environment != "" {
			c.Environment = ""
		}
		return
	}
	seen := map[string]bool{}
	for _, k := range c.unknownKeys {
		seen[k] = true
	}
	kept := make(map[string]string, len(c.Environments))
	dropped := false
	for name, path := range c.Environments {
		if validEnvironmentName(name) && IsCanonicalEnvironmentPath(path) {
			kept[name] = path
			continue
		}
		dropped = true
		if key := "environments." + name; !seen[key] {
			c.unknownKeys = append(c.unknownKeys, key)
			seen[key] = true
		}
	}
	c.Environments = kept
	if dropped {
		sort.Strings(c.unknownKeys)
	}
	if c.Environment != "" {
		if _, ok := c.Environments[c.Environment]; !ok {
			c.Environment = ""
		}
	}
}

// Load reads and decodes Path(). If the file is absent it returns a Config
// populated with defaults and a nil error — absence is not an error. Unknown
// keys are tolerated.
func Load() (*Config, error) {
	return LoadFrom(Path())
}

// LoadFrom reads and decodes a config.toml at an EXPLICIT path, for a caller that
// must inspect a file that is not the active one (`restore` validating an archive).
func LoadFrom(path string) (*Config, error) {
	c := &Config{}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			c.applyDefaults()
			return c, nil
		}
		return nil, err
	}
	md, err := toml.DecodeFile(path, c)
	if err != nil {
		return nil, err
	}
	c.unknownKeys = unknownIn(md.Undecoded())
	c.applyDefaults()
	return c, nil
}

// Plugin returns the spec for slot. With the [plugins.*] declaration retired, a
// loaded config always answers builtin here.
func (c *Config) Plugin(slot string) PluginSpec {
	if spec, ok := c.Plugins[slot]; ok {
		if spec.Impl == "" {
			spec.Impl = BuiltinImpl
		}
		return spec
	}
	return PluginSpec{Impl: BuiltinImpl}
}

// OpRefsPath resolves the absolute path of op-refs.env (the 1Password refs file
// the sbx gateway resolves via `op run --env-file`): <PIX_HOME>/op-refs.env.
func OpRefsPath() string {
	dir, err := configDir()
	if err != nil {
		return "op-refs.env"
	}
	return filepath.Join(dir, "op-refs.env")
}

// OpRefsMentalModel is the ≤4-line plain explanation of what op-refs.env is, reused
// VERBATIM in `pix setup`, the `secret` help, and the template header.
const OpRefsMentalModel = `op-refs.env maps ENV_VAR = op://vault/item/field. When the gateway spawns a
host MCP server it resolves those refs from 1Password and injects them as env
vars — the secret never touches disk or the sandbox. A server that needs no
credentials needs no entry here at all.`

// OpRefsTemplate is the seed content for a fresh op-refs.env: op:// references
// ONLY, every example line COMMENTED OUT. There are no vendor examples here on
// purpose — pix ships no built-in MCP server, so the only thing that can tell
// you which ENV_VARs to add is the pack you activate, and it says so in its
// own docs. A template that named vendors would go stale the moment one moved.
const OpRefsTemplate = `# pix op-refs.env — 1Password refs the sbx gateway resolves via
# ` + "`op run --env-file`" + ` when it spawns each host MCP server.
#
# op-refs.env maps ENV_VAR = op://vault/item/field. When the gateway spawns a
# host MCP server it resolves those refs from 1Password and injects them as env
# vars — the secret never touches disk or the sandbox. A server that needs no
# credentials needs no entry here at all.
#
# This file holds op://vault/item/field REFERENCES only. Everything secret
# (tokens, keyring passwords) is an op:// ref resolved from 1Password at spawn
# time — never a pasted secret. A NON-secret value (an account name, a home
# directory) may be a plain literal, but only when the active pack declares
# that variable as env_keys on the integration that needs it.
#
# A freshly-seeded file has zero entries: you add one when you wire a server,
# and your pack's docs name the variable.
#
# Add one:  pix secret set ENV_VAR op://<vault>/<item>/<field>
# Verify:   pix secret check
# Tip:      1Password app -> right-click a field -> "Copy Secret Reference".
`

// SeedOpRefs writes OpRefsTemplate to OpRefsPath() 0600 only if the file is absent,
// creating the config dir 0700. Returns the path and whether it created the file.
func SeedOpRefs() (path string, created bool, err error) {
	path = OpRefsPath()
	created, err = SeedOpRefsAt(path)
	return path, created, err
}

// SeedOpRefsAt is the path-parameterized seeder SeedOpRefs delegates to, with the
// same no-clobber + 0700 dir / 0600 file guarantees.
func SeedOpRefsAt(path string) (created bool, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	// Tighten an existing config dir that may have been created 0755 elsewhere.
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	// Atomic no-clobber: O_CREATE|O_EXCL fails if the file already exists, so we
	// never truncate a populated op-refs.env (no Stat-then-Write TOCTOU window).
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()
	if _, err := f.WriteString(OpRefsTemplate); err != nil {
		// Remove the empty file so future calls can retry (same pattern as Seed()).
		_ = os.Remove(path)
		return false, err
	}
	return true, nil
}
