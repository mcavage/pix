// host.go — the SURVIVING sliver of the deleted host-mode wrapper installer
// (packhost.go / packs-v2-impl.md F3). `pix host` (the unsandboxed escape
// hatch) was retired: nothing installs a pack's host-exec facets onto a PATH
// dir any more (installHostPackWrappersStaged / RefreshHostPackWrappers, and
// the machine-level host.enabled gate they read, are gone — see
// config.retiredConfigKeys and workflow/launch's deleted hostrun.go).
//
// Two pieces are still genuinely useful and kept:
//
//   - verifyPackBinSHA re-hashes a [[bin]] entry against its pinned sha at
//     `pack use` time (F5), BEFORE the Tier-1 gate even renders — this is
//     generic content-pin verification, not host-wrapper installation, and a
//     tampered/stale-pinned binary must still refuse activation.
//   - clearHostPackWrappers / clearInstalledHostPackWrappers(Locked) let
//     `pack rm` tidy up any wrapper files a PRE-retirement install left behind
//     in HostPackBinDir() (attributed in the trust store), so an upgrader is
//     never stranded with orphaned executables nothing references any more.
package pack

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"pix/host/workspace"
)

// HostPackBinDir is the well-known location a pre-retirement `pix host`
// wrapper install lived: <workspace.HostAgentDir>/bin. Nothing installs here
// any more; it exists so a stale, previously-attributed set can still be
// cleared.
func HostPackBinDir() string { return filepath.Join(workspace.HostAgentDir(), "bin") }

// hashFileSHA256 returns the lowercase hex sha256 of the file at path. It is
// the ADR-4 duplicate of the hashing core inside verifyPluginSHA
// (services/host/serve_plugin.go) — the launcher is a separate,
// dependency-light main package (same pattern as knowledge.CanonicalizeKnowledgeBundle),
// so the ~10 lines are duplicated rather than inventing a shared library
// package or a second checksum scheme. Keep the two in behavioral lockstep.
func hashFileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// verifyPackBinSHA re-hashes a [[bin]] entry's file (root-relative b.Path)
// against its mandatory pinned sha, failing closed on empty/missing/mismatch —
// the same contract verifyPluginSHA enforces for external go-plugin binaries.
// LoadPack already refuses an empty sha and a symlinked/escaping Path; the
// empty-sha check here is belt-and-suspenders for callers holding a packBin
// that never went through LoadPack.
func verifyPackBinSHA(root string, b packBin) error {
	want := strings.ToLower(strings.TrimSpace(b.SHA))
	if want == "" {
		return fmt.Errorf("[[bin]] %q has no sha — external binaries must be SHA-pinned (fail closed)", b.Name)
	}
	got, err := hashFileSHA256(filepath.Join(root, b.Path))
	if err != nil {
		return fmt.Errorf("[[bin]] %q: %v (cannot verify the pinned sha; refusing)", b.Name, err)
	}
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("[[bin]] %q sha256 mismatch: got %s, want %s (refusing — tampered binary or stale pin)", b.Name, got, want)
	}
	return nil
}

// clearHostPackWrappers removes the named wrappers from HostPackBinDir().
// Removal is strictly name-scoped (host-state attribution — the same
// never-remove-what-you-can't-attribute posture as the MCP/knowledge swap) and
// symlink-safe: a symlinked host bin dir is never traversed, and a name that
// fails safeArtifactName is never joined. Errors are RETURNED, never
// discarded: the caller must not drop attribution until removal is confirmed.
func clearHostPackWrappers(names []string) error {
	if len(names) == 0 {
		return nil
	}
	dir := HostPackBinDir()
	if isSymlinkPath(dir) {
		return fmt.Errorf("%s is a symlink; refusing to remove host wrappers through it", dir)
	}
	var errs []string
	for _, n := range names {
		if !safeArtifactName(n) {
			errs = append(errs, fmt.Sprintf("wrapper name %q is unsafe; not removed", n))
			continue
		}
		if err := os.Remove(filepath.Join(dir, n)); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// clearInstalledHostPackWrappers removes whatever the trust store attributes
// to HostPackBinDir() and, ONLY on confirmed removal, discards the
// attribution. A removal failure is surfaced (note + returned error) and the
// attribution is kept, so a later retry doesn't strand wrappers with no
// owner. Works with no pack directory at all — attribution lives in host
// state, never in the (possibly deleted) pack.
//
// The whole remove→drop-attribution cycle runs under the cross-process trust
// lock against a FRESH load (round-3 #1). This wrapper ACQUIRES the lock; a
// caller already holding it (`pack rm`) uses
// clearInstalledHostPackWrappersLocked instead — nesting withPackTrustLock
// self-deadlocks (flock is per open file description).
func clearInstalledHostPackWrappers(out io.Writer, store *PackTrustStore) error {
	if store == nil || store.Installed == nil || len(store.Installed.Wrappers) == 0 {
		return nil
	}
	return withPackTrustLock(func() error {
		return clearInstalledHostPackWrappersLocked(out, store)
	})
}

// clearInstalledHostPackWrappersLocked is the ALREADY-HOLDING-THE-LOCK core
// of clearInstalledHostPackWrappers: same fresh-load → union → remove →
// drop-attribution contract, NO lock acquisition of its own. Callers MUST
// hold withPackTrustLock.
func clearInstalledHostPackWrappersLocked(out io.Writer, store *PackTrustStore) error {
	if store == nil || store.Installed == nil || len(store.Installed.Wrappers) == 0 {
		return nil
	}
	disk, lerr := loadPackTrustStore()
	if lerr != nil {
		fmt.Fprintf(out, "note: pack trust state unreadable while clearing host wrappers: %v (attribution kept)\n", lerr)
		return lerr
	}
	names := map[string]bool{}
	for _, n := range store.Installed.Wrappers {
		names[n] = true
	}
	if disk.Installed != nil {
		for _, n := range disk.Installed.Wrappers {
			names[n] = true
		}
	}
	var all []string
	for n := range names {
		all = append(all, n)
	}
	sort.Strings(all)
	if err := clearHostPackWrappers(all); err != nil {
		fmt.Fprintf(out, "note: could not remove installed host wrappers: %v (attribution kept; removal will be retried)\n", err)
		return err
	}
	store.Installed = nil
	if disk.Installed == nil {
		return nil // nothing persisted to update
	}
	disk.Installed = nil
	if err := disk.Save(); err != nil {
		// The wrappers ARE gone; a stale attribution only over-claims (the next
		// removal is a no-op). Still surfaced AND returned so callers that must
		// not silently succeed can propagate it.
		fmt.Fprintf(out, "note: could not update pack trust state after removing host wrappers: %v\n", err)
		return err
	}
	return nil
}
