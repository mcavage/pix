package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// environment.go: the schema-level half of Story 1 (native sandbox
// environments, docs/design/environments.md §5.3). It owns exactly the two
// invariants the design calls out for `config.toml`:
//
//   - registration helpers accept a leading `~`, but only a CANONICAL
//     ABSOLUTE path is ever assigned to Environments;
//   - `environment` and `environments.*` have no hand-edit path — every
//     writer here is a method a `pix env` verb (Wave C) calls, never
//     something `pix config set/unset` reaches (see
//     workflow/provision/config.go's environmentKeyRefusal).
//
// It does not implement the `pix env` verbs, host trust review, or launch
// wiring; those are later Wave C/Story 1 units.

// CanonicalEnvironmentPath expands a leading `~` (via $HOME), then resolves
// the result to an absolute, cleaned path. It does not require the path to
// exist — registration may name a directory `pix env add` is about to
// scaffold. This is the ONLY transform AddEnvironment applies before storing,
// so it is exported for a caller (a future `pix env` verb, or a test) that
// needs the exact canonical form without going through the registry.
func CanonicalEnvironmentPath(path string) (string, error) {
	expanded, err := expandHome(path)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

// expandHome expands a leading "~" or "~/..." to the user's home directory.
// Anything else (including "~otheruser/...", which this does not special-case)
// passes through unchanged.
func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve ~: %w", err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
}

// isCanonicalEnvironmentPath reports whether path is already exactly what
// CanonicalEnvironmentPath would produce: absolute, no leading `~`, and equal
// to its own filepath.Clean. Load uses this to fail closed on a value that
// could only have reached the file by hand.
func isCanonicalEnvironmentPath(path string) bool {
	if path == "" || strings.HasPrefix(path, "~") || !filepath.IsAbs(path) {
		return false
	}
	return filepath.Clean(path) == path
}

// AddEnvironment registers name against path in the Environments index,
// canonicalizing path first (see CanonicalEnvironmentPath) so what gets
// persisted is always absolute regardless of what the caller typed. It
// overwrites an existing registration under the same name. Returns the
// canonical path actually stored.
//
// An empty or whitespace-only path is refused outright rather than passed to
// CanonicalEnvironmentPath: filepath.Abs("") resolves to the current working
// directory, so a blank path would silently register whatever directory the
// caller happened to be standing in as the environment root instead of
// failing loudly. Same for name: a whitespace-only name would otherwise
// register under a name indistinguishable from empty once trimmed elsewhere.
func (c *Config) AddEnvironment(name, path string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("environment name must not be empty")
	}
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("environment path must not be empty")
	}
	canon, err := CanonicalEnvironmentPath(path)
	if err != nil {
		return "", err
	}
	if c.Environments == nil {
		c.Environments = map[string]string{}
	}
	c.Environments[name] = canon
	return canon, nil
}

// RemoveEnvironment unregisters name, returning true when it changed. If name
// was the machine default (Environment), the default is cleared too — a
// default may never dangle, naming a registration that no longer exists.
func (c *Config) RemoveEnvironment(name string) bool {
	if _, ok := c.Environments[name]; !ok {
		return false
	}
	delete(c.Environments, name)
	if c.Environment == name {
		c.Environment = ""
	}
	return true
}

// UseEnvironment sets the machine default environment NAME. name must already
// be registered in Environments, or empty to clear the default outright. This
// enforces only "this name exists" — the host trust review Wave C's `pix env
// use` performs before selecting an environment is a separate, later gate.
func (c *Config) UseEnvironment(name string) error {
	if name == "" {
		c.Environment = ""
		return nil
	}
	if _, ok := c.Environments[name]; !ok {
		return fmt.Errorf("no environment named %q (register it first: pix env add %s [path])", name, name)
	}
	c.Environment = name
	return nil
}
