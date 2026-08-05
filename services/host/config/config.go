// Package config is the pix config schema + loader, shared by the host
// binary (pix-host) AND the launcher binary. Deliberately dependency-light:
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
	// The watcher must reliably emit STRUCTURED JSON (facts/events/corrections).
	DefaultMemoryWatcherModel = "qwen3.5:9b"
	DefaultMemoryEmbedModel   = "nomic-embed-text"
	// DefaultOllamaBridgeModel is the local model the sandbox's ollama-bridge
	// exposes to pi (the interactive Alt+P cycle) AND the router's local option.
	DefaultOllamaBridgeModel = "qwen3.5:9b"
	// DefaultRunIntent is the routing intent the top-level interactive session
	// (the "overlord") resolves to when the user pins neither --model nor --intent.
	DefaultRunIntent = "overlord"
	// BuiltinImpl is the default plugin impl: compiled into the host binary
	// rather than run as an external sub-process.
	BuiltinImpl = "builtin"
)

// DefaultServices is intentionally empty. Memory requires a verified local
// Ollama watcher + embedding model and is enabled by setup only after those
// requirements pass; a fresh API-key/gateway user should not inherit a broken
var DefaultServices = []string{}

// PluginSpec configures one plugin slot: how it is implemented and, for external
// impls, where the binary lives and how it is verified/reached.
type PluginSpec struct {
	Impl     string   `toml:"impl"`      // "builtin" (default) or an external impl name
	Path     string   `toml:"path"`      // path to an external plugin binary
	SHA      string   `toml:"sha"`       // expected checksum of the external binary
	Port     int      `toml:"port"`      // port an external plugin listens on
	ExtraEnv []string `toml:"extra_env"` // additional env vars granted to this plugin subprocess
}

// HostMode configures the `[host]` table. The unsandboxed escape hatch it used
// to gate (`pix host` / host.enabled / host.autonomy) was RETIRED and deleted —
// the sandbox is the only supported execution boundary now; see
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

	// Services is the RESOLVED runtime service set every consumer reads
	// (serve, ensureServe, doctor, …). It is never (de)serialized directly:
	Services []string `toml:"-"`
	// ServicesRaw is the TOML image of Services: nil = the key was ABSENT from
	// the file (resolve to DefaultServices); non-nil — even pointing at an empty
	// slice — = the key was PRESENT and is authoritative (`services = []` stays
	ServicesRaw *[]string `toml:"services,omitempty"`

	// MCP is every configured MCP server. S01: there is no eager/lazy split any
	// more — every configured server (plus every pack integration's server)
	// preloads at sandbox CREATE (`--static-mcp`). The retired mcp_static/
	MCP []string `toml:"mcp,omitempty"`

	MemoryWatcherModel string `toml:"memory_watcher_model,omitempty"`
	MemoryEmbedModel   string `toml:"memory_embed_model,omitempty"`
	OllamaBridgeModel  string `toml:"ollama_bridge_model,omitempty"`

	// RunIntent is the routing intent for the top-level interactive session (the
	// "overlord" that orchestrates the subagent crew). When neither --model nor
	// --intent is passed, `pix run` resolves this intent through the router to
	RunIntent string `toml:"run_intent,omitempty"`

	// Inference describes WHERE catalog models can be called. Model identity and
	// quality remain in the shipped routing catalog; this block contains only
	// user/pack-owned backend wiring and model-id bindings. Secrets never live
	Inference InferenceConfig `toml:"inference,omitempty"`

	// GogAccount is the Google Workspace account the gog host-MCP server serves.
	GogAccount string `toml:"google_workspace_account"`

	Kits struct {
		Stack []string `toml:"stack"`
	} `toml:"kits"`

	Skills struct {
		Paths []string `toml:"paths"`
	} `toml:"skills"`

	// Packs is the ordered active pack stack. Pack is retained as the last-pack
	// compatibility image while the command surface migrates; runtime composition
	// always prefers Packs and de-duplicates Pack.
	Packs []string `toml:"packs,omitempty"`
	Pack  string   `toml:"pack,omitempty"`

	Plugins map[string]PluginSpec `toml:"plugins"`

	// Host gates + configures `pix host` (the unsandboxed escape hatch).
	// GLOBAL, never per-profile: leaving the sandbox is a machine-level decision.
	Host HostMode `toml:"host,omitempty"`

	// retiredKeys are the top-level TOML keys in the file that map to no field
	// above (BurntSushi's MetaData.Undecoded()) AND are in retiredConfigKeys: a
	// key that once meant something, tolerated so an old config.toml still
	// loads, and reported via RetiredKeys so a caller can say "this no longer
	// does anything" instead of nothing at all.
	retiredKeys []string
}

// InferenceConfig is deliberately small. Setup and packs author it; ordinary
// users should not need to understand it. ExclusiveSource, when non-empty, is
// an enforcement boundary contributed by a pack: every runtime backend/model
type InferenceConfig struct {
	Backends map[string]InferenceBackend `toml:"backends,omitempty"`
	Models   []InferenceModelBinding     `toml:"models,omitempty"`
	// AllowedModels is the user's canonical catalog-model roster. An empty list
	// means no user restriction (pack declarations / legacy config remain
	// callable). Exclusive pack inference bypasses this personal preference
	AllowedModels []string `toml:"allowed_models,omitempty"`
	// RosterProviders records which providers the roster has already been
	// offered for. It is what lets a NEWLY added provider widen AllowedModels
	// while a deliberate narrowing within providers the user has already seen is
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

// retiredConfigKeys is the allowlist of top-level (or dotted-nested) config
// keys that once had meaning but were retired: mcp_static / mcp_dynamic, the
// per-server eager/lazy attach override S01 removed when every configured/pack
var retiredConfigKeys = map[string]bool{
	"mcp_static":    true,
	"mcp_dynamic":   true,
	"host.enabled":  true,
	"host.autonomy": true,
	// knowledge_bundles: the built-in OKF knowledge service (:11436) was retired
	// (W2 U03A) along with config.KnowledgeBundles/AddKnowledgeBundle/
	// AllKnowledgeBundles — a still-present key from an older config.toml is
	"knowledge_bundles": true,
	// slack: the built-in Slack OAuth/token table (client_id, redirect_uri,
	// oauth_vault_id, oauth_document_id, oauth_grant_expires_at), retired when
	// Slack was externalized (W2/U02a; see docs/design/slack-setup.md) — a
	"slack": true,
}

// RetiredKeys returns the retired top-level config keys (see retiredConfigKeys)
// found in the loaded file, sorted, deduplicated. Empty when the file has none
// — including when no file was loaded at all. A copy: callers cannot mutate the
func (c *Config) RetiredKeys() []string { return append([]string(nil), c.retiredKeys...) }

// retiredIn returns the retired keys (see retiredConfigKeys) among BurntSushi's
// undecoded keys, sorted + deduplicated. A toml.Key's String() joins its path
// with dots, so a nested unknown key renders dotted and can never collide with
// the top-level names retiredConfigKeys holds. Anything else is tolerated
// silently — an unknown key is either a typo or a field only a newer pix knows.
func retiredIn(keys []toml.Key) []string {
	seen := map[string]bool{}
	var retired []string
	for _, k := range keys {
		s := k.String()
		if retiredConfigKeys[s] && !seen[s] {
			seen[s] = true
			retired = append(retired, s)
		}
	}
	sort.Strings(retired)
	return retired
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
func ServeLazyMarkerPath() string {
	dir, err := StateDir()
	if err != nil {
		return "serve.lazy"
	}
	return filepath.Join(dir, "serve.lazy")
}

// MonitorStoreRoot is <state-dir>/monitor: the on-disk root the monitor
// ingest server (now composed inside `pix-host serve`, see serve.go) writes
// under and `pix monitor` (a pure offline reader with no listener of its
func MonitorStoreRoot() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "monitor"), nil
}

// StateDir resolves the per-user state dir: $XDG_STATE_HOME/pix, else
// ~/.local/state/pix. Used for logs (NOT config): serve.log lives here, and
// every launch mode writes to it — the lazy auto-start and the managed
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
func PackDir() string { return filepath.Join(dataDirOr(), "default") }

// ContextDir is the always-on, user-authored context layer. It is DATA (durable
// AGENTS.md + skills), not runtime config or ephemeral state. Team/project
// context belongs in packs; this directory remains personal and composes above
func ContextDir() string { return filepath.Join(dataDirOr(), "context") }

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

// MemoryLockPath is the advisory flock file the memory daemon and `restore` both
// take to be mutually exclusive around the sqlite store. It sits next to the
// memory db (honoring MEMORY_DB's dir) as .memory.lock, so both processes
func MemoryLockPath() string {
	return filepath.Join(filepath.Dir(MemoryDBPath()), ".memory.lock")
}

// removedServices are service names that no longer exist (e.g. gws, which the
// Google Workspace port replaced with the host `gog` MCP server). We drop them
// silently from a loaded config so a stale services list doesn't fatal `serve`.
var removedServices = map[string]bool{"gws": true, "gws-token": true, "knowledge": true} // knowledge: W1 U01a

// defaults returns a Config with the sane defaults applied to any unset field.
func (c *Config) applyDefaults() {
	if c.Inference.Backends == nil {
		c.Inference.Backends = map[string]InferenceBackend{}
	}
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
	// The ENTIRE [plugins.*] table is RETIRED (U07d; it subsumes the earlier
	// plugins.broker-only retirement): a config file can no longer name an
	// executable for the supervisor to run. External units are
	if len(c.Plugins) > 0 {
		seen := map[string]bool{}
		for _, k := range c.retiredKeys {
			seen[k] = true
		}
		for slot := range c.Plugins {
			if key := "plugins." + slot; !seen[key] {
				c.retiredKeys = append(c.retiredKeys, key)
				seen[key] = true
			}
		}
		c.Plugins = map[string]PluginSpec{}
		sort.Strings(c.retiredKeys)
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
	c.retiredKeys = retiredIn(md.Undecoded())
	c.applyDefaults()
	return c, nil
}

// Plugin returns the spec for slot. With the [plugins.*] declaration retired
// (U07d; applyDefaults sweeps every declared slot into retiredKeys and empties
// the map), a loaded config always answers builtin here — an external unit can
func (c *Config) Plugin(slot string) PluginSpec {
	if spec, ok := c.Plugins[slot]; ok {
		if spec.Impl == "" {
			spec.Impl = BuiltinImpl
		}
		return spec
	}
	return PluginSpec{Impl: BuiltinImpl}
}

// Save writes the config back to Path() as TOML. It is the write half of the
// repo-less workflow: the CLI (`pix config set`, `pix setup`) mutates a
// loaded Config in memory and calls Save() so the user NEVER hand-edits the file.
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
func (c *Config) sparseForSave() *Config {
	sp := *c
	// services: an untouched default is omitted (nil raw); ANY deviation —
	// including an explicitly-empty list — is serialized. The toml encoder
	// writes a non-nil pointer to an empty slice as `services = []`, which
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
func OpRefsPath() string {
	dir, err := configDir()
	if err != nil {
		return "op-refs.env"
	}
	return filepath.Join(dir, "op-refs.env")
}

// GWServerName is the google-workspace MCP server's registration + display
// name. It lives here, not in the gworkspace workflow, because five unrelated
// callers (secret, mcp, doctor, status, setup) need to recognise the
type MCPContainer struct {
	Manifest  string
	Image     string
	EnvKeys   []string          // env var names to forward into an Image container (-e KEY)
	EnvValues map[string]string // non-secret literals forwarded as -e KEY=VALUE
	RemoteURL string            // remote MCP endpoint URL (`sbx mcp add <name> --url <url>`)
}

const GWServerName = "google-workspace"

// GWInstallCmd is the ONE place the external binary's package name may reach
// a user; it crosses domain boundaries for the same reason GWServerName does.
const GWInstallCmd = "brew install openclaw/tap/gogcli"

// OpRefsMentalModel is the ≤4-line plain explanation of what op-refs.env is and
// how the gateway uses it. Reused VERBATIM in `pix setup`, the `secret`
// help, and the template header so the concept is described identically
const OpRefsMentalModel = `op-refs.env maps ENV_VAR = op://vault/item/field. When the gateway spawns a
host MCP server it resolves those refs from 1Password and injects them as env
vars — the secret never touches disk or the sandbox. A server with no creds
(pio) needs no entry.`

// NonSecretOpRefsKeys is the documented allowlist of NON-secret env vars that may
// appear in op-refs.env with a literal value; everything else must be an op://
// vault/item/field REFERENCE. GOG_ACCOUNT/GOG_HOME/GOG_KEYRING_BACKEND configure
var NonSecretOpRefsKeys = map[string]bool{
	"GOG_ACCOUNT":         true,
	"GOG_HOME":            true,
	"GOG_KEYRING_BACKEND": true,
}

// OpRefsTemplate is the seed content for a fresh op-refs.env: op:// references
// ONLY (plus the documented non-secret env allowlist), with generic placeholders
// to fill. Every example line is COMMENTED OUT so a freshly-seeded file has ZERO
const OpRefsTemplate = `# pix op-refs.env — 1Password refs the sbx gateway resolves via
# ` + "`op run --env-file`" + ` when it spawns each host MCP server.
#
# op-refs.env maps ENV_VAR = op://vault/item/field. When the gateway spawns a
# host MCP server it resolves those refs from 1Password and injects them as env
# vars — the secret never touches disk or the sandbox. A server with no creds
# (pio) needs no entry.
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

# Slack is no longer a built-in host MCP server (see docs/design/
# slack-setup.md, W2/U02a): a pinned external pack now owns registering it.
# If your active pack's Slack MCP server needs a token, it documents its own
# ENV_VAR here (still an op:// ref, same mechanism) — nothing to seed by
# default.

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
func SeedOpRefs() (path string, created bool, err error) {
	path = OpRefsPath()
	created, err = SeedOpRefsAt(path)
	return path, created, err
}

// SeedOpRefsAt is the path-parameterized seeder SeedOpRefs delegates to (so a
// caller that resolves op-refs.env through an injected env can reuse the exact
// same no-clobber + 0700 dir / 0600 file guarantees). It writes OpRefsTemplate
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
