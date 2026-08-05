// host.go — pack host-exec wrapper install/refresh. `pack use` installs a
// pack's ACCEPTED host=true [[proxy]] scripts and [[bin]] binaries into
// hostPackBinDir().
//
// Trust is HOST STATE, not pack payload: nothing installs unless the accepted
// FINGERPRINT matches the CURRENT surface exactly, and every artifact is
// content-pinned ([[bin]]s against their declared sha, proxy scripts against
// the hash inside that fingerprint), re-verified at every refresh. Installation
// is all-or-nothing (staged, verified, swapped atomically) and attribution
// lives in the trust store, so clearing works even when the pack is gone.
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

// hostPackBinDir is where the ACTIVE pack's host-exec wrappers live:
// <workspace.HostAgentDir>/bin — state-flavored and rebuildable.
func hostPackBinDir() string { return filepath.Join(workspace.HostAgentDir(), "bin") }

// hashFileSHA256 returns the lowercase hex sha256 of the file at path. A
// deliberate duplicate of verifyPluginSHA's hashing core (the launcher is a
// separate, dependency-light main package). Keep the two in behavioral lockstep.
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

// verifyPackBinSHA re-hashes a [[bin]] entry's file against its mandatory
// pinned sha, failing closed on empty/missing/mismatch — the same contract
// verifyPluginSHA enforces for external go-plugin binaries. The empty-sha check
// covers a caller holding a packBin that skipped LoadPack.
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

// clearHostPackWrappers removes the named wrappers from hostPackBinDir().
// Removal is strictly name-scoped (never remove what you cannot attribute) and
// symlink-safe: a symlinked bin dir is never traversed, an unsafe name never
// joined. Errors are RETURNED — attribution must outlive an unconfirmed removal.
func clearHostPackWrappers(names []string) error {
	if len(names) == 0 {
		return nil
	}
	dir := hostPackBinDir()
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

// clearInstalledHostPackWrappersLocked removes whatever the trust store
// attributes to hostPackBinDir() and, ONLY on confirmed removal, discards the
// attribution — a failure is surfaced and returned with attribution kept, so
// the next refresh retries instead of stranding unowned wrappers. It needs no
// pack directory: attribution lives in host state. The caller MUST already hold
// the trust lock (nesting the flock self-deadlocks); the fresh load under it is
// what keeps this from clobbering a concurrent `pack use`. The removal set is
// the UNION of the caller's view and disk — over-removal is a no-op.
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

// installHostPackWrappersStaged builds the COMPLETE host-wrapper set for pack p
// in a staging dir, verifying every artifact's content, then swaps it into
// hostPackBinDir() — all-or-nothing, never a half-installed set. Proxy scripts
// are re-hashed against the content hash inside the ACCEPTED fingerprint (so
// what lands is byte-for-byte what the user accepted, no TOCTOU) and [[bin]]s
// against their pinned sha; sources are Lstat-checked, names re-validated, and
// ANY failure aborts before the swap, leaving the previous verified contents.
// The swap replaces the whole dir, which is exclusively pack-owned rebuildable
// state, so it also flushes anything stale.
func installHostPackWrappersStaged(p *Info, proxySHA map[string]string) ([]string, error) {
	// One list: both artifact kinds are "read this source, require this sha,
	// stage it 0755" — only the label and where the expected sha comes from
	// differ (accepted fingerprint vs manifest pin).
	type artifact struct{ kind, name, src, wantSHA string }
	var artifacts []artifact
	for _, pr := range p.Manifest.Proxies {
		if pr.Host {
			artifacts = append(artifacts, artifact{"host wrapper", pr.Name, filepath.Join(p.Root, "bin", pr.Name), proxySHA[pr.Name]})
		}
	}
	for _, b := range p.Manifest.Bins {
		if b.Host {
			artifacts = append(artifacts, artifact{"external binary", b.Name, filepath.Join(p.Root, b.Path), strings.ToLower(strings.TrimSpace(b.SHA))})
		}
	}
	dir := hostPackBinDir()
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
	for _, a := range artifacts {
		if !safeArtifactName(a.name) {
			return nil, fmt.Errorf("host wrapper name %q is unsafe; refusing", a.name)
		}
		if seen[a.name] {
			return nil, fmt.Errorf("duplicate host wrapper name %q; refusing (ambiguous install)", a.name)
		}
		seen[a.name] = true
		if isSymlinkPath(a.src) {
			return nil, fmt.Errorf("%s %q is a symlink; refusing to install it", a.kind, a.name)
		}
		data, rerr := os.ReadFile(a.src)
		if rerr != nil {
			return nil, fmt.Errorf("%s %q: %v", a.kind, a.name, rerr)
		}
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); a.wantSHA == "" || !strings.EqualFold(got, a.wantSHA) {
			if a.kind == "external binary" {
				return nil, fmt.Errorf("external binary %q sha256 mismatch: got %s, want %s (refusing — tampered binary or stale pin)", a.name, got, a.wantSHA)
			}
			return nil, fmt.Errorf("host wrapper %q content sha256 mismatch against the accepted fingerprint (got %s) — script changed since acceptance; re-run `pix pack use` to review + re-accept", a.name, got)
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
// proceed with an unrefreshed or tampered wrapper set.
//
// Install + attribution is ONE fail-closed cross-process transaction: the whole
// lifecycle — fresh store load, gate check, intended-attribution write, dir
// swap, final attribution write — runs under a SINGLE hold of the trust flock,
// because separately-locked steps let a concurrent refresh (or `pack rm`)
// interleave around the swap and leave the live dir holding one process's
// wrappers under the other's attribution. The INTENDED attribution is written
// BEFORE the swap and trimmed after, so the store is at every point a SUPERSET
// of what is live — never a live wrapper attributed to nobody.
//
// The gate is the fingerprint: anything but an exact match for this pack
// identity installs NOTHING (strict mode refuses). It returns the loaded active
// pack (nil when none/degraded) so a caller can reuse it.
func refreshHostPackWrappers(out io.Writer, cfg *config.Config, strict bool) (*Info, error) {
	var p *Info
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
func refreshHostPackWrappersLocked(out io.Writer, cfg *config.Config, strict bool) (*Info, error) {
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
	root := ActivePackRoot(cfg.Pack, "")
	if root == "" {
		return nil, clearOrFail("stale pack host wrappers could not be cleared")
	}
	p, err := LoadPack(root)
	if err != nil {
		if errors.Is(err, ErrNotAPack) {
			// Genuinely absent pack: it still must not leave ITS wrappers live.
			if cerr := clearOrFail(fmt.Sprintf("active pack unavailable (%v) and its host wrappers could not be cleared", err)); cerr != nil {
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
		if cerr := clearOrFail("stale pack host wrappers could not be cleared"); cerr != nil {
			if strict {
				return nil, cerr
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
		// Never leave wrappers installed for a surface that is no longer accepted
		// (a mutated proxy script must stop being reachable).
		return p, clearOrFail("stale pack host wrappers could not be cleared")
	}

	// Fail-closed install transaction. Step 1: record the INTENDED attribution —
	// the new owner plus the UNION of previously-attributed and about-to-be-staged
	// names — BEFORE anything touches the bin dir. A failed write installs nothing
	// (no orphan possible); a failed or interrupted swap still leaves the union
	// covering BOTH sets, so a later clear can attribute whatever is live.
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
	// Step 2: the swap succeeded — trim the attribution to the ACTUAL set, still
	// under the same lock. A failure here keeps the union (no orphan) but the
	// install is NOT successful: the error propagates, strict callers refuse.
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
