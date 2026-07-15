// Package config is the pi-stack config schema + loader, shared by the host
// binary (pi-stack-host) AND the launcher binary. Deliberately dependency-light:
// only a TOML decoder — NO sqlite, NO go-plugin — so the launcher stays tiny.
//
// Config lives at $PI_STACK_CONFIG, else $XDG_CONFIG_HOME/pi-stack/config.toml,
// else ~/.config/pi-stack/config.toml. Absence is not an error: Load() returns a
// Config populated with sane defaults so a fresh install works with no file.
package config

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Defaults applied when a config file is absent or a field is unset.
const (
	DefaultMemoryWatcherModel = "gemma4"
	DefaultMemoryEmbedModel   = "nomic-embed-text"
	// BuiltinImpl is the default plugin impl: compiled into the host binary
	// rather than run as an external sub-process.
	BuiltinImpl = "builtin"
)

// DefaultServices is the service set a fresh install runs.
var DefaultServices = []string{"memory"}

// PluginSpec configures one plugin slot: how it is implemented and, for external
// impls, where the binary lives and how it is verified/reached.
type PluginSpec struct {
	Impl string `toml:"impl"` // "builtin" (default) or an external impl name
	Path string `toml:"path"` // path to an external plugin binary
	SHA  string `toml:"sha"`  // expected checksum of the external binary
	Port int    `toml:"port"` // port an external plugin listens on
}

// Profile is a named override set layered onto the base (flat) config so one
// host can run distinct contexts — e.g. WORK vs PERSONAL — that differ in their
// Google Workspace account, MCP servers, knowledge bundles, and overlay kit
// stack, without cross-contaminating. A slice field that is PRESENT (even empty)
// REPLACES the base value; an ABSENT (nil) field INHERITS the base. An empty
// GogAccount inherits the base account.
//
// Only the runtime-swappable surface lives here. Overlay HOST plugins compile
// into the single pi-stack-host binary (build time, global) and cannot be
// swapped per profile — see docs/design/profiles.md.
type Profile struct {
	GogAccount       string   `toml:"gog_account"`
	MCP              []string `toml:"mcp"`
	KnowledgeBundles []string `toml:"knowledge_bundles"`
	Kits             struct {
		Stack []string `toml:"stack"`
	} `toml:"kits"`
}

// Config is the pi-stack configuration, decoded from TOML.
type Config struct {
	VersionPin string   `toml:"version_pin"`
	Services   []string `toml:"services"`
	MCP        []string `toml:"mcp"`

	// ActiveProfile is the profile used when no --profile flag / PI_STACK_PROFILE
	// env is given. Empty means the base config (the implicit "default" profile).
	ActiveProfile string `toml:"active_profile"`
	// Profiles are named override sets layered onto the base config by Resolve.
	Profiles map[string]Profile `toml:"profiles"`

	MemoryWatcherModel string `toml:"memory_watcher_model"`
	MemoryEmbedModel   string `toml:"memory_embed_model"`

	// GogAccount is the Google Workspace account the gog host-MCP server serves.
	// It is the Go-side source of truth doctor probes against; it MUST match the
	// GOG_ACCOUNT in config/local.mk that `make mcp-register` registers with the
	// gateway (the make path can't see this file, so the two must be kept in sync).
	// doctor falls back to the GOG_ACCOUNT env var when this is empty.
	GogAccount string `toml:"gog_account"`

	// KnowledgeBundles are the git-mounted OKF bundle directory path(s) the
	// knowledge service (:11436) indexes at startup. Empty (the default) means no
	// bundles — the index is served empty and the service degrades cleanly.
	KnowledgeBundles []string `toml:"knowledge_bundles"`

	Kits struct {
		Stack []string `toml:"stack"`
	} `toml:"kits"`

	Skills struct {
		Paths []string `toml:"paths"`
	} `toml:"skills"`

	Plugins map[string]PluginSpec `toml:"plugins"`
}

// configDir resolves the directory that holds config.toml and the broker token.
// It honors $PI_STACK_CONFIG's parent when set, then $XDG_CONFIG_HOME/pi-stack,
// then ~/.config/pi-stack.
func configDir() (string, error) {
	if p := os.Getenv("PI_STACK_CONFIG"); p != "" {
		return filepath.Dir(p), nil
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "pi-stack"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "pi-stack"), nil
}

// Path resolves the config file path: $PI_STACK_CONFIG override, else
// <config-dir>/config.toml.
func Path() string {
	if p := os.Getenv("PI_STACK_CONFIG"); p != "" {
		return p
	}
	dir, err := configDir()
	if err != nil {
		// Fall back to a relative path; Load() will treat a missing file as
		// "use defaults", so this never hard-fails.
		return "config.toml"
	}
	return filepath.Join(dir, "config.toml")
}

// removedServices are service names that no longer exist (e.g. gws, which the
// Google Workspace port replaced with the host `gog` MCP server). We drop them
// silently from a loaded config so a stale services list doesn't fatal `serve`.
var removedServices = map[string]bool{"gws": true, "gws-token": true}

// defaults returns a Config with the sane defaults applied to any unset field.
func (c *Config) applyDefaults() {
	if len(c.Services) > 0 {
		kept := c.Services[:0]
		for _, s := range c.Services {
			if !removedServices[s] {
				kept = append(kept, s)
			}
		}
		c.Services = kept
	}
	if len(c.Services) == 0 {
		c.Services = append([]string(nil), DefaultServices...)
	}
	if c.MemoryWatcherModel == "" {
		c.MemoryWatcherModel = DefaultMemoryWatcherModel
	}
	if c.MemoryEmbedModel == "" {
		c.MemoryEmbedModel = DefaultMemoryEmbedModel
	}
	if c.Plugins == nil {
		c.Plugins = map[string]PluginSpec{}
	}
	for slot, spec := range c.Plugins {
		if spec.Impl == "" {
			spec.Impl = BuiltinImpl
			c.Plugins[slot] = spec
		}
	}
}

// Load reads and decodes Path(). If the file is absent it returns a Config
// populated with defaults and a nil error — absence is not an error. Unknown
// keys are tolerated.
func Load() (*Config, error) {
	c := &Config{}
	path := Path()
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			c.applyDefaults()
			return c, nil
		}
		return nil, err
	}
	if _, err := toml.DecodeFile(path, c); err != nil {
		return nil, err
	}
	c.applyDefaults()
	return c, nil
}

// DefaultProfile is the implicit profile name for the base (flat) config.
const DefaultProfile = "default"

// AllKnowledgeBundles returns the de-duplicated UNION of the base bundles and
// every profile's bundles. `serve` indexes the union (one shared index); a
// running sandbox scopes recall to its profile's subset at query time. This
// keeps `serve` profile-agnostic while still indexing everything any profile
// might ask for.
func (c *Config) AllKnowledgeBundles() []string {
	seen := map[string]bool{}
	var out []string
	add := func(list []string) {
		for _, b := range list {
			if b != "" && !seen[b] {
				seen[b] = true
				out = append(out, b)
			}
		}
	}
	add(c.KnowledgeBundles)
	for _, p := range c.Profiles {
		add(p.KnowledgeBundles)
	}
	return out
}

// ProfileNames returns the sorted list of configured profile names, always
// including the implicit "default" first.
func (c *Config) ProfileNames() []string {
	names := []string{DefaultProfile}
	for n := range c.Profiles {
		if n != DefaultProfile {
			names = append(names, n)
		}
	}
	sort.Strings(names[1:])
	return names
}

// Resolve returns a flat *Config with the named profile's overrides layered onto
// the base config, so every existing consumer (run, serve, doctor, status) keeps
// working against a plain flat config. name "" or "default" (or an unknown name)
// returns the base config unchanged. A present slice override REPLACES; an absent
// one INHERITS; a non-empty GogAccount overrides. The returned config is a copy —
// Resolve never mutates the receiver.
func (c *Config) Resolve(name string) *Config {
	out := *c // shallow copy; slices are replaced wholesale below, never appended
	if name == "" || name == DefaultProfile {
		return &out
	}
	p, ok := c.Profiles[name]
	if !ok {
		return &out
	}
	if strings.TrimSpace(p.GogAccount) != "" {
		out.GogAccount = strings.TrimSpace(p.GogAccount)
	}
	if p.MCP != nil {
		out.MCP = append([]string(nil), p.MCP...)
	}
	if p.KnowledgeBundles != nil {
		out.KnowledgeBundles = append([]string(nil), p.KnowledgeBundles...)
	}
	if p.Kits.Stack != nil {
		out.Kits.Stack = append([]string(nil), p.Kits.Stack...)
	}
	return &out
}

// Plugin returns the configured spec for slot, or a builtin default if unset.
func (c *Config) Plugin(slot string) PluginSpec {
	if spec, ok := c.Plugins[slot]; ok {
		if spec.Impl == "" {
			spec.Impl = BuiltinImpl
		}
		return spec
	}
	return PluginSpec{Impl: BuiltinImpl}
}

// defaultConfigTOML is the commented template Seed writes.
const defaultConfigTOML = `# pi-stack config. All fields optional; delete anything you don't override.

# Pin the pi-stack image/version the launcher runs (empty = latest baked).
version_pin = ""

# Host services ` + "`make serve`" + ` runs.
services = ["memory"]

# MCP servers attached at sandbox creation.
mcp = []

# Local Ollama models the memory service uses.
memory_watcher_model = "gemma4"
memory_embed_model = "nomic-embed-text"

# Google Workspace account the gog host-MCP server serves. This is the Go-side
# source of truth pi-stack doctor probes against, and it MUST match the
# GOG_ACCOUNT in config/local.mk that make mcp-register registers with the
# gateway. Empty falls back to the GOG_ACCOUNT env var.
gog_account = ""

# OKF knowledge bundle directories the knowledge service (:11436) indexes.
# Empty = no bundles (index served empty). The knowledge service is opt-in:
# add "knowledge" to the services list above to run it.
knowledge_bundles = []

# Kits stacked onto the sandbox (overlay mixin kits, etc).
[kits]
stack = []

# Profiles: named override sets for running distinct contexts (e.g. work vs
# personal) that differ in gog account, MCP servers, knowledge bundles, and
# overlay kit stack. Select one with ` + "`pi-stack --profile <name> run`" + `,
# ` + "`pi-stack profile use <name>`" + `, or the PI_STACK_PROFILE env var. A
# present list REPLACES the base value; an absent one INHERITS it.
# active_profile = ""
# [profiles.work]
# gog_account = "you@work.com"
# mcp = ["gog", "slack"]
# knowledge_bundles = ["/path/to/work-kb"]
# [profiles.work.kits]
# stack = ["../work-overlay/kit"]

# Extra skill directories loaded live (dev mode).
[skills]
paths = []

# Plugin slots. impl = "builtin" (default) compiles into the host binary;
# an external impl names a path + sha + port.
# [plugins.example]
# impl = "builtin"
# path = ""
# sha  = ""
# port = 0
`

// Save writes the config back to Path() as TOML. It is the write half of the
// repo-less workflow: the CLI (`pi-stack config set`, `pi-stack setup`) mutates a
// loaded Config in memory and calls Save() so the user NEVER hand-edits the file.
// The file is machine-managed (0600, dir 0700); the commented template Seed
// writes is only a first-touch convenience, so losing comments on a rewrite is
// acceptable.
func (c *Config) Save() error {
	path := Path()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(c); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o600)
}

// SetGogAccount sets the Google Workspace account (trimmed). An empty value
// clears it.
func (c *Config) SetGogAccount(account string) { c.GogAccount = strings.TrimSpace(account) }

// AddMCP adds name to the MCP set if absent, returning true when it changed.
func (c *Config) AddMCP(name string) bool { return addUnique(&c.MCP, name) }

// RemoveMCP removes name from the MCP set, returning true when it changed.
func (c *Config) RemoveMCP(name string) bool { return removeValue(&c.MCP, name) }

// AddService adds name to the Services set if absent, returning true when it
// changed.
func (c *Config) AddService(name string) bool { return addUnique(&c.Services, name) }

// RemoveService removes name from the Services set, returning true when it
// changed.
func (c *Config) RemoveService(name string) bool { return removeValue(&c.Services, name) }

// AddKnowledgeBundle adds an OKF bundle directory to the KnowledgeBundles set if
// absent, returning true when it changed. The path is canonicalized to an
// absolute path (best-effort — a path that can't be resolved is trimmed and
// used as-is) so the same bundle referenced two ways doesn't get indexed twice.
func (c *Config) AddKnowledgeBundle(path string) bool {
	return addUnique(&c.KnowledgeBundles, canonicalizeBundlePath(path))
}

// RemoveKnowledgeBundle removes an OKF bundle directory from the
// KnowledgeBundles set, returning true when it changed. The path is
// canonicalized the same way AddKnowledgeBundle canonicalizes so a bundle added
// by a relative path can be removed by that same relative path.
func (c *Config) RemoveKnowledgeBundle(path string) bool {
	return removeValue(&c.KnowledgeBundles, canonicalizeBundlePath(path))
}

// canonicalizeBundlePath normalizes a bundle path to the SAME canonical id every
// other writer produces. It MUST match the knowledge store's canonicalizeBundle
// (services/host/knowledge.go) and the launcher's canonicalizeKnowledgeBundle
// (cmd/pi-stack/knowledge.go) byte-for-byte in behavior — otherwise a symlink
// spelling vs the real path yields two config entries and remove-by-real-path
// can't drop a symlink entry (F6). The algorithm: trim, then abs ->
// EvalSymlinks -> Clean, with a cleaned-abs fallback when the path doesn't exist
// (so EvalSymlinks fails). An empty (or whitespace-only) path stays empty.
func canonicalizeBundlePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		return resolved
	}
	return filepath.Clean(abs)
}

// addUnique appends value to *list if it is not already present, returning true
// when the list changed.
func addUnique(list *[]string, value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	for _, v := range *list {
		if v == value {
			return false
		}
	}
	*list = append(*list, value)
	return true
}

// removeValue drops every occurrence of value from *list, returning true when
// the list changed.
func removeValue(list *[]string, value string) bool {
	value = strings.TrimSpace(value)
	kept := (*list)[:0]
	changed := false
	for _, v := range *list {
		if v == value {
			changed = true
			continue
		}
		kept = append(kept, v)
	}
	*list = kept
	return changed
}

// OpRefsPath resolves the absolute XDG path of op-refs.env (the 1Password refs
// file the sbx gateway resolves via `op run --env-file`), a sibling of
// config.toml: <config-dir>/op-refs.env. Repo-less hosts have no repo
// config/op-refs.env, so this is the canonical, absolute location.
func OpRefsPath() string {
	dir, err := configDir()
	if err != nil {
		return "op-refs.env"
	}
	return filepath.Join(dir, "op-refs.env")
}

// OpRefsTemplate is the seed content for a fresh op-refs.env: the 1Password refs
// each host MCP server needs, with placeholders to fill. Kept in sync with the
// repo's config/op-refs.env.example (which the make path uses).
const OpRefsTemplate = `# pi-stack op-refs.env — 1Password refs the sbx gateway resolves via
# ` + "`op run --env-file`" + ` when it spawns each host MCP server. Credentials live
# ONLY in 1Password — never in the sbx registration, on disk, or in the sandbox.
#
# Format:  ENV_VAR=op://<vault>/<item>/<field>     (plain literals are allowed too)
# Verify:  op read "op://Vault/Item/field" >/dev/null && echo OK
# Tip:     1Password app -> right-click a field -> "Copy Secret Reference".

# slack MCP server (its bot/user token). Required to register slack.
SLACK_TOKEN=op://<vault>/<slack-item>/credential

# gog (Google Workspace) MCP server. gog only needs op to inject a headless
# keyring password; a keyring reachable without a password does not need this.
# Uncomment + fill in only if the gateway can't unlock gog's keyring headlessly.
# GOG_ACCOUNT=you@example.com
# GOG_HOME=$HOME/.config/gog
# GOG_KEYRING_BACKEND=file
# GOG_KEYRING_PASSWORD=op://<vault>/<gog-item>/keyring-password
`

// SeedOpRefs writes OpRefsTemplate to OpRefsPath() with 0600 perms only if the
// file is absent, creating the config dir (0700) as needed. It returns the
// resolved path, whether it created the file (false if it already existed), and
// any error. It never clobbers an existing op-refs.env.
func SeedOpRefs() (path string, created bool, err error) {
	path = OpRefsPath()
	if _, statErr := os.Stat(path); statErr == nil {
		return path, false, nil
	} else if !os.IsNotExist(statErr) {
		return path, false, statErr
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return path, false, err
	}
	if err := os.WriteFile(path, []byte(OpRefsTemplate), 0o600); err != nil {
		return path, false, err
	}
	return path, true, nil
}

// Seed writes a commented default config.toml at path only if absent. It returns
// false (and a nil error) if the file already existed — it never clobbers.
func Seed(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	if err := os.WriteFile(path, []byte(defaultConfigTOML), 0o644); err != nil {
		return false, err
	}
	return true, nil
}
