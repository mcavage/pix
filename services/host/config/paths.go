// XDG storage path resolution for pi-stack, path-only (no sqlite, no fs
// mutation). This is the single module every consumer routes through so the
// layout stays consistent: CONFIG (~/.config/pi-stack) holds config.toml,
// op-refs.env, and the broker token; DATA (~/.local/share/pi-stack) holds the
// precious artifacts (memory, knowledge bundle, backups); STATE
// (~/.local/state/pi-stack) holds the regenerable ones (index, caches, tasks,
// serve.pid).
//
// Precedence per typed helper is exactly: explicit file/dir env override
// (MEMORY_DB, KNOWLEDGE_DB, KNOWLEDGE_CACHE_DIR, PI_STACK_CONFIG) > the NEW path
// if it exists > the LEGACY path if it exists > the NEW path fresh. The
// read-fallback is READ-ONLY and self-disarming: it returns a legacy location
// ONLY while the new one is absent, so the instant migration publishes the new
// path every resolver returns new (the single-authority rule). The legacy
// branches are documented for removal next major.
package config

import (
	"os"
	"path/filepath"
	"strings"
)

// DataDir resolves the DATA base ($XDG_DATA_HOME/pi-stack, else
// ~/.local/share/pi-stack) — the home of the precious artifacts.
func DataDir() (string, error) {
	if xdg := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); xdg != "" {
		return filepath.Join(xdg, "pi-stack"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "pi-stack"), nil
}

// StateDir resolves the STATE base ($XDG_STATE_HOME/pi-stack, else
// ~/.local/state/pi-stack) — the home of the regenerable artifacts.
func StateDir() (string, error) {
	if xdg := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); xdg != "" {
		return filepath.Join(xdg, "pi-stack"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "pi-stack"), nil
}

// pathExists reports whether p resolves to something on disk. Stat follows
// symlinks, so after migration a legacy dir that is a symlink→new still reads as
// existing — but the resolvers short-circuit on the new path first, so the legacy
// branch is never consulted once new exists.
func pathExists(p string) bool {
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

// legacyPiStackPath joins parts under the legacy ~/.pi-stack root, returning ""
// when the home dir cannot be resolved.
func legacyPiStackPath(parts ...string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(append([]string{home, ".pi-stack"}, parts...)...)
}

// legacyConfigPath joins parts under the legacy ~/.config/pi-stack root. Unlike
// ConfigDir this does NOT honor XDG_CONFIG_HOME / PI_STACK_CONFIG: it is the
// literal pre-XDG location a few artifacts (knowledge-cache, serve.pid) once used
// as a config sibling, kept only for the read-fallback.
func legacyConfigPath(parts ...string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(append([]string{home, ".config", "pi-stack"}, parts...)...)
}

// BrokerTokenPath is the formal name for the broker token location:
// <config-dir>/broker-token, a sibling of config.toml. It delegates to TokenPath
// (token.go) so the two never drift.
func BrokerTokenPath() string { return TokenPath() }

// LegacyServePidPath is the pre-XDG pidfile location (<config-dir>/serve.pid).
// `serve stop` / `serve status` probe ServePidPath() (STATE) first, then this,
// for one release so a serve started before the upgrade stays controllable.
// Removed next major.
func LegacyServePidPath() string {
	dir, err := ConfigDir()
	if err != nil {
		return "serve.pid"
	}
	return filepath.Join(dir, "serve.pid")
}

// BackupsDir resolves the backups directory with the read-fallback: NEW
// DATA/backups if it exists, else legacy ~/.pi-stack/backups if it exists, else
// NEW DATA/backups fresh.
func BackupsDir() (string, error) {
	data, err := DataDir()
	if err != nil {
		return "", err
	}
	newPath := filepath.Join(data, "backups")
	if pathExists(newPath) {
		return newPath, nil
	}
	if legacy := legacyPiStackPath("backups"); pathExists(legacy) {
		return legacy, nil
	}
	return newPath, nil
}

// KnowledgeBundleDefault is DATA/knowledge — the default location for a
// scaffolded OKF bundle. There is NO read-fallback here: configured bundle paths
// live in config (knowledge_bundles), not resolved through this module, so this
// only names the fresh default.
func KnowledgeBundleDefault() (string, error) {
	data, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(data, "knowledge"), nil
}

// KnowledgeIndexPath resolves the knowledge index sqlite path with the full
// 5-level precedence (finding 8): $KNOWLEDGE_DB > STATE/knowledge/index.db if it
// exists > STATE/knowledge/knowledge.db if it exists > legacy
// ~/.pi-stack/knowledge/knowledge.db if it exists > STATE/knowledge/index.db
// fresh. The index is a disposable cache, so migration rebuilds it at the new
// name (index.db) rather than moving the legacy file; the legacy names are read
// only until the rebuilt index exists.
func KnowledgeIndexPath() string {
	if p := strings.TrimSpace(os.Getenv("KNOWLEDGE_DB")); p != "" {
		return p
	}
	var indexNew string
	if state, err := StateDir(); err == nil {
		indexNew = filepath.Join(state, "knowledge", "index.db")
		if pathExists(indexNew) {
			return indexNew
		}
		if legacyName := filepath.Join(state, "knowledge", "knowledge.db"); pathExists(legacyName) {
			return legacyName
		}
	}
	if legacy := legacyPiStackPath("knowledge", "knowledge.db"); legacy != "" && pathExists(legacy) {
		return legacy
	}
	if indexNew != "" {
		return indexNew
	}
	return filepath.Join("knowledge", "index.db")
}

// KnowledgeCacheDir resolves the git-URL bundle cache directory with the
// read-fallback: $KNOWLEDGE_CACHE_DIR > STATE/knowledge-cache if it exists >
// legacy ~/.config/pi-stack/knowledge-cache if it exists > STATE/knowledge-cache
// fresh.
func KnowledgeCacheDir() string {
	if p := strings.TrimSpace(os.Getenv("KNOWLEDGE_CACHE_DIR")); p != "" {
		return p
	}
	var newPath string
	if state, err := StateDir(); err == nil {
		newPath = filepath.Join(state, "knowledge-cache")
		if pathExists(newPath) {
			return newPath
		}
	}
	if legacy := legacyConfigPath("knowledge-cache"); legacy != "" && pathExists(legacy) {
		return legacy
	}
	if newPath != "" {
		return newPath
	}
	return "knowledge-cache"
}

// TasksRoot is STATE/tasks — the base dir for launcher task state (worktrees +
// metadata). It is the same value task.go's taskStateRoot already computes; the
// launcher routes through this so the location lives in one place.
func TasksRoot() (string, error) {
	state, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, "tasks"), nil
}
