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
// A ref string alone is not enough: the SAME ref can resolve to a DIFFERENT
// value later (rotation in place, e.g. the 1Password item's field was
// updated without changing which item/field is referenced). So the store also
// keeps a SHA-256 DIGEST of the resolved value alongside the ref — metadata
// only, never the value itself, and never printed. "known-same" (safe to skip
// without asking) requires BOTH the ref string AND the digest to match; a
// legacy record with a ref but no digest (predates this feature) is treated
// as UNKNOWN, not same, and goes through the normal batched
// confirm-before-overwrite path exactly like a genuinely new ref.
//
// Mirrors packtruststore.go's posture for a host-state JSON file: Lstat-refuse
// a symlinked store on both read and write, same-dir temp + rename for an
// atomic write, and a cross-process flock serializing every
// read-modify-write.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"pix/host/config"
	"pix/host/sys"
)

const syncedRefsStoreName = "synced-refs.json"

// syncedRefsStore maps a provider's op-refs.env ENV var to the op:// ref
// string last successfully synced to sbx for it (never the resolved secret
// value), plus a parallel Digests map (SHA-256 hex of the resolved value at
// sync time — metadata only). Digests is new; a store written before this
// feature has Synced but no Digests entries, which is exactly the "legacy,
// unknown" case callers must treat as not-known-same. `omitempty` on both
// keeps an all-legacy or all-fresh store's JSON unsurprising.
type syncedRefsStore struct {
	Version int               `json:"version"`
	Synced  map[string]string `json:"synced,omitempty"`
	Digests map[string]string `json:"digests,omitempty"`
}

// secretDigestHex returns the hex-encoded SHA-256 digest of s. Used only to
// fingerprint a resolved secret value for the synced-refs store — the digest is metadata
// (proves "same value" without storing or printing the value itself) and must
// never itself be treated as sensitive-equivalent to the secret (it's one-way).
func secretDigestHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// syncedRefsStorePath is <config-dir>/synced-refs.json, beside config.toml —
// host-owned, same home as pack-trust.json.
func syncedRefsStorePath() string {
	return filepath.Join(filepath.Dir(config.Path()), syncedRefsStoreName)
}

// syncedRefsLockPath is the advisory cross-process lock serializing every
// read-modify-write, living in the STATE dir (ephemeral runtime state) rather
// than beside the store itself — the same posture as packTrustLockPath, so a
// `pix state reset` moving the config dir aside never orphans a held
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
	return sys.AtomicWriteInDir(dir, syncedRefsStoreName, append(b, '\n'), 0o600)
}

// mutateSyncedRefsStore is the sanctioned write path: under the cross-process
// lock it re-loads the store FRESH from disk, applies mutate, and saves — so
// two concurrent `setup`/`secret sync` runs can't clobber each other's record.
func mutateSyncedRefsStore(mutate func(*syncedRefsStore) error) error {
	return sys.Lock(syncedRefsLockPath(), func() error {
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
// Ref-only: callers deciding whether it's safe to skip a confirm must use
// syncedRefKnownSame instead, which also requires the digest to match.
func syncedRef(envVar string) (string, bool) {
	s, err := loadSyncedRefsStore()
	if err != nil || s.Synced == nil {
		return "", false
	}
	r, ok := s.Synced[envVar]
	return r, ok
}

// syncedRefDigest returns the digest recorded alongside envVar's synced ref,
// if any. Absent for a legacy record (predates the Digests map) or when there
// is no record at all; both degrade to "" which never matches a real digest.
func syncedRefDigest(envVar string) string {
	s, err := loadSyncedRefsStore()
	if err != nil || s.Digests == nil {
		return ""
	}
	return s.Digests[envVar]
}

// syncedRefKnownSame is the ONLY condition under which reconcile may skip a
// provider without asking: the recorded ref must equal ref (the CURRENT
// op-refs.env ref) AND the recorded digest must equal secretDigestHex(value) (the
// CURRENT resolved value). Either mismatch — including a legacy record whose
// digest is empty — returns false, so the caller treats it as changed/unknown
// and routes it through the normal batched confirm-before-overwrite (or
// --yes) path. This is what makes "same ref, rotated value" (the value at a
// stable op:// ref changed) and "legacy record, no digest" both fail closed
// instead of silently trusting a ref string alone.
func syncedRefKnownSame(envVar, ref, value string) bool {
	recordedRef, ok := syncedRef(envVar)
	if !ok || recordedRef != ref {
		return false
	}
	digest := syncedRefDigest(envVar)
	if digest == "" {
		return false // legacy record (or no digest yet) — unknown, not same
	}
	return digest == secretDigestHex(value)
}

// recordSyncedRef records ref as the value successfully synced to sbx for
// envVar just now, WITHOUT a digest — this is the legacy/back-compat shape
// (used by callers that don't have the resolved value handy, e.g. older
// bookkeeping and tests simulating a pre-digest record). Production code
// after an actual sbx sync must use recordSyncedRefWithDigest instead, so the
// record it leaves behind is never mistaken for "known-same" without also
// having proven the resolved value. Clears any stale digest for envVar (a new
// ref invalidates whatever digest was recorded against the old one).
func recordSyncedRef(envVar, ref string) error {
	return mutateSyncedRefsStore(func(s *syncedRefsStore) error {
		if s.Synced == nil {
			s.Synced = map[string]string{}
		}
		s.Synced[envVar] = ref
		if s.Digests != nil {
			delete(s.Digests, envVar)
		}
		return nil
	})
}

// recordSyncedRefWithDigest records ref AND the SHA-256 digest of the resolved
// value ATOMICALLY (a single mutateSyncedRefsStore call — one flock-guarded
// load+mutate+save, so a reader never observes the ref updated without its
// digest or vice versa) as the state successfully synced to sbx for envVar
// just now. This is the production path: called only after `sbx secret set`
// has actually succeeded (secret_sync.go), so a record always reflects a
// value genuinely in sbx. digest is metadata (proves sameness) and is never
// itself printed by any caller.
func recordSyncedRefWithDigest(envVar, ref, digest string) error {
	return mutateSyncedRefsStore(func(s *syncedRefsStore) error {
		if s.Synced == nil {
			s.Synced = map[string]string{}
		}
		if s.Digests == nil {
			s.Digests = map[string]string{}
		}
		s.Synced[envVar] = ref
		s.Digests[envVar] = digest
		return nil
	})
}
