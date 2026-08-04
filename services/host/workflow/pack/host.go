// host.go — pack host-exec wrapper install/refresh (packs-v2-impl.md F3,
// packs.md §9). This is CORE pack host-exec, independent of `pix host` (the
// unsandboxed escape hatch, retired in W1-U01a/W2-U03B): a pack can declare
// host=true [[proxy]] wrapper scripts and [[bin]] SHA-pinned external
// binaries, and `pack use` installs the ACCEPTED set into HostPackBinDir() —
// regardless of whether any particular launcher mode later exposes that dir
// on a child PATH. The Tier-1 adoption gate (packtrust.go + packtruststore.go)
// still guards every install.
//
// Trust is HOST STATE, not pack payload: nothing installs unless the trust
// store's accepted FINGERPRINT for this pack matches the CURRENT host-exec
// surface exactly. Every artifact is content-pinned — [[bin]]s against their
// declared sha, host proxy scripts against the content hash inside the
// accepted fingerprint — re-verified at every refresh, refusing on any
// change. Installation is all-or-nothing: the complete accepted set is built
// and verified in a staging dir, then swapped in atomically; a strict-mode
// failure refuses, never leaving a half-installed set. Installed-wrapper
// attribution lives in the trust store, so clearing/swapping works even when
// the pack directory is gone.
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

	"pix/host/config"
	"pix/host/workspace"
)

// HostPackBinDir is where the ACTIVE pack's host-exec wrappers live:
// <workspace.HostAgentDir>/bin. State-flavored and rebuildable (cleared +
// refreshed from the active pack), exactly like the rest of the host agent
// dir.
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
// attribution is kept, so the next refresh retries instead of stranding
// wrappers with no owner. Works with no pack directory at all — attribution
// lives in host state, never in the (possibly deleted) pack.
//
// The whole remove→drop-attribution cycle runs under the cross-process trust
// lock against a FRESH load (round-3 #1): saving the caller's in-memory store
// here could clobber an activation/acceptance a concurrent `pack use` just
// committed. The removal set is the UNION of the caller's view and the
// on-disk attribution (removing an absent name is a no-op, so over-removal is
// safe); the caller's view is synced on success.
//
// This wrapper ACQUIRES the lock; a caller already holding it (the
// transactional refresh, `pack rm`) uses clearInstalledHostPackWrappersLocked
// instead — nesting withPackTrustLock self-deadlocks (flock is per open file
// description).
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
		// removal is a no-op). Still surfaced AND returned so a strict caller
		// fails closed on an unwritable trust store.
		fmt.Fprintf(out, "note: could not update pack trust state after removing host wrappers: %v\n", err)
		return err
	}
	return nil
}

// installHostPackWrappersStaged builds the COMPLETE host-wrapper set for pack
// p in a staging directory, verifying every artifact's content, then swaps it
// into HostPackBinDir() — all-or-nothing, never a half-installed set:
//
//   - Every host=true [[proxy]] script's bytes are re-hashed and must match
//     the content hash inside the ACCEPTED fingerprint (proxySHA — computed by
//     the caller via ComputeHostExecFingerprint), so what lands in the bin
//     dir is byte-for-byte what the user accepted (no TOCTOU between the
//     fingerprint check and the copy).
//   - Every host=true [[bin]]'s bytes are re-hashed against the pinned sha and
//     REFUSED on mismatch.
//   - Sources are Lstat-checked (LoadPack already refuses symlinks in the
//     pack tree; belt-and-suspenders here) and names re-validated.
//   - ANY failure aborts with an error before the swap: the previous
//     (verified) contents stay in place untouched.
//
// The swap replaces the whole dir (rename old aside, rename staging in,
// remove old) — HostPackBinDir() is exclusively pack-owned, rebuildable
// state, so a full swap also flushes anything stale a prior activation left.
func installHostPackWrappersStaged(p *Info, proxySHA map[string]string) ([]string, error) {
	var hostProxies []PackProxy
	var hostBins []packBin
	for _, pr := range p.Manifest.Proxies {
		if pr.Host {
			hostProxies = append(hostProxies, pr)
		}
	}
	for _, b := range p.Manifest.Bins {
		if b.Host {
			hostBins = append(hostBins, b)
		}
	}
	dir := HostPackBinDir()
	if isSymlinkPath(dir) {
		return nil, fmt.Errorf("%s is a symlink; refusing to install host wrappers through it", dir)
	}
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, err
	}
	staging, err := os.MkdirTemp(parent, ".pack-bin-staging-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(staging) // no-op after a successful swap

	var names []string
	seen := map[string]bool{}
	stage := func(name string, data []byte) error {
		if !safeArtifactName(name) {
			return fmt.Errorf("host wrapper name %q is unsafe; refusing", name)
		}
		if seen[name] {
			return fmt.Errorf("duplicate host wrapper name %q; refusing (ambiguous install)", name)
		}
		seen[name] = true
		dest := filepath.Join(staging, name)
		if err := os.WriteFile(dest, data, 0o755); err != nil {
			return err
		}
		if err := os.Chmod(dest, 0o755); err != nil { // WriteFile perm is umask-subject
			return err
		}
		names = append(names, name)
		return nil
	}
	for _, pr := range hostProxies {
		src := filepath.Join(p.Root, "bin", pr.Name)
		if isSymlinkPath(src) {
			return nil, fmt.Errorf("host wrapper %q is a symlink; refusing to install it", pr.Name)
		}
		data, rerr := os.ReadFile(src)
		if rerr != nil {
			return nil, fmt.Errorf("host wrapper %q: %v", pr.Name, rerr)
		}
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); proxySHA == nil || !strings.EqualFold(got, proxySHA[pr.Name]) {
			return nil, fmt.Errorf("host wrapper %q content sha256 mismatch against the accepted fingerprint (got %s) — script changed since acceptance; re-run `pix pack use` to review + re-accept", pr.Name, got)
		}
		if serr := stage(pr.Name, data); serr != nil {
			return nil, serr
		}
	}
	for _, b := range hostBins {
		src := filepath.Join(p.Root, b.Path)
		if isSymlinkPath(src) {
			return nil, fmt.Errorf("external binary %q is a symlink; refusing to install it", b.Name)
		}
		data, rerr := os.ReadFile(src)
		if rerr != nil {
			return nil, fmt.Errorf("external binary %q: %v", b.Name, rerr)
		}
		sum := sha256.Sum256(data)
		got := hex.EncodeToString(sum[:])
		want := strings.ToLower(strings.TrimSpace(b.SHA))
		if !strings.EqualFold(got, want) {
			return nil, fmt.Errorf("external binary %q sha256 mismatch: got %s, want %s (refusing — tampered binary or stale pin)", b.Name, got, want)
		}
		if serr := stage(b.Name, data); serr != nil {
			return nil, serr
		}
	}

	// Atomic-ish swap: move the old dir aside, move the fully-verified staging
	// dir in, then drop the old one. A crash between the renames leaves no bin
	// dir at all (wrappers simply absent — fail safe), never a mixed set.
	oldDir := staging + ".old"
	hadOld := false
	if _, lerr := os.Lstat(dir); lerr == nil {
		if err := os.Rename(dir, oldDir); err != nil {
			return nil, err
		}
		hadOld = true
	}
	if err := os.Rename(staging, dir); err != nil {
		if hadOld {
			_ = os.Rename(oldDir, dir) // restore the previous verified set
		}
		return nil, err
	}
	if hadOld {
		_ = os.RemoveAll(oldDir)
	}
	return names, nil
}

// RefreshHostPackWrappers syncs HostPackBinDir() with the ACTIVE pack's
// ACCEPTED host-exec surface. Called from RunPackUse (post-commit swap,
// strict=false: notes, `pack use` never fails on a refresh problem). The
// strict=true mode is kept generic for any future fail-closed caller
// (anything that must refuse rather than proceed with an unrefreshed/tampered
// wrapper set) — nothing in this tree currently calls it that way, since
// `pix host` (the one-time strict caller) was retired.
//
// Wrapper install + attribution is a fail-closed transaction (round-2 D)
// that is also ONE cross-process transaction (concurrency review): the
// ENTIRE lifecycle — fresh store load, gate check, intended-attribution
// write, the filesystem dir swap, and the final attribution write — runs
// under a SINGLE hold of the pack trust flock (withPackTrustLock). Two
// concurrent refreshes (or a refresh racing `pack rm`) fully serialize; the
// old three-separately-locked-steps shape let them interleave around the
// unlocked dir swap and leave the live dir holding one process's wrappers
// under the other's attribution (a later clear would then orphan live host
// executables). The INTENDED attribution (owner + the union of the
// previously-attributed and about-to-be-installed names) is written to the
// trust store BEFORE the dir swap, and trimmed to the actual set after.
// Install counts as successful only when BOTH the swap and the store write
// succeeded; any store-write failure returns an error (a strict caller
// refuses), and at every point the store's attribution is a SUPERSET of
// what is live in HostPackBinDir() — never a live wrapper the store
// attributes to nobody. Clearing stale wrappers (no active pack, absent pack
// dir, Tier-0, no-longer-accepted surface) is equally fail-closed in strict
// mode.
//
// The gate here is the trust store's fingerprint (packtruststore.go): the
// CURRENT host-exec surface is recomputed — MCP argv, host proxy script
// CONTENT, bin pins, egress — and compared to the fingerprint the user
// accepted for this pack identity. Anything but an exact match installs
// NOTHING (and in strict mode refuses): a mutated proxy script, a changed
// gog_account, a new facet all fail closed until re-accepted via `pack use`.
// On a match, the complete set is staged, content-verified, and swapped in
// atomically; the installed set + owner are recorded in host state so the
// next switch/rm can clear them even if the pack dir disappears.
//
// It returns the loaded active pack (nil when none/degraded) so a caller can
// reuse it without a second LoadPack.
func RefreshHostPackWrappers(out io.Writer, cfg *config.Config, strict bool) (*Info, error) {
	var p *Info
	err := withPackTrustLock(func() error {
		var rerr error
		p, rerr = refreshHostPackWrappersLocked(out, cfg, strict)
		return rerr
	})
	return p, err
}

// refreshHostPackWrappersLocked is the ALREADY-HOLDING-THE-LOCK body of
// RefreshHostPackWrappers. It MUST NOT acquire withPackTrustLock (directly
// or via mutatePackTrustStore/clearInstalledHostPackWrappers) — only the
// *Locked variants and plain load/save, which are safe because the caller
// holds the flock for the whole lifecycle.
func refreshHostPackWrappersLocked(out io.Writer, cfg *config.Config, strict bool) (*Info, error) {
	store, serr := loadPackTrustStore()
	if serr != nil {
		if strict {
			return nil, fmt.Errorf("pack trust state unreadable: %v (refusing; fix or remove %s)", serr, packTrustStorePath())
		}
		fmt.Fprintf(out, "note: pack trust state unreadable (%v); treating as empty (nothing is accepted)\n", serr)
		store = &PackTrustStore{}
	}
	root := ActivePackRoot(cfg.Pack, "")
	if root == "" {
		// No active pack: clear whatever host state attributes to the bin dir
		// (works even though the previous pack's directory may be long gone).
		// A failed clear is FATAL for a strict caller: stale wrappers from a
		// detached pack must never stay reachable. A LENIENT caller gets the
		// error back too (round-3 #4): a clear failure is never reported as a
		// silent success.
		if cerr := clearInstalledHostPackWrappersLocked(out, store); cerr != nil {
			if strict {
				return nil, fmt.Errorf("stale pack host wrappers could not be cleared: %v (refusing)", cerr)
			}
			return nil, cerr
		}
		return nil, nil
	}
	p, err := LoadPack(root)
	if err != nil {
		if errors.Is(err, ErrNotAPack) {
			// Genuinely absent pack: it still must not leave ITS wrappers live.
			if cerr := clearInstalledHostPackWrappersLocked(out, store); cerr != nil {
				if strict {
					return nil, fmt.Errorf("active pack unavailable (%v) and its host wrappers could not be cleared: %v (refusing)", err, cerr)
				}
				return nil, cerr
			}
			fmt.Fprintf(out, "note: active pack unavailable (%v); host wrappers not refreshed\n", err)
			return nil, nil
		}
		return nil, fmt.Errorf("active pack %s: %v (refusing to use its host wrappers; fix the pack or `pix pack rm` to detach it)", root, err)
	}
	bom := ComputeHostBoM(p, cfg.GogAccount, PackLocalMCP())
	if !bom.Tier1() {
		// Tier-0 for host purposes: nothing to install; clear stale leftovers.
		if cerr := clearInstalledHostPackWrappersLocked(out, store); cerr != nil {
			if strict {
				return nil, fmt.Errorf("stale pack host wrappers could not be cleared: %v (refusing)", cerr)
			}
			return p, cerr
		}
		return p, nil
	}
	fp, proxySHA, ferr := ComputeHostExecFingerprint(root, bom)
	if ferr != nil {
		if strict {
			return nil, fmt.Errorf("pack %s: %v (refusing)", p.Manifest.Name, ferr)
		}
		fmt.Fprintf(out, "TODO: pack %s: %v — host wrappers not installed\n", p.Manifest.Name, ferr)
		return p, nil
	}
	key := store.TrustKey(root)
	if got, ok := store.acceptedFingerprint(key); !ok || got != fp {
		msg := fmt.Sprintf("pack %s declares host-exec facets that are not accepted (or changed since acceptance) — run `pix pack use %s` to review + accept the host BoM", p.Manifest.Name, root)
		if strict {
			return nil, errors.New(msg + " (refusing; fail closed)")
		}
		fmt.Fprintln(out, "note: "+msg)
		// Don't leave wrappers installed for a surface that is no longer
		// accepted (e.g. a mutated proxy script must stop being reachable). A
		// failed clear is surfaced to the lenient caller too (round-3 #4).
		if cerr := clearInstalledHostPackWrappersLocked(out, store); cerr != nil {
			return p, cerr
		}
		return p, nil
	}

	// Fail-closed install transaction (round-2 D). Step 1: record the INTENDED
	// attribution — the new owner plus the UNION of the previously-attributed
	// names and the names about to be staged — BEFORE anything touches the bin
	// dir. If this write fails nothing installs (no orphan possible); if the
	// swap below fails or is interrupted, the union still covers BOTH the old
	// and the (possibly) new set, so a later clear can always attribute
	// whatever is live. Over-attribution is safe: removing an absent name is a
	// no-op.
	// Both attribution writes mutate + save the store loaded FRESH at the top
	// of this locked region — the caller holds the cross-process trust lock
	// for the whole lifecycle (fresh load → gate → intended write → dir swap →
	// final write), so no concurrent `pack use`/refresh/rm can interleave: no
	// stale-object clobber (round-3 #1) and no wrong-owner attribution around
	// the swap (concurrency review). NOTE: store can only be the lenient
	// unreadable-store placeholder on paths that never reach here (an empty
	// store has no acceptance, so the gate above already returned).
	intended := map[string]bool{}
	if store.Installed != nil {
		for _, n := range store.Installed.Wrappers {
			intended[n] = true
		}
	}
	for _, pr := range p.Manifest.Proxies {
		if pr.Host {
			intended[pr.Name] = true
		}
	}
	for _, bn := range p.Manifest.Bins {
		if bn.Host {
			intended[bn.Name] = true
		}
	}
	var intendedNames []string
	for n := range intended {
		intendedNames = append(intendedNames, n)
	}
	sort.Strings(intendedNames)
	store.Installed = &packInstalledSet{Owner: key, Wrappers: intendedNames}
	if werr := store.Save(); werr != nil {
		err := fmt.Errorf("pack %s: could not record intended host-wrapper attribution: %v (host wrappers NOT installed; fail closed)", p.Manifest.Name, werr)
		if strict {
			return nil, fmt.Errorf("%v — refusing", err)
		}
		return p, err
	}
	installed, ierr := installHostPackWrappersStaged(p, proxySHA)
	if ierr != nil {
		if strict {
			return nil, fmt.Errorf("pack %s: %v (refusing; fail closed — no partial wrapper set is ever installed)", p.Manifest.Name, ierr)
		}
		fmt.Fprintf(out, "TODO: pack %s host wrappers not installed: %v\n", p.Manifest.Name, ierr)
		return p, nil
	}
	// Step 2: the swap succeeded — trim the attribution to the ACTUAL set
	// (still under the same lock hold, so nothing observed the intermediate
	// union). A failure here still leaves the union attribution covering
	// every live wrapper (no orphan), but the install is NOT considered
	// successful: the error propagates and a strict caller refuses.
	store.Installed = &packInstalledSet{Owner: key, Wrappers: installed}
	if werr := store.Save(); werr != nil {
		err := fmt.Errorf("pack %s: host wrappers installed but the attribution write failed: %v (attribution over-claims until the store is writable)", p.Manifest.Name, werr)
		if strict {
			return nil, fmt.Errorf("%v — refusing", err)
		}
		return p, err
	}
	if len(installed) > 0 {
		fmt.Fprintf(out, "host wrappers installed: %s\n", strings.Join(installed, ", "))
	}
	return p, nil
}
