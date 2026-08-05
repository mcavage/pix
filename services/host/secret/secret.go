package secret

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/hostenv"
)

// secret is the CRUD home of the 1Password / op-refs.env concept. It never
// writes a resolved secret to disk (values live in 1Password); it only reads,
// classifies, and edits the REFS themselves (op://vault/item/field lines). See
// the mental model in config.OpRefsMentalModel.

// EnvVarNameRe validates a `secret set`/`secret rm` KEY looks like a shell env
// var name, so a typo'd flag or a pasted ref never lands as the key half of a
// line.
var EnvVarNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// runSecretCmd stayed in cmd/pix with its kong command tree: choosing to build
// a Deps and dispatch is composition, not a secret concern.

// NormalizeOpRef cleans a pasted op:// reference: trims whitespace and strips ONE
// layer of matching surrounding quotes. 1Password's "Copy Secret Reference" hands
// you the ref WITH double quotes ("op://Vault/Item/field"), which would otherwise
// fail the op:// prefix check. Applied at every paste boundary.
func NormalizeOpRef(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			s = strings.TrimSpace(s[1 : len(s)-1])
		}
	}
	return s
}

// OpRef is one parsed KEY=VALUE line of op-refs.env.
type OpRef struct {
	Key         string
	Value       string
	IsRef       bool // value starts with op://
	Placeholder bool // value still carries an unfilled <...> placeholder
	NonSecret   bool // KEY is on the documented non-secret allowlist
}

// ParseOpRefs parses op-refs.env content into its non-comment KEY=VALUE lines,
// classifying each value as a filled op:// ref, an unfilled placeholder, or a
// documented non-secret literal. It NEVER surfaces the raw value to callers that
// print — classification only.
func ParseOpRefs(content string) []OpRef {
	var refs []OpRef
	for _, ln := range strings.Split(content, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		eq := strings.IndexByte(t, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(t[:eq])
		val := strings.TrimSpace(t[eq+1:])
		refs = append(refs, OpRef{
			Key:         key,
			Value:       val,
			IsRef:       strings.HasPrefix(val, "op://"),
			Placeholder: HasPlaceholder(val),
			NonSecret:   config.NonSecretOpRefsKeys[key],
		})
	}
	return refs
}

// HasPlaceholder reports whether a value still carries an unfilled template
// placeholder (an angle-bracketed token like <vault> / <item> / <field>).
func HasPlaceholder(val string) bool {
	i := strings.IndexByte(val, '<')
	if i < 0 {
		return false
	}
	return strings.IndexByte(val[i:], '>') > 0
}

// OpInstalled reports whether the 1Password CLI (op) is on PATH.
func OpInstalled(env hostenv.Env) bool {

	_, err := env.LookPath("op")
	return err == nil
}

// OpSignedIn reports whether op has an account CONFIGURED, using ONLY safe
// metadata (`op account list`) — never `op read` or an on-disk `op signin`. A
// non-empty account list proves an account is configured, NOT that the session
// is unlocked/usable. Best-effort: any error is "no account configured", never a
// crash.
func OpSignedIn(env hostenv.Env) bool {
	if !OpInstalled(env) {
		return false
	}
	out, err := env.Run("op", "account", "list")
	return err == nil && strings.TrimSpace(out) != ""
}

// OpRefsContent resolves + reads op-refs.env through the injected env, returning
// its path, contents, and whether it exists.
func OpRefsContent(env hostenv.Env) (path, content string, exists bool) {
	path = DefaultOpRefsPath(env)

	c, err := env.ReadFile(path)
	if err != nil {
		return path, "", false
	}
	return path, c, true
}

// RunSecretLs prints op install/sign-in state + op-refs.env presence and, per
// configured ref, filled-vs-placeholder-vs-pasted-secret. It NEVER prints a
// secret value. This is the default `secret` action (no subcommand).
func RunSecretLs(env hostenv.Env, out io.Writer) {
	fmt.Fprintln(out, "Secrets (1Password):")
	fmt.Fprintln(out, indent(config.OpRefsMentalModel))
	fmt.Fprintln(out)

	switch {
	case !OpInstalled(env):
		fmt.Fprintln(out, "  op (1Password CLI): ✗ not installed — https://developer.1password.com/docs/cli")
	case !OpSignedIn(env):
		fmt.Fprintln(out, "  op (1Password CLI): · installed, no account configured — run: op signin")
	default:
		fmt.Fprintln(out, "  op (1Password CLI): ✓ installed + account configured (advisory)")
	}

	path, content, exists := OpRefsContent(env)
	if !exists {
		fmt.Fprintf(out, "  op-refs.env: ✗ not present — create it with: pix secret set <ENV_VAR> op://vault/item/field\n  (%s)\n", path)
		return
	}
	fmt.Fprintf(out, "  op-refs.env: ✓ %s\n", path)
	refs := ParseOpRefs(content)
	if len(refs) == 0 {
		fmt.Fprintln(out, "  refs: (none set yet — add ENV_VAR=op://vault/item/field lines)")
		return
	}
	for _, r := range refs {
		switch {
		case r.NonSecret:
			fmt.Fprintf(out, "    · %s (non-secret env)\n", r.Key)
		case r.IsRef && r.Placeholder:
			fmt.Fprintf(out, "    ✗ %s = placeholder (fill in the op:// ref)\n", r.Key)
		case r.IsRef:
			fmt.Fprintf(out, "    ✓ %s = op:// ref\n", r.Key)
		case r.Placeholder:
			fmt.Fprintf(out, "    ✗ %s = placeholder (fill in the op:// ref)\n", r.Key)
		case config.LooksSecretShaped(r.Key, r.Value):
			// A non-ref that looks like a pasted secret: flag WITHOUT printing the value.
			fmt.Fprintf(out, "    ✗ %s = possible pasted secret — replace with op://vault/item/field\n", r.Key)
		default:
			// Any other non-ref, non-allowlisted value: refs-only policy => flag it
			// WITHOUT printing the value.
			fmt.Fprintf(out, "    ✗ %s = not an op:// ref — this file is refs-only; use op://vault/item/field or move it to the non-secret allowlist\n", r.Key)
		}
	}
}

// RunSecretSet is the ONE authoring primitive: it upserts ENV_VAR=value into
// op-refs.env, no editor involved. It enforces the refs-only policy — value
// must be an op:// ref unless ENV_VAR is on config.NonSecretOpRefsKeys — and
// normalizes any %20 in a ref to a literal space (op read/op run --env-file
// both require a literal space and reject a percent-encoded one) so a spaced
// 1Password field name, e.g. "api key", is stored the way op actually parses
// it. It seeds the file (via the
// ONE seeder, config.SeedOpRefs) if absent, so the header/mental-model comment
// is always present, then upserts preserving every other line untouched. It
// prints the REF it stored (never a resolved secret — a ref is safe to echo).
//
// CLI-ARGUMENT validation failures (a bad env-var name, a control character, a
// non-ref value for a secret key) still call os.Exit(2) directly, exactly as
// before: they are immediate, unrecoverable rejections of the invocation
// itself, and existing subprocess tests depend on the process actually
// exiting from within this call. Everything AFTER argument validation — the
// read/seed/upsert of op-refs.env plus the provider-key mirror into
// hostmode.env — is one transaction under the provider-refs lock
// (WithProviderRefsLock), so a concurrent `secret set`/`secret rm`/setup in
// another process can never interleave between the two file writes. File
// failures inside the transaction return errors (never os.Exit — the lock's
// deferred release must run); runSecretCmd turns any non-nil error into a
// nonzero exit. The one failure mode that must NOT exit silently-successful
// is a provider key's hostmode.env MIRROR failing after op-refs.env was
// already written — the op-refs.env write genuinely succeeded (the sandbox
// is wired), but the CLI as a whole must still report failure.
func RunSecretSet(env hostenv.Env, out io.Writer, key, value string) error {
	if !EnvVarNameRe.MatchString(key) {
		fmt.Fprintf(out, "pix secret set: %q does not look like an env var name (want %s)\n", key, EnvVarNameRe.String())
		os.Exit(2)
	}
	// 1Password's "Copy Secret Reference" wraps the ref in quotes; strip them so
	// a pasted `"op://…"` is accepted (only for an op:// ref — a genuine literal
	// value keeps its quotes and still trips the refs-only guard below).
	if nv := NormalizeOpRef(value); strings.HasPrefix(nv, "op://") {
		value = nv
	}

	// Reject control characters (newline, carriage return, NUL, ...) in the value.
	// op-refs.env is line-oriented and consumed by `op run --env-file`, so a value
	// carrying a newline could inject a SECOND, attacker-controlled KEY=value line
	// (e.g. a pasted plaintext secret) into the file. One ref = one clean line.
	if i := strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }); i >= 0 {
		fmt.Fprintf(out, "pix secret set: %s value contains a control character at byte %d; op-refs.env is one ref per line, so newlines/control chars are not allowed\n", key, i)
		os.Exit(2)
	}

	isRef := strings.HasPrefix(value, "op://")
	if !isRef && !config.NonSecretOpRefsKeys[key] {
		if config.LooksSecretShaped(key, value) {
			fmt.Fprintf(out, "pix secret set: %s looks like a pasted secret — this file is refs-only; pass op://vault/item/field, or your secret would land on disk\n", key)
		} else {
			fmt.Fprintf(out, "pix secret set: %s is not an op:// ref — this file is refs-only; pass op://vault/item/field, or your secret would land on disk\n", key)
		}
		os.Exit(2)
	}

	// Store refs with LITERAL spaces (an item/field name like "Anthropic API Key"
	// is common): op 2.35.0's `op read` AND `op run --env-file` both require a
	// literal space and reject %20. The write chokepoint normalizes any stray %20
	// back to a space, so we pass the value through untouched here.

	// Arguments are valid; the rest is the both-file transaction. A lock
	// acquisition failure fails the command honestly — never write unlocked.
	var txErr error
	if lerr := WithProviderRefsLock(env, func() error {
		txErr = RunSecretSetLocked(env, out, key, value)
		return nil
	}); lerr != nil {
		fmt.Fprintf(out, "pix secret set: could not lock provider refs (%s): %v\n", ProviderRefsLockPath(env), lerr)
		return lerr
	}
	return txErr
}

// RunSecretSetLocked is RunSecretSet's file transaction (read/seed/upsert
// op-refs.env + the provider-key hostmode.env mirror). Caller MUST hold the
// provider-refs lock; every failure returns an error (never os.Exit) so the
// lock is always released.
func RunSecretSetLocked(env hostenv.Env, out io.Writer, key, value string) error {
	// Normalize %20 to a literal space BEFORE writing op-refs.env: op 2.35.0's
	// `op read` AND `op run --env-file` both require a literal space in a ref
	// (a spaced 1Password field name, e.g. "Anthropic API Key") and reject a
	// percent-encoded one outright. WriteOpRefFileQuietLocked (the hostmode.env
	// mirror below, and the standalone entry point) already self-heals %20 on
	// write; without this normalization here, a %20 value would land literally
	// in op-refs.env while the mirror decoded it, leaving the two files
	// permanently out of sync for the very key that matters most.
	value = strings.ReplaceAll(value, "%20", " ")
	path := DefaultOpRefsPath(env)
	content := ""
	exists := false
	c, err := env.ReadFile(path)
	switch {
	case err == nil:
		content, exists = c, true
	case os.IsNotExist(err):
		// A missing refs file is seeded below.
	default:
		fmt.Fprintf(out, "pix secret set: could not read %s: %v\n", path, err)
		return fmt.Errorf("read %s: %w", path, err)
	}
	if !exists {
		seededPath, _, err := config.SeedOpRefs()
		if err != nil {
			fmt.Fprintf(out, "pix secret set: could not seed %s: %v\n", seededPath, err)
			return fmt.Errorf("seed %s: %w", seededPath, err)
		}
		path = seededPath
		content = config.OpRefsTemplate
		if c, err := env.ReadFile(path); err == nil {
			content = c
		}
	}
	content, _ = repairLegacyOpRefsTemplate(content)

	newContent := upsertOpRef(content, key, value)

	if err := env.WriteFile(path, []byte(newContent), 0o600); err != nil {
		fmt.Fprintf(out, "pix secret set: could not write %s: %v\n", path, err)
		return fmt.Errorf("write %s: %w", path, err)
	}

	// One of the three model-provider keys ALSO needs to land in hostmode.env
	// (`pix host` resolves cloud keys from THAT file, never op-refs.env) —
	// otherwise "run `pix secret set` three times" would wire the sandbox
	// but silently leave host mode local/Ollama-only until the next full
	// `pix setup`. Mirror it here so a single `secret set` per provider is
	// really enough, matching what the command's own guidance elsewhere
	// promises. The op-refs.env write above already landed (the sandbox is
	// wired regardless), so a failed mirror is reported as a partial, actionable
	// shortfall — never silently dropped, and never claimed as done alongside a
	// success line that would contradict it.
	if _, isProviderKey := providerKeyRefs[key]; isProviderKey {
		if err := WriteOpRefFileQuietLocked(env, HostModeRefsPath(env), key, value); err != nil {
			fmt.Fprintf(out, "set %s = %s in %s, but could not mirror it to %s: %v — host mode won't see this key until you fix that (or re-run `pix setup`)\n", key, value, path, HostModeRefsPath(env), err)
			return fmt.Errorf("mirror %s to hostmode.env: %w", key, err)
		}
	}
	fmt.Fprintf(out, "set %s = %s in %s\n", key, value, path)
	// A provider key is only half-wired at this point. `secret set` deliberately
	// does NOT reconcile: it is a file transaction over arbitrary keys, and making
	// it fire N live inference probes would mean a dead provider API could fail a
	// credential write. But saying nothing is what produced "I set the key and
	// nothing happened, and I could not find where to finish", so name the next
	// command here, where the user actually is.
	if p, isProviderKey := providerKeyRefs[key]; isProviderKey {
		fmt.Fprintf(out, "%s is stored but not yet wired to any model. Finish with: pix models add %s\n", key, p)
	}
	return nil
}

// RunSecretRm removes ENV_VAR's line from op-refs.env, preserving every other
// line (comments, blanks, other refs). A missing file or a key that was never
// present is a clean, exit-0 no-op — `rm` is idempotent.
//
// For one of the three model-provider keys, it ALSO removes the same line
// from hostmode.env — the mirror RunSecretSet writes there — so `secret rm`
// fully undoes what `secret set` did in BOTH files, never leaving a stale
// key in one of them. A non-provider key is unchanged: op-refs.env only.
//
// Every write goes through env.WriteFile, which for the real CLI
// (defaultShellEnv) is symlink-safe and atomic (a same-directory temp file +
// rename, so a symlinked leaf is replaced rather than followed/truncated —
// see atomicWriteInDir). A partial failure (one file's removal succeeds, the
// other's write errors) is reported HONESTLY and returns a non-nil error so
// the dispatcher exits nonzero — it never claims a clean removal while a
// file still carries the key. Never prints a resolved secret value: rm takes
// no value at all, only a ref (refs are safe to echo).
//
// The whole both-file removal is one transaction under the provider-refs
// lock, mirroring RunSecretSet: a concurrent set/setup cannot interleave
// between op-refs.env and hostmode.env being updated. A lock acquisition
// failure fails the command honestly.
func RunSecretRm(env hostenv.Env, out io.Writer, key string) error {
	var txErr error
	if lerr := WithProviderRefsLock(env, func() error {
		txErr = runSecretRmLocked(env, out, key)
		return nil
	}); lerr != nil {
		fmt.Fprintf(out, "pix secret rm: could not lock provider refs (%s): %v\n", ProviderRefsLockPath(env), lerr)
		return lerr
	}
	return txErr
}

// runSecretRmLocked is RunSecretRm's file transaction. Caller MUST hold the
// provider-refs lock.
func runSecretRmLocked(env hostenv.Env, out io.Writer, key string) error {
	path := DefaultOpRefsPath(env)
	content := ""
	exists := false
	c, err := env.ReadFile(path)
	switch {
	case err == nil:
		content, exists = c, true
	case os.IsNotExist(err):
		// Missing is an idempotent no-op unless hostmode.env has the key.
	default:
		fmt.Fprintf(out, "pix secret rm: could not read %s: %v\n", path, err)
		return fmt.Errorf("read %s: %w", path, err)
	}
	opRemoved := false
	if exists {
		newContent, removed := removeOpRef(content, key)
		if removed {

			if err := env.WriteFile(path, []byte(newContent), 0o600); err != nil {
				fmt.Fprintf(out, "pix secret rm: could not write %s: %v\n", path, err)
				return err
			}
			opRemoved = true
		}
	}

	hmPath := HostModeRefsPath(env)
	hmRemoved := false
	if _, isProviderKey := providerKeyRefs[key]; isProviderKey {
		hmContent, rerr := env.ReadFile(hmPath)
		switch {
		case rerr == nil:
			newHm, removed := removeOpRef(hmContent, key)
			if removed {

				if err := env.WriteFile(hmPath, []byte(newHm), 0o600); err != nil {
					fmt.Fprintf(out, "pix secret rm: removed %s from %s, but could not remove it from %s: %v — host mode still has this key until you fix that\n", key, path, hmPath, err)
					return fmt.Errorf("remove %s from hostmode.env: %w", key, err)
				}
				hmRemoved = true
			}
		case os.IsNotExist(rerr):
			// hostmode.env absent: nothing there to remove, not an error.
		default:
			// A real read error must never be silently treated as "nothing to
			// remove" — that would leave a stale key in hostmode.env while
			// claiming a clean removal.
			fmt.Fprintf(out, "pix secret rm: removed %s from %s, but could not check %s: %v\n", key, path, hmPath, rerr)
			return fmt.Errorf("check %s: %w", hmPath, rerr)
		}
	}

	switch {
	case !exists && !hmRemoved:
		fmt.Fprintf(out, "op-refs.env not found (%s) — nothing to remove\n", path)
	case !opRemoved && !hmRemoved:
		fmt.Fprintf(out, "no ref named %s in %s\n", key, path)
	case opRemoved && hmRemoved:
		fmt.Fprintf(out, "removed %s from %s and %s\n", key, path, hmPath)
	case opRemoved:
		fmt.Fprintf(out, "removed %s from %s\n", key, path)
	default: // hmRemoved only
		fmt.Fprintf(out, "removed %s from %s\n", key, hmPath)
	}
	return nil
}

// upsertOpRef returns content with KEY=value in place of an existing KEY= line
// (comments/blanks/other entries untouched), or appended at the end if KEY was
// not already present.
func upsertOpRef(content, key, value string) string {
	newLine := key + "=" + value
	lines := strings.Split(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1] // drop the trailing empty element from a final "\n"
	}
	found := false
	out := make([]string, 0, len(lines)+1)
	for _, line := range lines {
		if opRefLineKey(line) != key {
			out = append(out, line)
			continue
		}
		if !found {
			out = append(out, newLine)
			found = true
		}
		// Drop later duplicates. Keeping conflicting KEY= lines would let one
		// caller validate the first while another applies the last.
	}
	if !found {
		out = append(out, newLine)
	}
	return strings.Join(out, "\n") + "\n"
}

// removeOpRef returns content with KEY's line dropped (everything else
// preserved), and whether KEY was found at all.
func removeOpRef(content, key string) (out string, removed bool) {
	lines := strings.Split(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	kept := make([]string, 0, len(lines))
	for _, ln := range lines {
		if opRefLineKey(ln) == key {
			removed = true
			continue
		}
		kept = append(kept, ln)
	}
	if !removed {
		return content, false
	}
	if len(kept) == 0 {
		return "", true
	}
	return strings.Join(kept, "\n") + "\n", true
}

// opRefLineKey returns the KEY of a raw op-refs.env line, or "" if the line is
// blank, a comment, or not a KEY=VALUE line at all.
func opRefLineKey(ln string) string {
	t := strings.TrimSpace(ln)
	if t == "" || strings.HasPrefix(t, "#") {
		return ""
	}
	eq := strings.IndexByte(t, '=')
	if eq <= 0 {
		return ""
	}
	return strings.TrimSpace(t[:eq])
}

// repairLegacyOpRefsTemplate comments the three prose lines emitted without a
// leading # by Pix 0.1.14's template interpolation bug. Match exact known text
// only: user-authored malformed lines are never silently rewritten.
func repairLegacyOpRefsTemplate(content string) (string, bool) {
	legacy := map[string]bool{
		"host MCP server it resolves those refs from 1Password and injects them as env": true,
		"vars — the secret never touches disk or the sandbox. A server with no creds":   true,
		"(pio) needs no entry.": true,
	}
	lines := strings.Split(content, "\n")
	changed := false
	for i, line := range lines {
		if legacy[line] {
			lines[i] = "# " + line
			changed = true
		}
	}
	return strings.Join(lines, "\n"), changed
}

func RepairLegacyOpRefsFile(env hostenv.Env, path string) error {

	content, err := env.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	repaired, changed := repairLegacyOpRefsTemplate(content)
	if !changed {
		return nil
	}
	return env.WriteFile(path, []byte(repaired), 0o600)
}

// RunSecretCheck resolves every op:// ref in op-refs.env with `op read` and
// reports OK/FAIL per KEY. It NEVER prints the resolved value (only OK/FAIL).
// Degrades clearly when op is absent or not signed in.
func RunSecretCheck(env hostenv.Env, out io.Writer) {
	path, content, exists := OpRefsContent(env)
	if !exists {
		fmt.Fprintf(out, "op-refs.env not found (%s) — create it with: pix secret set <ENV_VAR> op://vault/item/field\n", path)
		os.Exit(3)
	}
	if !OpInstalled(env) {
		fmt.Fprintln(out, "op (1Password CLI) not installed — https://developer.1password.com/docs/cli")
		os.Exit(3)
	}
	if !OpSignedIn(env) {
		fmt.Fprintln(out, "op is installed but no account configured — run: op signin, then retry")
		os.Exit(3)
	}
	refs := ParseOpRefs(content)
	var checked, failed int
	for _, r := range refs {
		if !r.IsRef {
			continue
		}
		checked++
		if r.Placeholder {
			fmt.Fprintf(out, "  ✗ %s: FAIL (unfilled placeholder)\n", r.Key)
			failed++
			continue
		}
		// op read resolves the ref; we discard the value (stdout) and only look
		// at the exit status, so no secret is ever printed.
		if _, err := env.Run("op", "read", r.Value); err != nil {
			fmt.Fprintf(out, "  ✗ %s: FAIL\n", r.Key)
			failed++
		} else {
			fmt.Fprintf(out, "  ✓ %s: OK\n", r.Key)
		}
	}
	if checked == 0 {
		fmt.Fprintln(out, "no op:// refs to check (add ENV_VAR=op://vault/item/field lines)")
		return
	}
	if failed > 0 {
		fmt.Fprintf(out, "%d of %d refs failed to resolve.\n", failed, checked)
		os.Exit(1)
	}
	fmt.Fprintf(out, "all %d refs resolve.\n", checked)
}

// indent prefixes every line of s with two spaces (for nested help/status text).
func indent(s string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = "  " + ln
	}
	return strings.Join(lines, "\n")
}

// AnyOpWrappedServer reports whether any configured MCP server makes the Secrets
// (1Password) group relevant: a NON-gog server. gog is deliberately excluded —
// it authenticates via its own OAuth grant (no built-in guided setup; see
// docs/gworkspace.md), never an op-refs token, so a gog-only config needs no
// op-refs.env (mcp-register registers gog BARE for exactly this reason, and
// setup's Step 4 skips it via hasNonGogMCP). gog's ONE conditional op-refs
// need — a headless keyring password — is owned by the gog group's
// headless-spawn check, not this group, so counting gog here produced a
// phantom `pix secret set` TODO on a fresh gog-only install. Remote
// gateway-catalog servers don't strictly need op-refs either, but distinguishing
// them requires probing pix-host; for this coarse gate any non-gog name
// counts (mirrors setup's hasNonGogMCP).
func AnyOpWrappedServer(cfg *config.Config) bool {
	for _, m := range cfg.MCP {
		if m != config.GWServerName {
			return true
		}
	}
	return false
}

// ── moved here from four unrelated files ─────────────────────────────────────
//
// Each of these answers a question about SECRETS and had come to rest wherever
// its first caller happened to live: CurrentOpRef in setup.go, DefaultOpRefsPath
// in mcp.go, ProbeSbxSecrets in bootstrap.go, looksSecretShaped in
// doctor_secrets.go. Scattering a domain across its callers is what made this
// package a web; putting each function where it answers a question about is
// what has driven every extraction's inbound count down.

// CurrentOpRef returns the current FILLED op:// ref for a provider env var. It
// checks op-refs.env (sandbox) AND hostmode.env (host mode): a ref given via
// EITHER path counts, so setup never re-prompts for a ref the user already
// provided in one file but not the other. Pure/read-only — it does NOT write
// anything: a ref found only in hostmode.env is backfilled into op-refs.env by
// the caller (setupProvisionKeys' hasRef branch), which writes to BOTH files
// and fails setup outright if either write errors, rather than this function
// doing a silent best-effort backfill whose failure nobody checks.
func CurrentOpRef(env hostenv.Env, envVar string) (string, bool) {
	if _, content, exists := OpRefsContent(env); exists {
		for _, r := range ParseOpRefs(content) {
			if r.Key == envVar && r.IsRef && !r.Placeholder {
				return r.Value, true
			}
		}
	}
	if content, err := env.ReadFile(HostModeRefsPath(env)); err == nil {
		for _, r := range ParseOpRefs(content) {
			if r.Key == envVar && r.IsRef && !r.Placeholder {
				return r.Value, true
			}
		}
	}
	return "", false
}

// DefaultOpRefsPath computes the absolute XDG op-refs.env path from the injected
// env (mirrors config.OpRefsPath but stays hermetic under test): $PIX_CONFIG
// dir, else $XDG_CONFIG_HOME/pix, else ~/.config/pix — all + op-refs.env.
// This is the path repo-less hosts must create, so every user-facing message and
// the seeder reference it (never a meaningless repo-relative config/op-refs.env).
func DefaultOpRefsPath(env hostenv.Env) string {
	if p := env.Getenv("PIX_CONFIG"); p != "" {
		return filepath.Join(filepath.Dir(p), "op-refs.env")
	}
	if xdg := env.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "pix", "op-refs.env")
	}
	if home := env.HomeDir(); home != "" {
		return filepath.Join(home, ".config", "pix", "op-refs.env")
	}
	return config.OpRefsPath()
}

// ProbeSbxSecrets runs `sbx secret ls` and classifies the result into
// SbxSecretsProbeState. out is only meaningful when state == SbxSecretsOK.
func ProbeSbxSecrets(env hostenv.Env) (out string, state SbxSecretsProbeState) {
	if _, err := env.LookPath("sbx"); err != nil {
		return "", SbxSecretsAbsent
	}
	// BOUNDED (probeRun): a hung `sbx secret ls` classifies as SbxSecretsError
	// (sbx IS on PATH — a real, diagnosable problem, never "absent") instead of
	// hanging the caller forever.
	o, timedOut, err := env.RunTimed("sbx", "secret", "ls")
	if err != nil || timedOut {
		return "", SbxSecretsError
	}
	return o, SbxSecretsOK
}

// SbxSecretsProbeState distinguishes WHY `sbx secret ls` couldn't answer, so
// callers with a MANDATORY-keys invariant (setupProvisionKeys) can fail-open
// only for genuine portability (sbx isn't installed here at all — there is
// nothing to reconcile against) and fail CLOSED with a diagnostic when sbx IS
// installed but its control plane errored (a real, fixable problem, not "no
// sandbox here"). sbxModelKeyState (the `run`/bootstrap path) deliberately
// keeps its own coarser tri-state — `run` fails open on EITHER cause, since
// its only question is "is there a key", not "can I trust a completeness
// claim".
type SbxSecretsProbeState int

const (
	SbxSecretsAbsent SbxSecretsProbeState = iota // sbx not on PATH: fail-open (portability)
	SbxSecretsError                              // sbx on PATH but `sbx secret ls` failed: fail CLOSED
	SbxSecretsOK                                 // sbx on PATH and `sbx secret ls` succeeded
)

// SbxAllModelKeysPresent probes sbx for ALL THREE model provider keys
// (anthropic/openai/google). `pix setup`'s mandatory-keys invariant
// requires ALL three (not merely one), so this is deliberately stricter than
// sbxModelKeyState, which `run` uses to decide "is there ANY usable key".
func SbxAllModelKeysPresent(env hostenv.Env) (all bool, state SbxSecretsProbeState) {
	out, state := ProbeSbxSecrets(env)
	if state != SbxSecretsOK {
		return false, state
	}
	for _, k := range ModelProviders {
		if !cli.GrepWord(out, k) {
			return false, SbxSecretsOK
		}
	}
	return true, SbxSecretsOK
}

// ModelProviders are the model-provider secret keys a pi session needs at least
// one of to run. github is deliberately excluded: it authorizes git operations,
// not the model.
var ModelProviders = []string{"anthropic", "openai", "google"}

// AnyModelKeyInOutput reports whether out (the text of `sbx secret ls`) shows
// any model provider key as set. Pure, and the SINGLE definition of "what
// counts as a present model key", so the launch gate and every reporter agree.
// It says nothing about whether the probe ANSWERED — that tri-state is the
// caller's (SbxSecretsProbeState), and conflating the two is exactly how a
// failed probe turns into a false "you have no key".
func AnyModelKeyInOutput(out string) bool {
	for _, k := range ModelProviders {
		if cli.GrepWord(out, k) {
			return true
		}
	}
	return false
}

// ModelKeyMissingMessage is the guidance printed when no model key could be put
// in place. (The launch-blocking presence CHECK lives in runRun/launchTask via
// sbxModelKeyState's tri-state; this is only the how-to-fix text.)
func ModelKeyMissingMessage(env hostenv.Env) string {
	msg := fmt.Sprintf("pix run: no model provider key is set (need one of %s).\n",
		strings.Join(ModelProviders, ", "))
	if ProviderKeyRefsPresent(env) {
		msg += "You have 1Password key refs; resolve them into sbx with:\n  pix secret sync\n"
	} else {
		msg += "Keys come from 1Password (op is required). Configure them, then re-run:\n" +
			"  pix setup                                                       (guided, all providers)\n" +
			"  pix models add anthropic                                        (one provider, prompts for the ref)\n" +
			"  pix secret set ANTHROPIC_API_KEY op://vault/item/field           (scripted; then `pix models add anthropic`)\n"
	}
	return msg
}
