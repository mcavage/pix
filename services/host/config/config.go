// Package config is the pix config schema + loader, shared by the host binary
// (pix-host) AND the launcher binary. Deliberately dependency-light.
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
	// A small, fast, extraction-grade local model DEDICATED to fact capture: it
	// must reliably emit STRUCTURED JSON (facts/events/corrections).
	DefaultMemoryWatcherModel = "qwen3.5:9b"
	DefaultMemoryEmbedModel   = "nomic-embed-text"
	// DefaultMemoryCapture: no automatic observation until a user opts in.
	DefaultMemoryCapture = MemoryCaptureExplicit
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

// DefaultServices is intentionally empty: memory needs a verified local Ollama
// watcher + embedding model, so only setup enables it once those probes pass.
var DefaultServices = []string{}

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

	// Services is the RESOLVED runtime service set every consumer reads (serve,
	// ensureServe, doctor, …); it is never (de)serialized directly.
	Services []string `toml:"-"`
	// ServicesRaw is the TOML image of Services: nil = the key was ABSENT (resolve
	// to DefaultServices); non-nil — even an empty slice — = PRESENT, authoritative.
	ServicesRaw *[]string `toml:"services,omitempty"`

	// MCP is every configured MCP server. There is no eager/lazy split: every
	// configured server (plus every pack integration's) preloads at sandbox
	// CREATE (`--static-mcp`), and the mcp_static/mcp_dynamic knobs are retired.
	MCP []string `toml:"mcp,omitempty"`

	MemoryWatcherModel string `toml:"memory_watcher_model,omitempty"`
	MemoryEmbedModel   string `toml:"memory_embed_model,omitempty"`
	OllamaBridgeModel  string `toml:"ollama_bridge_model,omitempty"`
	// MemoryCapture: explicit (default) or experimental-auto. See
	// config.MemoryCaptureModes; a garbled value resolves to explicit.
	MemoryCapture string `toml:"memory_capture,omitempty"`

	// RunIntent is the routing intent for the top-level interactive session (the
	// "overlord"), resolved through the router when neither --model nor --intent.
	RunIntent string `toml:"run_intent,omitempty"`

	// Environment is the machine default environment NAME (Story 1, native
	// sandbox environments — docs/design/environments.md §5.3), resolved
	// against Environments. Empty = no registered default. `pix env use` is
	// the ONLY writer; `pix config set/unset` refuses this key outright (see
	// provision.environmentKeyRefusal).
	Environment string `toml:"environment,omitempty"`

	// Environments is the registered name -> CANONICAL ABSOLUTE local
	// directory path index. `pix env add`/`pix env forget` are the only writers;
	// AddEnvironment canonicalizes (expanding a leading ~, then making the
	// result absolute and clean) before a value is ever assigned here, so
	// Save() never persists anything else. A hand-edited noncanonical entry
	// is dropped on Load, fail closed (see applyDefaults).
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

	// Packs is the ordered active pack stack. Pack is retained as the last-pack
	// compatibility image while the command surface migrates; runtime composition
	// always prefers Packs and de-duplicates Pack.
	Packs []string `toml:"packs,omitempty"`
	Pack  string   `toml:"pack,omitempty"`

	Plugins map[string]PluginSpec `toml:"plugins"`

	// Host gates + configures `pix host` (the unsandboxed escape hatch). GLOBAL,
	// never per-profile: leaving the sandbox is a machine-level decision.
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

// ServePidPath resolves the absolute path of serve.pid — the pidfile `pix-host
// serve` writes so `serve stop`/`serve status` can signal the supervisor SAFELY.
func ServePidPath() string {
	dir, err := StateDir()
	if err != nil {
		return "serve.pid"
	}
	return filepath.Join(dir, "serve.pid")
}

// ServeUnitsPath is <state-dir>/serve.units.json — the supervision-tree snapshot
// `pix-host serve` publishes and `pix serve status --json` / `pix doctor --json`
// read back. It lives in the STATE dir with the pidfile, so the same "move the
// config aside" move can never leave a stale snapshot beside a live daemon.
func ServeUnitsPath() string {
	dir, err := StateDir()
	if err != nil {
		return "serve.units.json"
	}
	return filepath.Join(dir, "serve.units.json")
}

// ServeSpawnLockPath is the flock file the launcher's lazy auto-start takes around
// its spawn decision (double-checked locking against a concurrent `pix run`).
func ServeSpawnLockPath() string {
	dir, err := StateDir()
	if err != nil {
		return "serve.spawn.lock"
	}
	return filepath.Join(dir, "serve.spawn.lock")
}

// ServeLazyMarkerPath is the marker the launcher writes after a successful LAZY
// detached spawn: a lazy daemon is safe to stop-and-restart, a foreground one not.
func ServeLazyMarkerPath() string {
	dir, err := StateDir()
	if err != nil {
		return "serve.lazy"
	}
	return filepath.Join(dir, "serve.lazy")
}

// PidFileLockPath is the STABLE sibling flock path for a pid-bearing file
// (serve.pid, serve.lazy): every writer (the daemon's own writeServePidFile,
// the launcher's recordSpawnedServePid/markLazy) and the daemon's own
// compare-and-delete cleanup (removeOwnedPidFile) all serialize on THIS path
// via sys.Lock/withFlock, never on the guarded file itself. That matters
// because the guarded file gets REPLACED (removed, then rewritten by a
// respawned daemon) across its lifetime, and locking a path that can be
// unlinked out from under an open fd is the TOCTOU lock.go's flockHandle
// guards against elsewhere in this tree — a fixed, never-removed sibling
// path sidesteps that class entirely rather than re-solving it here.
func PidFileLockPath(path string) string {
	return path + ".lock"
}

// StateDir resolves the per-user state dir: $XDG_STATE_HOME/pix, else
// ~/.local/state/pix. Runtime state and serve.log live here, never config.
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
// ~/.local/share/pix — the durable root for the memory store, backups, routing.
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

// PackDir is the per-user DEFAULT PACK root: $XDG_DATA_HOME/pix/default, else
// ~/.local/share/pix/default — a proper pack (pack.toml + skills/ + knowledge/).
func PackDir() string { return filepath.Join(dataDirOr(), "default") }

// ContextDir is the always-on, user-authored context layer: DATA (durable
// AGENTS.md + skills), personal — team context belongs in a pack.
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
// take around the sqlite store: .memory.lock beside the db (honoring MEMORY_DB).
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
// ever exercises, and it never persists anything but a canonical absolute
// path, so a `~`-bearing or relative value can only have reached the file by
// hand. It is dropped rather than trusted as a local root, and recorded in
// unknownKeys — the same "tell them" contract a retired [plugins.*] slot
// gets — so the user sees why their edit had no effect. A default naming a
// dropped (or never-registered) name resolves to no default rather than a
// dangling selection.
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
		if isCanonicalEnvironmentPath(path) {
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

// writeFileAtomic writes data to a temp file IN THE SAME dir (so the rename is on
// one filesystem), fsyncs it, then renames it over path — never a truncate in place.
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

// sparseForSave returns a shallow copy with every defaultable field that equals its
// default zeroed, so Save (via `,omitempty`) writes only explicit deviations.
func (c *Config) sparseForSave() *Config {
	sp := *c
	// services: an untouched default is omitted (nil raw); ANY deviation — including
	// an explicitly-empty list — is serialized (`services = []`).
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
	if sp.MemoryCapture == DefaultMemoryCapture {
		sp.MemoryCapture = ""
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

// OpRefsPath resolves the absolute XDG path of op-refs.env (the 1Password refs file
// the sbx gateway resolves via `op run --env-file`): <config-dir>/op-refs.env.
func OpRefsPath() string {
	dir, err := configDir()
	if err != nil {
		return "op-refs.env"
	}
	return filepath.Join(dir, "op-refs.env")
}

// MCPServer is one pack-declared MCP server, resolved to everything pix needs
// to register it with the gateway. Exactly one TRANSPORT is set, and that
// choice decides both the argv and whether credentials are injected at all:
//
//	Command   a host binary the gateway spawns over stdio (creds via op-refs)
//	Image     a container run by the gateway            (creds via op-refs, -e)
//	Manifest  an OCI server manifest the gateway resolves (creds Docker-side)
//	RemoteURL a hosted endpoint                           (OAuth host-side)
//
// Pix ships NO built-in servers: every one of these comes from a pack. There
// is no special case for any particular vendor — a server that needs hardened
// flags declares them as Args, where a reviewer can see them.
type MCPServer struct {
	// Command + Args are the host binary and its LITERAL argv. Pix never
	// templates Args, so what a reviewer reads in the pack manifest is
	// character-for-character what the gateway spawns. Anything that varies
	// per user travels as an environment variable instead (EnvKeys).
	Command string
	Args    []string

	Image     string
	EnvKeys   []string          // env var NAMES forwarded to a Command/Image server (-e KEY)
	EnvValues map[string]string // non-secret literals forwarded as -e KEY=VALUE

	Manifest  string
	RemoteURL string // remote MCP endpoint URL (`sbx mcp add <name> --url <url>`)

	// Probe is the argv that answers "can this server actually do its job",
	// as distinct from "is it registered". Registration is not health: a
	// server can be registered and unable to authenticate. Doctor runs this
	// and shows its output; nothing else does. Empty means the pack declared
	// no probe, which doctor reports as unverifiable rather than as healthy.
	Probe []string
}

// HostExec reports whether this server runs a command on the HOST — the
// distinction that decides whether it enters a pack's Tier-1 trust surface.
// Manifest and RemoteURL servers do not: the gateway resolves and runs them.
func (s MCPServer) HostExec() bool { return s.Command != "" || s.Image != "" }

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
