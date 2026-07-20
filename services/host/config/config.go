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
	// A small, fast, extraction-grade local model DEDICATED to fact capture. It is
	// deliberately decoupled from the (bigger) ollama-bridge/router model: fact
	// extraction is a bounded task that does not need a 9b, and a 9b cold-load was
	// the cause of watcher timeouts. gemma4:e4b-mlx is small + MLX-accelerated on
	// Apple Silicon; warm-on-start (memWatcherWarm) keeps the first capture fast.
	// Override via `pi-stack config set memory_watcher_model <model>` (e.g.
	// smolstruct:1.7b or osmosis-structure:0.6b on non-Apple hardware).
	DefaultMemoryWatcherModel = "gemma4:e4b-mlx"
	DefaultMemoryEmbedModel   = "nomic-embed-text"
	// DefaultOllamaBridgeModel is the local model the sandbox's ollama-bridge
	// exposes to pi (the interactive Alt+P cycle) AND the router's local option.
	// It loads on demand (not resident), so it can be bigger than the watcher;
	// qwen3.5:9b (~6.6GB) is the current all-rounder that still fits a 16GB box.
	DefaultOllamaBridgeModel = "qwen3.5:9b"
	// BuiltinImpl is the default plugin impl: compiled into the host binary
	// rather than run as an external sub-process.
	BuiltinImpl = "builtin"
)

// DefaultServices is the service set a fresh install runs.
var DefaultServices = []string{"memory"}

// PluginSpec configures one plugin slot: how it is implemented and, for external
// impls, where the binary lives and how it is verified/reached.
type PluginSpec struct {
	Impl     string   `toml:"impl"`      // "builtin" (default) or an external impl name
	Path     string   `toml:"path"`      // path to an external plugin binary
	SHA      string   `toml:"sha"`       // expected checksum of the external binary
	Port     int      `toml:"port"`      // port an external plugin listens on
	ExtraEnv []string `toml:"extra_env"` // additional env vars granted to this plugin subprocess
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
//
// The slice fields are POINTERS so the schema can distinguish three states that
// a plain slice cannot: a nil pointer = INHERIT (omitted on Save via omitempty),
// while a non-nil pointer — even to an empty slice — = an explicit REPLACE that
// IS serialized (including `mcp = []`). This is what lets `RemoveProfileMCP`
// disable the last inherited server: it stores a present-empty list that survives
// Save+Load instead of a nil that would revert to inherit. A plain `[]string`
// with `omitempty` could not tell present-empty from absent, so do NOT flatten
// these back to non-pointer slices — the round-trip tests gate them. GogAccount
// stays a string: empty = inherit.
type Profile struct {
	GogAccount       string    `toml:"gog_account,omitempty"`
	MCP              *[]string `toml:"mcp,omitempty"`
	KnowledgeBundles *[]string `toml:"knowledge_bundles,omitempty"`
	Kits             struct {
		Stack *[]string `toml:"stack,omitempty"`
	} `toml:"kits,omitempty"`
}

// HostMode gates `pi-stack host` — running pi DIRECTLY on this machine with
// no sandbox, no network fence, and real credentials. Enabled is default-OFF
// on purpose: the friction of `pi-stack config set host.enabled true` is the
// deliberate opt-in (see docs/design/host-mode.md). Autonomy is RESERVED for a
// future knob on the host-guard extension's strictness; Phase 1 stores it but
// nothing reads it yet.
type HostMode struct {
	Enabled  bool   `toml:"enabled"`
	Autonomy string `toml:"autonomy,omitempty"`
	// Autoserve gates the launcher's LAZY AUTO-START of `pi-stack-host serve`
	// (docs/design/serve-lifecycle.md §1). nil = default TRUE (auto-start on).
	// A pointer so "unset" (inherit the default, follow future default changes)
	// is distinguishable from an explicit `false`. The PI_STACK_NO_AUTOSERVE env
	// var wins over this flag.
	Autoserve *bool `toml:"autoserve,omitempty"`
}

// AutoserveEnabled reports whether lazy auto-start is enabled (default true).
func (c *Config) AutoserveEnabled() bool {
	return c.Host.Autoserve == nil || *c.Host.Autoserve
}

// Config is the pi-stack configuration, decoded from TOML.
type Config struct {
	VersionPin string `toml:"version_pin"`

	// Services is the RESOLVED runtime service set every consumer reads
	// (serve, ensureServe, doctor, …). It is never (de)serialized directly:
	// ServicesRaw below is the TOML-facing tri-state field, and applyDefaults /
	// sparseForSave translate between the two. Services has a NON-EMPTY default
	// (DefaultServices), so unlike mcp/knowledge_bundles a plain `[]string` with
	// omitempty cannot distinguish "unset → default" from "explicitly empty →
	// stays empty": `config unset services memory` used to report [] but reload
	// silently restored ["memory"], losing intent and triggering a spurious
	// daemon restart (H2). The pointer carries the missing presence bit, the
	// same trick the per-profile slices use.
	Services []string `toml:"-"`
	// ServicesRaw is the TOML image of Services: nil = the key was ABSENT from
	// the file (resolve to DefaultServices); non-nil — even pointing at an empty
	// slice — = the key was PRESENT and is authoritative (`services = []` stays
	// empty). Do not read it outside applyDefaults/sparseForSave; read Services.
	ServicesRaw *[]string `toml:"services,omitempty"`

	MCP []string `toml:"mcp,omitempty"`

	// ActiveProfile is the profile used when no --profile flag / PI_STACK_PROFILE
	// env is given. Empty means the base config (the implicit "default" profile).
	ActiveProfile string `toml:"active_profile"`
	// Profiles are named override sets layered onto the base config by Resolve.
	Profiles map[string]Profile `toml:"profiles"`

	MemoryWatcherModel string `toml:"memory_watcher_model,omitempty"`
	MemoryEmbedModel   string `toml:"memory_embed_model,omitempty"`
	OllamaBridgeModel  string `toml:"ollama_bridge_model,omitempty"`

	// GogAccount is the Google Workspace account the gog host-MCP server serves.
	// It is THE source of truth: doctor probes against it, and `make mcp-register`
	// sources it via `pi-stack config get gog_account` when registering with the
	// gateway. doctor falls back to the GOG_ACCOUNT env var when this is empty.
	GogAccount string `toml:"gog_account"`

	// KnowledgeBundles are the git-mounted OKF bundle directory path(s) the
	// knowledge service (:11436) indexes at startup. Empty (the default) means no
	// bundles — the index is served empty and the service degrades cleanly.
	KnowledgeBundles []string `toml:"knowledge_bundles,omitempty"`

	Kits struct {
		Stack []string `toml:"stack"`
	} `toml:"kits"`

	Skills struct {
		Paths []string `toml:"paths"`
	} `toml:"skills"`

	// Pack is the active pack: a git-backed directory carrying skills + knowledge
	// (+ later mcp/proxies/routing/config). Empty = no active pack. `pi-stack pack
	// use <path>` sets it; `run` mounts the pack's skills + knowledge. This is the
	// unifying successor to the loose skills-dir + knowledge_bundles + (eventually)
	// profile. See docs/design/packs.md.
	Pack string `toml:"pack,omitempty"`

	Plugins map[string]PluginSpec `toml:"plugins"`

	// Host gates + configures `pi-stack host` (the unsandboxed escape hatch).
	// GLOBAL, never per-profile: leaving the sandbox is a machine-level decision.
	Host HostMode `toml:"host,omitempty"`
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

// ServePidPath resolves the absolute path of serve.pid — the pidfile
// `pi-stack-host serve` writes on startup so the launcher's `serve stop` /
// `serve status` can find and signal the running supervisor SAFELY (instead of a
// blind `pkill -f`). It lives in the STATE dir (<state-dir>/serve.pid), NOT the
// config dir: it is ephemeral runtime state (like serve.log), not user config.
// Keeping it out of the config dir also means `pi-stack reset` (which moves the
// config dir aside) never orphans a running daemon from its pidfile. Both the
// host (writer) and the launcher (readers) call this so the two always agree.
func ServePidPath() string {
	dir, err := StateDir()
	if err != nil {
		return "serve.pid"
	}
	return filepath.Join(dir, "serve.pid")
}

// ServeSpawnLockPath is the flock file the launcher's lazy auto-start takes
// around its spawn decision (double-checked locking against a concurrent
// `pi-stack run`). Ephemeral runtime state — a sibling of the pidfile in the
// STATE dir.
func ServeSpawnLockPath() string {
	dir, err := StateDir()
	if err != nil {
		return "serve.spawn.lock"
	}
	return filepath.Join(dir, "serve.spawn.lock")
}

// ServeLazyMarkerPath is the marker file the launcher writes after a successful
// LAZY detached spawn of `pi-stack-host serve`, so config propagation can tell
// a lazy daemon (safe to stop-and-restart) from a FOREGROUND one the user is
// watching (never killed, only advised). Cleared by `serve stop` and by the
// daemon's graceful shutdown; a stale marker is harmless because mode detection
// also requires a live, verified-ours pidfile. Ephemeral runtime state — a
// sibling of the pidfile in the STATE dir.
func ServeLazyMarkerPath() string {
	dir, err := StateDir()
	if err != nil {
		return "serve.lazy"
	}
	return filepath.Join(dir, "serve.lazy")
}

// StateDir resolves the per-user state dir: $XDG_STATE_HOME/pi-stack, else
// ~/.local/state/pi-stack. Used for logs (NOT config): serve.log lives here on
// BOTH macOS and Linux, and every launch mode writes to it — the lazy
// auto-start, the managed launchd LaunchAgent (StandardOutPath/
// StandardErrorPath), and the managed systemd --user unit (StandardOutput=/
// StandardError=append:) all point at the SAME file (ServeLogPath()), so
// there is exactly one place to look regardless of how serve was started.
// Only a FOREGROUND `pi-stack serve` is different — that one is interactive
// and goes to its own terminal, not this file.
func StateDir() (string, error) {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "pi-stack"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "pi-stack"), nil
}

// ServeLogPath is <state-dir>/serve.log — where a lazily auto-started
// `pi-stack-host serve` writes its stdout/stderr.
func ServeLogPath() string {
	dir, err := StateDir()
	if err != nil {
		return "serve.log"
	}
	return filepath.Join(dir, "serve.log")
}

// DataDir resolves the per-user DATA dir: $XDG_DATA_HOME/pi-stack, else
// ~/.local/share/pi-stack. This is the durable data root — the captured memory
// store, the knowledge index, backups, and routing overrides all live under it,
// as distinct from StateDir (ephemeral: logs, pidfiles) and configDir
// (config.toml). It is the single source of truth for the data root; every
// default below is derived from it so there is no second copy to drift.
func DataDir() (string, error) {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "pi-stack"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "pi-stack"), nil
}

// PackDir is the per-user PERSONAL PACK root: $XDG_DATA_HOME/pi-stack/pack, else
// ~/.local/share/pi-stack/pack. A proper pack (pack.toml + skills/ + knowledge/),
// git-initialized, the default home for what you author for yourself. The active
// pack (config `pack`) overrides it; `pi-stack reset` moves it aside (it's a git
// working copy — the user pushes it to their own remote). See docs/design/packs.md.
func PackDir() string { return filepath.Join(dataDirOr(), "pack") }

// PacksDir is where adopted REMOTE packs are cloned:
// $XDG_DATA_HOME/pi-stack/packs, else ~/.local/share/pi-stack/packs. Each lives
// at <PacksDir>/<name>. Distinct from PackDir (the single personal pack).
func PacksDir() string { return filepath.Join(dataDirOr(), "packs") }

// dataDirOr returns DataDir() or, if HOME cannot be resolved, a relative
// "pi-stack" so path builders never panic on an empty base.
func dataDirOr() string {
	d, err := DataDir()
	if err != nil {
		return "pi-stack"
	}
	return d
}

// MemoryDBPath resolves the live memory sqlite path: $MEMORY_DB if set, else
// <data-dir>/memory/memory.db. Shared by the daemon and `restore` so both point
// at the SAME store (and the SAME lock dir, below).
func MemoryDBPath() string {
	if p := strings.TrimSpace(os.Getenv("MEMORY_DB")); p != "" {
		return p
	}
	return filepath.Join(dataDirOr(), "memory", "memory.db")
}

// KnowledgeDBPath resolves the knowledge index sqlite path: $KNOWLEDGE_DB if
// set, else <data-dir>/knowledge/knowledge.db. The index is rebuildable from the
// OKF bundle; it lives beside the memory store under the data root.
func KnowledgeDBPath() string {
	if p := strings.TrimSpace(os.Getenv("KNOWLEDGE_DB")); p != "" {
		return p
	}
	return filepath.Join(dataDirOr(), "knowledge", "knowledge.db")
}

// BackupsDir is <data-dir>/backups — the default destination for
// `pi-stack state backup` archives.
func BackupsDir() string {
	return filepath.Join(dataDirOr(), "backups")
}

// MemoryLockPath is the advisory flock file the memory daemon and `restore` both
// take to be mutually exclusive around the sqlite store. It sits next to the
// memory db (honoring MEMORY_DB's dir) as .memory.lock, so both processes
// resolve the SAME path and the lock is the authority that closes the port-probe
// TOCTOU (the daemon opens the db BEFORE binding its port).
func MemoryLockPath() string {
	return filepath.Join(filepath.Dir(MemoryDBPath()), ".memory.lock")
}

// removedServices are service names that no longer exist (e.g. gws, which the
// Google Workspace port replaced with the host `gog` MCP server). We drop them
// silently from a loaded config so a stale services list doesn't fatal `serve`.
var removedServices = map[string]bool{"gws": true, "gws-token": true}

// defaults returns a Config with the sane defaults applied to any unset field.
func (c *Config) applyDefaults() {
	// services is TRI-STATE (see the ServicesRaw field doc): absent key →
	// DefaultServices; present key (even `services = []`) → authoritative.
	if c.ServicesRaw != nil {
		kept := make([]string, 0, len(*c.ServicesRaw))
		dropped := false
		for _, s := range *c.ServicesRaw {
			if removedServices[s] {
				dropped = true
				continue
			}
			kept = append(kept, s)
		}
		c.Services = kept
		// A list that became empty ONLY because every entry was a removed service
		// (a stale `services = ["gws"]`) falls back to defaults — that user never
		// chose an empty set. A file-explicit `services = []` (nothing dropped)
		// stays empty: that IS the user's intent.
		if len(c.Services) == 0 && dropped {
			c.Services = append([]string(nil), DefaultServices...)
		}
	} else if len(c.Services) == 0 {
		c.Services = append([]string(nil), DefaultServices...)
	}
	// Keep the TOML image aliased to the resolved field so `config show`'s
	// whole-struct encode still prints a services line. Save never trusts this
	// alias — sparseForSave recomputes it from Services.
	c.ServicesRaw = &c.Services
	if c.MemoryWatcherModel == "" {
		c.MemoryWatcherModel = DefaultMemoryWatcherModel
	}
	if c.OllamaBridgeModel == "" {
		c.OllamaBridgeModel = DefaultOllamaBridgeModel
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
	return LoadFrom(Path())
}

// LoadFrom reads and decodes a config.toml at an EXPLICIT path (rather than the
// resolved Path()). It is used by callers that must inspect a config file which
// is not the active one — e.g. `restore` reading the config.toml just written
// back from a backup archive to report the profiles it now carries. Absence is
// not an error: it returns defaults, matching Load().
func LoadFrom(path string) (*Config, error) {
	c := &Config{}
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
		if p.KnowledgeBundles != nil {
			add(*p.KnowledgeBundles)
		}
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
		out.MCP = append([]string(nil), *p.MCP...)
	}
	if p.KnowledgeBundles != nil {
		out.KnowledgeBundles = append([]string(nil), *p.KnowledgeBundles...)
	}
	if p.Kits.Stack != nil {
		out.Kits.Stack = append([]string(nil), *p.Kits.Stack...)
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
memory_watcher_model = "qwen3.5:9b"
memory_embed_model = "nomic-embed-text"
ollama_bridge_model = "qwen3.5:9b"

# Google Workspace account the gog host-MCP server serves. This is the single
# source of truth: pi-stack doctor probes against it, and make mcp-register
# sources it via pi-stack config get gog_account. Empty falls back to the
# GOG_ACCOUNT env var.
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
//
// Save persists ONLY explicit deviations from the defaults, never resolved
// defaults. Load()/applyDefaults fills every unset defaultable field in MEMORY
// (so cfg.MemoryWatcherModel etc. stay populated for every reader), and a naive
// whole-struct encode would then FREEZE the then-current defaults into the file
// on the first `config set <anything>` — permanently pinning the user to (say)
// a stale memory_watcher_model they never chose, defeating every future default
// change. sparseForSave resets each defaultable field that still equals its
// current default back to the zero value, and the `,omitempty` toml tags drop
// it from the file, so an untouched default re-resolves on every Load.
//
// Tradeoff (deliberate): a value the user sets EQUAL to the current default is
// indistinguishable from an untouched one, so it is omitted and will re-resolve
// to whatever the default is THEN — the user cannot pin against a future
// default change by setting today's default explicitly. Acceptable: the value
// is identical until the default moves, and pinning-the-default is a far rarer
// intent than the petrification bug this prevents.
func (c *Config) Save() error {
	path := Path()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(c.sparseForSave()); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o600)
}

// sparseForSave returns a shallow copy with every defaultable field that equals
// its current default reset to the zero value, so Save (via `,omitempty`)
// writes only explicit deviations. mcp and knowledge_bundles default to EMPTY,
// so plain omitempty already covers them; services must be compared against
// DefaultServices, and the model strings against their Default consts. The
// receiver is never mutated — callers keep reading resolved values.
func (c *Config) sparseForSave() *Config {
	sp := *c
	// services: an untouched default is omitted (nil raw); ANY deviation —
	// including an explicitly-empty list — is serialized. The toml encoder
	// writes a non-nil pointer to an empty slice as `services = []`, which
	// Load reads back as present-empty (stays empty), closing the
	// remove-last-service round-trip hole (H2).
	if stringSlicesEqual(sp.Services, DefaultServices) {
		sp.ServicesRaw = nil
	} else {
		// make (not append-to-nil): a pointer to a NIL slice is dropped by the
		// encoder's omitempty, but a pointer to an allocated EMPTY slice encodes
		// as `services = []` — exactly the presence bit we need.
		explicit := make([]string, len(sp.Services))
		copy(explicit, sp.Services)
		sp.ServicesRaw = &explicit
	}
	if sp.MemoryWatcherModel == DefaultMemoryWatcherModel {
		sp.MemoryWatcherModel = ""
	}
	if sp.MemoryEmbedModel == DefaultMemoryEmbedModel {
		sp.MemoryEmbedModel = ""
	}
	if sp.OllamaBridgeModel == DefaultOllamaBridgeModel {
		sp.OllamaBridgeModel = ""
	}
	// applyDefaults allocates an empty Plugins map (and fills Impl=builtin) so
	// readers never nil-check; don't petrify that resolution either.
	if len(sp.Plugins) == 0 {
		sp.Plugins = nil
	}
	return &sp
}

// stringSlicesEqual reports element-wise equality (order-sensitive).
func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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

// profile fetches (or lazily creates) the named profile entry so a mutator can
// modify it and reassign. Map values are not addressable, so every per-profile
// mutator is a get-modify-set: profileEntry -> mutate the struct -> putProfile.
// Creating a profile that did not exist is intentional: `pi-stack config set
// --profile <new> ...` scaffolds the [profiles.<new>] table.
func (c *Config) profileEntry(name string) Profile {
	if c.Profiles == nil {
		return Profile{}
	}
	return c.Profiles[name]
}

// putProfile writes p back into the profile map, allocating the map on first use.
func (c *Config) putProfile(name string, p Profile) {
	if c.Profiles == nil {
		c.Profiles = map[string]Profile{}
	}
	c.Profiles[name] = p
}

// SetProfileGogAccount sets (or, with an empty value, clears) the Google
// Workspace account override on the named profile, creating the profile if
// absent.
func (c *Config) SetProfileGogAccount(name, account string) {
	p := c.profileEntry(name)
	p.GogAccount = strings.TrimSpace(account)
	c.putProfile(name, p)
}

// The per-profile list mutators below operate on the RESOLVED EFFECTIVE list
// (base overlaid with any existing profile override), NOT on the raw override
// slice. So unsetting an INHERITED value materializes base-minus-value as the
// profile's explicit list (e.g. base [gog,slack], no prior work override,
// `unset --profile work mcp slack` -> work.mcp=[gog]); and adding starts from
// what the profile effectively sees today. They store the full explicit list as
// the override only when it CHANGED, so a no-op leaves an inheriting field nil
// (still inherit) rather than silently materializing it.

// effectiveList returns a fresh COPY of the named profile's effective value for
// one field, via Resolve so it reflects base+override. The copy is essential:
// Resolve shallow-copies the config, so an inherited field aliases the base
// slice — mutating it in place would corrupt the base.
func (c *Config) effectiveList(name string, pick func(*Config) []string) []string {
	return append([]string(nil), pick(c.Resolve(name))...)
}

// AddProfileMCP adds an MCP server to the named profile's effective mcp list,
// storing the result as the profile's explicit override. Returns true when it
// changed. Creates the profile if absent.
func (c *Config) AddProfileMCP(name, server string) bool {
	eff := c.effectiveList(name, func(rc *Config) []string { return rc.MCP })
	if !addUnique(&eff, server) {
		return false
	}
	p := c.profileEntry(name)
	p.MCP = &eff
	c.putProfile(name, p)
	return true
}

// RemoveProfileMCP removes an MCP server from the named profile's EFFECTIVE mcp
// list (base+override), storing the remainder as the profile's explicit
// override. Returns true when it changed — so removing an inherited value
// materializes base-minus-value.
func (c *Config) RemoveProfileMCP(name, server string) bool {
	eff := c.effectiveList(name, func(rc *Config) []string { return rc.MCP })
	if !removeValue(&eff, server) {
		return false
	}
	p := c.profileEntry(name)
	// Store a non-nil pointer even when eff is now empty: removing the LAST
	// inherited value must persist an explicit empty list (`mcp = []`), NOT a nil
	// that Save would drop and Load would read back as inherit.
	p.MCP = &eff
	c.putProfile(name, p)
	return true
}

// AddProfileKnowledgeBundle adds a canonicalized OKF bundle dir to the named
// profile's effective knowledge_bundles list, storing the result as the
// profile's explicit override. Returns true when it changed. Creates the
// profile if absent.
func (c *Config) AddProfileKnowledgeBundle(name, path string) bool {
	eff := c.effectiveList(name, func(rc *Config) []string { return rc.KnowledgeBundles })
	if !addUnique(&eff, canonicalizeBundlePath(path)) {
		return false
	}
	p := c.profileEntry(name)
	p.KnowledgeBundles = &eff
	c.putProfile(name, p)
	return true
}

// RemoveProfileKnowledgeBundle removes a canonicalized OKF bundle dir from the
// named profile's EFFECTIVE knowledge_bundles list, storing the remainder as the
// profile's explicit override. Returns true when it changed.
func (c *Config) RemoveProfileKnowledgeBundle(name, path string) bool {
	eff := c.effectiveList(name, func(rc *Config) []string { return rc.KnowledgeBundles })
	if !removeValue(&eff, canonicalizeBundlePath(path)) {
		return false
	}
	p := c.profileEntry(name)
	p.KnowledgeBundles = &eff
	c.putProfile(name, p)
	return true
}

// AddProfileKit adds an overlay kit path to the named profile's effective
// kits.stack list, storing the result as the profile's explicit override.
// Returns true when it changed. Creates the profile if absent.
func (c *Config) AddProfileKit(name, kit string) bool {
	eff := c.effectiveList(name, func(rc *Config) []string { return rc.Kits.Stack })
	if !addUnique(&eff, kit) {
		return false
	}
	p := c.profileEntry(name)
	p.Kits.Stack = &eff
	c.putProfile(name, p)
	return true
}

// RemoveProfileKit removes an overlay kit path from the named profile's
// EFFECTIVE kits.stack list, storing the remainder as the profile's explicit
// override. Returns true when it changed.
func (c *Config) RemoveProfileKit(name, kit string) bool {
	eff := c.effectiveList(name, func(rc *Config) []string { return rc.Kits.Stack })
	if !removeValue(&eff, kit) {
		return false
	}
	p := c.profileEntry(name)
	p.Kits.Stack = &eff
	c.putProfile(name, p)
	return true
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

// HostRefsPath resolves the absolute XDG path of hostmode.env — the OPTIONAL
// 1Password refs file `pi-stack host` resolves via `op run --env-file` for
// host-mode provider keys (e.g. ANTHROPIC_API_KEY=op://vault/item/field). Same
// mental model as op-refs.env: refs only, never a value on disk. Absent file =
// Ollama-only host mode (valid, no cloud key).
func HostRefsPath() string {
	dir, err := configDir()
	if err != nil {
		return "hostmode.env"
	}
	return filepath.Join(dir, "hostmode.env")
}

// OpRefsMentalModel is the ≤4-line plain explanation of what op-refs.env is and
// how the gateway uses it. Reused VERBATIM in `pi-stack setup`, the `secret`
// help, and the template header so the concept is described identically
// everywhere. Keep it in sync if you change any copy.
const OpRefsMentalModel = `op-refs.env maps ENV_VAR = op://vault/item/field. When the gateway spawns a
host MCP server it resolves those refs from 1Password and injects them as env
vars — the secret never touches disk or the sandbox. A server with no creds
(pio) needs no entry.`

// NonSecretOpRefsKeys is the documented allowlist of NON-secret env vars that may
// appear in op-refs.env with a literal value; everything else must be an op://
// vault/item/field REFERENCE. These configure gog's headless keyring +
// account/home; the keyring PASSWORD is a secret and must still be an op:// ref,
// so it is DELIBERATELY not listed here.
var NonSecretOpRefsKeys = map[string]bool{
	"GOG_ACCOUNT":         true,
	"GOG_HOME":            true,
	"GOG_KEYRING_BACKEND": true,
}

// OpRefsTemplate is the seed content for a fresh op-refs.env: op:// references
// ONLY (plus the documented non-secret env allowlist), with generic placeholders
// to fill. Every example line is COMMENTED OUT so a freshly-seeded file has ZERO
// active entries — the user uncomments (or adds) a line only when wiring a
// server. Kept in sync with the repo's config/op-refs.env.example (which the
// make path uses). Its header repeats OpRefsMentalModel verbatim.
const OpRefsTemplate = `# pi-stack op-refs.env — 1Password refs the sbx gateway resolves via
# ` + "`op run --env-file`" + ` when it spawns each host MCP server.
#
# ` + OpRefsMentalModel + `
#
# This file holds op://vault/item/field REFERENCES only, plus the documented
# non-secret env allowlist (GOG_ACCOUNT, GOG_HOME, GOG_KEYRING_BACKEND).
# Everything secret (tokens, keyring passwords) is an op:// ref resolved from
# 1Password at spawn time — never a pasted secret.
#
# Every line below is COMMENTED OUT: a freshly-seeded file has zero active
# entries. Uncomment + fill in a line only when you wire that server.
#
# Verify:  op read "op://<vault>/<item>/<field>" >/dev/null && echo OK
# Tip:     1Password app -> right-click a field -> "Copy Secret Reference".

# slack MCP server (its bot/user token). Required to register slack.
# SLACK_TOKEN=op://<vault>/<item>/<field>

# gog (Google Workspace) MCP server. gog only needs op to inject a headless
# keyring password; a keyring reachable without a password does not need this.
# Uncomment + fill in only if the gateway can't unlock gog's keyring headlessly.
# GOG_ACCOUNT=you@example.com
# GOG_HOME=$HOME/.config/gog
# GOG_KEYRING_BACKEND=file
# GOG_KEYRING_PASSWORD=op://<vault>/<item>/<field>
`

// SeedOpRefs writes OpRefsTemplate to OpRefsPath() with 0600 perms only if the
// file is absent, creating the config dir (0700) as needed. It returns the
// resolved path, whether it created the file (false if it already existed), and
// any error. It never clobbers an existing op-refs.env. It is the ONE seeder:
// setup and `pi-stack mcp register` both route through it (via SeedOpRefsAt) so
// the template + 0700/0600 perms + no-clobber rule live in a single place.
func SeedOpRefs() (path string, created bool, err error) {
	path = OpRefsPath()
	created, err = SeedOpRefsAt(path)
	return path, created, err
}

// SeedOpRefsAt is the path-parameterized seeder SeedOpRefs delegates to (so a
// caller that resolves op-refs.env through an injected env can reuse the exact
// same no-clobber + 0700 dir / 0600 file guarantees). It writes OpRefsTemplate
// to path only if the file is absent, creating the parent dir (0700) as needed,
// and never clobbers an existing file.
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

// Seed writes a commented default config.toml at path only if absent. It returns
// false (and a nil error) if the file already existed — it never clobbers.
// Uses O_CREATE|O_EXCL for an atomic no-clobber write (no Stat-then-Write TOCTOU
// window), matching the SeedOpRefsAt() pattern (L-1).
func Seed(path string) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	// Tighten an existing config dir that may have been created 0755 elsewhere,
	// matching SeedOpRefsAt() (F5).
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()
	if _, err := f.WriteString(defaultConfigTOML); err != nil {
		// Remove the empty file so future Seed() calls can retry rather than
		// hitting os.IsExist and silently returning (false, nil).
		_ = os.Remove(path)
		return false, err
	}
	return true, nil
}
