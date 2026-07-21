// syncedrefs.go — the per-provider "which op:// ref did we last successfully
// sync into sbx" record backing setupProvisionKeys' STEP 2 (secret_sync.go).
//
// CONSTRAINT this file exists to work around: sbx secret values are
// WRITE-ONLY. `sbx secret ls` lists NAMES only — there is no way to read back
// what value a name currently holds. So "has the 1Password ref changed since
// we last synced it?" can't be answered by comparing sbx's value to the ref's
// resolved value; it's answered by remembering the REF STRING (not the
// resolved secret) we synced last time, in launcher-owned host state. Never
// inside a pack, never sbx's own store, never the resolved secret value.
//
// Mirrors packtruststore.go's posture for a host-state JSON file: Lstat-refuse
// a symlinked store on both read and write, same-dir temp + rename for an
// atomic write, and a cross-process flock serializing every
// read-modify-write.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"pi-stack/host/config"
)

const syncedRefsStoreName = "synced-refs.json"

// syncedRefsStore maps a provider's op-refs.env ENV var to the op:// ref
// string last successfully synced to sbx for it (never the resolved secret
// value).
type syncedRefsStore struct {
	Version int               `json:"version"`
	Synced  map[string]string `json:"synced,omitempty"`
}

// syncedRefsStorePath is <config-dir>/synced-refs.json, beside config.toml —
// host-owned, same home as pack-trust.json.
func syncedRefsStorePath() string {
	return filepath.Join(filepath.Dir(config.Path()), syncedRefsStoreName)
}

// syncedRefsLockPath is the advisory cross-process lock serializing every
// read-modify-write, living in the STATE dir (ephemeral runtime state) rather
// than beside the store itself — the same posture as packTrustLockPath, so a
// `pi-stack state reset` moving the config dir aside never orphans a held
// lock.
func syncedRefsLockPath() string {
	dir, err := config.StateDir()
	if err != nil {
		return filepath.Join(filepath.Dir(config.Path()), "synced-refs.lock")
	}
	return filepath.Join(dir, "synced-refs.lock")
}

// loadSyncedRefsStore reads the store. Absent -> an empty store (fresh host,
// nothing synced yet). A symlinked or unparsable store is an ERROR: callers
// fail closed (treat it as "no recorded ref", which only ever costs an extra
// confirm-before-overwrite prompt, never a silent overwrite).
func loadSyncedRefsStore() (*syncedRefsStore, error) {
	if fi, lerr := os.Lstat(syncedRefsStorePath()); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink; refusing to read through it", syncedRefsStorePath())
	}
	b, err := os.ReadFile(syncedRefsStorePath())
	if err != nil {
		if os.IsNotExist(err) {
			return &syncedRefsStore{Version: 1}, nil
		}
		return nil, err
	}
	var s syncedRefsStore
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", syncedRefsStorePath(), err)
	}
	return &s, nil
}

// save writes the store symlink-safe + atomic: Lstat-refuse a symlinked
// destination, then a same-dir temp + rename via atomicWriteInDir.
func (s *syncedRefsStore) save() error {
	dir := filepath.Dir(config.Path())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	dest := filepath.Join(dir, syncedRefsStoreName)
	if fi, err := os.Lstat(dest); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to write through it", dest)
	}
	s.Version = 1
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteInDir(dir, syncedRefsStoreName, append(b, '\n'), 0o600)
}

// mutateSyncedRefsStore is the sanctioned write path: under the cross-process
// lock it re-loads the store FRESH from disk, applies mutate, and saves — so
// two concurrent `setup`/`secret sync` runs can't clobber each other's record.
func mutateSyncedRefsStore(mutate func(*syncedRefsStore) error) error {
	return withFlock(syncedRefsLockPath(), func() error {
		s, lerr := loadSyncedRefsStore()
		if lerr != nil {
			return lerr
		}
		if merr := mutate(s); merr != nil {
			return merr
		}
		return s.save()
	})
}

// syncedRef returns the ref recorded as last synced to sbx for envVar, if
// any. A load error (symlinked/corrupt store) degrades to "no record" — the
// caller then just re-confirms before overwriting, never silently overwrites.
func syncedRef(envVar string) (string, bool) {
	s, err := loadSyncedRefsStore()
	if err != nil || s.Synced == nil {
		return "", false
	}
	r, ok := s.Synced[envVar]
	return r, ok
}

// recordSyncedRef records ref as the value successfully synced to sbx for
// envVar just now.
func recordSyncedRef(envVar, ref string) error {
	return mutateSyncedRefsStore(func(s *syncedRefsStore) error {
		if s.Synced == nil {
			s.Synced = map[string]string{}
		}
		s.Synced[envVar] = ref
		return nil
	})
}
