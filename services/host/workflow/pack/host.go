// host.go — pack host-exec wrapper install/refresh. `pack use` installs a
// pack's ACCEPTED host=true [[proxy]] scripts and [[bin]] binaries into
// hostPackBinDir().
//
// Trust is HOST STATE, not pack payload: nothing installs unless the accepted
// FINGERPRINT matches the CURRENT surface exactly, every artifact is
// content-pinned (readPinned) and re-verified at every refresh, installation is
// all-or-nothing, and attribution lives in the trust store — so clearing works
// even when the pack is gone.
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
	"pix/host/packinfo"
	"pix/host/workspace"
)

// hostPackBinDir is where the ACTIVE pack's host-exec wrappers live:
// <workspace.HostAgentDir>/bin — state-flavored and rebuildable.
func hostPackBinDir() string { return filepath.Join(workspace.HostAgentDir(), "bin") }

// readPinned reads path and returns its bytes only if they hash to wantSHA —
// the ONE content-pin check every host-exec artifact goes through. It fails
// closed on an empty pin, an unreadable file, a symlink or a mismatch, and
// never re-opens the file after hashing it (no TOCTOU).
func readPinned(path, wantSHA string) ([]byte, error) {
	if strings.TrimSpace(wantSHA) == "" {
		return nil, fmt.Errorf("no sha — host-exec artifacts must be SHA-pinned (fail closed)")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%v (cannot verify the pinned sha; refusing)", err)
	}
	if packinfo.IsSymlinkPath(path) {
		return nil, fmt.Errorf("is a symlink; refusing to install it")
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, wantSHA) {
		return nil, fmt.Errorf("sha256 mismatch: got %s, want %s (refusing — tampered content or stale pin)", got, wantSHA)
	}
	return data, nil
}

// verifyPackBinSHA re-hashes a [[bin]] entry against its mandatory pinned sha,
// failing closed on empty/missing/mismatch — the same contract verifyPluginSHA
// enforces for external go-plugin binaries.
func verifyPackBinSHA(root string, b packinfo.Bin) error {
	if _, err := readPinned(filepath.Join(root, b.Path), b.SHA); err != nil {
		return fmt.Errorf("[[bin]] %q: %v", b.Name, err)
	}
	return nil
}

// clearHostPackWrappers removes the named wrappers from hostPackBinDir().
// Removal is strictly name-scoped (never remove what you cannot attribute) and
// symlink-safe: a symlinked bin dir is never traversed, an unsafe name never
// joined. Errors are RETURNED — attribution must outlive an unconfirmed removal.
func clearHostPackWrappers(names []string) error {
	if len(names) == 0 {
		return nil
	}
	dir := hostPackBinDir()
	if packinfo.IsSymlinkPath(dir) {
		return fmt.Errorf("%s is a symlink; refusing to remove host wrappers through it", dir)
	}
	var errs []string
	for _, n := range names {
		if !packinfo.SafeArtifactName(n) {
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

// clearInstalledHostPackWrappersLocked removes whatever the trust store
// attributes to hostPackBinDir() and, ONLY on confirmed removal, discards the
// attribution — a failure keeps attribution so the next refresh retries instead
// of stranding unowned wrappers. It needs no pack directory. The caller MUST
// already hold the trust lock (nesting the flock self-deadlocks); the fresh
// load under it is what keeps this from clobbering a concurrent `pack use`, and
// the removal set is the UNION of both views (over-removal is a no-op).
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
	for _, set := range []*packInstalledSet{store.Installed, disk.Installed} {
		if set != nil {
			for _, n := range set.Wrappers {
				names[n] = true
			}
		}
	}
	if err := clearHostPackWrappers(sortedKeys(names)); err != nil {
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

// hostArtifact is one file the ACCEPTED surface installs into hostPackBinDir().
// Both kinds are "read this source, require this sha, stage it 0755"; only the
// label and the sha's origin differ (the accepted fingerprint for a host
// [[proxy]] script, the manifest pin for a [[bin]]).
type hostArtifact struct{ kind, name, src, wantSHA string }

func hostArtifacts(p *packinfo.Info) []hostArtifact {
	var out []hostArtifact
	for _, pr := range p.Manifest.Proxies {
		if pr.Host {
			out = append(out, hostArtifact{"host wrapper", pr.Name, filepath.Join(p.Root, "bin", pr.Name), ""})
		}
	}
	for _, b := range p.Manifest.Bins {
		if b.Host {
			out = append(out, hostArtifact{"external binary", b.Name, filepath.Join(p.Root, b.Path), strings.ToLower(strings.TrimSpace(b.SHA))})
		}
	}
	return out
}

// installHostPackWrappersStaged builds the COMPLETE host-wrapper set for pack p
// in a staging dir, content-pinning every artifact (a proxy script against the
// hash inside the ACCEPTED fingerprint, a [[bin]] against its manifest pin),
// then swaps it into hostPackBinDir(). All-or-nothing: ANY failure aborts
// before the swap, leaving the previous verified contents. The swap replaces
// the whole dir — exclusively pack-owned rebuildable state — so it also flushes
// anything stale.
func installHostPackWrappersStaged(p *packinfo.Info, proxySHA map[string]string) ([]string, error) {
	dir := hostPackBinDir()
	if packinfo.IsSymlinkPath(dir) {
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
	for _, a := range hostArtifacts(p) {
		if a.kind == "host wrapper" {
			a.wantSHA = proxySHA[a.name]
		}
		if !packinfo.SafeArtifactName(a.name) {
			return nil, fmt.Errorf("host wrapper name %q is unsafe; refusing", a.name)
		}
		if seen[a.name] {
			return nil, fmt.Errorf("duplicate host wrapper name %q; refusing (ambiguous install)", a.name)
		}
		seen[a.name] = true
		data, rerr := readPinned(a.src, a.wantSHA)
		if rerr != nil {
			if a.kind == "external binary" {
				return nil, fmt.Errorf("external binary %q: %v", a.name, rerr)
			}
			return nil, fmt.Errorf("host wrapper %q: %v — script changed since acceptance; re-run `pix pack use` to review + re-accept", a.name, rerr)
		}
		dest := filepath.Join(staging, a.name)
		if err := os.WriteFile(dest, data, 0o755); err != nil {
			return nil, err
		}
		if err := os.Chmod(dest, 0o755); err != nil { // WriteFile perm is umask-subject
			return nil, err
		}
		names = append(names, a.name)
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

// refreshHostPackWrappers syncs hostPackBinDir() with the ACTIVE pack's
// ACCEPTED host-exec surface. `pack use` calls it post-commit with strict=false
// (notes only); strict=true is for a caller that must refuse rather than
// proceed with an unrefreshed or tampered wrapper set. It returns the loaded
// active pack (nil when none/degraded) so a caller can reuse it.
//
// Install + attribution is ONE fail-closed cross-process transaction: the whole
// lifecycle — fresh store load, gate check, intended-attribution write, dir
// swap, final attribution write — runs under a SINGLE hold of the trust flock,
// because separately-locked steps let a concurrent refresh (or `pack rm`)
// interleave and strand wrappers under the wrong attribution. The INTENDED
// attribution is written BEFORE the swap and trimmed after, so the store is
// always a SUPERSET of what is live — never a wrapper owned by nobody.
func refreshHostPackWrappers(out io.Writer, cfg *config.Config, strict bool) (*packinfo.Info, error) {
	var p *packinfo.Info
	err := withPackTrustLock(func() error {
		var rerr error
		p, rerr = refreshHostPackWrappersLocked(out, cfg, strict)
		return rerr
	})
	return p, err
}

// refreshHostPackWrappersLocked is the ALREADY-HOLDING-THE-LOCK body of
// refreshHostPackWrappers. It MUST NOT acquire withPackTrustLock (directly or
// via mutatePackTrustStore) — only the *Locked variants and plain load/save.
func refreshHostPackWrappersLocked(out io.Writer, cfg *config.Config, strict bool) (*packinfo.Info, error) {
	// degrade is the ONE strict-vs-lenient decision: a strict caller refuses,
	// a lenient one gets a note and keeps whatever the step left behind.
	degrade := func(p *packinfo.Info, note string, err error) (*packinfo.Info, error) {
		if strict {
			return nil, fmt.Errorf("%v (refusing; fail closed)", err)
		}
		fmt.Fprintf(out, "%s: %v\n", note, err)
		return p, nil
	}
	store, serr := loadPackTrustStore()
	if serr != nil {
		if strict {
			return nil, fmt.Errorf("pack trust state unreadable: %v (refusing; fix or remove %s)", serr, packTrustStorePath())
		}
		fmt.Fprintf(out, "note: pack trust state unreadable (%v); treating as empty (nothing is accepted)\n", serr)
		store = &PackTrustStore{}
	}
	// Every "nothing should be installed" path clears what host state attributes
	// to the bin dir — which works even with the pack gone. A failed clear is
	// FATAL for a strict caller (stale wrappers must never stay reachable) and
	// returned to a lenient one too.
	clearOrFail := func(context string) error {
		cerr := clearInstalledHostPackWrappersLocked(out, store)
		if cerr != nil && strict {
			return fmt.Errorf("%s: %v (refusing)", context, cerr)
		}
		return cerr
	}
	root := packinfo.ActivePackRoot(cfg.Pack, "")
	if root == "" {
		return nil, clearOrFail("stale pack host wrappers could not be cleared")
	}
	p, err := packinfo.LoadPack(root)
	if err != nil {
		if !errors.Is(err, packinfo.ErrNotAPack) {
			return nil, fmt.Errorf("active pack %s: %v (refusing to use its host wrappers; fix the pack or `pix pack rm` to detach it)", root, err)
		}
		// Genuinely absent pack: it still must not leave ITS wrappers live.
		if cerr := clearOrFail(fmt.Sprintf("active pack unavailable (%v) and its host wrappers could not be cleared", err)); cerr != nil {
			return nil, cerr
		}
		fmt.Fprintf(out, "note: active pack unavailable (%v); host wrappers not refreshed\n", err)
		return nil, nil
	}
	bom := ComputeHostBoM(p)
	if !bom.Tier1() {
		// Tier-0 for host purposes: nothing to install; clear stale leftovers.
		cerr := clearOrFail("stale pack host wrappers could not be cleared")
		if cerr != nil && strict {
			return nil, cerr
		}
		return p, cerr
	}
	fp, proxySHA, ferr := ComputeHostExecFingerprint(root, bom)
	if ferr != nil {
		return degrade(p, "TODO", fmt.Errorf("pack %s: %v — host wrappers not installed", p.Manifest.Name, ferr))
	}
	key := store.TrustKey(root)
	if got, ok := store.acceptedFingerprint(key); !ok || got != fp {
		msg := fmt.Sprintf("pack %s declares host-exec facets that are not accepted (or changed since acceptance) — run `pix pack use %s` to review + accept the host BoM", p.Manifest.Name, root)
		if strict {
			return nil, errors.New(msg + " (refusing; fail closed)")
		}
		fmt.Fprintln(out, "note: "+msg)
		// Never leave wrappers installed for a surface that is no longer accepted
		// (a mutated proxy script must stop being reachable).
		return p, clearOrFail("stale pack host wrappers could not be cleared")
	}

	// Step 1: record the INTENDED attribution — the new owner plus the UNION of
	// previously-attributed and about-to-be-staged names — BEFORE anything
	// touches the bin dir. A failed write installs nothing (no orphan possible);
	// a failed or interrupted swap still leaves the union covering BOTH sets, so
	// a later clear can attribute whatever is live.
	intended := map[string]bool{}
	if store.Installed != nil {
		for _, n := range store.Installed.Wrappers {
			intended[n] = true
		}
	}
	for _, a := range hostArtifacts(p) {
		intended[a.name] = true
	}
	if werr := store.saveInstalled(key, sortedKeys(intended)); werr != nil {
		return degrade(p, "note", fmt.Errorf("pack %s: could not record intended host-wrapper attribution: %v (host wrappers NOT installed; fail closed)", p.Manifest.Name, werr))
	}
	installed, ierr := installHostPackWrappersStaged(p, proxySHA)
	if ierr != nil {
		return degrade(p, "TODO", fmt.Errorf("pack %s host wrappers not installed: %v", p.Manifest.Name, ierr))
	}
	// Step 2: the swap succeeded — trim the attribution to the ACTUAL set, still
	// under the same lock. A failure here keeps the union (no orphan) but the
	// install is NOT successful: the error propagates, strict callers refuse.
	if werr := store.saveInstalled(key, installed); werr != nil {
		return degrade(p, "note", fmt.Errorf("pack %s: host wrappers installed but the attribution write failed: %v (attribution over-claims until the store is writable)", p.Manifest.Name, werr))
	}
	if len(installed) > 0 {
		fmt.Fprintf(out, "host wrappers installed: %s\n", strings.Join(installed, ", "))
	}
	return p, nil
}

// saveInstalled records names as the wrapper set owned by key.
func (s *PackTrustStore) saveInstalled(key string, names []string) error {
	s.Installed = &packInstalledSet{Owner: key, Wrappers: names}
	return s.Save()
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
