// Package config is the pix config schema + loader, shared by the host
// binary (pix-host) AND the launcher binary. Deliberately dependency-light:
// only a TOML decoder — NO sqlite, NO go-plugin — so the launcher stays tiny.
//
// Config lives at $PIX_CONFIG, else $XDG_CONFIG_HOME/pix/config.toml,
// else ~/.config/pix/config.toml. Absence is not an error: Load() returns a
// Config populated with sane defaults so a fresh install works with no file.
package config

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Defaults applied when a config file is absent or a field is unset.
const (
	// A small, fast, extraction-grade local model DEDICATED to fact capture. It is
	// The watcher must reliably emit STRUCTURED JSON (facts/events/corrections).
	// The previous default (gemma4:e4b-mlx) could not — it returned unparseable
	// output on every extraction, so auto-capture silently stored nothing. A capable
	// instruct model that honors ollama structured outputs is required; qwen3.5:9b
	// works. warm-on-start (memWatcherWarm) keeps the first capture fast. Override
	// via `pix config set memory_watcher_model <model>` if you have a smaller
	// model that reliably does structured extraction on your hardware.
	DefaultMemoryWatcherModel = "qwen3.5:9b"
	DefaultMemoryEmbedModel   = "nomic-embed-text"
	// DefaultOllamaBridgeModel is the local model the sandbox's ollama-bridge
	// exposes to pi (the interactive Alt+P cycle) AND the router's local option.
	// It loads on demand (not resident), so it can be bigger than the watcher;
	// qwen3.5:9b (~6.6GB) is the current all-rounder that still fits a 16GB box.
	DefaultOllamaBridgeModel = "qwen3.5:9b"
	// DefaultRunIntent is the routing intent the top-level interactive session
	// (the "overlord") resolves to when the user pins neither --model nor --intent.
	// The stack ships "overlord" -> GPT-5.6 Sol: the orchestrator is pinned OFF
	// Anthropic on purpose (Claude is the weak writer/communicator, and a same-vendor
	// orchestrator shares its authors' blind spots). Change it with `pix config
	// set run_intent <intent>` (e.g. `strategy` for Opus, or any intent from
	// `pix route show`). NOTE: this default assumes an OpenAI key is present; an
	// Anthropic-only host should point run_intent at an Anthropic intent.
	DefaultRunIntent = "overlord"
	// BuiltinImpl is the default plugin impl: compiled into the host binary
	// rather than run as an external sub-process.
	BuiltinImpl = "builtin"
)

// DefaultSlackOAuthRedirectURI is the local HTTP callback the generic Slack
// rotating-PKCE OAuth flow (services/host/slackoauth, docs/design/
// slack-setup.md) uses when a client id is configured but no redirect uri was
// set explicitly. It ONLY ever applies once SlackOAuth.ClientID is non-empty
// (see applyDefaults) — there is deliberately NO baked-in default client id
// (no shared/"Docker" OAuth app ships in core; see SlackOAuth.ClientID), so an
// unconfigured install resolves neither a client id nor a redirect uri.
const DefaultSlackOAuthRedirectURI = "http://localhost:17373/slack/callback"

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

// HostMode gates `pix host` — running pi DIRECTLY on this machine with
// no sandbox, no network fence, and real credentials. Enabled is default-OFF
// on purpose: the friction of `pix config set host.enabled true` is the
// deliberate opt-in (see docs/design/host-mode.md). Autonomy is RESERVED for a
// future knob on the host-guard extension's strictness; Phase 1 stores it but
// nothing reads it yet.
type HostMode struct {
	Enabled  bool   `toml:"enabled"`
	Autonomy string `toml:"autonomy,omitempty"`
	// Autoserve gates the launcher's LAZY AUTO-START of `pix-host serve`
	// (docs/design/serve-lifecycle.md §1). nil = default TRUE (auto-start on).
	// A pointer so "unset" (inherit the default, follow future default changes)
	// is distinguishable from an explicit `false`. The PIX_NO_AUTOSERVE env
	// var wins over this flag.
	Autoserve *bool `toml:"autoserve,omitempty"`
}

// AutoserveEnabled reports whether lazy auto-start is enabled (default true).
func (c *Config) AutoserveEnabled() bool {
	return c.Host.Autoserve == nil || *c.Host.Autoserve
}

// SlackOAuth configures the generic Slack rotating-PKCE OAuth app wiring (see
// the slackoauth package and the org-owned-callback-service design in
// docs/design/slack-setup.md): the PUBLIC client id Slack issued the app, the
// registered redirect uri a local callback listens on, and WHERE the
// resulting credential blob lives in 1Password (a vault + document id —
// never the blob itself, which is the whole point of slackoauth.OPStore).
// OAuthGrantExpiresAt is a CACHED mirror of slackoauth.Blob.GrantExpiresAt so
// a status check can report "needs re-auth soon" without a live `op document
// get` round trip; it is advisory only — the 1Password document remains the
// source of truth, and a stale or missing cache here never blocks reading the
// real blob.
//
// Every field here is PUBLIC (an app id, a callback URL, a 1Password
// location, a cached timestamp) — none of it is a credential. The
// access_token/refresh_token never touch config.toml; they live only in the
// 1Password document identified by OAuthVaultID + OAuthDocumentID.
type SlackOAuth struct {
	ClientID            string    `toml:"client_id,omitempty"`
	RedirectURI         string    `toml:"redirect_uri,omitempty"`
	OAuthVaultID        string    `toml:"oauth_vault_id,omitempty"`
	OAuthDocumentID     string    `toml:"oauth_document_id,omitempty"`
	OAuthGrantExpiresAt time.Time `toml:"oauth_grant_expires_at,omitempty"`
}

// Config is the pix configuration, decoded from TOML.
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

	// MCP is every configured MCP server. S01: there is no eager/lazy split any
	// more — every configured server (plus every pack integration's server)
	// preloads at sandbox CREATE (`--static-mcp`). The retired mcp_static/
	// mcp_dynamic per-server override lists are gone; see retiredConfigKeys,
	// RetiredKeys, and cmd/pix's allPreloadedMCP.
	MCP []string `toml:"mcp,omitempty"`

	MemoryWatcherModel string `toml:"memory_watcher_model,omitempty"`
	MemoryEmbedModel   string `toml:"memory_embed_model,omitempty"`
	OllamaBridgeModel  string `toml:"ollama_bridge_model,omitempty"`

	// RunIntent is the routing intent for the top-level interactive session (the
	// "overlord" that orchestrates the subagent crew). When neither --model nor
	// --intent is passed, `pix run` resolves this intent through the router to
	// pick the session model. Defaults to DefaultRunIntent ("overlord" -> GPT-5.6
	// Sol); a bad value degrades to pi's own default rather than blocking launch.
	// Change it with `pix config set run_intent <intent>` (e.g. `strategy` for
	// Opus on an Anthropic-only host); `unset` restores the "overlord" default.
	// Sparse-saved: omitted from the file when it equals the default.
	RunIntent string `toml:"run_intent,omitempty"`

	// GogAccount is the Google Workspace account the gog host-MCP server serves.
	// It is THE source of truth: doctor probes against it, and `make mcp-register`
	// sources it via `pix config get gog_account` when registering with the
	// gateway. doctor falls back to the GOG_ACCOUNT env var when this is empty.
	// The Go identifier keeps the dependency binary's short name; the KEY on
	// disk is the public one.
	GogAccount string `toml:"google_workspace_account"`

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
	// (+ later mcp/proxies/routing/config). Empty = no active pack. `pix pack
	// use <path>` sets it; `run` mounts the pack's skills + knowledge. This is the
	// unifying successor to the loose skills-dir + knowledge_bundles + (eventually)
	// profile. See docs/design/packs.md.
	Pack string `toml:"pack,omitempty"`

	Plugins map[string]PluginSpec `toml:"plugins"`

	// Host gates + configures `pix host` (the unsandboxed escape hatch).
	// GLOBAL, never per-profile: leaving the sandbox is a machine-level decision.
	Host HostMode `toml:"host,omitempty"`

	// Slack configures the generic Slack rotating-PKCE OAuth app wiring. See
	// SlackOAuth. Empty (the default): no OAuth app configured, matching
	// today's op://-ref-only `pix slack setup` (docs/design/slack-setup.md).
	Slack SlackOAuth `toml:"slack,omitempty"`

	// retiredKeys / unknownKeys capture the top-level TOML keys Load/LoadFrom
	// found in the file that don't map to any field above (BurntSushi's
	// MetaData.Undecoded()). retiredKeys is the subset in retiredConfigKeys —
	// a key that once meant something and is now silently accepted-and-dropped
	// so an older config.toml never hard-fails Load; unknownKeys is everything
	// else (a genuine typo, or a field from a newer pix). Unexported: never
	// (de)serialized, and untouched by an absent-file Load (nothing to report).
	// See RetiredKeys / UnknownKeys.
	retiredKeys []string
	unknownKeys []string
}

// retiredConfigKeys is the allowlist of top-level config keys that once had
// meaning but were retired: mcp_static / mcp_dynamic, the per-server eager/lazy
// attach override S01 removed when every configured/pack MCP server started
// preloading at sandbox CREATE unconditionally. Load tolerates them (no error);
// RetiredKeys reports them so a caller (`config show`, `doctor`) can tell the
// user "this key no longer does anything" instead of "you made a typo" (which
// is what an unrecognized key normally means — see UnknownKeys).
var retiredConfigKeys = map[string]bool{
	"mcp_static":  true,
	"mcp_dynamic": true,
}

// RetiredKeys returns the retired top-level config keys (see retiredConfigKeys)
// found in the loaded file, sorted, deduplicated. Empty when the file has none
// — including when no file was loaded at all. A copy: callers cannot mutate the
// Config's internal state through the returned slice.
func (c *Config) RetiredKeys() []string { return append([]string(nil), c.retiredKeys...) }

// UnknownKeys returns the top-level (or dotted, for a nested table) config keys
// found in the loaded file that are NEITHER a live Config field NOR a retired
// key — most likely a typo, or a field only a newer pix understands. Load
// tolerates them (never a hard error, matching the package's documented
// "unknown keys are tolerated" contract); a caller can surface UnknownKeys to
// warn the user. A copy: callers cannot mutate the Config's internal state
// through the returned slice.
func (c *Config) UnknownKeys() []string { return append([]string(nil), c.unknownKeys...) }

// partitionUndecoded splits BurntSushi's MetaData.Undecoded() keys into the
// retired subset (retiredConfigKeys) and everything else (unknown), both sorted
// + deduplicated. A toml.Key's String() joins its path with dots, so a
// top-level scalar/list key (mcp_static) renders as itself and a nested unknown
// key (some_table.some_field) renders dotted — retiredConfigKeys only ever
// matches top-level names, so a same-named nested key is never misclassified.
func partitionUndecoded(keys []toml.Key) (retired, unknown []string) {
	retiredSeen := map[string]bool{}
	unknownSeen := map[string]bool{}
	for _, k := range keys {
		s := k.String()
		if retiredConfigKeys[s] {
			if !retiredSeen[s] {
				retiredSeen[s] = true
				retired = append(retired, s)
			}
			continue
		}
		if !unknownSeen[s] {
			unknownSeen[s] = true
			unknown = append(unknown, s)
		}
	}
	sort.Strings(retired)
	sort.Strings(unknown)
	return retired, unknown
}

// configDir resolves the directory that holds config.toml and the broker token.
// It honors $PIX_CONFIG's parent when set, then $XDG_CONFIG_HOME/pix,
// then ~/.config/pix.
func configDir() (string, error) {
	if p := os.Getenv("PIX_CONFIG"); p != "" {
		return filepath.Dir(p), nil
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "pix"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "pix"), nil
}

// Path resolves the config file path: $PIX_CONFIG override, else
// <config-dir>/config.toml.
func Path() string {
	if p := os.Getenv("PIX_CONFIG"); p != "" {
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
// `pix-host serve` writes on startup so the launcher's `serve stop` /
// `serve status` can find and signal the running supervisor SAFELY (instead of a
// blind `pkill -f`). It lives in the STATE dir (<state-dir>/serve.pid), NOT the
// config dir: it is ephemeral runtime state (like serve.log), not user config.
// Keeping it out of the config dir also means `pix reset` (which moves the
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
// `pix run`). Ephemeral runtime state — a sibling of the pidfile in the
// STATE dir.
func ServeSpawnLockPath() string {
	dir, err := StateDir()
	if err != nil {
		return "serve.spawn.lock"
	}
	return filepath.Join(dir, "serve.spawn.lock")
}

// ServeLazyMarkerPath is the marker file the launcher writes after a successful
// LAZY detached spawn of `pix-host serve`, so config propagation can tell
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

// StateDir resolves the per-user state dir: $XDG_STATE_HOME/pix, else
// ~/.local/state/pix. Used for logs (NOT config): serve.log lives here on
// BOTH macOS and Linux, and every launch mode writes to it — the lazy
// auto-start, the managed launchd LaunchAgent (StandardOutPath/
// StandardErrorPath), and the managed systemd --user unit (StandardOutput=/
// StandardError=append:) all point at the SAME file (ServeLogPath()), so
// there is exactly one place to look regardless of how serve was started.
// Only a FOREGROUND `pix serve` is different — that one is interactive
// and goes to its own terminal, not this file.
func StateDir() (string, error) {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "pix"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "pix"), nil
}

// ServeLogPath is <state-dir>/serve.log — where a lazily auto-started
// `pix-host serve` writes its stdout/stderr.
func ServeLogPath() string {
	dir, err := StateDir()
	if err != nil {
		return "serve.log"
	}
	return filepath.Join(dir, "serve.log")
}

// DataDir resolves the per-user DATA dir: $XDG_DATA_HOME/pix, else
// ~/.local/share/pix. This is the durable data root — the captured memory
// store, the knowledge index, backups, and routing overrides all live under it,
// as distinct from StateDir (ephemeral: logs, pidfiles) and configDir
// (config.toml). It is the single source of truth for the data root; every
// default below is derived from it so there is no second copy to drift.
func DataDir() (string, error) {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "pix"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "pix"), nil
}

// PackDir is the per-user DEFAULT PACK root: $XDG_DATA_HOME/pix/default,
// else ~/.local/share/pix/default. A proper pack (pack.toml + skills/ +
// knowledge/), git-initialized, the default home for what you author for
// yourself — named "default" (the pack's name derives from the dir basename)
// so the auto-created pack and its messaging ("created pack ...", "active pack
// -> this (default) pack") are coherent. The active pack (config `pack`)
// overrides it; `pix reset` moves it aside (it's a git working copy — the
// user pushes it to their own remote). See docs/design/packs.md.
//
// The basename has been renamed twice: originally "pack", then briefly
// "personal" (which wrongly implied non-work use), now "default".
// defaultPackRoot() migrates an existing ".../personal" dir (preferred) or an
// older ".../pack" dir to ".../default" on first resolution, so no user is
// orphaned (see migrateLegacyPackDir in pack.go).
func PackDir() string { return filepath.Join(dataDirOr(), "default") }

// PacksDir is where adopted REMOTE packs are cloned:
// $XDG_DATA_HOME/pix/packs, else ~/.local/share/pix/packs. Each lives
// at <PacksDir>/<name>. Distinct from PackDir (the single default pack).
func PacksDir() string { return filepath.Join(dataDirOr(), "packs") }

// dataDirOr returns DataDir() or, if HOME cannot be resolved, a relative
// "pix" so path builders never panic on an empty base.
func dataDirOr() string {
	d, err := DataDir()
	if err != nil {
		return "pix"
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
// `pix state backup` archives.
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
	if c.RunIntent == "" {
		c.RunIntent = DefaultRunIntent
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
	// The redirect uri only ever defaults once a client id is configured — an
	// install with no OAuth app has neither (see DefaultSlackOAuthRedirectURI).
	if c.Slack.ClientID != "" && c.Slack.RedirectURI == "" {
		c.Slack.RedirectURI = DefaultSlackOAuthRedirectURI
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
// is not the active one — e.g. `restore` validating that an archived
// config.toml parses as TOML before installing it. Absence is not an error: it
// returns defaults, matching Load().
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
	c.retiredKeys, c.unknownKeys = partitionUndecoded(md.Undecoded())
	c.applyDefaults()
	return c, nil
}

// AllKnowledgeBundles returns the de-duplicated knowledge bundles. (Formerly a
// union across profiles; profiles were removed, so it is now just the base list,
// deduped. Kept as a method so `serve`/backup callers are unchanged.)
func (c *Config) AllKnowledgeBundles() []string {
	seen := map[string]bool{}
	var out []string
	for _, b := range c.KnowledgeBundles {
		if b != "" && !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	return out
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
const defaultConfigTOML = `# pix config. All fields optional; delete anything you don't override.

# Pin the kit/image release the launcher runs. Empty (the default) tracks the
# LATEST STABLE release, so an installed launcher keeps picking up fixes. Set a
# version ("0.1.0") or any git ref ("main") to freeze it instead. Override for a
# single run with: pix run --kit-ref REF
version_pin = ""

# Host services ` + "`make serve`" + ` runs.
services = ["memory"]

# MCP servers attached at sandbox creation.
mcp = []

# Local Ollama models the memory service uses.
memory_watcher_model = "qwen3.5:9b"
memory_embed_model = "nomic-embed-text"
ollama_bridge_model = "qwen3.5:9b"

# Google Workspace account the google-workspace host-MCP server serves. This is the single
# source of truth: pix doctor probes against it, and make mcp-register
# sources it via pix config get google_workspace_account. Empty falls back to the
# GOG_ACCOUNT env var.
google_workspace_account = ""

# OKF knowledge bundle directories the knowledge service (:11436) indexes.
# Empty = no bundles (index served empty). The knowledge service is opt-in:
# add "knowledge" to the services list above to run it.
knowledge_bundles = []

# Kits stacked onto the sandbox (mixin kits, etc).
[kits]
stack = []

# Active pack (git-backed context bundle: skills + knowledge + integrations).
# Usually set via ` + "`pix pack use <path|git-url>`" + `.
# pack = ""

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

# Generic Slack rotating-PKCE OAuth app wiring (public fields only — the
# credential blob itself lives in 1Password, never here). No client_id ships
# by default; set one to configure your own Slack app.
# [slack]
# client_id = ""
# redirect_uri = ""
# oauth_vault_id = ""
# oauth_document_id = ""
# oauth_grant_expires_at = ""
`

// Save writes the config back to Path() as TOML. It is the write half of the
// repo-less workflow: the CLI (`pix config set`, `pix setup`) mutates a
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
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(c.sparseForSave()); err != nil {
		return err
	}
	return writeFileAtomic(dir, path, buf.Bytes(), 0o600)
}

// writeFileAtomic writes data to path by writing a temp file IN THE SAME dir
// (so the rename below is on one filesystem), fsync-ing it, then atomically
// renaming it over path. os.WriteFile truncates the destination in place, so a
// process killed (or a disk-full write error) mid-write can leave path
// half-written/empty — the classic torn-config bug. A temp-file + fsync +
// rename means path either has its old complete content or its new complete
// content, never a truncated in-between: a failed write here NEVER touches
// path, so the prior file is always left intact. The temp file is removed on
// every failure path (best-effort; a leftover .tmp-* is harmless clutter, never
// read by any loader) and is a no-op after a successful rename.
func writeFileAtomic(dir, path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false // renamed away; nothing left at tmpPath to remove
	return nil
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
	if sp.RunIntent == DefaultRunIntent {
		sp.RunIntent = ""
	}
	// applyDefaults allocates an empty Plugins map (and fills Impl=builtin) so
	// readers never nil-check; don't petrify that resolution either.
	if len(sp.Plugins) == 0 {
		sp.Plugins = nil
	}
	// Same tradeoff as every other defaultable field: a redirect uri that
	// equals the resolved default is omitted rather than pinned, so a future
	// default change still reaches an install that only ever set a client id.
	if sp.Slack.RedirectURI == DefaultSlackOAuthRedirectURI {
		sp.Slack.RedirectURI = ""
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

// SetSlackClientID sets the Slack app's public OAuth client id (trimmed). An
// empty value clears it — and, since RedirectURI only ever defaults off a
// non-empty ClientID, also stops a future Load from resolving a default
// redirect uri.
func (c *Config) SetSlackClientID(id string) { c.Slack.ClientID = strings.TrimSpace(id) }

// SetSlackRedirectURI sets the registered OAuth redirect uri (trimmed). An
// empty value clears the explicit override; applyDefaults then resolves
// DefaultSlackOAuthRedirectURI on the next Load if a ClientID is set.
func (c *Config) SetSlackRedirectURI(uri string) { c.Slack.RedirectURI = strings.TrimSpace(uri) }

// SetSlackOAuthVaultID sets the 1Password vault (name or id) holding the
// rotating credential document (trimmed). An empty value clears it.
func (c *Config) SetSlackOAuthVaultID(id string) { c.Slack.OAuthVaultID = strings.TrimSpace(id) }

// SetSlackOAuthDocumentID sets the 1Password document (item) id holding the
// rotating credential blob (trimmed). An empty value clears it.
func (c *Config) SetSlackOAuthDocumentID(id string) {
	c.Slack.OAuthDocumentID = strings.TrimSpace(id)
}

// SetSlackOAuthGrantExpiresAt sets the cached grant expiry mirrored from
// slackoauth.Blob.GrantExpiresAt (see SlackOAuth's doc for why this is only
// advisory). A zero time clears it.
func (c *Config) SetSlackOAuthGrantExpiresAt(t time.Time) { c.Slack.OAuthGrantExpiresAt = t }

// ClearSlackOAuthManaged clears exactly the fields a rotating OAuth grant
// owns (the 1Password vault/document locators and the cached grant expiry)
// while RETAINING ClientID and RedirectURI. `pix slack disable`'s OAuth-mode
// path calls this after the grant has been revoked at Slack and its
// 1Password document deleted, so config never claims a document that no
// longer exists — but the public app wiring stays put, making a later `pix
// slack setup` re-authorization a one-step operation rather than asking for
// the client id and redirect uri all over again.
func (c *Config) ClearSlackOAuthManaged() {
	c.Slack.OAuthVaultID = ""
	c.Slack.OAuthDocumentID = ""
	c.Slack.OAuthGrantExpiresAt = time.Time{}
}

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
// (cmd/pix/knowledge.go) byte-for-byte in behavior — otherwise a symlink
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
// 1Password refs file `pix host` resolves via `op run --env-file` for
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
// how the gateway uses it. Reused VERBATIM in `pix setup`, the `secret`
// help, and the template header so the concept is described identically
// everywhere. Keep it in sync if you change any copy.
const OpRefsMentalModel = `op-refs.env maps ENV_VAR = op://vault/item/field. When the gateway spawns a
host MCP server it resolves those refs from 1Password and injects them as env
vars — the secret never touches disk or the sandbox. A server with no creds
(pio) needs no entry.`

// NonSecretOpRefsKeys is the documented allowlist of NON-secret env vars that may
// appear in op-refs.env with a literal value; everything else must be an op://
// vault/item/field REFERENCE. GOG_ACCOUNT/GOG_HOME/GOG_KEYRING_BACKEND configure
// gog's headless keyring + account/home; the keyring PASSWORD is a secret and
// must still be an op:// ref, so it is DELIBERATELY not listed here.
// SLACK_TEAM_ID/SLACK_USER_ID are `pix slack setup`'s identity pins: the
// team/user id its live auth.test resolved AT setup time, written as plain
// literals so `pix slack status` can flag SLACK_TOKEN silently resolving to a
// DIFFERENT identity later. Neither value is a credential on its own.
var NonSecretOpRefsKeys = map[string]bool{
	"GOG_ACCOUNT":         true,
	"GOG_HOME":            true,
	"GOG_KEYRING_BACKEND": true,
	"SLACK_TEAM_ID":       true,
	"SLACK_USER_ID":       true,
}

// OpRefsTemplate is the seed content for a fresh op-refs.env: op:// references
// ONLY (plus the documented non-secret env allowlist), with generic placeholders
// to fill. Every example line is COMMENTED OUT so a freshly-seeded file has ZERO
// active entries — the user uncomments (or adds) a line only when wiring a
// server. Kept in sync with the repo's config/op-refs.env.example (which the
// make path uses). Its header repeats OpRefsMentalModel verbatim.
const OpRefsTemplate = `# pix op-refs.env — 1Password refs the sbx gateway resolves via
# ` + "`op run --env-file`" + ` when it spawns each host MCP server.
#
# ` + OpRefsMentalModel + `
#
# This file holds op://vault/item/field REFERENCES only, plus the documented
# non-secret env allowlist (GOG_ACCOUNT, GOG_HOME, GOG_KEYRING_BACKEND,
# SLACK_TEAM_ID, SLACK_USER_ID). Everything secret (tokens, keyring passwords)
# is an op:// ref resolved from 1Password at spawn time — never a pasted secret.
#
# Every line below is COMMENTED OUT: a freshly-seeded file has zero active
# entries. Uncomment + fill in a line only when you wire that server.
#
# Verify:  op read "op://<vault>/<item>/<field>" >/dev/null && echo OK
# Tip:     1Password app -> right-click a field -> "Copy Secret Reference".

# slack MCP server. Run 'pix slack setup --token-ref op://<vault>/<item>/<field>'
# instead of hand-editing these lines — it resolves the ref, requires an xoxp-
# PERSONAL user token (every Slack call the server makes acts AS the token's
# owner; auth.test proves the identity live), and writes both lines below
# itself. It is per-user and must never be a shared "employee"/team/bot token,
# and never handed to a second person to reuse — each user runs their own
# pix slack setup instead. See docs/design/slack-setup.md for how to obtain a
# token, minimal scopes, and revocation.
# SLACK_TOKEN=op://<vault>/<item>/<field>
# SLACK_TEAM_ID=<team id auth.test resolved at setup>
# SLACK_USER_ID=<user id auth.test resolved at setup>

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
// setup and `pix mcp register` both route through it (via SeedOpRefsAt) so
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
