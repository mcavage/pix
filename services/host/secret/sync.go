// sync.go: WHICH op:// refs pix knows about, and the interactive offer that
// writes them. The resolving half — turning a ref into a credential the
// sandbox can use — lives in scoped.go and only ever writes a SANDBOX-SCOPED
// sbx secret; there is no global push anywhere in this package any more.
//
// secrets.env is the SINGLE refs file: the sbx gateway resolves it with `op
// run --env-file` for a wrapped MCP server, and each launch resolves it with
// `op read` for that launch's own sandbox. There is no second, mirrored copy
// to drift.
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
	ParallelSearchKeyRef,
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
// never shows where op can't be used. Accepting writes op:// refs and nothing
// else: 1Password stays the store, this file stays the configuration, and no
// host-wide sbx secret is written or read. Declining (the default) leaves keys
// exactly as they were.
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
	// Saved, and that is the whole job: the refs ARE the configuration. Nothing
	// is resolved here and nothing is pushed into a host-wide store — each
	// launch resolves this PIX_HOME's refs into the ONE sandbox it is about to
	// enter (PrepareSandboxSecrets), so a rotated 1Password item takes effect on
	// the next run without a second "sync" ritual.
	fmt.Fprintln(out, "Saved. Each run resolves them into that run's own sandbox.")
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

// firstRefsIn scans content and returns, for each ENV var in a CLOSED key
// set, the FIRST valid entry it sees — matching CurrentOpRef, which treats a
// key as having exactly one ref: the first non-placeholder op:// value for
// that env var, never whichever duplicate line happens to come last. A later
// non-placeholder ref still supersedes an earlier PLACEHOLDER (a placeholder
// never counts as configured), but once a real ref is recorded for a key, any
// further duplicate is ignored.
//
// This is the ONE place every resolver decides "which ref wins" — the model
// provider set (providerKeyRefs, doctor's and the launch gate's evidence) and
// the scoped launch set (scopedKeyRefs, which adds github) both go through
// it — so a scoped write, a status report and CurrentOpRef can never disagree.
func firstRefsIn(content string, set map[string]string) map[string]OpRef {
	best := map[string]OpRef{}
	for _, r := range ParseOpRefs(content, nil) {
		if _, ok := set[r.Key]; !ok || !r.IsRef {
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

// ProviderKeyRefsPresent reports whether secrets.env declares at least one
// FILLED provider-key ref (model or tool), so the trusted host-state payload
// can report source="1password" and the setup offer knows not to nag.
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
