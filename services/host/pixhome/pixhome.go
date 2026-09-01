// Package pixhome resolves and lays out PIX_HOME — the single root that
// holds every Pix-owned file and every piece of runtime state
// (docs/design/pix-v2-architecture.md §5, docs/design/pix-v2-surface.md §4).
//
// There is no XDG fallback: architecture §5 is explicit that v2 does not
// split one personal tool across XDG config/data/state/cache roots the way
// the v1 launcher's config package still does. Resolution is exactly two
// rules — $PIX_HOME when set, else ~/.pix — and nothing else consults the
// environment.
//
// This package is dependency-light on purpose (the same posture config's own
// doc comment states for itself): stdlib only, no domain knowledge, so any
// later package can depend on it without pulling in a capability.
package pixhome

import (
	"os"
	"path/filepath"
	"strings"
)

// EnvVar is the one environment variable pixhome consults. Setting it
// overrides the default location for tests and advanced installations
// (pix-v2-surface.md §3.4); nothing else — no XDG_* variable of any kind —
// changes where PIX_HOME resolves.
const EnvVar = "PIX_HOME"

// DefaultDirName is the directory pixhome creates under the user's home
// directory when EnvVar is unset.
const DefaultDirName = ".pix"

// Dir resolves the Pix home directory: $PIX_HOME when non-empty (made
// absolute and cleaned relative to the current working directory), else
// ~/.pix. It never consults XDG_CONFIG_HOME, XDG_DATA_HOME, XDG_STATE_HOME,
// or any other legacy variable.
func Dir() (string, error) {
	if p := strings.TrimSpace(os.Getenv(EnvVar)); p != "" {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", err
		}
		return filepath.Clean(abs), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, DefaultDirName), nil
}

// Paths is every path below one resolved Pix home, matching the layout in
// docs/design/pix-v2-architecture.md §5 byte-for-byte. Building this value
// never touches the filesystem; only Init does.
type Paths struct {
	// Home is the resolved PIX_HOME root itself.
	Home string

	Git       string // .git
	Gitignore string // .gitignore
	README    string // README.md

	Context             string // context/ — the one personal content layer
	ContextSkills       string // context/skills/
	ContextOutputStyles string // context/output-styles/
	Envs                string // envs/<name>/

	ConfigTOML string // config.toml — machine-wide choices only
	SecretsEnv string // secrets.env — op:// references only

	Runtime string // runtime/<pix-version>/

	State                  string // .state/
	StateEffective         string // .state/effective/<sandbox>/
	StateMemory            string // .state/memory/
	StateMemoryBackups     string // .state/memory/backups/
	StateSandboxes         string // .state/sandboxes/<sandbox>/
	StateSessions          string // .state/sessions/<tree-id>/
	StateTasks             string // .state/tasks/<repo-key>/
	StateTrust             string // .state/trust/
	StateTrustEnvironments string // .state/trust/environments/<name>.json
}

// Resolve builds Paths from Dir().
func Resolve() (Paths, error) {
	home, err := Dir()
	if err != nil {
		return Paths{}, err
	}
	return New(home), nil
}

// New builds Paths for an explicit home root — a test's temp directory, or
// Dir()'s own result. It performs no filesystem I/O and no validation of
// home itself; callers that need an existing, well-formed root call Init.
func New(home string) Paths {
	p := Paths{Home: home}

	p.Git = filepath.Join(home, ".git")
	p.Gitignore = filepath.Join(home, ".gitignore")
	p.README = filepath.Join(home, "README.md")

	p.Context = filepath.Join(home, "context")
	p.ContextSkills = filepath.Join(p.Context, "skills")
	p.ContextOutputStyles = filepath.Join(p.Context, "output-styles")
	p.Envs = filepath.Join(home, "envs")

	p.ConfigTOML = filepath.Join(home, "config.toml")
	p.SecretsEnv = filepath.Join(home, "secrets.env")

	p.Runtime = filepath.Join(home, "runtime")

	p.State = filepath.Join(home, ".state")
	p.StateEffective = filepath.Join(p.State, "effective")
	p.StateMemory = filepath.Join(p.State, "memory")
	p.StateMemoryBackups = filepath.Join(p.StateMemory, "backups")
	p.StateSandboxes = filepath.Join(p.State, "sandboxes")
	p.StateSessions = filepath.Join(p.State, "sessions")
	p.StateTasks = filepath.Join(p.State, "tasks")
	p.StateTrust = filepath.Join(p.State, "trust")
	p.StateTrustEnvironments = filepath.Join(p.StateTrust, "environments")

	return p
}

// EnvironmentDir returns the canonical directory for a named environment
// under envs/ (pix-v2-surface.md §3.4). It does not resolve symlinks or
// verify existence; that is workflow/env's job, layered above this package.
func (p Paths) EnvironmentDir(name string) string {
	return filepath.Join(p.Envs, name)
}

// RuntimeVersionDir returns runtime/<version>/ for a specific Pix version
// (architecture §5, surface §4.2).
func (p Paths) RuntimeVersionDir(version string) string {
	return filepath.Join(p.Runtime, version)
}
