// secret_sync.go: resolve provider-key op:// refs from 1Password and push the
// values into sbx secrets (the sandbox proxy's store), so the sandbox reaches
// the models exactly as before while 1Password stays the single store and
// pix owns only the REFS. No value ever lands on pix's disk or in the
// VM: `op read` resolves in-process, we hand the value straight to `sbx secret
// set`, and drop it.
//
// secrets.env is the SINGLE refs file: the sbx gateway resolves it with `op
// run --env-file` for a wrapped MCP server, and the sync below resolves it
// with `op read` for the sandbox's proxy secret store. There is no second,
// mirrored copy to drift.
//
// All op/sbx calls go through env.Run so this is unit-testable with fakes; the
// LIVE run (real 1Password + sbx) happens on the host (op is a host tool).
package secret

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"pix/host/cli"
	"pix/host/hostenv"
	"sort"
	"strings"
)

// ProviderKeyRefOrder is the deterministic MODEL-provider list for prompting
// (env var used in secrets.env -> sbx secret name).
//
// Membership here means "a provider you can route a model to": it is what
// `pix models add` offers and what setup prompts for. A key that buys a TOOL
// rather than a model does NOT belong here, or the CLI starts offering `pix
// models add parallel` for something that serves no models. Those live in
// ToolKeyRefOrder.
var ProviderKeyRefOrder = []ProviderKeyRef{
	{"ANTHROPIC_API_KEY", "anthropic"},
	{"OPENAI_API_KEY", "openai"},
	{"GEMINI_API_KEY", "google"},
}

// ToolKeyRefOrder is the same mechanism for keys that buy a CAPABILITY instead
// of a model: today, the web-search backends pi-web-access can call from inside
// the sandbox. They are seeded, checked and mirrored into the sbx secret store
// exactly like a model key (same op:// discipline, same `pix secret` verbs, the
// value never touches the VM), but they are deliberately invisible to `models
// add` and to the router, and their absence is never a launch blocker: no web
// search key means degraded search, not a dead agent.
//
// The env var names are pi-web-access's, not ours (parallel.ts reads
// PARALLEL_API_KEY and sends it as the x-api-key header). The kit must carry a
// matching credentials[] entry plus an egress allowlist entry for the API host,
// or the sentinel reaches the VM with nowhere to be swapped.
var ToolKeyRefOrder = []ProviderKeyRef{
	{"PARALLEL_API_KEY", "parallel"},
}

// ProviderKeyRef pairs a key's secrets.env variable with its short name.
type ProviderKeyRef struct{ EnvVar, Name string }

// OpReadNonEmpty resolves ref via `op read` and reports whether it succeeded
// AND produced a non-empty value — the one validation every configured
// provider-key ref must pass (a ref that exists but doesn't resolve, or
// resolves to nothing, is as broken as no ref at all). Never logs the value.
func OpReadNonEmpty(env hostenv.Env, ref string) (string, bool) {
	// Decode any %20 from refs written by the old (buggy) encoding path: `op read`
	// rejects a percent-encoded ref, so a space-containing item name (e.g.
	// "Anthropic API Key") stored as %20 would never resolve. Reading it decoded
	// resolves it now; the write path stores literal so files self-heal.
	ref = strings.ReplaceAll(ref, "%20", " ")
	val, err := env.Run("op", "read", ref)
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

// OfferOnePasswordKeys is the OPT-IN (default-No) setup step that wires model
// keys to 1Password. It fires ONLY on a TTY when `op` is installed AND no
// provider-key refs exist yet — so it never nags someone who already chose, and
// never shows where op can't be used. Accepting writes op:// refs and force-syncs
// them into sbx (overwriting the raw secrets), making 1Password the source of
// truth. Declining (the default) leaves keys exactly as they were.
func OfferOnePasswordKeys(env hostenv.Env, in io.Reader, out io.Writer, tty bool) {
	if !tty || in == nil || !OpInstalled(env) || ProviderKeyRefsPresent(env) {
		return
	}
	fmt.Fprintln(out, "")
	if !cli.ConfirmYN(in, out, "Manage model keys in 1Password (op:// refs) instead of raw sbx secrets? [y/N]: ", false) {
		return
	}
	fmt.Fprintln(out, "Paste an op:// ref per provider (op://Vault/Item/field), or Enter to skip each.")
	sc := bufio.NewScanner(in)
	wrote := false
	for _, p := range ProviderKeyRefOrder {
		fmt.Fprintf(out, "  %s: ", p.Name)
		if !sc.Scan() {
			break
		}
		ref := NormalizeOpRef(sc.Text())
		if ref == "" {
			continue
		}
		if !strings.HasPrefix(ref, "op://") {
			fmt.Fprintf(out, "    skipped %s: not an op:// ref\n", p.Name)
			continue
		}
		// Write the ref to secrets.env under ONE WithProviderRefsLock
		// acquisition (via the *Locked helper) — never nested (a nested
		// acquisition of the same lock file would deadlock against the real
		// flock). A lock-acquisition failure is reported and this provider is
		// skipped, honestly, rather than writing unlocked.
		var refErr error
		if lerr := WithProviderRefsLock(env, func() error {
			refErr = WriteOpRefQuietLocked(env, p.EnvVar, ref)
			return nil
		}); lerr != nil {
			fmt.Fprintf(out, "    could not lock provider refs for %s: %v\n", p.Name, lerr)
			continue
		}
		if refErr != nil {
			fmt.Fprintf(out, "    could not save %s: %v\n", p.Name, refErr)
			continue
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

// WriteOpRefQuiet upserts KEY=op://ref into secrets.env without the CLI
// wrapper's rejection contract, so the interactive offer can loop. It VALIDATES
// the key as a shell env var name (so a malicious pack.toml integration name
// can't inject extra secrets.env lines) and the value as a single-line op://
// ref (never a literal secret) — defense in depth beside the caller's own
// op:// check.
//
// PUBLIC (standalone) entry: takes the provider-refs transaction lock around
// the read-modify-write. Callers already inside a locked transaction (setup's
// strict flow) use WriteOpRefQuietLocked instead.
func WriteOpRefQuiet(env hostenv.Env, key, value string) error {
	return WithProviderRefsLock(env, func() error {
		return WriteOpRefQuietLocked(env, key, value)
	})
}

// WriteOpRefQuietLocked is WriteOpRefQuiet's transaction body (the validation
// + upsert of secrets.env, the single refs file). Caller MUST hold the
// provider-refs lock.
func WriteOpRefQuietLocked(env hostenv.Env, key, value string) error {
	path := DefaultOpRefsPath()
	if !EnvVarNameRe.MatchString(key) {
		return fmt.Errorf("invalid env var name %q", key)
	}
	if !strings.HasPrefix(value, "op://") || strings.ContainsAny(value, "\n\r") {
		return fmt.Errorf("value must be a single-line op:// ref")
	}
	// Store refs with LITERAL spaces. Verified on op 2.35.0: BOTH `op read` (the
	// setup strict flow) and `op run --env-file` (the MCP gateway)
	// require literal spaces and REJECT a percent-encoded ref outright ("invalid
	// character in secret reference: '%'"). An earlier version encoded spaces to
	// %20 here on a false premise — that broke every item name with a space (e.g.
	// "Anthropic API Key"). Decode any %20 we find so already-broken files
	// self-heal on the next write (e.g. setup's re-mirror).
	value = strings.ReplaceAll(value, "%20", " ")
	content := ""
	c, rerr := env.ReadFile(path)
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
	return env.WriteFile(path, []byte(upsertOpRef(content, key, value)), 0o600)
}

// providerKeyRefs maps every secrets.env ENV var pix mirrors into the sbx secret
// store to its secret name: model providers AND capability/tool keys. This is
// the SYNC set, deliberately wider than ProviderKeyRefOrder (the routing set),
// because a tool key needs the same seeding, checking and mirroring that a model
// key does. Derived from the two ordered lists so a new entry cannot be added to
// one and forgotten in the other.
var providerKeyRefs = func() map[string]string {
	m := make(map[string]string, len(ProviderKeyRefOrder)+len(ToolKeyRefOrder))
	for _, p := range ProviderKeyRefOrder {
		m[p.EnvVar] = p.Name
	}
	for _, p := range ToolKeyRefOrder {
		m[p.EnvVar] = p.Name
	}
	return m
}()

// isModelProviderKey reports whether an secrets.env key buys a MODEL (and so has
// a `pix models add` follow-up) rather than a tool capability.
func isModelProviderKey(envVar string) bool {
	for _, p := range ProviderKeyRefOrder {
		if p.EnvVar == envVar {
			return true
		}
	}
	return false
}

// firstProviderKeyRefs scans content and returns, for each provider-key op-refs
// ENV var, the FIRST valid entry it sees — matching CurrentOpRef, which treats
// a provider key as having exactly one ref: the first non-placeholder op://
// value for that env var, never whichever duplicate line happens to come last.
// A later non-placeholder ref still supersedes an earlier PLACEHOLDER (a
// placeholder never counts as configured), but once a real ref is recorded for
// a key, any further duplicate is ignored. This is the one place
// EnsureProviderKeysFromRefs and syncProviderKeys resolve "which ref wins", so
// they can never disagree with setup/CurrentOpRef.
func firstProviderKeyRefs(content string) map[string]OpRef {
	best := map[string]OpRef{}
	for _, r := range ParseOpRefs(content, nil) {
		if _, ok := providerKeyRefs[r.Key]; !ok || !r.IsRef {
			continue
		}
		cur, exists := best[r.Key]
		switch {
		case !exists:
			best[r.Key] = r
		case cur.Placeholder && !r.Placeholder:
			best[r.Key] = r
		}
	}
	return best
}

// ProviderKeyRefsPresent reports whether secrets.env declares at least one FILLED
// provider-key ref, so the keys gate can point at `pix secret sync` and the
// trusted host-state payload can report source="1password".
func ProviderKeyRefsPresent(env hostenv.Env) bool {
	_, content, exists := OpRefsContent(env)
	if !exists {
		return false
	}
	for _, r := range ParseOpRefs(content, nil) {
		if _, ok := providerKeyRefs[r.Key]; ok && r.IsRef && !r.Placeholder {
			return true
		}
	}
	return false
}

// EnsureProviderKeysFromRefs is the NO-RITUAL path: for each provider-key op://
// ref whose sbx secret is currently MISSING, resolve it from 1Password and push
// it into sbx. Because it acts only on missing keys, `op` (which may prompt) is
// touched at most once per key ever. Best-effort and Quiet: any failure just
// leaves the key unset and the keys gate guides the user. `pix secret sync` is
// the force-resync (rotate/repair) counterpart.
//
// PUBLIC entry: it acquires the provider-refs transaction lock for its whole
// read-op-refs / resolve-from-1Password / sbx-sync pass, so a concurrent
// `pix secret set`/`secret rm` in another process can never change
// secrets.env between the snapshot this reads and the sbx values it pushes.
// "Best-effort" describes how failures degrade, never proceeding without the
// lock: a lock-acquisition failure is reported and this call is a no-op.
func EnsureProviderKeysFromRefs(env hostenv.Env, out io.Writer) {
	if lerr := WithProviderRefsLock(env, func() error {
		EnsureProviderKeysFromRefsLocked(env, out)
		return nil
	}); lerr != nil {
		fmt.Fprintf(out, "pix: could not lock provider refs (%s): %v\n", ProviderRefsLockPath(env), lerr)
	}
}

// EnsureProviderKeysFromRefsLocked is EnsureProviderKeysFromRefs' transaction
// body. Caller MUST hold the provider-refs lock.
func EnsureProviderKeysFromRefsLocked(env hostenv.Env, out io.Writer) {
	_, content, exists := OpRefsContent(env)
	if !exists {
		return
	}
	// BOUNDED (probeRun): a hung `sbx secret ls` times out and aborts the sync
	// — can't tell what's set, don't guess, never touch op or `sbx secret set`.
	sbxOut, timedOut, err := env.RunTimed("sbx", "secret", "ls")
	if timedOut || err != nil {
		return // can't tell what's set; don't guess
	}
	firstRefs := firstProviderKeyRefs(content)
	type pending struct{ name, ref string }
	var todo []pending
	for _, p := range ProviderKeyRefOrder {
		r, ok := firstRefs[p.EnvVar]
		if !ok || r.Placeholder {
			continue
		}
		if cli.GrepWord(sbxOut, p.Name) {
			continue // already in sbx — no op call, no prompt
		}
		todo = append(todo, pending{p.Name, r.Value})
	}
	if len(todo) == 0 {
		return // nothing missing that we can fill; never touch op
	}
	if !OpInstalled(env) || !OpSignedIn(env) {
		return // can't resolve now; the keys gate points at `pix secret sync`
	}
	for _, p := range todo {
		val, ok := OpReadNonEmpty(env, p.ref)
		if !ok {
			continue
		}
		if _, err := setSbxSecret(env, p.name, val); err == nil {
			fmt.Fprintf(out, "pix: resolved %s from 1Password\n", p.name)
		}
	}
}

// setSbxSecret pushes one already-resolved value into sbx's secret store —
// the ONE place `sbx secret set` is invoked, so the lazy fill above and the
// force sync below can never disagree about the flags or the leak posture.
// `-f` overwrites: 1Password is the source of truth, and without it sbx
// errors on an existing secret. The value is briefly an argv element on the
// HOST; it never lands on pix's disk and never enters the VM. On failure the
// returned detail is sbx's first output line (or the Go error text) with every
// occurrence of the value redacted — an exec error can echo the full argv
// ("-t <value>") back verbatim, so the error string is as much a leak vector
// as sbx's own stdout.
func setSbxSecret(env hostenv.Env, name, val string) (detail string, err error) {
	sbxOut, err := env.Run("sbx", "secret", "set", "-f", "-g", name, "-t", val)
	if err == nil {
		return "", nil
	}
	detail = strings.TrimSpace(FirstLine(sbxOut))
	if detail == "" {
		detail = err.Error()
	}
	return RedactSecretValue(detail, val), err
}

// syncProviderKeys is the PUBLIC entry: resolve each present provider-key ref
// via `op read` and push it into sbx (setSbxSecret). It writes per-key ✓/✗ to
// out (NEVER the value) and returns counts plus a fatal error for a
// precondition failure (op/sbx unavailable) so the CLI wrapper can pick an
// exit code and setup can degrade quietly.
//
// It acquires the provider-refs transaction lock for its whole pass
// (delegating to the Locked core), so a concurrent `pix secret set`/`secret
// rm` can never change secrets.env between the snapshot this reads and the sbx
// values it pushes. A lock-acquisition failure is fatal — never unlocked.
func syncProviderKeys(env hostenv.Env, out io.Writer) (synced, failed int, fatal error) {
	lerr := WithProviderRefsLock(env, func() error {
		synced, failed, fatal = syncProviderKeysLocked(env, out)
		return nil
	})
	if lerr != nil {
		fmt.Fprintf(out, "  \u2717 could not lock provider refs (%s): %v\n", ProviderRefsLockPath(env), lerr)
		return 0, 0, fmt.Errorf("could not lock provider refs (%s): %w", ProviderRefsLockPath(env), lerr)
	}
	return synced, failed, fatal
}

// syncProviderKeysLocked is syncProviderKeys' transaction body. Caller MUST
// hold the provider-refs lock. Uses firstProviderKeyRefs so a duplicated
// env-var line resolves the SAME ref CurrentOpRef would pick.
func syncProviderKeysLocked(env hostenv.Env, out io.Writer) (synced, failed int, fatal error) {
	_, content, exists := OpRefsContent(env)
	if !exists {
		return 0, 0, fmt.Errorf("secrets.env not found (%s)", DefaultOpRefsPath())
	}
	if !OpInstalled(env) {
		return 0, 0, fmt.Errorf("op (1Password CLI) not installed")
	}
	if !OpSignedIn(env) {
		return 0, 0, fmt.Errorf("op installed but no account configured (run: op signin)")
	}
	if _, err := env.LookPath("sbx"); err != nil {
		return 0, 0, fmt.Errorf("sbx not on PATH")
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
		if r.Placeholder {
			fmt.Fprintf(out, "  \u2717 %s (%s): unfilled placeholder\n", name, envVar)
			failed++
			continue
		}
		val, err := env.Run("op", "read", r.Value)
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
		if detail, err := setSbxSecret(env, name, val); err != nil {
			fmt.Fprintf(out, "  \u2717 %s (%s): sbx secret set failed: %s\n", name, envVar, detail)
			failed++
			continue
		}
		fmt.Fprintf(out, "  \u2713 %s synced from 1Password\n", name)
		synced++
	}
	return synced, failed, nil
}

// RedactSecretValue replaces every occurrence of val in s with "***", so a
// resolved secret can never reach printed output even if it leaks back through
// an unexpected channel (sbx echoing its own argv, a Go exec error wrapping
// the command line, etc). A no-op when val is empty (never redacts to "***"
// for an empty needle, which would corrupt unrelated text).
func RedactSecretValue(s, val string) string {
	if val == "" {
		return s
	}
	return strings.ReplaceAll(s, val, "***")
}

// FirstLine returns the first non-empty line of s (sbx errors are one line;
// guards against echoing a value if sbx unexpectedly emits one).
func FirstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) != "" {
			return ln
		}
	}
	return ""
}
