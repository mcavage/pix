// packhost.go — F3: pack host-mode wrappers (packs-v2-impl.md F3, packs.md §9).
//
// A pack's host-exec facets ([[proxy]] host=true wrapper scripts and [[bin]]
// SHA-pinned external binaries) install into hostPackBinDir(), which
// hostChildEnv prepends to the child PATH — so they are reachable from
// `pi-stack host` ONLY (never the sandbox, never the login shell). Two gates in
// series guard them: the machine-level host.enabled opt-in (hostrun.go, off by
// default) and the F5 Tier-1 adoption BoM gate (packtrust.go) — only facets the
// user ACCEPTED at `pack use` (recorded in pack.lock) are ever installed, and
// every accepted [[bin]] is re-hashed against its pinned sha at install AND at
// every host launch, refusing on mismatch (a tampered binary never runs).
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"pi-stack/host/config"
)

// hostPackBinDir is where the ACTIVE pack's host-mode wrappers live:
// <hostAgentDir>/bin. State-flavored and rebuildable (cleared + refreshed from
// the active pack), exactly like the rest of the host agent dir. It reaches a
// PATH in exactly one place: hostChildEnv (hostrun.go) — host mode only.
func hostPackBinDir() string { return filepath.Join(hostAgentDir(), "bin") }

// hashFileSHA256 returns the lowercase hex sha256 of the file at path. It is
// the ADR-4 duplicate of the hashing core inside verifyPluginSHA
// (services/host/serve_plugin.go) — the launcher is a separate,
// dependency-light main package (same pattern as canonicalizeKnowledgeBundle),
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
// loadPack already refuses an empty sha and a symlinked/escaping Path; the
// empty-sha check here is belt-and-suspenders for callers holding a packBin
// that never went through loadPack.
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

// packBinPair is the "name=sha" form a [[bin]] acceptance is recorded under in
// pack.lock (AcceptedBins). Keying acceptance on the PAIR means a sha changed
// since adoption is simply not accepted any more — it re-triggers the F5 gate
// at the next `pack use` and is skipped (never installed) until then.
func packBinPair(b packBin) string {
	return b.Name + "=" + strings.ToLower(strings.TrimSpace(b.SHA))
}

// declaredHostWrapperNames lists the wrapper names a pack's manifest declares
// for HOST installation (host=true proxies + host=true bins), deduplicated in
// manifest order. This is what pack.lock's HostWrappers records at activation:
// the intended install set, so the next switch/refresh removes exactly these
// (lock-attributed removal — never a file this pack cannot claim).
func declaredHostWrapperNames(p *packInfo) []string {
	seen := map[string]bool{}
	var names []string
	add := func(n string) {
		if n != "" && !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	for _, pr := range p.Manifest.Proxies {
		if pr.Host {
			add(pr.Name)
		}
	}
	for _, b := range p.Manifest.Bins {
		if b.Host {
			add(b.Name)
		}
	}
	return names
}

// clearHostPackWrappers removes the named wrappers from hostPackBinDir(),
// best-effort. Removal is strictly name-scoped (lock attribution — the same
// never-remove-what-you-can't-attribute posture as the MCP/knowledge swap) and
// symlink-safe: a symlinked host bin dir is never traversed, and a name that
// fails safeArtifactName (a lock edited to smuggle a path) is never joined.
func clearHostPackWrappers(names []string) {
	if len(names) == 0 {
		return
	}
	dir := hostPackBinDir()
	if isSymlinkPath(dir) {
		return // refuse to remove through a symlinked dir
	}
	for _, n := range names {
		if !safeArtifactName(n) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, n))
	}
}

// installHostPackWrappers copies pack p's ACCEPTED host-mode wrappers into
// hostPackBinDir() (0755) and returns the names actually installed.
//
//   - Only facets the lock's accepted BoM covers are installed (the F5 gate is
//     the ONLY thing that writes acceptance): a host wrapper declared since the
//     last `pack use` is skipped with a pointer to re-run it — never silently
//     put on the host PATH.
//   - Every accepted [[bin]] is RE-HASHED here (the bytes about to be written,
//     so there is no hash-then-copy TOCTOU) and REFUSED on mismatch.
//   - Writes are atomic + symlink-safe (atomicWriteInDir; a symlinked dest is
//     replaced, never followed) and sources are Lstat-checked (loadPack already
//     refuses symlinks in bin/; belt-and-suspenders here).
//   - Per-item failures print a TODO and continue (the runHostSetup contract:
//     a failed copy must not fail setup) — callers needing hard fail-closed
//     semantics (the host LAUNCH) verify first via refreshHostPackWrappers.
func installHostPackWrappers(out io.Writer, p *packInfo, lock packLock) []string {
	var hostProxies []packProxy
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
	if len(hostProxies) == 0 && len(hostBins) == 0 {
		return nil // Tier-0 for host purposes: nothing to install
	}
	dir := hostPackBinDir()
	if isSymlinkPath(dir) {
		fmt.Fprintf(out, "TODO: %s is a symlink; refusing to install host wrappers through it\n", dir)
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(out, "TODO: host wrappers not installed (%v)\n", err)
		return nil
	}
	var installed []string
	notAccepted := func(name string) {
		fmt.Fprintf(out, "note: host wrapper %q is not accepted yet — run `pi-stack pack use %s` to review + accept the host BoM\n", name, p.Root)
	}
	install := func(name string, data []byte) {
		if err := atomicWriteInDir(dir, name, data, 0o755); err != nil {
			fmt.Fprintf(out, "TODO: host wrapper %q not installed: %v\n", name, err)
			return
		}
		installed = append(installed, name)
	}
	for _, pr := range hostProxies {
		if !containsStr(lock.AcceptedHostProxies, pr.Name) {
			notAccepted(pr.Name)
			continue
		}
		src := filepath.Join(p.Root, "bin", pr.Name)
		if isSymlinkPath(src) {
			fmt.Fprintf(out, "TODO: host wrapper %q is a symlink; refusing to install it\n", pr.Name)
			continue
		}
		data, err := os.ReadFile(src)
		if err != nil {
			fmt.Fprintf(out, "TODO: host wrapper %q not installed: %v\n", pr.Name, err)
			continue
		}
		install(pr.Name, data)
	}
	for _, b := range hostBins {
		if !containsStr(lock.AcceptedBins, packBinPair(b)) {
			notAccepted(b.Name)
			continue
		}
		src := filepath.Join(p.Root, b.Path)
		if isSymlinkPath(src) {
			fmt.Fprintf(out, "TODO: external binary %q is a symlink; refusing to install it\n", b.Name)
			continue
		}
		data, err := os.ReadFile(src)
		if err != nil {
			fmt.Fprintf(out, "TODO: external binary %q not installed: %v\n", b.Name, err)
			continue
		}
		sum := sha256.Sum256(data)
		got := hex.EncodeToString(sum[:])
		want := strings.ToLower(strings.TrimSpace(b.SHA))
		if !strings.EqualFold(got, want) {
			fmt.Fprintf(out, "REFUSED: external binary %q sha256 mismatch: got %s, want %s — not installed (tampered binary or stale pin)\n", b.Name, got, want)
			continue
		}
		install(b.Name, data)
	}
	return installed
}

// refreshHostPackWrappers syncs hostPackBinDir() with the ACTIVE pack's
// accepted host wrappers: clear what the pack's last activation installed
// (lock.HostWrappers), reinstall the accepted set from the manifest, and record
// the new installed set back into pack.lock. Idempotent; a missing pack or a
// Tier-0 pack degrades to (nearly) a no-op. Called from runPackUse (post-Save
// swap), runHostSetup (strict=false: per-item TODOs, setup never fails), and
// runHostLaunch (strict=true).
//
// strict is the LAUNCH contract: every ACCEPTED host [[bin]] is re-hashed
// against its pinned sha FIRST and a mismatch returns an error — the caller
// must refuse the launch (a tampered external binary never runs; packs.md §9
// safeguard 2). A broken/unloadable active pack is an error in both modes
// (fail closed, mirroring applyPackToLaunch); only the errNotAPack "genuinely
// absent" case degrades with a warning.
//
// It returns the loaded active pack (nil when none/degraded) so runHostLaunch
// can reuse it for the memory scope tag without a second loadPack.
func refreshHostPackWrappers(out io.Writer, cfg *config.Config, strict bool) (*packInfo, error) {
	root := activePackRoot(cfg.Pack, "")
	if root == "" {
		return nil, nil // no active pack; nothing to install
	}
	p, err := loadPack(root)
	if err != nil {
		if errors.Is(err, errNotAPack) {
			fmt.Fprintf(out, "note: active pack unavailable (%v); host wrappers not refreshed\n", err)
			return nil, nil
		}
		return nil, fmt.Errorf("active pack %s: %v (refusing to use its host wrappers; fix the pack or `pi-stack pack rm` to detach it)", root, err)
	}
	lock := readPackLock(root)
	if strict {
		for _, b := range p.Manifest.Bins {
			if !b.Host || !containsStr(lock.AcceptedBins, packBinPair(b)) {
				continue // never accepted → never installed → nothing launches
			}
			if verr := verifyPackBinSHA(root, b); verr != nil {
				return nil, fmt.Errorf("pack %s: %v", p.Manifest.Name, verr)
			}
		}
	}
	clearHostPackWrappers(lock.HostWrappers)
	installed := installHostPackWrappers(out, p, lock)
	if strings.Join(installed, "\x00") != strings.Join(lock.HostWrappers, "\x00") {
		// Record what is ACTUALLY in the host bin dir now, so the next
		// clear removes exactly that. Best-effort: a failed write leaves the
		// lock over-claiming, and removal of an absent file is a no-op.
		l2 := readPackLock(root)
		l2.HostWrappers = installed
		if werr := writePackLock(root, l2); werr != nil {
			fmt.Fprintf(out, "note: could not record installed host wrappers in pack.lock: %v\n", werr)
		}
	}
	return p, nil
}
