// truststore.go — the launcher-owned HOST trust state for packs (packs.md §9).
//
// CORE PRINCIPLE: nothing security-relevant is ever trusted from inside the
// pack payload. Acceptance in pack.lock would sit INSIDE the pack directory —
// attacker-controlled for any downloaded/cloned pack — so a pre-filled lock
// could pre-accept its own host-exec surface and walk past the Tier-1 gate.
// Everything security-relevant therefore lives in <config-dir>/pack-trust.json,
// a file only the launcher writes:
//
//   - Accepted: per pack identity, the FINGERPRINT of the entire host-exec
//     surface the user approved. The gate skips the prompt ONLY on an exact
//     match — a changed MCP argv, a mutated host proxy script, a changed [[bin]]
//     sha or a new egress domain all re-gate.
//   - Adopted: clone provenance (remote + commit) recorded by the HOST at clone
//     time, keyed by canonical path — the trusted source for pack identity and
//     the adopted-pack guard.
//   - Installed: which wrapper names are currently in HostPackBinDir() and which
//     pack put them there, so clear/swap stays reliable even when the pack
//     directory is gone. Attribution is discarded only on CONFIRMED removal.
//   - Activations: the ACTIVATION PROVENANCE ledger (which mcp/gog_account/
//     ollama_bridge_model entries each active pack's last activation
//     contributed, plus the prior values to restore), one record per pack in
//     command order. This is the ONLY source revertPackPriorContribution reads;
//     pack.lock remains a human-readable local hint, never trusted.
//
// Pack identity (TrustKey): "remote:<url>" when the launcher's own adoption
// provenance exists for the canonical path, else "path:<canonical-abs-path>".
// Identity is STABLE across commits — the commit is provenance METADATA, never
// part of the key — so a README-only pull never re-prompts while any surface
// change still re-gates via the fingerprint. Even a forged identity buys
// nothing: matching the fingerprint requires a byte-identical surface.
//
// CONCURRENCY: every read-modify-write of this file is serialized by a
// cross-process flock held across a FRESH load → mutate → save (see
// mutatePackTrustStore/withPackTrustLock). Otherwise `pack use` racing a
// wrapper refresh is last-writer-wins over a stale in-memory object.
package pack

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"pix/host/config"
	"pix/host/sys"
)

const packTrustStoreName = "pack-trust.json"

// packTrustLockPath is the advisory cross-process lock file serializing every
// trust-store read-modify-write. It lives in the STATE dir — never in the config
// dir beside the store, so a `pix state reset` cannot orphan a held lock.
func packTrustLockPath() string {
	dir, err := config.StateDir()
	if err != nil {
		return filepath.Join(filepath.Dir(config.Path()), "pack-trust.lock")
	}
	return filepath.Join(dir, "pack-trust.lock")
}

// withPackTrustLock runs fn holding the exclusive cross-process flock that
// serializes trust-store writes (the non-unix shim runs fn unserialized;
// single-process correctness is unaffected there). SINGLE lock, never nested:
// fn must not call another withPackTrustLock/mutatePackTrustStore/
// clearInstalledHostPackWrappers/RefreshHostPackWrappers — flock is per open
// file description, so a nested acquire in the same process self-deadlocks.
// Code already holding the lock uses the *Locked variants.
func withPackTrustLock(fn func() error) error {
	return sys.Lock(packTrustLockPath(), fn)
}

// mutatePackTrustStore is the sanctioned way to WRITE the trust store: under
// the cross-process lock it re-loads the store FRESH from disk, applies mutate,
// and saves — so no caller can save a stale in-memory object over a concurrent
// writer's committed record. Returning an error aborts without saving. The
// freshly-saved store is returned so callers can sync their own view.
func mutatePackTrustStore(mutate func(*PackTrustStore) error) (*PackTrustStore, error) {
	var fresh *PackTrustStore
	err := withPackTrustLock(func() error {
		var e error
		fresh, e = mutatePackTrustStoreLocked(mutate)
		return e
	})
	return fresh, err
}

// mutatePackTrustStoreLocked is the ALREADY-HOLDING-THE-LOCK core of
// mutatePackTrustStore: fresh load → mutate → save, no lock acquisition of its
// own. It exists precisely so a locked region (the transactional wrapper
// refresh, `pack rm`) never nests the flock and self-deadlocks.
func mutatePackTrustStoreLocked(mutate func(*PackTrustStore) error) (*PackTrustStore, error) {
	s, lerr := loadPackTrustStore()
	if lerr != nil {
		return nil, lerr
	}
	if merr := mutate(s); merr != nil {
		return nil, merr
	}
	if serr := s.Save(); serr != nil {
		return nil, serr
	}
	return s, nil
}

// packTrustStorePath is <config-dir>/pack-trust.json — beside config.toml,
// host-owned, never inside any pack.
func packTrustStorePath() string {
	return filepath.Join(filepath.Dir(config.Path()), packTrustStoreName)
}

// PackTrustRecord is one accepted host-exec surface: the fingerprint the user
// approved at the Tier-1 gate, plus provenance for the record's own hygiene
// (recordAcceptance drops stale records for the same path/remote).
type PackTrustRecord struct {
	Path        string `json:"path,omitempty"`
	Remote      string `json:"remote,omitempty"`
	Commit      string `json:"commit,omitempty"`
	Fingerprint string `json:"fingerprint"`
}

// packProvenance is host-recorded clone provenance (written by markPackAdopted
// at clone time — never read from the pack payload).
type packProvenance struct {
	Remote string `json:"remote"`
	Commit string `json:"commit,omitempty"`
}

// packInstalledSet records what is currently installed in HostPackBinDir()
// and which pack (trust key) put it there. There is at most one: the dir only
// ever holds the ACTIVE pack's accepted wrappers.
type packInstalledSet struct {
	Owner    string   `json:"owner"`
	Wrappers []string `json:"wrappers"`
}

// packActivationRecord is the HOST-owned copy of one activation's contribution
// set (what the pack-payload pack.lock must never be trusted for). It is keyed
// by the same pack identity as acceptance (Owner = trustKey at activation time)
// plus the canonical path, so lookups survive a path→remote upgrade.
type packActivationRecord struct {
	Owner                  string   `json:"owner"`
	Path                   string   `json:"path"`
	MCP                    []string `json:"mcp,omitempty"`
	GogAccount             string   `json:"gog_account,omitempty"`
	PriorGogAccount        string   `json:"prior_gog_account,omitempty"`
	OllamaBridgeModel      string   `json:"ollama_bridge_model,omitempty"`
	PriorOllamaBridgeModel string   `json:"prior_ollama_bridge_model,omitempty"`
}

type PackTrustStore struct {
	Version   int                        `json:"version"`
	Accepted  map[string]PackTrustRecord `json:"accepted,omitempty"`
	Adopted   map[string]packProvenance  `json:"adopted,omitempty"`
	Installed *packInstalledSet          `json:"installed,omitempty"`
	// Activations is the ordered ownership ledger: one record per active pack,
	// in command order (a single-pack `pack use` writes a one-element ledger).
	Activations []packActivationRecord `json:"activations,omitempty"`
}

// loadPackTrustStore reads the trust store. Absent → an empty store (fresh
// host, nothing accepted). Unreadable/unparsable → an ERROR, never a partial
// decode: callers fail closed rather than trust half a store.
func loadPackTrustStore() (*PackTrustStore, error) {
	// Lstat-REFUSE a symlinked store file on READ too (write already does): a
	// pack-trust.json symlinked at an attacker-readable/-writable file must
	// never supply crafted acceptance records. Fail closed, never follow.
	if fi, lerr := os.Lstat(packTrustStorePath()); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink; refusing to read through it", packTrustStorePath())
	}
	b, err := os.ReadFile(packTrustStorePath())
	if err != nil {
		if os.IsNotExist(err) {
			return &PackTrustStore{Version: 1}, nil
		}
		return nil, err
	}
	var s PackTrustStore
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", packTrustStorePath(), err)
	}
	return &s, nil
}

// Save writes the store symlink-safe + atomic (the same posture as
// writePackLockBytes): Lstat-REFUSE a symlinked destination, then a same-dir
// temp + rename.
func (s *PackTrustStore) Save() error {
	dir := filepath.Dir(config.Path())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	dest := filepath.Join(dir, packTrustStoreName)
	if fi, err := os.Lstat(dest); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to write through it", dest)
	}
	s.Version = 1
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return sys.AtomicWriteInDir(dir, packTrustStoreName, append(b, '\n'), 0o644)
}

// TrustKey resolves a pack's identity for trust-store lookup: launcher-recorded
// clone provenance ("remote:<url>" — STABLE across commits) when present for the
// canonical path, else the canonical absolute path. The commit is provenance
// metadata on the records, never identity: keying by commit re-gated every pull
// even when the fingerprint — the actual control — was byte-identical. NEVER
// derived from pack.lock (untrusted payload).
func (s *PackTrustStore) TrustKey(root string) string {
	canon := CanonicalizePackRoot(root)
	if s != nil {
		if prov, ok := s.Adopted[canon]; ok && strings.TrimSpace(prov.Remote) != "" {
			return "remote:" + strings.TrimSpace(prov.Remote)
		}
	}
	return "path:" + canon
}

// acceptedFingerprint returns the fingerprint accepted for key, if any.
func (s *PackTrustStore) acceptedFingerprint(key string) (string, bool) {
	if s == nil || s.Accepted == nil {
		return "", false
	}
	if r, ok := s.Accepted[key]; ok && strings.TrimSpace(r.Fingerprint) != "" {
		return r.Fingerprint, true
	}
	return "", false
}

// recordAcceptance stores rec under key and drops stale records for the same
// remote or path (e.g. an earlier commit of the same clone) — acceptance is
// always for the CURRENT surface only.
func (s *PackTrustStore) RecordAcceptance(key string, rec PackTrustRecord) {
	if s.Accepted == nil {
		s.Accepted = map[string]PackTrustRecord{}
	}
	for k, r := range s.Accepted {
		if k == key {
			continue
		}
		if (rec.Remote != "" && r.Remote == rec.Remote) || (rec.Path != "" && r.Path == rec.Path) {
			delete(s.Accepted, k)
		}
	}
	s.Accepted[key] = rec
}

// activationFor returns the activation provenance HOST state attributes to
// root, as a packLock for revertPackPriorContribution. The record must be
// attributed to THIS pack — canonical path or trust-key match — else the zero
// value is returned (remove NOTHING). Nothing here reads the pack payload.
func (s *PackTrustStore) activationFor(root string) packLock {
	a := s.activationRecordFor(root)
	if a == nil {
		return packLock{}
	}
	return packLock{
		MCP:                    append([]string(nil), a.MCP...),
		GogAccount:             a.GogAccount,
		PriorGogAccount:        a.PriorGogAccount,
		OllamaBridgeModel:      a.OllamaBridgeModel,
		PriorOllamaBridgeModel: a.PriorOllamaBridgeModel,
	}
}

func (s *PackTrustStore) activationRecordFor(root string) *packActivationRecord {
	if s == nil {
		return nil
	}
	path, owner := CanonicalizePackRoot(root), s.TrustKey(root)
	for i := len(s.Activations) - 1; i >= 0; i-- {
		a := &s.Activations[i]
		if a.Path == path || a.Owner == owner {
			return a
		}
	}
	return nil
}

// setActivation records lock as the single active pack's contribution set (the
// caller saves the store; commitPackActivation owns the write ordering).
func (s *PackTrustStore) setActivation(root string, lock packLock) {
	s.setActivationStack([]packActivationRecord{s.newActivationRecord(root, lock)})
}

func (s *PackTrustStore) newActivationRecord(root string, lock packLock) packActivationRecord {
	return packActivationRecord{
		Owner:                  s.TrustKey(root),
		Path:                   CanonicalizePackRoot(root),
		MCP:                    append([]string(nil), lock.MCP...),
		GogAccount:             lock.GogAccount,
		PriorGogAccount:        lock.PriorGogAccount,
		OllamaBridgeModel:      lock.OllamaBridgeModel,
		PriorOllamaBridgeModel: lock.PriorOllamaBridgeModel,
	}
}

func (s *PackTrustStore) setActivationStack(records []packActivationRecord) {
	s.Activations = append([]packActivationRecord(nil), records...)
}

// recordPackAdoptionInTrustStore durably records clone provenance in HOST
// state, keyed by the clone's canonical path (called from markPackAdopted),
// under the cross-process store lock. A load error propagates (never clobber a
// store the user might fix); the caller treats it as best-effort — the
// pack.lock marker and the under-PacksDir location check keep the
// adopted-pack guard fail-safe.
func recordPackAdoptionInTrustStore(root, remote, commit string) error {
	_, err := mutatePackTrustStore(func(s *PackTrustStore) error {
		if s.Adopted == nil {
			s.Adopted = map[string]packProvenance{}
		}
		s.Adopted[CanonicalizePackRoot(root)] = packProvenance{Remote: remote, Commit: commit}
		return nil
	})
	return err
}
