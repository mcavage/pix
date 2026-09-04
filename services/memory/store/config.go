// Package store is the pix-memory data layer: schema, migration,
// remember/recall/forget/stats, the Ollama-backed embedder and watcher
// capture, the secret filter, and snapshot/restore. It has no dependency on
// the pix-host module: this package is copied out of services/host/{memory.go,
// memembed.go,memory_snapshot.go,memory_secretfilter.go,memory_capture_mode.go}
// (pix-v2 U2) to run standalone inside the pix-memory container. Config that
// used to come from ~/.config/pix/config.toml is now plain environment
// variables, matching the container contract in
// docs/design/pix-v2-architecture.md §9.1: one mount, /data, no config file.
package store

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultDataDir is where the standalone container mounts its state
// (docs/design/pix-v2-architecture.md §9.1: "mount: ~/.pix/state/memory:/data").
const DefaultDataDir = "/data"

// DBPath resolves the live memory sqlite path: $MEMORY_DB if set, else
// <data-dir>/memory.db. $MEMORY_DATA_DIR overrides DefaultDataDir (tests and
// non-container runs).
func DBPath() string {
	if p := strings.TrimSpace(os.Getenv("MEMORY_DB")); p != "" {
		return p
	}
	return filepath.Join(DataDir(), "memory.db")
}

// DataDir resolves the mounted state directory: $MEMORY_DATA_DIR if set, else
// DefaultDataDir.
func DataDir() string {
	if d := strings.TrimSpace(os.Getenv("MEMORY_DATA_DIR")); d != "" {
		return d
	}
	return DefaultDataDir
}

// SnapshotDir is where memory_snapshot writes by default:
// <data-dir>/backups (docs/design/pix-v2-architecture.md §9.2).
func SnapshotDir() string {
	return filepath.Join(DataDir(), "backups")
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// envMs parses a millisecond duration from an env var, defensively: an
// absent, empty, unparsable, or non-positive value falls back to def rather
// than ever disabling the timeout.
func envMs(key string, defMs int) time.Duration {
	ms := defMs
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			ms = p
		}
	}
	return time.Duration(ms) * time.Millisecond
}
