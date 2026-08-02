// packtruststore.go — the launcher-owned HOST trust state for packs (the F5
// trust-model rework; packs-v2-impl.md F5, packs.md §9).
//
// CORE PRINCIPLE: nothing security-relevant is ever trusted from inside the
// pack payload. Trust acceptance used to live in pack.lock (Accepted*), which
// is INSIDE the pack directory — attacker-controlled for any downloaded/
// unzipped/cloned pack — so a pre-filled pack.lock could pre-accept its own
// host-exec surface and walk straight past the Tier-1 gate on a local-path
// adoption. This file moves everything security-relevant into
// <config-dir>/pack-trust.json, a file only the launcher writes:
//
//   - Accepted: per pack identity, the FINGERPRINT of the entire host-exec
//     surface the user approved (computeHostExecFingerprint). The gate skips
//     the prompt ONLY on an exact fingerprint match — a changed MCP argv
//     (e.g. gog_account), a mutated host proxy script, a changed [[bin]] sha,
//     or a new egress domain all change the fingerprint and re-gate.
//   - Adopted: clone provenance (remote + commit) recorded by the HOST at
//     clone time, keyed by canonical path — the trusted source for both pack
//     identity and the adopted-pack knowledge guard, independent of anything
//     the pack ships.
//   - Installed: which wrapper names are currently in hostPackBinDir() and
//     which pack put them there, so clear/swap stays reliable even when the
//     pack directory itself is gone. Attribution is only discarded once
//     removal is CONFIRMED.
//   - Activation/Activations: the Phase-1 ACTIVATION PROVENANCE (which mcp/knowledge/
//     gog_account/ollama_bridge_model entries the ACTIVE pack's last
//     activation contributed, plus the prior config values to restore). The
//     ordered Activations ledger represents a composed stack; Activation is
//     retained for backward compatibility with single-pack state.
//     This used to live only in pack.lock — INSIDE the pack payload — so a
//     local `git pull`/zip update could forge it and make the next
//     switch-away DELETE the user's own config entries (the same-pack
//     reactivation path trusted the lock unconditionally). The launcher's
//     own record here is now the ONLY source revertPackPriorContribution
//     reads; pack.lock remains as a human-readable local hint but is never
//     trusted for reversibility.
//
// Pack identity (trustKey): "remote:<url>" when the launcher's own adoption
// provenance exists for the pack's canonical path, else
// "path:<canonical-abs-path>". The identity is STABLE across commits
// (round-3 #5): the commit is provenance METADATA on the records, never part
// of the key — acceptance is (stable identity, fingerprint), so a
// README-only pull with an unchanged host-exec surface never re-prompts,
// while any surface change still re-gates via the fingerprint. Identity is
// never derived from pack.lock — but even a forged identity buys nothing:
// the fingerprint is the actual control, and matching it requires a
// byte-identical host-exec surface.
//
// CONCURRENCY (round-3 #1): every read-modify-write of this file is
// serialized by a cross-process flock (packTrustLockPath) held across a
// FRESH load → mutate → save — see mutatePackTrustStore/withPackTrustLock.
// `pack use` racing a `pix host` wrapper refresh used to be plain
// last-writer-wins: whichever process loaded first could save its stale
// in-memory object over the other's committed activation/acceptance.
package main

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
// trust-store read-modify-write (round-3 #1). It lives in the STATE dir —
// ephemeral runtime state, the same home as the serve spawn lock — never in
// the config dir beside the store itself (a `pix state reset` moving the
// config dir aside must not orphan a held lock).
func packTrustLockPath() string {
	dir, err := config.StateDir()
	if err != nil {
		return filepath.Join(filepath.Dir(config.Path()), "pack-trust.lock")
	}
	return filepath.Join(dir, "pack-trust.lock")
}

// withPackTrustLock runs fn holding the exclusive cross-process flock that
// serializes trust-store writes. It reuses the shared blocking withFlock
// helper (serve_start_unix.go; the non-unix shim runs fn unserialized —
// single-process correctness is unaffected there). SINGLE lock, never
// nested: fn must not call another withPackTrustLock/mutatePackTrustStore/
// clearInstalledHostPackWrappers/refreshHostPackWrappers — flock is per open
// file description, so a nested acquire in the same process self-deadlocks.
// Code that already holds the lock uses the *Locked variants
// (mutatePackTrustStoreLocked, clearInstalledHostPackWrappersLocked,
// refreshHostPackWrappersLocked) instead.
func withPackTrustLock(fn func() error) error {
	return withFlock(packTrustLockPath(), fn)
}

// mutatePackTrustStore is the sanctioned way to WRITE the trust store
// (round-3 #1): under the cross-process lock it re-loads the store FRESH
// from disk, applies mutate, and saves — so no caller can ever save a stale
// in-memory object over a concurrent writer's committed record. mutate sees
// the CURRENT on-disk state; returning an error aborts without saving. The
// freshly-saved store is returned so callers can sync their own view.
func mutatePackTrustStore(mutate func(*packTrustStore) error) (*packTrustStore, error) {
	var fresh *packTrustStore
	err := withPackTrustLock(func() error {
		var e error
		fresh, e = mutatePackTrustStoreLocked(mutate)
		return e
	})
	return fresh, err
}

// mutatePackTrustStoreLocked is the ALREADY-HOLDING-THE-LOCK core of
// mutatePackTrustStore: fresh load → mutate → save, with NO lock acquisition
// of its own. Callers MUST hold withPackTrustLock — the flock is per open
// file description, so re-acquiring it in the same process self-deadlocks;
// this variant exists precisely so a locked region (the transactional
// wrapper refresh, `pack rm`) never nests the lock.
func mutatePackTrustStoreLocked(mutate func(*packTrustStore) error) (*packTrustStore, error) {
	s, lerr := loadPackTrustStore()
	if lerr != nil {
		return nil, lerr
	}
	if merr := mutate(s); merr != nil {
		return nil, merr
	}
	if serr := s.save(); serr != nil {
		return nil, serr
	}
	return s, nil
}

// packTrustStorePath is <config-dir>/pack-trust.json — beside config.toml,
// host-owned, never inside any pack.
func packTrustStorePath() string {
	return filepath.Join(filepath.Dir(config.Path()), packTrustStoreName)
}

// packTrustRecord is one accepted host-exec surface: the fingerprint the user
// approved at the Tier-1 gate, plus provenance for the record's own hygiene
// (recordAcceptance drops stale records for the same path/remote).
type packTrustRecord struct {
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

// packInstalledSet records what is currently installed in hostPackBinDir()
// and which pack (trust key) put it there. There is at most one: the dir only
// ever holds the ACTIVE pack's accepted wrappers.
type packInstalledSet struct {
	Owner    string   `json:"owner"`
	Wrappers []string `json:"wrappers"`
}

// packActivationRecord is the HOST-owned copy of one activation's Phase-1
// contribution set (what commitPackActivation used to trust pack.lock for).
// A composed stack has one record per active pack, in command order. It is
// keyed by the same pack identity as acceptance (Owner = trustKey at activation
// time) plus the canonical path, so lookups survive a path→remote upgrade.
type packActivationRecord struct {
	Owner                  string   `json:"owner"`
	Path                   string   `json:"path"`
	MCP                    []string `json:"mcp,omitempty"`
	Knowledge              []string `json:"knowledge,omitempty"`
	GogAccount             string   `json:"gog_account,omitempty"`
	PriorGogAccount        string   `json:"prior_gog_account,omitempty"`
	OllamaBridgeModel      string   `json:"ollama_bridge_model,omitempty"`
	PriorOllamaBridgeModel string   `json:"prior_ollama_bridge_model,omitempty"`
}

type packTrustStore struct {
	Version    int                        `json:"version"`
	Accepted   map[string]packTrustRecord `json:"accepted,omitempty"`
	Adopted    map[string]packProvenance  `json:"adopted,omitempty"`
	Installed  *packInstalledSet          `json:"installed,omitempty"`
	Activation *packActivationRecord      `json:"activation,omitempty"`
	// Activations is the ordered ownership ledger for a composed pack stack.
	// Activation remains the backward-compatible single-pack field. New stack
	// writes populate Activations and clear Activation; readers accept both.
	Activations []packActivationRecord `json:"activations,omitempty"`
}

// loadPackTrustStore reads the trust store. Absent → an empty store (fresh
// host, nothing accepted). Unreadable/unparsable → an ERROR, never a partial
// decode: callers fail closed (the gate re-prompts; a strict host launch
// refuses) rather than trusting half a store.
func loadPackTrustStore() (*packTrustStore, error) {
	// Lstat-REFUSE a symlinked store file on READ too (write already does): a
	// pack-trust.json symlinked at an attacker-readable/-writable file must
	// never supply crafted acceptance records. Fail closed, never follow.
	if fi, lerr := os.Lstat(packTrustStorePath()); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink; refusing to read through it", packTrustStorePath())
	}
	b, err := os.ReadFile(packTrustStorePath())
	if err != nil {
		if os.IsNotExist(err) {
			return &packTrustStore{Version: 1}, nil
		}
		return nil, err
	}
	var s packTrustStore
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", packTrustStorePath(), err)
	}
	return &s, nil
}

// save writes the store symlink-safe + atomic (the same posture as
// writePackLockBytes): Lstat-REFUSE a symlinked destination, then a same-dir
// temp + rename via atomicWriteInDir. The config dir is host-owned, but the
// consistency costs nothing and the class fix stays uniform.
func (s *packTrustStore) save() error {
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

// trustKey resolves a pack's identity for trust-store lookup: launcher-recorded
// clone provenance ("remote:<url>" — STABLE across commits, round-3 #5) when
// present for the canonical path, else the canonical absolute path. The
// commit is provenance metadata on the records, never identity: keying
// acceptance by commit re-gated every pull even when the host-exec
// fingerprint — the actual control — was byte-identical. NEVER derived from
// pack.lock (untrusted payload).
func (s *packTrustStore) trustKey(root string) string {
	canon := canonicalizePackRoot(root)
	if s != nil {
		if prov, ok := s.Adopted[canon]; ok && strings.TrimSpace(prov.Remote) != "" {
			return "remote:" + strings.TrimSpace(prov.Remote)
		}
	}
	return "path:" + canon
}

// acceptedFingerprint returns the fingerprint accepted for key, if any. A
// legacy record keyed "remote:<url>#<commit>" (the pre-round-3 scheme) is
// honored via a one-time prefix fallback so an existing acceptance does not
// spuriously re-prompt after the identity became commit-stable; the
// fingerprint comparison at the call site is still the control, and
// recordAcceptance sweeps the legacy key on the next write.
func (s *packTrustStore) acceptedFingerprint(key string) (string, bool) {
	if s == nil || s.Accepted == nil {
		return "", false
	}
	if r, ok := s.Accepted[key]; ok && strings.TrimSpace(r.Fingerprint) != "" {
		return r.Fingerprint, true
	}
	for k, r := range s.Accepted {
		if strings.HasPrefix(k, key+"#") && strings.TrimSpace(r.Fingerprint) != "" {
			return r.Fingerprint, true
		}
	}
	return "", false
}

// recordAcceptance stores rec under key and drops stale records for the same
// remote or path (e.g. an earlier commit of the same clone) — acceptance is
// always for the CURRENT surface only.
func (s *packTrustStore) recordAcceptance(key string, rec packTrustRecord) {
	if s.Accepted == nil {
		s.Accepted = map[string]packTrustRecord{}
	}
	for k, r := range s.Accepted {
		if k == key {
			continue
		}
		if strings.HasPrefix(k, key+"#") || // legacy commit-suffixed key (pre-round-3 #5)
			(rec.Remote != "" && r.Remote == rec.Remote) || (rec.Path != "" && r.Path == rec.Path) {
			delete(s.Accepted, k)
		}
	}
	s.Accepted[key] = rec
}

// activationFor returns the activation provenance HOST state attributes to
// root, as a packLock for revertPackPriorContribution. The record must be
// attributed to THIS pack — canonical path or trust-key match — else the
// zero value is returned (remove NOTHING; the safe default, same posture as
// a missing lock). Nothing here ever reads the pack payload.
func (s *packTrustStore) activationFor(root string) packLock {
	a := s.activationRecordFor(root)
	if a == nil {
		return packLock{}
	}
	return packLock{
		MCP:                    append([]string(nil), a.MCP...),
		Knowledge:              append([]string(nil), a.Knowledge...),
		GogAccount:             a.GogAccount,
		PriorGogAccount:        a.PriorGogAccount,
		OllamaBridgeModel:      a.OllamaBridgeModel,
		PriorOllamaBridgeModel: a.PriorOllamaBridgeModel,
	}
}

// hasActivationFor reports whether the store carries an activation record
// attributed to root (canonical path or trust-key match) — the existence test
// behind activationFor's zero-value contract, split out so the one-time
// Phase-1 migration (migratePhase1Activation) can tell "no record" apart
// from "a record with an empty contribution set".
func (s *packTrustStore) hasActivationFor(root string) bool {
	return s.activationRecordFor(root) != nil
}

func (s *packTrustStore) activationRecordFor(root string) *packActivationRecord {
	if s == nil {
		return nil
	}
	path, owner := canonicalizePackRoot(root), s.trustKey(root)
	for i := len(s.Activations) - 1; i >= 0; i-- {
		a := &s.Activations[i]
		if a.Path == path || a.Owner == owner {
			return a
		}
	}
	if s.Activation != nil && (s.Activation.Path == path || s.Activation.Owner == owner) {
		return s.Activation
	}
	return nil
}

// setActivation records lock as the active pack's contribution set (the
// caller saves the store; commitPackActivation owns the write ordering).
func (s *packTrustStore) setActivation(root string, lock packLock) {
	a := s.newActivationRecord(root, lock)
	s.Activation = &a
	s.Activations = nil
}

func (s *packTrustStore) newActivationRecord(root string, lock packLock) packActivationRecord {
	return packActivationRecord{
		Owner:                  s.trustKey(root),
		Path:                   canonicalizePackRoot(root),
		MCP:                    append([]string(nil), lock.MCP...),
		Knowledge:              append([]string(nil), lock.Knowledge...),
		GogAccount:             lock.GogAccount,
		PriorGogAccount:        lock.PriorGogAccount,
		OllamaBridgeModel:      lock.OllamaBridgeModel,
		PriorOllamaBridgeModel: lock.PriorOllamaBridgeModel,
	}
}

func (s *packTrustStore) setActivationStack(records []packActivationRecord) {
	s.Activation = nil
	s.Activations = append([]packActivationRecord(nil), records...)
}

func (s *packTrustStore) clearActivations() {
	s.Activation = nil
	s.Activations = nil
}

// recordPackAdoptionInTrustStore durably records clone provenance in HOST
// state, keyed by the clone's canonical path (called from markPackAdopted),
// under the cross-process store lock (round-3 #1). A load error propagates
// (never clobber a store the user might fix); the caller treats it as
// best-effort — the pack.lock marker and the under-PacksDir location check
// keep the adopted-pack guard fail-safe.
func recordPackAdoptionInTrustStore(root, remote, commit string) error {
	_, err := mutatePackTrustStore(func(s *packTrustStore) error {
		if s.Adopted == nil {
			s.Adopted = map[string]packProvenance{}
		}
		s.Adopted[canonicalizePackRoot(root)] = packProvenance{Remote: remote, Commit: commit}
		return nil
	})
	return err
}
