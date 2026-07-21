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
//
// Pack identity (trustKey): "remote:<url>#<commit>" when the launcher's own
// adoption provenance exists for the pack's canonical path, else
// "path:<canonical-abs-path>". Identity is never derived from pack.lock — but
// even a forged identity buys nothing: the fingerprint is the actual control,
// and matching it requires a byte-identical host-exec surface.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"pi-stack/host/config"
)

const packTrustStoreName = "pack-trust.json"

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

type packTrustStore struct {
	Version   int                        `json:"version"`
	Accepted  map[string]packTrustRecord `json:"accepted,omitempty"`
	Adopted   map[string]packProvenance  `json:"adopted,omitempty"`
	Installed *packInstalledSet          `json:"installed,omitempty"`
}

// loadPackTrustStore reads the trust store. Absent → an empty store (fresh
// host, nothing accepted). Unreadable/unparsable → an ERROR, never a partial
// decode: callers fail closed (the gate re-prompts; a strict host launch
// refuses) rather than trusting half a store.
func loadPackTrustStore() (*packTrustStore, error) {
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
	return atomicWriteInDir(dir, packTrustStoreName, append(b, '\n'), 0o644)
}

// trustKey resolves a pack's identity for trust-store lookup: launcher-recorded
// clone provenance (remote#commit) when present for the canonical path, else
// the canonical absolute path. NEVER derived from pack.lock (untrusted payload).
func (s *packTrustStore) trustKey(root string) string {
	canon := canonicalizePackRoot(root)
	if s != nil {
		if prov, ok := s.Adopted[canon]; ok && strings.TrimSpace(prov.Remote) != "" {
			k := "remote:" + strings.TrimSpace(prov.Remote)
			if c := strings.TrimSpace(prov.Commit); c != "" {
				k += "#" + c
			}
			return k
		}
	}
	return "path:" + canon
}

// acceptedFingerprint returns the fingerprint accepted for key, if any.
func (s *packTrustStore) acceptedFingerprint(key string) (string, bool) {
	if s == nil || s.Accepted == nil {
		return "", false
	}
	r, ok := s.Accepted[key]
	if !ok || strings.TrimSpace(r.Fingerprint) == "" {
		return "", false
	}
	return r.Fingerprint, true
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
		if (rec.Remote != "" && r.Remote == rec.Remote) || (rec.Path != "" && r.Path == rec.Path) {
			delete(s.Accepted, k)
		}
	}
	s.Accepted[key] = rec
}

// recordPackAdoptionInTrustStore durably records clone provenance in HOST
// state, keyed by the clone's canonical path (called from markPackAdopted).
// A load error propagates (never clobber a store the user might fix); the
// caller treats it as best-effort — the pack.lock marker and the
// under-PacksDir location check keep the adopted-pack guard fail-safe.
func recordPackAdoptionInTrustStore(root, remote, commit string) error {
	s, err := loadPackTrustStore()
	if err != nil {
		return err
	}
	if s.Adopted == nil {
		s.Adopted = map[string]packProvenance{}
	}
	s.Adopted[canonicalizePackRoot(root)] = packProvenance{Remote: remote, Commit: commit}
	return s.save()
}
