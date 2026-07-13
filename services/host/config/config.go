// Package config is the pi-stack config schema + loader, shared by the host
// binary (pi-stack-host) AND the launcher binary. Deliberately dependency-light:
// only a TOML decoder — NO sqlite, NO go-plugin — so the launcher stays tiny.
//
// Config lives at $PI_STACK_CONFIG, else $XDG_CONFIG_HOME/pi-stack/config.toml,
// else ~/.config/pi-stack/config.toml. Absence is not an error: Load() returns a
// Config populated with sane defaults so a fresh install works with no file.
package config

import (
	"os"
	"path/filepath"

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

// Config is the pi-stack configuration, decoded from TOML.
type Config struct {
	VersionPin string   `toml:"version_pin"`
	Services   []string `toml:"services"`
	MCP        []string `toml:"mcp"`

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

// defaults returns a Config with the sane defaults applied to any unset field.
func (c *Config) applyDefaults() {
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
