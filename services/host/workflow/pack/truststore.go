// truststore.go — the launcher-owned HOST trust state for packs (packs.md §9).
//
// CORE PRINCIPLE: nothing security-relevant is ever trusted from inside the
// pack payload. Acceptance in pack.lock would sit INSIDE the pack directory —
// attacker-controlled for any cloned pack — so a pre-filled lock could
// pre-accept its own host-exec surface and walk past the Tier-1 gate. It
// therefore lives in <config-dir>/pack-trust.json, which only the launcher
// writes; the PackTrustStore fields below document what each section holds.
//
// CONCURRENCY: every read-modify-write is serialized by a cross-process flock
// held across a FRESH load → mutate → save; otherwise `pack use` racing a
// wrapper refresh is last-writer-wins over a stale in-memory object.
package pack

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"pix/host/config"
	"pix/host/packinfo"
	"pix/host/sys"
)

const packTrustStoreName = "pack-trust.json"

// packTrustLockPath is the advisory cross-process lock file serializing every
// trust-store read-modify-write. It lives in the STATE dir — never in the config
// dir beside the store, so moving the config dir aside cannot orphan a held lock.
func packTrustLockPath() string {
	dir, err := config.StateDir()
	if err != nil {
		return filepath.Join(filepath.Dir(config.Path()), "pack-trust.lock")
	}
	return filepath.Join(dir, "pack-trust.lock")
}

// withPackTrustLock runs fn holding the exclusive cross-process flock that
// serializes trust-store writes. SINGLE lock, never nested: fn must not call
// another withPackTrustLock/mutatePackTrustStore/refreshHostPackWrappers —
// flock is per open file description, so a nested acquire self-deadlocks. Code
// already holding the lock uses the *Locked variants.
func withPackTrustLock(fn func() error) error {
	return sys.Lock(packTrustLockPath(), fn)
}

// mutatePackTrustStore is the sanctioned way to WRITE the trust store: under
// the lock it re-loads FRESH, applies mutate and saves, so no caller can put a
// stale object over a concurrent writer's record.
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
// mutatePackTrustStore: fresh load → mutate → save, acquiring nothing, so a
// locked region (`pack rm`, the wrapper refresh) never self-deadlocks.
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
// approved, plus provenance for the record's own hygiene.
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

// packInstalledSet records which wrapper names are in hostPackBinDir() and
// which pack (trust key) put them there, so clear/swap works even when the pack
// directory is gone. At most one: the dir only holds the ACTIVE pack's.
type packInstalledSet struct {
	Owner    string   `json:"owner"`
	Wrappers []string `json:"wrappers"`
}

// packActivationRecord is one entry in the ownership ledger: what an active
// pack contributed and the prior values to restore, the ONLY source
// revertPackPriorContribution reads. Keyed by the same identity as acceptance
// (Owner) PLUS the canonical path, so lookups survive a path→remote upgrade.
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
	migrateLegacyActivation(&s, b)
	return &s, nil
}

// migrateLegacyActivation is the read-time backward-compat bridge for a
// pack-trust.json written before the dual-field collapse, whose wire format
// carried a single `"activation"` alongside `"activations"`. Without it an
// existing user's record vanishes unread on upgrade, losing the attribution
// needed to reverse that pack's contribution. It re-parses raw JUST for the
// legacy key and appends it when the ledger has no matching Owner+Path; Save
// has no such field, so the key folds in once and never returns.
func migrateLegacyActivation(s *PackTrustStore, raw []byte) {
	var legacy struct {
		Activation *packActivationRecord `json:"activation"`
	}
	if err := json.Unmarshal(raw, &legacy); err != nil || legacy.Activation == nil {
		return
	}
	for _, a := range s.Activations {
		if a.Owner == legacy.Activation.Owner && a.Path == legacy.Activation.Path {
			return // already represented in the ledger; not a duplicate append
		}
	}
	s.Activations = append(s.Activations, *legacy.Activation)
}

// Save writes the store symlink-safe + atomic: Lstat-REFUSE a symlinked
// destination, then a same-dir temp + rename.
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
// clone provenance ("remote:<url>", stable across commits) when present, else
// the canonical absolute path. Keying by commit would re-gate every pull even
// when the fingerprint — the actual control — was byte-identical. A forged
// identity buys nothing (the fingerprint still has to match byte-for-byte), and
// it is NEVER derived from pack.lock (untrusted payload).
func (s *PackTrustStore) TrustKey(root string) string {
	canon := packinfo.CanonicalizePackRoot(root)
	if s != nil {
		if prov, ok := s.Adopted[canon]; ok && strings.TrimSpace(prov.Remote) != "" {
			return "remote:" + strings.TrimSpace(prov.Remote)
		}
	}
	return "path:" + canon
}

// requireAcceptedFingerprint is the ONE trust check every consumer of a
// gate-passed surface shares: inference, [[services]] and [[setup]] all
// re-verify here before acting on a pack that stayed mutable after adoption.
// Under the lock, against a FRESH store, only an exact match for this identity
// passes; every other answer names `pix pack use` as the re-review path.
func requireAcceptedFingerprint(p *packinfo.Info, fingerprint, what string) error {
	return withPackTrustLock(func() error {
		store, err := loadPackTrustStore()
		if err != nil {
			return fmt.Errorf("pack trust state unreadable: %w", err)
		}
		if got, ok := store.acceptedFingerprint(store.TrustKey(p.Root)); !ok || got != fingerprint {
			return fmt.Errorf("pack %s %s are not accepted (or changed since acceptance) — run `pix pack use %s` to review them", p.Manifest.Name, what, p.Root)
		}
		return nil
	})
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

// RecordAcceptance stores rec under key and drops stale records for the same
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

// lock is one record as a packLock, the shape revertPackPriorContribution and
// the pack.lock hint both read.
func (a packActivationRecord) lock() packLock {
	return packLock{
		MCP:                    append([]string(nil), a.MCP...),
		GogAccount:             a.GogAccount,
		PriorGogAccount:        a.PriorGogAccount,
		OllamaBridgeModel:      a.OllamaBridgeModel,
		PriorOllamaBridgeModel: a.PriorOllamaBridgeModel,
	}
}

// activationFor returns the activation provenance HOST state attributes to
// root. Unattributed to THIS pack (canonical path or trust key) → the zero
// value: remove NOTHING.
func (s *PackTrustStore) activationFor(root string) packLock {
	if s == nil {
		return packLock{}
	}
	path, owner := packinfo.CanonicalizePackRoot(root), s.TrustKey(root)
	for i := len(s.Activations) - 1; i >= 0; i-- {
		if a := s.Activations[i]; a.Path == path || a.Owner == owner {
			return a.lock()
		}
	}
	return packLock{}
}

func (s *PackTrustStore) newActivationRecord(root string, lock packLock) packActivationRecord {
	return packActivationRecord{
		Owner:                  s.TrustKey(root),
		Path:                   packinfo.CanonicalizePackRoot(root),
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
// state, keyed by canonical path, under the store lock. A load error propagates
// rather than clobbering a store the user might fix.
func recordPackAdoptionInTrustStore(root, remote, commit string) error {
	_, err := mutatePackTrustStore(func(s *PackTrustStore) error {
		if s.Adopted == nil {
			s.Adopted = map[string]packProvenance{}
		}
		s.Adopted[packinfo.CanonicalizePackRoot(root)] = packProvenance{Remote: remote, Commit: commit}
		return nil
	})
	return err
}
