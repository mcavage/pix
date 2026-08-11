// pack.go implements `pix pack` — the git-backed context bundle (skills,
// knowledge, mcp/proxy/config facets; docs/design/packs.md). All OS/git calls
// go through hostenv so the logic is testable with fakes.
package pack

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/inference"
	"pix/host/packinfo"
	"pix/host/secret"
	"pix/host/service"
	"pix/host/sys"

	"github.com/BurntSushi/toml"
)

// ApplyPackInference projects a pack's inference contract into launcher config:
// public wiring metadata only (the schema cannot carry a secret). Available is
// NOT probe evidence — it means "this pack's schema passed validation and its
// host-exec surface cleared the Tier-1 trust gate" (every call site runs this
// AFTER that gate: packUse via gatePackHostSurface, launch via
// VerifyPackLaunchTrust), which is exactly what a proxy-injected sbx-session
// credential needs: there is no host-reachable endpoint to probe, so
// inference.HostProofRequired already exempts it. A backend inference CAN
// probe from the host (1Password, Ollama) starts and stays Available:false
// until setup actually earns it; Verified is reserved for that probe and is
// never set here.
func ApplyPackInference(cfg *config.Config, inf *packinfo.Inference, source string) error {
	if cfg == nil || inf == nil {
		return nil
	}
	for name := range inf.Backends {
		if existing, ok := cfg.Inference.Backends[name]; ok && existing.Source != source {
			owner := "user configuration"
			if existing.Source != "" {
				owner = existing.Source
			}
			return fmt.Errorf("pack inference backend %q conflicts with %s; backend names cannot replace another source", name, owner)
		}
	}
	// Reapplying an unchanged active pack must not erase the availability
	// evidence setup just earned: preserved only across an exact backend +
	// binding match, so any change starts unverified.
	type evidence struct{ available, verified bool }
	prior := map[string]evidence{}
	for _, binding := range cfg.Inference.Models {
		backend, ok := cfg.Inference.Backends[binding.Backend]
		if !ok || binding.Source != source {
			continue
		}
		key := inferenceEvidenceKey(binding, backend)
		prior[key] = evidence{binding.Available, binding.Verified}
	}
	ClearPackInference(cfg, source)
	if cfg.Inference.Backends == nil {
		cfg.Inference.Backends = map[string]config.InferenceBackend{}
	}
	for name, b := range inf.Backends {
		cfg.Inference.Backends[name] = config.InferenceBackend{
			Driver: b.Driver, Protocol: b.Protocol, BaseURL: b.BaseURL, Auth: b.Auth, KeyEnv: b.KeyEnv, Source: source,
			CredentialService: b.CredentialService, CredentialHeader: b.CredentialHeader, CredentialFormat: b.CredentialFormat,
		}
	}
	for _, b := range inf.Models {
		binding := config.InferenceModelBinding{
			Model: b.Model, Backend: b.Backend, Upstream: b.Upstream, Source: source,
		}
		if backend, ok := cfg.Inference.Backends[b.Backend]; ok {
			if ev, found := prior[inferenceEvidenceKey(binding, backend)]; found {
				binding.Available, binding.Verified = ev.available, ev.verified
			} else if !inference.HostProofRequired(backend, source) {
				// Structurally injectable the moment this pack cleared trust: the
				// backend has no host-reachable endpoint to probe (sbx-session, or
				// any other mode needsHostProof already exempts), so "unavailable
				// pending a probe that can never run" is not a real intermediate
				// state — it only ever produced a permanently unwired binding.
				binding.Available = true
			}
		}
		cfg.Inference.Models = append(cfg.Inference.Models, binding)
	}
	if inf.Exclusive {
		cfg.Inference.ExclusiveSource = source
	}
	return nil
}

func inferenceEvidenceKey(binding config.InferenceModelBinding, backend config.InferenceBackend) string {
	return strings.Join([]string{
		binding.Source, binding.Model, binding.Backend, binding.Upstream,
		backend.Driver, backend.Protocol, backend.BaseURL, backend.Auth,
		backend.KeyEnv, backend.Source, backend.CredentialService,
		backend.CredentialHeader, backend.CredentialFormat,
	}, "\x00")
}

// ClearPackInference removes only pack-owned inference. An empty source clears
// every pack contribution; setup-authored backends have Source="" and survive.
func ClearPackInference(cfg *config.Config, source string) {
	if cfg == nil {
		return
	}
	for name, backend := range cfg.Inference.Backends {
		if backend.Source != "" && (source == "" || backend.Source == source) {
			delete(cfg.Inference.Backends, name)
		}
	}
	kept := cfg.Inference.Models[:0]
	for _, binding := range cfg.Inference.Models {
		if binding.Source != "" && (source == "" || binding.Source == source) {
			continue
		}
		kept = append(kept, binding)
	}
	cfg.Inference.Models = kept
	if cfg.Inference.ExclusiveBackend != "" {
		if _, ok := cfg.Inference.Backends[cfg.Inference.ExclusiveBackend]; !ok {
			cfg.Inference.ExclusiveBackend = ""
		}
	}
	if cfg.Inference.ExclusiveSource != "" && (source == "" || cfg.Inference.ExclusiveSource == source) {
		cfg.Inference.ExclusiveSource = ""
	}
}

// PersistPackStack composes every declared config facet after each pack has
// independently passed adoption and trust checks: collections union, scalars
// apply in command order (last wins), ownership is recorded PER PACK.
func PersistPackStack(roots []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	store, err := requireTrustStore()
	if err != nil {
		return err
	}
	var packs []*packinfo.Info
	for _, root := range packinfo.UniquePackRoots(roots) {
		p, perr := packinfo.LoadPack(root)
		if perr != nil {
			return perr
		}
		packs = append(packs, p)
	}
	records, _, err := applyPackStack(cfg, store, packs)
	if err != nil {
		return err
	}
	return packTxn{records: records}.commit(cfg)
}

// requireTrustStore loads the launcher-owned trust store or fails closed. An
// unreadable store is FATAL wherever it is read: it is the reversibility AND
// acceptance backbone, so an empty stand-in would lose the previous removal set
// and clobber the store at the commit point.
func requireTrustStore() (*PackTrustStore, error) {
	store, err := loadPackTrustStore()
	if err != nil {
		return nil, fmt.Errorf("pack trust state unreadable: %v (fix or remove %s and re-run)", err, packTrustStorePath())
	}
	return store, nil
}

// applyPackFacets applies ONE pack's config facets and returns its ownership
// record: the MCP names it actually added (never one the user already had) plus
// each scalar's PRIOR value, so switching away restores exactly that.
func applyPackFacets(cfg *config.Config, p *packinfo.Info) (packLock, error) {
	var lock packLock
	for _, name := range packinfo.McpNames(p) {
		if cfg.AddMCP(name) {
			lock.MCP = append(lock.MCP, name)
		}
	}
	if v := strings.TrimSpace(p.Manifest.OllamaBridgeModel); v != "" {
		lock.PriorOllamaBridgeModel = cfg.OllamaBridgeModel
		lock.OllamaBridgeModel = v
		cfg.OllamaBridgeModel = v
	}
	// Exclusive policy is an ordered scalar, not an additive facet: a later
	// non-exclusive declaration clears an earlier pack's exclusivity.
	if p.Manifest.Inference != nil && !p.Manifest.Inference.Exclusive {
		cfg.Inference.ExclusiveSource = ""
	}
	if err := ApplyPackInference(cfg, p.Manifest.Inference, p.Root); err != nil {
		return packLock{}, err
	}
	return lock, nil
}

// applyPackStack is the ONE compose path — `pack use` passes a single pack,
// `pix setup --pack ...` the whole stack. It rewinds the live activation to its
// pre-pack baseline (so the Prior* values each record captures are the real
// ones), applies the packs in command order, and returns one ownership record
// per pack plus every MCP name the rewind removed. Nothing is written here; the
// caller commits.
func applyPackStack(cfg *config.Config, store *PackTrustStore, packs []*packinfo.Info) ([]packActivationRecord, []string, error) {
	removed := revertPackStack(cfg, store, packinfo.ActivePackRoots(cfg, ""))
	ClearPackInference(cfg, "")
	cfg.Pack, cfg.Packs = "", nil
	var records []packActivationRecord
	for _, p := range packs {
		lock, err := applyPackFacets(cfg, p)
		if err != nil {
			return nil, nil, err
		}
		cfg.Pack = p.Root
		cfg.Packs = append(cfg.Packs, p.Root)
		records = append(records, store.newActivationRecord(p.Root, lock))
	}
	dedupeInferenceBindings(cfg)
	return records, removed, nil
}

// dedupeInferenceBindings keeps the LAST declaration of each (model,backend) in
// stack order, so a later pack can replace an earlier pack's upstream alias.
func dedupeInferenceBindings(cfg *config.Config) {
	if len(cfg.Inference.Models) == 0 {
		return
	}
	seen := map[string]bool{}
	kept := make([]config.InferenceModelBinding, 0, len(cfg.Inference.Models))
	for i := len(cfg.Inference.Models) - 1; i >= 0; i-- {
		b := cfg.Inference.Models[i]
		if key := b.Model + "\x00" + b.Backend; !seen[key] {
			seen[key] = true
			kept = append(kept, b)
		}
	}
	slices.Reverse(kept)
	cfg.Inference.Models = kept
}

// revertPackStack unwinds an activation stack in REVERSE command order — the
// only order where each scalar restore sees the value its predecessor set —
// reporting every MCP name actually removed.
func revertPackStack(cfg *config.Config, store *PackTrustStore, roots []string) []string {
	var removed []string
	for i := len(roots) - 1; i >= 0; i-- {
		removed = append(removed, revertPackPriorContribution(cfg, store.activationFor(roots[i]))...)
	}
	return removed
}

// packKitDir is the PER-PACK KEY ephemeral mixin kits are synthesized under
// (<StateDir>/pix/pack-kits/<hash>): a naming PREFIX, not a live dir — each
// launch builds its own <hash>.kit-XXXX beside it.
func packKitDir(root string) string {
	sum := sha256.Sum256([]byte(root))
	dir, err := config.StateDir()
	if err != nil {
		dir = "pix-state"
	}
	return filepath.Join(dir, "pix", "pack-kits", hex.EncodeToString(sum[:])[:16])
}

// SynthesizePackKit builds the ephemeral mixin kit that mounts a pack's
// sandbox-side files: a `kind: mixin` spec.yaml plus files/home/.local/bin/
// <name> (0755) per non-host [[proxy]], capabilities.json and web-search.json.
// Returns (dir, nil), ("", nil) when there is nothing to mount (the caller must
// not stack an empty kit), or ("", err) when something IS declared but the kit
// cannot be built — the caller then fails the launch closed. Copies, never
// symlinks; each call builds its OWN MkdirTemp dir COMPLETELY before returning,
// so concurrent launches never clash and a partial kit is never mounted.
func SynthesizePackKit(p *packinfo.Info) (string, error) {
	var sandboxProxies []packinfo.PackProxy
	for _, pr := range p.Manifest.Proxies {
		if !pr.Host {
			sandboxProxies = append(sandboxProxies, pr)
		}
	}
	base := packKitDir(p.Root)
	parent := filepath.Dir(base)
	sweepStaleKitTemps(parent, filepath.Base(base))
	if len(sandboxProxies) == 0 && p.CapabilitiesFile == "" && p.WebSearchFile == "" {
		return "", nil // nothing to mount; the sweep above reaps any previous kit
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", fmt.Errorf("pack kit for %s: %v", p.Manifest.Name, err)
	}
	dir, err := os.MkdirTemp(parent, filepath.Base(base)+kitLaunchInfix)
	if err != nil {
		return "", fmt.Errorf("pack kit for %s: %v", p.Manifest.Name, err)
	}
	fail := func(format string, a ...any) (string, error) {
		_ = os.RemoveAll(dir) // never leave a half-built kit dir behind
		return "", fmt.Errorf(format, a...)
	}
	_ = os.Chmod(dir, 0o755) // MkdirTemp creates 0700; the kit is a mounted tree
	// A stacked kit needs a valid manifest: schemaVersion (required by the loader),
	// kind: mixin, and a name. Match the base kit's schemaVersion "2".
	spec := fmt.Sprintf("schemaVersion: \"2\"\nkind: mixin\nname: %s\n", p.Manifest.Name)
	// Fold each sandbox proxy's declared egress into permissions.network.allow so
	// the wrapper can reach its host endpoint — the sbx egress proxy blocks (403)
	// anything off the allowlist. Kit stacking unions this with the base kit's.
	var egress []string
	egSeen := map[string]bool{}
	addEgress := func(e string) {
		if e == "" || egSeen[e] {
			return
		}
		egSeen[e] = true
		egress = append(egress, e)
	}
	for _, pr := range sandboxProxies {
		for _, e := range pr.Egress {
			e = strings.TrimSpace(e)
			addEgress(e)
			// The sbx egress proxy matches host.docker.internal and localhost as
			// DISTINCT rules, so a host-loopback egress must allow BOTH forms.
			if h := strings.TrimPrefix(e, "host.docker.internal:"); h != e {
				addEgress("localhost:" + h)
			} else if l := strings.TrimPrefix(e, "localhost:"); l != e {
				addEgress("host.docker.internal:" + l)
			}
		}
	}
	if len(egress) > 0 {
		spec += "permissions:\n  network:\n    allow:\n"
		for _, e := range egress {
			spec += "      - " + e + "\n"
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "spec.yaml"), []byte(spec), 0o644); err != nil {
		return fail("pack kit for %s: %v", p.Manifest.Name, err)
	}
	// Everything else is a copy into files/home/** (the sbx mixin mount honors
	// files/home/** into $HOME but NOT files/usr/local/**, so a wrapper written
	// there never lands). Any declared-but-unreadable file fails the whole synth
	// — never launch with a partial kit.
	type kitFile struct {
		label, src string
		dest       []string
		mode       os.FileMode
	}
	var files []kitFile
	for _, pr := range sandboxProxies {
		files = append(files, kitFile{"pack proxy " + pr.Name, filepath.Join(p.Root, "bin", pr.Name),
			[]string{"files", "home", ".local", "bin", pr.Name}, 0o755})
	}
	if p.CapabilitiesFile != "" {
		files = append(files, kitFile{"pack capabilities.json", p.CapabilitiesFile,
			[]string{"files", "home", ".pi", "agent", "capabilities.json"}, 0o644})
	}
	if p.WebSearchFile != "" {
		files = append(files, kitFile{"pack web-search.json", p.WebSearchFile,
			[]string{"files", "home", ".pi", "web-search.json"}, 0o644})
	}
	for _, f := range files {
		dest := filepath.Join(append([]string{dir}, f.dest...)...)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fail("pack kit for %s: %v", p.Manifest.Name, err)
		}
		b, err := os.ReadFile(f.src)
		if err != nil {
			return fail("%s: %v (refusing to build the pack kit)", f.label, err)
		}
		if err := os.WriteFile(dest, b, f.mode); err != nil {
			return fail("%s: %v (refusing to build the pack kit)", f.label, err)
		}
	}
	return dir, nil
}

// kitLaunchInfix suffixes each per-launch kit dir onto the pack hash, so launch
// dirs sit beside their key and sweepStaleKitTemps finds them by prefix.
const kitLaunchInfix = ".kit-"

// sweepStaleKitTemps best-effort removes old per-launch kit dirs for THIS pack.
// Only entries older than an hour are touched, so a concurrent launch's kit is
// never yanked out from under it.
func sweepStaleKitTemps(parent, base string) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-time.Hour)
	for _, e := range entries {
		name := e.Name()
		// base+"." covers kitLaunchInfix and any other dotted debris beside the
		// key; the bare base is the stable kit path older builds synthesized into.
		if name != base && !strings.HasPrefix(name, base+".") {
			continue
		}
		if info, ierr := e.Info(); ierr != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(parent, name))
	}
}

// packLock is <pack-root>/pack.lock: GENERATED activation provenance (what the
// last `pack use` contributed to cfg), git-ignored.
//
// TRUST: a LOCAL, HUMAN-READABLE HINT only. It sits inside the pack directory —
// attacker-writable for any cloned pack via a plain `git pull` — so nothing
// that drives a config mutation is read from it. The authoritative record is
// the launcher-owned trust store, written at the same commit point. The only
// field read back is Remote/Commit, a FAIL-SAFE adoption marker.
type packLock struct {
	MCP []string `toml:"mcp,omitempty"`
	// Remote/Commit are set ONLY by a `pack use <git-url>` adoption and kept
	// across re-activations, so adoption can't be laundered by pointing `pack
	// use` at the already-cloned local directory.
	Remote string `toml:"remote,omitempty"`
	Commit string `toml:"commit,omitempty"`
	// OllamaBridgeModel records the value THIS pack's last activation set on
	// cfg. Prior* is whatever cfg held immediately BEFORE this pack overwrote
	// it, so reverting on switch-away restores exactly that.
	//
	// gog_account/prior_gog_account used to live here too. They are gone with
	// the rest of the built-in Google Workspace surface: a pack that needs a
	// per-user account for a server now forwards it as that integration's
	// env_keys, which needs no config key and therefore no revert-on-switch.
	// An OLD lock file may still carry those keys; TOML ignores unknown fields,
	// so it loads and the stale values are simply never read again.
	OllamaBridgeModel      string `toml:"ollama_bridge_model,omitempty"`
	PriorOllamaBridgeModel string `toml:"prior_ollama_bridge_model,omitempty"`
}

const PackLockName = "pack.lock"

func PackLockPath(root string) string { return filepath.Join(root, PackLockName) }

// readPackLock reads root's pack.lock, best-effort: an absent OR UNPARSABLE
// file returns the zero value — never guess at (or half-decode) what an older
// activation contributed, since that mis-reports a removal set.
func readPackLock(root string) packLock {
	b, err := os.ReadFile(PackLockPath(root))
	if err != nil {
		return packLock{}
	}
	var l packLock
	if err := toml.Unmarshal(b, &l); err != nil {
		return packLock{}
	}
	return l
}

// writePackLock writes root's pack.lock (0644; NAMES and paths, never a
// credential).
func writePackLock(root string, l packLock) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(l); err != nil {
		return err
	}
	return writePackLockBytes(root, buf.Bytes())
}

// writePackLockBytes is the raw-bytes half of writePackLock (so a rollback can
// restore the prior lock byte-for-byte without a decode round-trip). It
// Lstat-REFUSES a symlinked destination — a malicious pack could redirect the
// write at any host file — and writes temp + rename.
func writePackLockBytes(root string, data []byte) error {
	dest := PackLockPath(root)
	if fi, err := os.Lstat(dest); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to write through it", dest)
	}
	return sys.AtomicWriteInDir(root, PackLockName, data, 0o644)
}

// packTxn is the ONE commit point every activation shares (`pack use`, the
// active-pack `pack add mcp`, and `pix setup --pack ...`). Three writes,
// ordered so the failure residue is always safe: (1) pack.lock, the local hint
// — unwritable aborts before anything commits; (2) the AUTHORITATIVE ownership
// ledger — a failure also aborts before cfg.Save, so config is never committed
// unattributed; (3) cfg.Save, whose ordinary failure rolls BOTH back. Only a
// hard kill between (2) and (3) leaves an over-claiming record (a no-op
// removal). Steps 2-3 and the rollback run under the trust lock against a
// FRESH store load.
type packTxn struct {
	records  []packActivationRecord
	lockRoot string   // pack whose pack.lock hint to write ("" writes none)
	lock     packLock // the hint itself; only meaningful with lockRoot
}

func (t packTxn) commit(cfg *config.Config) error {
	restoreLock, err := t.writeLockHint()
	if err != nil {
		return err
	}
	return withPackTrustLock(func() error {
		fresh, lerr := loadPackTrustStore()
		if lerr != nil {
			return t.abort(restoreLock, "pack trust state unreadable: %v", lerr)
		}
		prior := append([]packActivationRecord(nil), fresh.Activations...)
		records := append([]packActivationRecord(nil), t.records...)
		for i := range records {
			// Re-key against the FRESH store: identity may have been upgraded
			// path->remote by a clone recorded since the caller loaded its view.
			records[i].Owner = fresh.TrustKey(records[i].Path)
		}
		fresh.setActivationStack(records)
		if serr := fresh.Save(); serr != nil {
			return t.abort(restoreLock, "recording activation in pack trust state: %v", serr)
		}
		if cerr := cfg.Save(); cerr != nil {
			// Roll BOTH the ledger and the lock back so they match the
			// (unchanged) on-disk config.
			fresh.Activations = prior
			serr, rerr := fresh.Save(), restoreLock()
			if serr != nil || rerr != nil {
				return fmt.Errorf("saving config: %v (rollback incomplete — trust store: %v, pack.lock: %v — the activation record may over-claim this activation's contributions; harmless, but re-run `pack use` once the config is writable)", cerr, serr, rerr)
			}
			return fmt.Errorf("saving config: %v (activation record rolled back; nothing was committed)", cerr)
		}
		return nil
	})
}

// writeLockHint snapshots and writes pack.lock, returning the rollback that
// restores the prior bytes (or removes the file) byte-for-byte. Without a
// snapshot a Save-failure rollback would be impossible, so an unreadable prior
// lock aborts BEFORE anything is written.
func (t packTxn) writeLockHint() (func() error, error) {
	noop := func() error { return nil }
	if t.lockRoot == "" {
		return noop, nil
	}
	const abort = "aborting without saving config (nothing was committed; fix the pack directory and re-run)"
	prior, priorErr := os.ReadFile(PackLockPath(t.lockRoot))
	if priorErr != nil && !os.IsNotExist(priorErr) {
		return nil, fmt.Errorf("reading prior pack.lock for %s: %v — %s", t.lockRoot, priorErr, abort)
	}
	if err := writePackLock(t.lockRoot, t.lock); err != nil {
		return nil, fmt.Errorf("writing pack.lock for %s: %v — %s", t.lockRoot, err, abort)
	}
	existed := priorErr == nil
	return func() error {
		if existed {
			return writePackLockBytes(t.lockRoot, prior)
		}
		return os.Remove(PackLockPath(t.lockRoot))
	}, nil
}

// abort restores the pack.lock hint and reports that nothing was committed.
func (t packTxn) abort(restoreLock func() error, format string, a ...any) error {
	cause := fmt.Sprintf(format, a...)
	if rerr := restoreLock(); rerr != nil {
		return fmt.Errorf("%s (and restoring the prior pack.lock failed: %v) — aborting without saving config (nothing was committed)", cause, rerr)
	}
	return fmt.Errorf("%s — aborting without saving config (nothing was committed; fix %s and re-run)", cause, packTrustStorePath())
}

// gatePackHostSurface is the Tier-1 host-exec trust gate `pack use` runs before
// any commit. Tier-0 returns ("", "", nil) and adopts silently; Tier-1 halts at the BoM
// screen unless HOST trust state already holds this identity's acceptance of
// the EXACT current surface. A non-nil error means the caller commits NOTHING.
func gatePackHostSurface(env hostenv.Env, out io.Writer, store *PackTrustStore, p *packinfo.Info, root string, yes bool) (fingerprint, key string, err error) {
	bom := ComputeHostBoM(p)
	if !bom.Tier1() {
		return "", "", nil
	}
	fp, _, ferr := ComputeHostExecFingerprint(root, bom)
	if ferr != nil {
		return "", "", fmt.Errorf("pack %s: %v", root, ferr)
	}
	key = store.TrustKey(root)
	if got, ok := store.acceptedFingerprint(key); !ok || got != fp {
		if gerr := packTrustGate(os.Stdin, out, cli.IsTTY(os.Stdin), yes, p.Manifest.Name, bom); gerr != nil {
			return "", "", gerr
		}
	}
	return fp, key, nil
}

// recordPackAcceptance persists an accepted Tier-1 host-exec fingerprint in
// HOST state; an empty fingerprint (Tier-0) records nothing. Provenance is
// HOST-recorded ONLY — never the pack-supplied lock, whose forged Remote could
// alias a legit pack and make RecordAcceptance's hygiene sweep DELETE its
// acceptance. Best-effort: a failed write only re-prompts — never opens.
func recordPackAcceptance(out io.Writer, key, root, fingerprint, remote, commit string) {
	if fingerprint == "" {
		return
	}
	rec := PackTrustRecord{Path: packinfo.CanonicalizePackRoot(root), Fingerprint: fingerprint, Remote: remote, Commit: commit}
	if _, werr := mutatePackTrustStore(func(s *PackTrustStore) error {
		if rec.Remote == "" {
			if prov, ok := s.Adopted[rec.Path]; ok {
				rec.Remote, rec.Commit = prov.Remote, prov.Commit
			}
		}
		s.RecordAcceptance(key, rec)
		return nil
	}); werr != nil {
		fmt.Fprintf(out, "note: could not record the accepted host BoM: %v (the Tier-1 gate will re-prompt)\n", werr)
	}
}

// isAdoptedPack reports whether root was cloned from a remote via `pack use
// <git-url>` — attacker-controlled content, so its manifest never gets to point
// host reads at an arbitrary directory. Three fail-safe signals (a forged
// marker only RESTRICTS): the pack.lock Remote marker, the trust store's
// adoption provenance, and the clone LOCATION under PacksDir.
func isAdoptedPack(root string) bool {
	if strings.TrimSpace(readPackLock(root).Remote) != "" {
		return true
	}
	if store, err := loadPackTrustStore(); err == nil {
		if _, ok := store.Adopted[packinfo.CanonicalizePackRoot(root)]; ok {
			return true
		}
	}
	return packRootInPacksDir(root)
}

// packRootInPacksDir reports whether root lives under config.PacksDir() — the
// directory only clonePack ever populates, so location alone proves adoption.
func packRootInPacksDir(root string) bool {
	packs := packinfo.CanonicalizePackRoot(config.PacksDir())
	r := packinfo.CanonicalizePackRoot(root)
	return packs != "" && strings.HasPrefix(r, packs+string(filepath.Separator))
}

// scrubUntrustedPackLock removes a pack-supplied pack.lock before adopting a
// pack that is NOT currently active: a downloaded pack can ship a forged one,
// or a symlink redirecting the fresh write (os.Remove never follows it).
func scrubUntrustedPackLock(root string) error {
	path := PackLockPath(root)
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing untrusted %s symlink: %w", PackLockName, err)
		}
		return nil
	}
	if fi.IsDir() {
		// A DIRECTORY named pack.lock carries no forged content (readPackLock
		// zero-values it); leave it for the commit point, which fails loudly.
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing untrusted %s: %w", PackLockName, err)
	}
	return nil
}

// revertPackPriorContribution undoes ONE previous activation: it removes
// exactly the MCP entries the ledger attributes to that pack (never one it
// doesn't mention) and restores the scalars to what cfg held before.
func revertPackPriorContribution(cfg *config.Config, prevLock packLock) (removedMCP []string) {
	for _, m := range prevLock.MCP {
		if cfg.RemoveMCP(m) {
			removedMCP = append(removedMCP, m)
		}
	}
	// Only revert if cfg still holds exactly what THIS pack set — never clobber
	// a value something else changed in the meantime.
	if prevLock.OllamaBridgeModel != "" && cfg.OllamaBridgeModel == prevLock.OllamaBridgeModel {
		cfg.OllamaBridgeModel = prevLock.PriorOllamaBridgeModel
	}
	return removedMCP
}

// printPackRecreateLine is the "same breath" recreate instruction every
// sandbox-facet change MUST print, because --mcp/--kit are create-only.
func printPackRecreateLine(out io.Writer) {
	fmt.Fprintln(out, "MCP attach + sandbox bin/ wrappers + pack skills only take effect on a sandbox CREATE.")
	fmt.Fprintln(out, "Recreate to pick them up:  pix rm <box> && pix run  (`pix ls` names the box)")
}

// --- verb tree --------------------------------------------------------------
//
// Every verb RETURNS a typed error and writes only to the injected writer: the
// domain neither exits the process nor picks a stream. cli.UsageError is a BAD
// INVOCATION; anything else is a failed operation. cmd/pix (L4) is the ONE
// place either becomes an exit code, because that is the layer that owns the
// process.
func usagef(format string, a ...any) error { return cli.Usagef(format, a...) }

// packFlags is the ONE argv parse `pack use` needs (cmd/pix owns the typed
// grammar and renders it back to argv). `pack add`'s --host/--env retired
// with the verb, so this is just --yes plus positionals.
type packFlags struct {
	positionals []string
	yes         bool
}

func parsePackFlags(rest []string) (packFlags, error) {
	var f packFlags
	for _, a := range rest {
		switch {
		case a == "--yes", a == "-y":
			f.yes = true
		case strings.HasPrefix(a, "-"):
			return f, fmt.Errorf("unknown flag %q", a)
		default:
			f.positionals = append(f.positionals, a)
		}
	}
	return f, nil
}

// packTarget resolves an optional positional PATH to a pack root, defaulting to
// the default pack root.
func packTarget(rest []string) string {
	if len(rest) > 0 && strings.TrimSpace(rest[0]) != "" {
		return packinfo.ExpandUser(rest[0])
	}
	return packinfo.DefaultPackRoot()
}

// RegisterFn registers the named servers with the sbx gateway: pack may not
// call mcp (both are capabilities), so the caller supplies this seam.
type RegisterFn func(cfg *config.Config, env hostenv.Env, out io.Writer, names []string,
	servers map[string]config.MCPServer) error

// registerPackMCP registers names with the sbx gateway. Idempotent, and it runs
// even for a name ALREADY in cfg.MCP: a retry after a failed registration must
// re-register, and a changed declaration must re-resolve. Best-effort.
// It returns the registration error rather than only printing it. "Best-effort"
// used to mean the error became a `note:` line and `pix pack use` exited 0 —
// so adopting a pack on a machine missing one of its commands (the normal first
// run) reported success for servers that had not been registered. The pack IS
// still activated either way, which is the part that must not be undone; the
// caller decides the exit code.
func registerPackMCP(register RegisterFn, cfg *config.Config, env hostenv.Env, out io.Writer, p *packinfo.Info, names []string) error {
	if len(names) == 0 {
		return nil
	}
	if err := register(cfg, env, out, names, packinfo.ServerMCP(p)); err != nil {
		fmt.Fprintf(out, "note: mcp registration: %v\n", err)
		return err
	}
	return nil
}

func RunPackLs(out io.Writer) error {
	return packLs(out)
}

func packLs(out io.Writer) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	active := packinfo.ActivePackRoot(cfg.Pack, "")
	if active == "" {
		fmt.Fprintln(out, "no active pack (create a pack.toml + skills/ by hand, then `pix pack use <path|git-url>`)")
		return nil
	}
	p, err := packinfo.LoadPack(active)
	if err != nil {
		fmt.Fprintf(out, "pack %s: %v\n", active, err)
		return nil
	}
	fmt.Fprintf(out, "active pack: %s (%s)\n", p.Manifest.Name, p.Root)
	return nil
}

func RunPackShow(env hostenv.Env, out io.Writer, rest []string) error {
	return packShow(env, out, rest)
}

func packShow(env hostenv.Env, out io.Writer, rest []string) error {
	root := packTarget(rest)
	if len(rest) == 0 {
		if cfg, err := config.Load(); err == nil && packinfo.ActivePackRoot(cfg.Pack, "") != "" {
			root = packinfo.ActivePackRoot(cfg.Pack, "")
		}
	}
	p, err := packinfo.LoadPack(root)
	if err != nil {
		return err
	}
	for _, row := range []struct{ label, value string }{
		{"pack", p.Manifest.Name},
		{"root", p.Root},
		{"skills", present(p.SkillsDir)},
		{"knowledge", present(p.KnowledgeDir) + " (inert; not indexed by any service)"},
		{"ollama", p.Manifest.OllamaBridgeModel},
		{"memory", p.Manifest.MemoryScope},
		{"capabilities", labelIf(p.CapabilitiesFile, "yes (mounts to ~/.pi/agent/capabilities.json)")},
		{"web search", labelIf(p.WebSearchFile, "yes (mounts to ~/.pi/web-search.json)")},
	} {
		if row.value != "" {
			fmt.Fprintf(out, "%-10s %s\n", row.label+":", row.value)
		}
	}
	if len(p.Manifest.Setup) > 0 {
		fmt.Fprintln(out, "setup:")
		for _, s := range p.Manifest.Setup {
			kind := "optional"
			if s.Required {
				kind = "required"
			}
			// A declarative step has no file; describe it by what it REQUIRES,
			// which is the useful fact anyway ("needs gog, a 1Password ref, and
			// a passing probe" beats a path nobody will open).
			detail := s.Path
			if s.Declarative() {
				var needs []string
				for _, r := range s.Require {
					switch r.Kind {
					case "bin":
						needs = append(needs, r.Name)
					case "op-ref":
						needs = append(needs, r.Env)
					case "probe":
						needs = append(needs, strings.Join(r.Argv, " "))
					}
				}
				detail = "needs " + strings.Join(needs, ", ")
			}
			fmt.Fprintf(out, "  - %s (%s; %s)\n", s.ID, kind, detail)
		}
	}
	if len(p.Manifest.Proxies) > 0 {
		fmt.Fprintln(out, "proxies:")
		for _, pr := range p.Manifest.Proxies {
			kind := "sandbox bin/"
			if pr.Host {
				kind = "HOST (`pix host` only, Tier-1)"
			}
			fmt.Fprintf(out, "  - %s (%s)\n", pr.Name, kind)
		}
	}
	if len(p.Manifest.Integrations) > 0 {
		fmt.Fprintln(out, "integrations:")
		for _, ig := range p.Manifest.Integrations {
			showIntegration(env, out, p, ig)
		}
	}
	return nil
}

// labelIf renders text only when the facet it describes is present.
func labelIf(present, text string) string {
	if present == "" {
		return ""
	}
	return text
}

// showIntegration renders ONE integration line: its registration mode and,
// where the secret comes from op-refs (packinfo.Manifest containers get theirs
// Docker-side), whether that ref is filled.
func showIntegration(env hostenv.Env, out io.Writer, p *packinfo.Info, ig packinfo.Integration) {
	fmt.Fprintf(out, "  - %s", ig.Name)
	if ig.MCP != "" {
		fmt.Fprintf(out, " (mcp: %s)", ig.MCP)
	}
	switch {
	case ig.Command != "":
		// Show the WHOLE argv, not just the binary: the flags are the security
		// posture (read-only, no-send), and a reader skimming `pack show`
		// should see them without opening the manifest.
		fmt.Fprintf(out, " — runs: %s", strings.Join(append([]string{ig.Command}, ig.Args...), " "))
	case ig.Manifest != "":
		fmt.Fprintf(out, " — manifest: %s (creds Docker-side)", ig.Manifest)
	case ig.Image != "":
		fmt.Fprintf(out, " — image: %s", ig.Image)
	case ig.URL != "":
		fmt.Fprintf(out, " — url: %s (OAuth host-side)", ig.URL)
	}
	switch {
	case ig.Env == "" || ig.Manifest != "":
	case secret.OpRefFilled(env, ig.Env):
		fmt.Fprintf(out, " — %s ✓", ig.Env)
	case ig.Setup != "":
		fmt.Fprintf(out, "; later: pix setup --pack %s --with %s", sys.ShellQuote(p.Root), sys.ShellQuote(ig.Setup))
	default:
		fmt.Fprintf(out, " — %s ✗ (run: pix secret set %s op://vault/item/field)", ig.Env, ig.Env)
	}
	fmt.Fprintln(out)
}

// solicitPackCredentials, on a TTY, prompts for any pack integration whose op://
// credential ref is missing and writes each accepted ref. No-op off-TTY or
// without op. The pack ships no secret — only the user's own op:// reference.
func solicitPackCredentials(env hostenv.Env, in io.Reader, out io.Writer, tty bool, p *packinfo.Info) {
	if !tty || in == nil || !secret.OpInstalled(env) {
		return
	}
	var missing []packinfo.Integration
	for _, ig := range p.Manifest.Integrations {
		if ig.Env == "" || ig.Setup != "" {
			continue
		}
		if !secret.EnvVarNameRe.MatchString(ig.Env) {
			fmt.Fprintf(out, "  (skipping integration %q: invalid env var name %q)\n", ig.Name, ig.Env)
			continue
		}
		if !secret.OpRefFilled(env, ig.Env) {
			missing = append(missing, ig)
		}
	}
	if len(missing) == 0 {
		return
	}
	fmt.Fprintf(out, "\nThis pack uses %d integration(s) needing a 1Password credential.\n", len(missing))
	sc := bufio.NewScanner(in)
	for _, ig := range missing {
		fmt.Fprintf(out, "  %s -> op:// ref for %s (Enter to skip): ", ig.Name, ig.Env)
		if !sc.Scan() {
			return
		}
		ref := secret.NormalizeOpRef(sc.Text())
		if ref == "" {
			continue
		}
		if !strings.HasPrefix(ref, "op://") {
			fmt.Fprintf(out, "    skipped %s: not an op:// ref\n", ig.Env)
			continue
		}
		if err := secret.WriteOpRefQuiet(env, ig.Env, ref); err != nil {
			fmt.Fprintf(out, "    could not save %s: %v\n", ig.Env, err)
			continue
		}
		fmt.Fprintf(out, "    saved %s\n", ig.Env)
	}
}

func RunPackUse(env hostenv.Env, out io.Writer, rest []string, register RegisterFn) error {
	return packUse(env, out, rest, register)
}

// packUse switches the active pack in ONE transaction: resolve the target
// (cloning a git URL), verify its pins, gate its host surface, apply the facet
// set to an in-memory cfg, and commit. On any failure NOTHING is committed.
// Everything after the commit is a best-effort, idempotent side effect.
func packUse(env hostenv.Env, out io.Writer, rest []string, register RegisterFn) error {
	flags, err := parsePackFlags(rest)
	if err != nil {
		return usagef("pix pack use: %v", err)
	}
	if len(flags.positionals) == 0 {
		return usagef("usage: pix pack use [--yes] <path|git-url|default>")
	}
	root, remote, commit, err := resolveUseTarget(env, out, flags.positionals[0])
	if err != nil {
		return err
	}
	p, err := packinfo.LoadPack(root)
	if err != nil {
		return err
	}
	// Re-hash every SHA-pinned [[bin]] BEFORE the gate even renders: a
	// mismatched pin is refused outright, so the BoM always shows the bytes on
	// disk.
	for _, bn := range p.Manifest.Bins {
		if verr := verifyPackBinSHA(root, bn); verr != nil {
			return fmt.Errorf("pack %s: %v", root, verr)
		}
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// The pack-supplied pack.lock is NEVER trusted for reversibility (see the
	// packLock doc); only its fail-safe adoption marker is read, and it is
	// SCRUBBED on a not-currently-active local-path adoption.
	hint := readPackLock(root)
	if cfg.Pack != root && remote == "" {
		if serr := scrubUntrustedPackLock(root); serr != nil {
			return fmt.Errorf("%v (refusing to adopt with an untrusted %s in place)", serr, PackLockName)
		}
	}
	// The Tier-1 gate runs against TRUSTED HOST STATE, never anything the pack
	// ships.
	store, err := requireTrustStore()
	if err != nil {
		return err
	}
	fingerprint, key, err := gatePackHostSurface(env, out, store, p, root, flags.yes)
	if err != nil {
		return err
	}
	// `pack use` remains a single-pack switch: multi-pack composition is an
	// explicit `pix setup --pack ... --pack ...` transaction, so applying a
	// one-pack stack is exactly right — it also reverts THIS pack's own prior
	// contribution first, without which a re-activation would claim NOTHING and
	// a field dropped from the manifest would stay live forever.
	records, removedMCP, err := applyPackStack(cfg, store, []*packinfo.Info{p})
	if err != nil {
		return err
	}
	lock := records[0].lock()
	lock.Remote, lock.Commit = remote, commit
	if lock.Remote == "" {
		// Not cloned THIS activation: keep the marker this pack already carried
		// (a local-path re-activation must not un-adopt it).
		lock.Remote, lock.Commit = adoptionMarker(store, root, hint)
	}
	if err := (packTxn{records: records, lockRoot: root, lock: lock}).commit(cfg); err != nil {
		return err
	}
	recordPackAcceptance(out, key, root, fingerprint, remote, commit)

	// --- post-commit: best-effort side effects (each already idempotent). ---

	if !env.Quiet {
		reportPackActivation(out, cfg, root, removedMCP, lock.MCP)
	}
	regErr := registerPackMCP(register, cfg, env, out, p, packinfo.McpNames(p))
	// Swap the host-exec wrappers NOW: clear the previous activation's, then
	// stage+verify+swap this pack's ACCEPTED set.
	if _, werr := refreshHostPackWrappers(quietly(out, env), cfg, false); werr != nil {
		fmt.Fprintf(out, "note: host wrappers not refreshed: %v\n", werr)
	}
	// Solicit any 1Password creds this pack's reference-only integrations need.
	solicitPackCredentials(env, os.Stdin, out, cli.IsTTY(os.Stdin), p)
	// A knowledge change is daemon-affecting: advise the running serve so the new
	// bundle is indexed — on THIS writer now, so --quiet silences the restart too.
	propagateConfig(quietly(out, env))
	// The pack IS activated and everything above is committed — that must not be
	// undone by a server that could not register. But the command must not exit
	// 0 having listed servers it did not register, so the failure is returned
	// here, last, after every side effect has run.
	return regErr
}

// propagateConfig advises a running serve that daemon-affecting config changed.
// It is a VARIABLE for the same reason provision's installLaunchd is one: the
// real implementation resolves to `launchctl kickstart -k` against the
// developer's OWN LaunchAgent, so a test that adopts a pack was restarting the
// live pix daemon on the machine running the test — repeatedly, serially, and
// blocking on each one. That is what made this package ~507s of a ~578s
// `go test ./...` locally while CI (no agent loaded, so the kickstart fails
// instantly) stayed fast, and it is why the fast gate's own documented 32.8s
// baseline stopped describing anybody's laptop.
var propagateConfig = func(out io.Writer) {
	service.PropagateConfig(service.DefaultReloader(out), out)
}

// quietly resolves the stream a best-effort side effect may narrate on.
func quietly(out io.Writer, env hostenv.Env) io.Writer {
	if env.Quiet {
		return io.Discard
	}
	return out
}

// resolveUseTarget maps the `pack use` positional to a local pack root plus its
// clone provenance. "default" is a built-in alias for the default pack root
// (NOT $PWD/default) and "personal" a deprecated alias for it; only the EXACT
// bare token matches, so a real path or URL of that name still resolves as one.
func resolveUseTarget(env hostenv.Env, out io.Writer, arg string) (root, remote, commit string, err error) {
	switch arg = strings.TrimSpace(arg); arg {
	case "default":
		arg = packinfo.DefaultPackRoot()
	case "personal":
		fmt.Fprintln(out, "pix pack use: \"personal\" is deprecated; use \"default\" instead.")
		arg = packinfo.DefaultPackRoot()
	}
	if !isPackGitURL(arg) {
		root = packinfo.ExpandUser(arg)
		if abs, aerr := filepath.Abs(root); aerr == nil {
			root = abs
		}
		return root, "", "", nil
	}
	if root, err = clonePack(env, out, arg); err != nil {
		return "", "", "", err
	}
	remote, _ = parsePackURL(arg)
	if sha, cerr := env.Run("git", "-C", root, "rev-parse", "HEAD"); cerr == nil {
		commit = strings.TrimSpace(sha)
	}
	return root, remote, commit, nil
}

// adoptionMarker resolves the fail-safe adoption marker to carry forward: HOST
// state first, the pack's own lock only as a hint (a forged one only RESTRICTS).
func adoptionMarker(store *PackTrustStore, root string, hint packLock) (remote, commit string) {
	if prov, ok := store.Adopted[packinfo.CanonicalizePackRoot(root)]; ok {
		return prov.Remote, prov.Commit
	}
	return strings.TrimSpace(hint.Remote), strings.TrimSpace(hint.Commit)
}

// reportPackActivation summarizes the swap. `registered`/`deregistered` are the
// honest words for what this actually did (added/removed the gateway's HOST
// registration + cfg.MCP) — never `attached`/`detached`, which would claim
// knowledge of a live sandbox this command never touched (see mcpLsAttachmentNote
// / doctor's attachmentCaveat for the same distinction). Only MCP names that
// STAYED out are reported as deregistered: a reactivation removes and
// immediately re-adds every still-declared entry.
func reportPackActivation(out io.Writer, cfg *config.Config, root string, removedMCP, addedMCP []string) {
	fmt.Fprintf(out, "active pack -> %s\n", root)
	var deregistered []string
	for _, m := range removedMCP {
		if !slices.Contains(cfg.MCP, m) {
			deregistered = append(deregistered, m)
		}
	}
	if len(deregistered) > 0 {
		fmt.Fprintf(out, "deregistered mcp (previous activation): %s\n", strings.Join(deregistered, ", "))
	}
	if len(addedMCP) > 0 {
		// "added to your mcp list", not "registered". This runs BEFORE
		// registration and knows only what went into the config; registration
		// reports itself, per server, and can legitimately skip one whose
		// command is not installed yet. Claiming otherwise here announced
		// success for servers that had not been registered and, when a command
		// was missing, for servers that never would be on that run.
		fmt.Fprintf(out, "added to your mcp list: %s\n", strings.Join(addedMCP, ", "))
	}
	// --mcp/--kit are create-only, so the recreate line is UNCONDITIONAL: the
	// sandbox-facet-changing case is never silently skipped.
	printPackRecreateLine(out)
}

func RunPackRm(out io.Writer, rest []string) error {
	if len(rest) > 0 {
		return usagef("pix pack rm: unexpected argument %q (rm detaches the ACTIVE pack; it takes no name)", rest[0])
	}
	detached, err := packRm(out)
	if err != nil {
		return err // a failed detach reports nothing: the ledger is untouched
	}
	reportPackDetach(out, detached)
	return nil
}

// packDetach is what `pack rm` undid; nil means there was no active pack.
type packDetach struct {
	root     string
	wrappers []string
	mcp      []string
}

// packRm detaches the active pack. The ENTIRE detach — re-reading cfg and the
// store, clearing the wrappers, reverting the contribution set, cfg.Save,
// dropping the spent activation — runs under ONE hold of the trust lock:
// deciding from a PRE-lock snapshot let a concurrent refresh install AFTER
// rm reported "detached".
func packRm(out io.Writer) (*packDetach, error) {
	var detached *packDetach
	err := withPackTrustLock(func() error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		if cfg.Pack == "" {
			return nil
		}
		// `rm` must undo the active pack's contributions, not just clear
		// cfg.Pack, or "detached" is a lie.
		store, serr := requireTrustStore()
		if serr != nil {
			return serr
		}
		d := &packDetach{root: cfg.Pack}
		// "detached" includes the host wrappers: remove exactly what HOST state
		// attributes to hostPackBinDir() (works even when the pack dir is gone),
		// FIRST — a failed clear aborts BEFORE anything detaches.
		if store.Installed != nil && len(store.Installed.Wrappers) > 0 {
			d.wrappers = append([]string(nil), store.Installed.Wrappers...)
			if cerr := clearInstalledHostPackWrappersLocked(out, store); cerr != nil {
				return fmt.Errorf("stale host wrappers could not be removed: %v — nothing detached; fix that and re-run", cerr)
			}
		}
		d.mcp = revertPackStack(cfg, store, packinfo.ActivePackRoots(cfg, ""))
		ClearPackInference(cfg, "")
		cfg.Pack, cfg.Packs = "", nil
		if err := cfg.Save(); err != nil {
			return err
		}
		detached = d
		// The ledger is spent (its contributions were just reverted). The lock
		// is HELD, so use the already-locked mutation — never nest
		// withPackTrustLock. A failed write merely over-claims (a no-op removal).
		if len(store.Activations) > 0 {
			if _, werr := mutatePackTrustStoreLocked(func(s *PackTrustStore) error {
				s.Activations = nil
				return nil
			}); werr != nil {
				fmt.Fprintf(out, "note: could not clear the activation record: %v (harmless over-claim; re-run `pack rm` once %s is writable)\n", werr, packTrustStorePath())
			}
		}
		return nil
	})
	return detached, err
}

func reportPackDetach(out io.Writer, d *packDetach) {
	if d == nil {
		fmt.Fprintln(out, "no active pack to detach")
		return
	}
	fmt.Fprintf(out, "detached active pack (%s). The files are untouched; re-attach with `pix pack use`.\n", d.root)
	if len(d.wrappers) > 0 {
		fmt.Fprintf(out, "removed host wrappers: %s\n", strings.Join(d.wrappers, ", "))
	}
	if len(d.mcp) > 0 {
		fmt.Fprintf(out, "deregistered mcp: %s\n", strings.Join(d.mcp, ", "))
		printPackRecreateLine(out)
	}
}

// --- git-URL adoption -------------------------------------------------------

// isPackGitURL classifies s as a git URL (cloneable) and additionally accepts
// the "git+" scheme prefix used by kit URLs.
func isPackGitURL(s string) bool {
	s = strings.TrimSpace(s)
	// A git transport-helper string (ext::, fd::, ...) is URL-SHAPED, not a path:
	// routing it here gets a clear "unsafe transport" rejection from safeGitURL
	// instead of a confusing "not a pack" error.
	if strings.Contains(s, "::") {
		return true
	}
	switch {
	case strings.HasPrefix(s, "git+"),
		strings.HasPrefix(s, "http://"),
		strings.HasPrefix(s, "https://"),
		strings.HasPrefix(s, "git://"),
		strings.HasPrefix(s, "ssh://"),
		strings.HasPrefix(s, "git@"):
		return true
	case strings.HasSuffix(s, ".git"):
		return true
	}
	return false
}

// parsePackURL splits an optional "#ref=<ref>" (or bare "#<ref>") pin off a git
// URL and strips a leading "git+" scheme prefix. Returns (url, ref).
func parsePackURL(raw string) (url, ref string) {
	url = strings.TrimPrefix(raw, "git+")
	if i := strings.IndexByte(url, '#'); i >= 0 {
		frag := url[i+1:]
		url = url[:i]
		ref = strings.TrimPrefix(frag, "ref=")
	}
	return url, ref
}

// packNameFromURL derives a SAFE, stable local dir name from a git URL: the
// basename (minus .git) sanitized to [A-Za-z0-9._-], plus a short hash of the
// FULL url so two remotes sharing a basename never collide on one dest.
func packNameFromURL(url string) string {
	u := strings.TrimSuffix(url, ".git")
	u = strings.TrimRight(u, "/")
	base := u
	if i := strings.LastIndexAny(u, "/:"); i >= 0 {
		base = u[i+1:]
	}
	safe := make([]rune, 0, len(base))
	for _, r := range base {
		if !packinfo.SafeArtifactRune(r) {
			r = '-'
		}
		safe = append(safe, r)
	}
	name := strings.Trim(string(safe), ".-")
	if name == "" || name == ".." {
		name = "pack"
	}
	sum := sha256.Sum256([]byte(url))
	return name + "-" + hex.EncodeToString(sum[:])[:16]
}

// clonePack clones (or updates) a remote pack into PacksDir/<name>, pinned to
// the optional ref, and returns the local path. The git remote is trusted for
// Tier-0 content; anything host-executing is gated separately at adoption.
func clonePack(env hostenv.Env, out io.Writer, raw string) (string, error) {
	url, ref := parsePackURL(raw)
	if !cli.SafeGitURL(url) {
		return "", fmt.Errorf("refusing unsafe git URL %q (only https/ssh/git remotes; no ext::/file:: transports)", url)
	}
	if ref != "" && strings.HasPrefix(ref, "-") {
		return "", fmt.Errorf("refusing ref %q (leading dash)", ref)
	}
	name := packNameFromURL(url)
	dest := filepath.Join(config.PacksDir(), name)
	if err := os.MkdirAll(config.PacksDir(), 0o755); err != nil {
		return "", err
	}
	freshClone := false
	if _, err := os.Stat(filepath.Join(dest, ".git")); err == nil {
		// A dir already exists at this URL-hash dest: verify its origin first — a
		// collision (or pre-planted dir) must never activate the wrong repo.
		if got, _ := env.Run("git", "-C", dest, "remote", "get-url", "origin"); strings.TrimSpace(got) != url {
			_ = os.RemoveAll(dest)
		} else {
			fmt.Fprintf(out, "updating pack %q...\n", name)
			if _, err := env.Run("git", "-C", dest, "fetch", "--tags", "--", "origin"); err != nil {
				return "", fmt.Errorf("git fetch %s: %w", url, err)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(dest, ".git")); err != nil {
		fmt.Fprintf(out, "cloning pack %q from %s...\n", name, url)
		if _, err := env.Run("git", "clone", "--", url, dest); err != nil {
			return "", fmt.Errorf("git clone %s: %w", url, err)
		}
		freshClone = true
	}
	if ref != "" {
		// No `--` before a ref: `git checkout -- <ref>` means path-checkout, not a
		// ref switch. ref is already validated (no leading dash), so this is safe.
		if _, err := env.Run("git", "-C", dest, "checkout", ref); err != nil {
			if freshClone {
				_ = os.RemoveAll(dest)
			}
			return "", fmt.Errorf("git checkout %s: %w", ref, err)
		}
		// Advance to the fetched tip when ref is a branch (no-op for a tag/sha).
		_, _ = env.Run("git", "-C", dest, "reset", "--hard", "origin/"+ref)
	} else if !freshClone {
		// Unpinned existing clone: advance to the remote default branch's tip.
		_, _ = env.Run("git", "-C", dest, "reset", "--hard", "@{upstream}")
	}
	// A clone that has no pack.toml is not a pack: clean up the fresh clone so a
	// retry starts clean, and fail with a clear message.
	if _, err := os.Stat(filepath.Join(dest, packinfo.PackManifestName)); err != nil {
		if freshClone {
			_ = os.RemoveAll(dest)
		}
		return "", fmt.Errorf("cloned %s but it has no %s — not a pack", url, packinfo.PackManifestName)
	}
	// pack.lock is LOCAL GENERATED state and must NEVER come from the remote.
	// Scrub AFTER every git op, BEFORE markPackAdopted; a failed scrub fails
	// the adoption.
	if err := scrubRemotePackLock(env, dest, freshClone); err != nil {
		if freshClone {
			_ = os.RemoveAll(dest)
		}
		return "", err
	}
	// Mark the clone ADOPTED durably before returning: an UNMARKED clone would be
	// treated as user-authored on retry, so an unwritable marker fails adoption.
	if err := markPackAdopted(env, dest, url); err != nil {
		if freshClone {
			_ = os.RemoveAll(dest)
		}
		return "", fmt.Errorf("recording adoption provenance for %s: %w", url, err)
	}
	return dest, nil
}

// scrubRemotePackLock deletes a pack.lock that came from the REMOTE — a
// checked-in symlink would redirect the adoption marker at an arbitrary host
// file, and a regular one could claim the user's own entries. On a fresh clone
// any lock did; on an update, one that is a symlink (never legitimate) or that
// git tracks. A legit LOCAL lock (untracked regular file) is preserved.
func scrubRemotePackLock(env hostenv.Env, dest string, freshClone bool) error {
	path := PackLockPath(dest)
	fi, err := os.Lstat(path)
	if err != nil {
		return nil // no pack.lock at all — nothing to scrub
	}
	fromRemote := freshClone || fi.Mode()&os.ModeSymlink != 0
	if !fromRemote {
		// Tracked by git => restored from the remote by checkout/reset above.
		if _, lerr := env.Run("git", "-C", dest, "ls-files", "--error-unmatch", "--", PackLockName); lerr == nil {
			fromRemote = true
		}
	}
	if !fromRemote {
		return nil
	}
	if rerr := os.Remove(path); rerr != nil && !os.IsNotExist(rerr) {
		return fmt.Errorf("removing checked-in %s: %w", PackLockName, rerr)
	}
	return nil
}

// markPackAdopted durably records adoption provenance (Remote + Commit),
// MERGING into any existing lock so a re-clone never sheds earlier attribution.
// The trust-store mirror is best-effort: the lock marker plus the PacksDir check
// keep the guard fail-safe.
func markPackAdopted(env hostenv.Env, root, remote string) error {
	lock := readPackLock(root)
	lock.Remote = remote
	lock.Commit = ""
	if sha, err := env.Run("git", "-C", root, "rev-parse", "HEAD"); err == nil {
		lock.Commit = strings.TrimSpace(sha)
	}
	_ = recordPackAdoptionInTrustStore(root, remote, lock.Commit)
	return writePackLock(root, lock)
}

// --- helpers ----------------------------------------------------------------

// WriteManifest writes root's pack.toml symlink-safe + atomically: the pack
// root is untrusted input, so a symlinked destination is REFUSED.
func WriteManifest(root string, m packinfo.Manifest) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(m); err != nil {
		return err
	}
	dest := filepath.Join(root, packinfo.PackManifestName)
	if fi, err := os.Lstat(dest); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to write through it", dest)
	}
	return sys.AtomicWriteInDir(root, packinfo.PackManifestName, buf.Bytes(), 0o644)
}

func present(p string) string {
	if p == "" {
		return "(none)"
	}
	return p
}
