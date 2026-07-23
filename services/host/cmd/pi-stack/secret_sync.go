// secret_sync.go: resolve provider-key op:// refs from 1Password and push the
// values into sbx secrets (the sandbox proxy's store), so the sandbox reaches
// the models exactly as before while 1Password stays the single store and
// pi-stack owns only the REFS. No value ever lands on pi-stack's disk or in the
// VM: `op read` resolves in-process, we hand the value straight to `sbx secret
// set`, and drop it.
//
// This mirrors host mode, which already resolves op:// refs via `op run
// --env-file` (hostmode.env). The sandbox needs the extra sync step only because
// its keys ride the sbx proxy's secret store rather than pi's process env.
//
// All op/sbx calls go through env.run so this is unit-testable with fakes; the
// LIVE run (real 1Password + sbx) happens on the host (op is a host tool).
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// providerKeyRefOrder is the deterministic provider list for prompting (env var
// used in op-refs.env -> sbx secret name).
var providerKeyRefOrder = []struct{ envVar, name string }{
	{"ANTHROPIC_API_KEY", "anthropic"},
	{"OPENAI_API_KEY", "openai"},
	{"GEMINI_API_KEY", "google"},
}

// opReadNonEmpty resolves ref via `op read` and reports whether it succeeded
// AND produced a non-empty value — the one validation every configured
// provider-key ref must pass (a ref that exists but doesn't resolve, or
// resolves to nothing, is as broken as no ref at all). Never logs the value.
func opReadNonEmpty(env shellEnv, ref string) (string, bool) {
	if env.run == nil {
		return "", false
	}
	// Decode any %20 from refs written by the old (buggy) encoding path: `op read`
	// rejects a percent-encoded ref, so a space-containing item name (e.g.
	// "Anthropic API Key") stored as %20 would never resolve. Reading it decoded
	// resolves it now; the write path stores literal so files self-heal.
	ref = strings.ReplaceAll(ref, "%20", " ")
	val, err := env.run("op", "read", ref)
	if err != nil {
		return "", false
	}
	// TrimSpace, not just TrimRight("\r\n"): `op read` output can carry leading
	// or interior-edge whitespace too (a tab-indented item, a trailing space a
	// human left in the 1Password field), and a value that is ONLY whitespace
	// after trimming is exactly as broken as an empty one — it must be rejected,
	// not treated as a valid (if odd-looking) secret.
	val = strings.TrimSpace(val)
	return val, val != ""
}

// offerOnePasswordKeys is the OPT-IN (default-No) setup step that wires model
// keys to 1Password. It fires ONLY on a TTY when `op` is installed AND no
// provider-key refs exist yet — so it never nags someone who already chose, and
// never shows where op can't be used. Accepting writes op:// refs and force-syncs
// them into sbx (overwriting the raw secrets), making 1Password the source of
// truth. Declining (the default) leaves keys exactly as they were.
func offerOnePasswordKeys(env shellEnv, in io.Reader, out io.Writer, tty bool) {
	if !tty || in == nil || !opInstalled(env) || providerKeyRefsPresent(env) {
		return
	}
	fmt.Fprintln(out, "")
	if !confirmYN(in, out, "Manage model keys in 1Password (op:// refs) instead of raw sbx secrets? [y/N]: ", false) {
		return
	}
	fmt.Fprintln(out, "Paste an op:// ref per provider (op://Vault/Item/field), or Enter to skip each.")
	sc := bufio.NewScanner(in)
	wrote := false
	for _, p := range providerKeyRefOrder {
		fmt.Fprintf(out, "  %s: ", p.name)
		if !sc.Scan() {
			break
		}
		ref := normalizeOpRef(sc.Text())
		if ref == "" {
			continue
		}
		if !strings.HasPrefix(ref, "op://") {
			fmt.Fprintf(out, "    skipped %s: not an op:// ref\n", p.name)
			continue
		}
		// Write the ref to BOTH credential files: op-refs.env (sandbox: the gateway
		// + `pi-stack secret sync` resolve it into sbx) AND hostmode.env (host mode:
		// `op run --env-file` resolves it at launch). One paste wires both worlds,
		// and it is ONE transaction under ONE withProviderRefsLock acquisition (via
		// the *Locked helpers) — never two separate lock windows for the pair, and
		// never nested (a nested acquisition of the same lock file would deadlock
		// against the real flock). A lock-acquisition failure is reported and this
		// provider is skipped, honestly, rather than writing unlocked.
		var refErr, hostErr error
		if lerr := withProviderRefsLock(env, func() error {
			refErr = writeOpRefQuietLocked(env, p.envVar, ref)
			if refErr == nil {
				hostErr = writeOpRefFileQuietLocked(env, hostModeRefsPath(env), p.envVar, ref)
			}
			return nil
		}); lerr != nil {
			fmt.Fprintf(out, "    could not lock provider refs for %s: %v\n", p.name, lerr)
			continue
		}
		if refErr != nil {
			fmt.Fprintf(out, "    could not save %s: %v\n", p.name, refErr)
			continue
		}
		if hostErr != nil {
			fmt.Fprintf(out, "    (host-mode ref not saved for %s: %v)\n", p.name, hostErr)
		}
		wrote = true
	}
	if !wrote {
		fmt.Fprintln(out, "No refs entered; keys unchanged.")
		return
	}
	fmt.Fprintln(out, "Resolving from 1Password into sbx...")
	// Called only after every per-provider lock above was released — syncProviderKeys
	// takes the provider-refs lock itself for its own read/resolve/sbx-sync
	// transaction, so this must never run from inside a held lock (nesting
	// deadlocks the real flock).
	syncProviderKeys(env, out) // force-overwrite sbx from the new refs
}

// hostModeRefsPath is hostmode.env, a sibling of op-refs.env in the config dir
// (host mode resolves it via `op run --env-file`). Derived from the same env as
// op-refs.env so it stays test-injectable.
func hostModeRefsPath(env shellEnv) string {
	return filepath.Join(filepath.Dir(defaultOpRefsPath(env)), "hostmode.env")
}

// providerRefSet reports whether op-refs.env already declares a FILLED op:// ref
// for one provider env var (used by setup to skip re-prompting a provider that's
// already wired).
func providerRefSet(env shellEnv, envVar string) bool {
	_, ok := currentOpRef(env, envVar)
	return ok
}

// hostModeProviderKeys lists the provider sbx-names (anthropic/openai/google)
// that have a FILLED op:// ref in hostmode.env — i.e. the cloud models host mode
// can actually reach (it doesn't use the sandbox proxy). Sorted and DEDUPED BY
// NAME (not merely by input line): a duplicate/aliased env-var line for the
// same provider must never inflate the count past the real distinct-provider
// set, or a completeness check comparing len() against 3 could pass with only
// two providers actually wired (see hasAllProviderKeyNames, which compares the
// exact set rather than a count for exactly this reason).
//
// TRI-STATE via the returned error: hostmode.env genuinely ABSENT (ENOENT) is
// not an error — it means "no refs configured yet", the same as an empty
// file, so callers get (nil, nil). Any OTHER read error (EACCES, a symlink
// loop, a real I/O failure) is a credential-state-UNREADABLE condition and is
// returned as a non-nil error wrapping the path — callers MUST report that
// honestly ("credential state unreadable: ..."), never silently treat it as
// "local-only" or "configured", both of which would be a confident guess
// about state we could not actually read.
func hostModeProviderKeys(env shellEnv) ([]string, error) {
	path := hostModeRefsPath(env)
	if env.readFile == nil {
		return nil, nil
	}
	content, err := env.readFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var names []string
	seen := map[string]bool{}
	for _, r := range parseOpRefs(content) {
		if name, ok := providerKeyRefs[r.key]; ok && r.isRef && !r.placeholder && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// hasAllProviderKeyNames reports whether names covers the EXACT required
// provider set (anthropic/openai/google) — a set-membership check, never a
// mere len() comparison, so a duplicate/aliased entry can't fake completeness
// by padding the count while a real provider is still missing.
func hasAllProviderKeyNames(names []string) bool {
	have := map[string]bool{}
	for _, n := range names {
		have[n] = true
	}
	for _, want := range modelProviders {
		if !have[want] {
			return false
		}
	}
	return true
}

// mirrorProviderRefsToHostMode copies every FILLED provider-key op:// ref from
// op-refs.env into hostmode.env, so host mode's `op run --env-file=hostmode.env`
// has the same model keys as the sandbox even when the refs were set BEFORE this
// feature (e.g. via `pi-stack secret set`, which writes only op-refs.env, or by a
// bootstrap that resolved EXISTING refs without ever touching the offer path).
// Upserts (never truncates unrelated host-mode entries); best-effort BY
// CONTRACT (callers treat a partial mirror as re-verifiable, never fatal).
// This PUBLIC entry takes the provider-refs transaction lock around the whole
// read-op-refs -> write-hostmode pass; code already holding the lock must use
// mirrorProviderRefsToHostModeLocked instead (nested flock on the same file
// deadlocks).
func mirrorProviderRefsToHostMode(env shellEnv) {
	_ = withProviderRefsLock(env, func() error {
		mirrorProviderRefsToHostModeLocked(env)
		return nil
	})
}

// mirrorProviderRefsToHostModeLocked is the transaction body of
// mirrorProviderRefsToHostMode. Caller MUST hold the provider-refs lock.
func mirrorProviderRefsToHostModeLocked(env shellEnv) {
	_, content, exists := opRefsContent(env)
	if !exists {
		return
	}
	dst := hostModeRefsPath(env)
	seen := map[string]bool{}
	for _, r := range parseOpRefs(content) {
		if _, ok := providerKeyRefs[r.key]; !ok || !r.isRef || r.placeholder || seen[r.key] {
			continue
		}
		seen[r.key] = true
		_ = writeOpRefFileQuietLocked(env, dst, r.key, r.value)
	}
}

// writeOpRefQuiet upserts KEY=op://ref into op-refs.env without the CLI wrapper's
// os.Exit, so the interactive offer can loop. It VALIDATES the key as a shell env
// var name (so a malicious pack.toml integration name can't inject extra
// op-refs.env lines) and the value as a single-line op:// ref (never a literal
// secret) — defense in depth beside the caller's own op:// check.
//
// PUBLIC (standalone) entry: takes the provider-refs transaction lock around
// the read-modify-write. Callers already inside a locked transaction (setup's
// strict flow) use writeOpRefQuietLocked instead.
func writeOpRefQuiet(env shellEnv, key, value string) error {
	return withProviderRefsLock(env, func() error {
		return writeOpRefQuietLocked(env, key, value)
	})
}

// writeOpRefQuietLocked is writeOpRefQuiet's transaction body. Caller MUST
// hold the provider-refs lock.
func writeOpRefQuietLocked(env shellEnv, key, value string) error {
	return writeOpRefFileQuietLocked(env, defaultOpRefsPath(env), key, value)
}

// writeOpRefFileQuiet is writeOpRefQuiet targeting an EXPLICIT refs file (used to
// write both op-refs.env and hostmode.env). Same validation: env-var-name key +
// single-line op:// value, upsert preserving other lines. PUBLIC (standalone)
// entry: takes the provider-refs transaction lock; locked callers use
// writeOpRefFileQuietLocked.
func writeOpRefFileQuiet(env shellEnv, path, key, value string) error {
	return withProviderRefsLock(env, func() error {
		return writeOpRefFileQuietLocked(env, path, key, value)
	})
}

// writeOpRefFileQuietLocked is writeOpRefFileQuiet's transaction body (the
// validation + upsert). Caller MUST hold the provider-refs lock.
func writeOpRefFileQuietLocked(env shellEnv, path, key, value string) error {
	if env.writeFile == nil {
		return fmt.Errorf("no writer available")
	}
	if !envVarNameRe.MatchString(key) {
		return fmt.Errorf("invalid env var name %q", key)
	}
	if !strings.HasPrefix(value, "op://") || strings.ContainsAny(value, "\n\r") {
		return fmt.Errorf("value must be a single-line op:// ref")
	}
	// Store refs with LITERAL spaces. Verified on op 2.35.0: BOTH `op read` (the
	// setup strict flow) and `op run --env-file` (host mode + the MCP gateway)
	// require literal spaces and REJECT a percent-encoded ref outright ("invalid
	// character in secret reference: '%'"). An earlier version encoded spaces to
	// %20 here on a false premise — that broke every item name with a space (e.g.
	// "Anthropic API Key"). Decode any %20 we find so already-broken files
	// self-heal on the next write (e.g. setup's re-mirror).
	value = strings.ReplaceAll(value, "%20", " ")
	content := ""
	if env.readFile != nil {
		c, rerr := env.readFile(path)
		switch {
		case rerr == nil:
			content = c
		case os.IsNotExist(rerr):
			// absent file = empty, upsert creates it
		default:
			// A real read error (e.g. EACCES on a write-only file) must NOT be
			// treated as empty — overwriting would truncate existing refs.
			return fmt.Errorf("read %s: %w", path, rerr)
		}
	}
	return env.writeFile(path, []byte(upsertOpRef(content, key, value)), 0o600)
}

// providerKeyRefs maps each cloud model provider's op-refs.env ENV var to its
// sbx secret name. Extend here to add a provider.
var providerKeyRefs = map[string]string{
	"ANTHROPIC_API_KEY": "anthropic",
	"OPENAI_API_KEY":    "openai",
	"GEMINI_API_KEY":    "google",
}

// firstProviderKeyRefs scans content and returns, for each provider-key op-refs
// ENV var, the FIRST valid entry it sees — matching currentOpRef and
// mirrorProviderRefsToHostModeLocked, which both treat a provider key as
// having exactly one ref: the first non-placeholder op:// value for that env
// var, never whichever duplicate line happens to come last. A later
// non-placeholder ref still supersedes an earlier PLACEHOLDER for the same
// key (a placeholder never counts as configured), but once a real ref is
// recorded for a key, any further duplicate — placeholder or not — is
// ignored. This is the one place ensureProviderKeysFromRefs and
// syncProviderKeys resolve "which ref wins" for a duplicated env var, so
// they can never disagree with setup/currentOpRef/mirror about which
// provider key is actually configured.
func firstProviderKeyRefs(content string) map[string]opRef {
	best := map[string]opRef{}
	for _, r := range parseOpRefs(content) {
		if _, ok := providerKeyRefs[r.key]; !ok || !r.isRef {
			continue
		}
		cur, exists := best[r.key]
		switch {
		case !exists:
			best[r.key] = r
		case cur.placeholder && !r.placeholder:
			best[r.key] = r
		}
	}
	return best
}

// providerKeyRefsPresent reports whether op-refs.env declares at least one FILLED
// provider-key ref, so the keys gate can point at `pi-stack secret sync` and the
// trusted host-state payload can report source="1password".
func providerKeyRefsPresent(env shellEnv) bool {
	_, content, exists := opRefsContent(env)
	if !exists {
		return false
	}
	for _, r := range parseOpRefs(content) {
		if _, ok := providerKeyRefs[r.key]; ok && r.isRef && !r.placeholder {
			return true
		}
	}
	return false
}

// ensureProviderKeysFromRefs is the NO-RITUAL path: for each provider-key op://
// ref whose sbx secret is currently MISSING, resolve it from 1Password and push
// it into sbx. Because it acts only on missing keys, `op` (which may prompt) is
// touched at most once per key ever — once sbx has the secret, later launches
// skip op entirely. Best-effort and quiet: any failure just leaves the key unset
// and the keys gate guides the user. Called from `run` (bootstrapProviderKeys)
// and `task new`; the explicit `pi-stack secret sync` is the force-resync
// (rotate/repair) counterpart, and `setup` uses setupProvisionKeys (which
// force-syncs via syncProviderKeys).
//
// PUBLIC entry: it acquires the provider-refs transaction lock for its whole
// read-op-refs / resolve-from-1Password / sbx-sync pass, so a concurrent
// `pi-stack secret set`/`secret rm` in another process can never change
// op-refs.env between the snapshot this reads and the sbx values it pushes.
// "Best-effort" describes how failures degrade (quietly, leaving the key
// unset for the keys gate to explain) — it never means proceeding without the
// lock: a lock-acquisition failure is reported and this call is a no-op for
// the run.
func ensureProviderKeysFromRefs(env shellEnv, out io.Writer) {
	if lerr := withProviderRefsLock(env, func() error {
		ensureProviderKeysFromRefsLocked(env, out)
		return nil
	}); lerr != nil {
		fmt.Fprintf(out, "pi-stack: could not lock provider refs (%s): %v\n", providerRefsLockPath(env), lerr)
	}
}

// ensureProviderKeysFromRefsLocked is ensureProviderKeysFromRefs' transaction
// body. Caller MUST hold the provider-refs lock. Uses firstProviderKeyRefs so
// a duplicated env-var line resolves the SAME ref setup/currentOpRef/mirror
// would pick, never whichever duplicate happens to come last in the file.
func ensureProviderKeysFromRefsLocked(env shellEnv, out io.Writer) {
	if env.run == nil {
		return
	}
	_, content, exists := opRefsContent(env)
	if !exists {
		return
	}
	sbxOut, err := env.run("sbx", "secret", "ls")
	if err != nil {
		return // can't tell what's set; don't guess
	}
	firstRefs := firstProviderKeyRefs(content)
	type pk struct{ name, ref string }
	var todo []pk
	for _, p := range providerKeyRefOrder {
		r, ok := firstRefs[p.envVar]
		if !ok || r.placeholder {
			continue
		}
		if grepWord(sbxOut, p.name) {
			continue // already in sbx — no op call, no prompt
		}
		todo = append(todo, pk{p.name, r.value})
	}
	if len(todo) == 0 {
		return // nothing missing that we can fill; never touch op
	}
	if !opInstalled(env) || !opSignedIn(env) {
		return // can't resolve now; the keys gate points at `pi-stack secret sync`
	}
	for _, p := range todo {
		val, ok := opReadNonEmpty(env, p.ref)
		if !ok {
			continue
		}
		// -f: 1Password is the source of truth, so overwrite whatever sbx has
		// (sbx errors on an existing secret without it).
		if _, err := env.run("sbx", "secret", "set", "-f", "-g", p.name, "-t", val); err == nil {
			fmt.Fprintf(out, "pi-stack: resolved %s from 1Password\n", p.name)
		}
	}
}

// reconcileProviderKeysWithSbx is setupProvisionKeys' STEP 3: bring sbx to the
// same state as each provider's VALIDATED ref — the refs snapshot STEP 1
// built (envVar -> ref, every entry already `op read`-validated and
// canonical-written to BOTH refs files under the provider-refs lock the
// caller still holds), never a fresh reread of the files (a reread could see
// a different ref than the one that was validated; the snapshot keeps
// validation and reconciliation working on the same state) — using the
// launcher-owned synced-ref record
// (syncedrefs.go) — never sbx's own value, which is WRITE-ONLY (`sbx secret
// ls` lists names only, so "did the value change" can't be read back):
//
//   - sbx MISSING the key: op read (or the cached STEP-1 value) + `sbx secret
//     set -f` + record. No ask. A failure here is a hard failure — sbx ends up
//     with NO key for that provider, so the caller must treat this as false.
//   - sbx HAS the key and the recorded state is KNOWN-SAME — the recorded ref
//     equals the current ref AND the recorded digest equals sha256Hex of the
//     current resolved value (syncedRefKnownSame): NO OP — skip `sbx secret
//     set` (op read has already happened once, in setupProvisionKeys' STEP 1,
//     whose already-resolved value is what's compared here via resolved[]).
//   - sbx HAS the key but the state is NOT known-same — a genuinely new/changed
//     ref, OR the same ref whose resolved value has rotated in place, OR a
//     legacy record with no digest at all (predates this feature, so it can't
//     prove sameness): these are all treated the same way, batched into ONE
//     prompt for the whole run, not one prompt per provider — 1Password is the source of truth, so the default
//     answer is YES (replace), not the old default-No. Accepting overwrites
//     every changed provider in the batch; DECLINING is a real failure now
//     (never a quiet "sbx already has a key, so it's fine"): setup must never
//     report success while sbx and host mode would be sourcing a provider's
//     key from two different places. Non-interactive requires --yes to
//     replace; without it, reconcile fails with the exact rerun command.
//
// refs carries the STEP-1 validated ref snapshot (envVar -> op:// ref);
// resolved carries the same validation's already-resolved op-read values
// (envVar -> value) so a provider whose sbx key is genuinely missing doesn't
// pay for a second `op read` of the same ref.
//
// sbx being entirely ABSENT (not installed here) is portability, not failure:
// reconcile fails OPEN. sbx being installed but `sbx secret ls` erroring is a
// real, diagnosable problem: reconcile fails CLOSED (see sbxSecretsProbeState
// in bootstrap.go, shared with the final probe in setupProvisionKeys).
func reconcileProviderKeysWithSbx(env shellEnv, sc *bufio.Scanner, out io.Writer, interactive, assumeYes bool, refs, resolved map[string]string) bool {
	if !opInstalled(env) || !opSignedIn(env) {
		// setupProvisionKeys already enforces these as HARD preconditions before
		// ever calling reconcile; treat an unexpected miss defensively as failure
		// rather than silently doing nothing.
		return false
	}
	sbxOut, state := probeSbxSecrets(env)
	switch state {
	case sbxSecretsAbsent:
		return true // no sbx to reconcile against — not a failure (portability)
	case sbxSecretsError:
		fmt.Fprintln(out, "  \u2717 could not verify sbx's provider keys (`sbx secret ls` failed) — check sbx and re-run the same setup command")
		return false
	}

	ok := true
	type changedKey struct{ envVar, name, ref string }
	var toConfirm []changedKey
	for _, p := range providerKeyRefOrder {
		ref, hasRef := refs[p.envVar]
		if !hasRef || ref == "" {
			continue // required earlier as a hard precondition; defensive no-op here
		}
		if !grepWord(sbxOut, p.name) {
			if !syncProviderKeyToSbx(env, out, p, ref, resolved[p.envVar]) {
				ok = false
			}
			continue
		}
		if syncedRefKnownSame(p.envVar, ref, resolved[p.envVar]) {
			continue // ref AND digest both match — no op read, no sbx set
		}
		toConfirm = append(toConfirm, changedKey{p.envVar, p.name, ref})
	}

	if len(toConfirm) == 0 {
		return ok
	}

	names := make([]string, len(toConfirm))
	for i, c := range toConfirm {
		names[i] = c.name
	}
	replace := assumeYes
	if interactive {
		fmt.Fprintf(out, "  sbx already has a value for: %s\n", strings.Join(names, ", "))
		fmt.Fprint(out, "  Replace these sbx values from 1Password so sandbox and host mode use the same source? [Y/n]: ")
		line, gotAnswer := scanYN(sc)
		if !gotAnswer {
			fmt.Fprintln(out, "  no answer read (EOF) — that is not consent; re-run and answer y or n.")
			return false
		}
		replace = true // default YES: 1Password is the source of truth
		if line != "" {
			replace = line == "y" || line == "yes"
		}
	}
	if !replace {
		fmt.Fprintf(out, "  kept sbx's existing value for: %s\n", strings.Join(names, ", "))
		fmt.Fprintln(out, "  setup incomplete; sbx and host mode would use different sources.")
		if !interactive {
			fmt.Fprintln(out, "  re-run this command with --yes to replace them from 1Password")
		}
		return false
	}
	for _, c := range toConfirm {
		p := struct{ envVar, name string }{c.envVar, c.name}
		if !syncProviderKeyToSbx(env, out, p, c.ref, resolved[c.envVar]) {
			ok = false
		}
	}
	return ok
}

// syncProviderKeyToSbx pushes ref's value into sbx with `-f` (overwrite — the
// caller already decided this key should change) and records the ref as
// synced. It reuses cachedValue (STEP 1's validation op-read) when non-empty
// instead of reading again; otherwise it resolves ref itself. Never prints the
// resolved value. Returns whether sbx now genuinely holds the key.
func syncProviderKeyToSbx(env shellEnv, out io.Writer, p struct{ envVar, name string }, ref, cachedValue string) bool {
	val := cachedValue
	if val == "" {
		v, ok := opReadNonEmpty(env, ref)
		if !ok {
			fmt.Fprintf(out, "  \u2717 %s: op read failed or resolved empty\n", p.name)
			return false
		}
		val = v
	}
	if sbxOut, err := env.run("sbx", "secret", "set", "-f", "-g", p.name, "-t", val); err != nil {
		detail := strings.TrimSpace(firstLine(sbxOut))
		if detail == "" {
			detail = err.Error()
		}
		// Redact the resolved value from BOTH sbx's own output AND the Go error
		// text before printing either — exec errors can echo the full argv
		// ("-t <value>") back verbatim, so `err.Error()` is just as much a leak
		// vector as sbx's stdout/stderr. val is never empty here (checked above),
		// so this can't accidentally no-op the redaction.
		detail = redactSecretValue(detail, val)
		fmt.Fprintf(out, "  \u2717 %s: sbx secret set failed: %s\n", p.name, detail)
		return false
	}
	// Record ref + the resolved value's digest ATOMICALLY, and only AFTER sbx
	// has genuinely accepted the value above — never before, and never split
	// into two writes (a ref recorded without its digest would be indistinguishable
	// from a legacy record, defeating the whole point of adding the digest).
	if err := recordSyncedRefWithDigest(p.envVar, ref, secretDigestHex(val)); err != nil {
		fmt.Fprintf(out, "  \u2713 %s synced (record not saved: %v)\n", p.name, err)
		return true // sbx itself has the key; only our bookkeeping record failed
	}
	fmt.Fprintf(out, "  \u2713 %s synced from 1Password\n", p.name)
	return true
}

// scanYN reads one line from sc as a yes/no PROMPT ANSWER, sharing the
// caller's bufio.Scanner instead of reading the underlying io.Reader directly
// (mixing fmt.Fscanln with a bufio.Scanner on the same reader can desync,
// since the scanner buffers ahead). It returns the trimmed, lowercased line
// text and ok=true when a line was actually read. ok is false when the scan
// FAILED — EOF (no input available) or a genuine Scanner error (including an
// oversized token past bufio.Scanner's default buffer) — and that is NEVER
// treated as consent: a caller seeing ok=false must fail the prompt/reconcile
// outright with a clear message, not silently apply its default answer (the
// bug this replaces: the old bool-returning scanYN collapsed "no input at
// all" and "blank line" into the same default-value branch, so a broken/EOF'd
// stdin during the reconcile-overwrite confirm was silently read as an
// affirmative "yes, replace my sbx secrets"). A blank (Enter-only) line is
// legitimate and returns ("", true); the CALLER applies its own default for
// that case, since different prompts default differently.
func scanYN(sc *bufio.Scanner) (line string, ok bool) {
	if !sc.Scan() {
		return "", false
	}
	return strings.ToLower(strings.TrimSpace(sc.Text())), true
}

// syncProviderKeys is the PUBLIC entry: resolve each present provider-key ref
// via `op read` and push it into sbx with `sbx secret set -f -g <name> -t <value>`
// (-f overwrites: 1Password is the source of truth).
// It writes per-key ✓/✗ to out (NEVER the value) and returns counts plus a fatal
// error for a precondition failure (op/sbx unavailable) so the CLI wrapper can
// pick an exit code and setup can degrade quietly.
//
// It acquires the provider-refs transaction lock for its whole
// read-op-refs / resolve-from-1Password / sbx-sync pass (delegating to the
// Locked core), so a concurrent `pi-stack secret set`/`secret rm` can never
// change op-refs.env between the snapshot this reads and the sbx values it
// pushes. A lock-acquisition failure is reported and returned as fatal —
// sync never proceeds unlocked.
func syncProviderKeys(env shellEnv, out io.Writer) (synced, failed int, fatal error) {
	lerr := withProviderRefsLock(env, func() error {
		synced, failed, fatal = syncProviderKeysLocked(env, out)
		return nil
	})
	if lerr != nil {
		fmt.Fprintf(out, "  \u2717 could not lock provider refs (%s): %v\n", providerRefsLockPath(env), lerr)
		return 0, 0, fmt.Errorf("could not lock provider refs (%s): %w", providerRefsLockPath(env), lerr)
	}
	return synced, failed, fatal
}

// syncProviderKeysLocked is syncProviderKeys' transaction body. Caller MUST
// hold the provider-refs lock. Uses firstProviderKeyRefs so a duplicated
// env-var line resolves the SAME ref setup/currentOpRef/mirror would pick,
// never whichever duplicate happens to come last in the file (a naive
// map[key]=r overwrite would silently take the LAST line instead).
func syncProviderKeysLocked(env shellEnv, out io.Writer) (synced, failed int, fatal error) {
	_, content, exists := opRefsContent(env)
	if !exists {
		return 0, 0, fmt.Errorf("op-refs.env not found (%s)", defaultOpRefsPath(env))
	}
	if !opInstalled(env) {
		return 0, 0, fmt.Errorf("op (1Password CLI) not installed")
	}
	if !opSignedIn(env) {
		return 0, 0, fmt.Errorf("op installed but no account configured (run: op signin)")
	}
	if env.lookPath != nil {
		if _, err := env.lookPath("sbx"); err != nil {
			return 0, 0, fmt.Errorf("sbx not on PATH")
		}
	}

	present := firstProviderKeyRefs(content)
	if len(present) == 0 {
		return 0, 0, nil // nothing to do (not fatal)
	}
	envVars := make([]string, 0, len(present))
	for k := range present {
		envVars = append(envVars, k)
	}
	sort.Strings(envVars) // deterministic output

	for _, envVar := range envVars {
		r := present[envVar]
		name := providerKeyRefs[envVar]
		if r.placeholder {
			fmt.Fprintf(out, "  \u2717 %s (%s): unfilled placeholder\n", name, envVar)
			failed++
			continue
		}
		val, err := env.run("op", "read", r.value)
		if err != nil {
			fmt.Fprintf(out, "  \u2717 %s (%s): op read failed\n", name, envVar)
			failed++
			continue
		}
		val = strings.TrimSpace(val)
		if val == "" {
			fmt.Fprintf(out, "  \u2717 %s (%s): resolved empty\n", name, envVar)
			failed++
			continue
		}
		// `sbx secret set -f -g <name> -t <value>` is sbx's own documented interface.
		// -f overwrites an existing secret: 1Password is the source of truth, so a
		// re-sync must replace the sbx copy (without it sbx errors "secret already
		// exists"). The value is briefly an argv element on the HOST; it is never
		// written to pi-stack's disk and never enters the VM.
		if sbxOut, err := env.run("sbx", "secret", "set", "-f", "-g", name, "-t", val); err != nil {
			detail := strings.TrimSpace(firstLine(sbxOut))
			if detail == "" {
				detail = err.Error()
			}
			// Redact the resolved value from BOTH sbx's own output AND the wrapping
			// Go error text before printing — an exec error can echo the full argv
			// ("-t <value>") back verbatim just as readily as sbx's own stdout/stderr,
			// so `err.Error()` is just as much a leak vector (mirrors
			// syncProviderKeyToSbx's redaction; val is never empty here, checked above).
			detail = redactSecretValue(detail, val)
			fmt.Fprintf(out, "  \u2717 %s (%s): sbx secret set failed: %s\n", name, envVar, detail)
			failed++
			continue
		}
		fmt.Fprintf(out, "  \u2713 %s synced from 1Password\n", name)
		synced++
	}
	return synced, failed, nil
}

// redactSecretValue replaces every occurrence of val in s with "***", so a
// resolved secret can never reach printed output even if it leaks back through
// an unexpected channel (sbx echoing its own argv, a Go exec error wrapping
// the command line, etc). A no-op when val is empty (never redacts to "***"
// for an empty needle, which would corrupt unrelated text).
func redactSecretValue(s, val string) string {
	if val == "" {
		return s
	}
	return strings.ReplaceAll(s, val, "***")
}

// firstLine returns the first non-empty line of s (sbx errors are one line;
// guards against echoing a value if sbx unexpectedly emits one).
func firstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) != "" {
			return ln
		}
	}
	return ""
}

// runSecretSync is the `pi-stack secret sync` entry: resolve provider-key op://
// refs -> sbx secrets, with exit codes for scripting.
func runSecretSync(env shellEnv, out io.Writer) {
	synced, failed, fatal := syncProviderKeys(env, out)
	if fatal != nil {
		fmt.Fprintf(out, "pi-stack secret sync: %v\n", fatal)
		fmt.Fprintln(out, "Add provider keys with: pi-stack secret set ANTHROPIC_API_KEY op://vault/item/field")
		os.Exit(3)
	}
	if synced == 0 && failed == 0 {
		fmt.Fprintln(out, "no provider-key refs in op-refs.env (add e.g. ANTHROPIC_API_KEY=op://vault/item/field)")
		return
	}
	if failed > 0 {
		fmt.Fprintf(out, "%d synced, %d failed.\n", synced, failed)
		os.Exit(1)
	}
	fmt.Fprintf(out, "%d provider key(s) synced from 1Password into sbx.\n", synced)
}
