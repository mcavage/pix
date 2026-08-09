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

// exitCode carries an exit code up to L4 for a message this package ALREADY
// worded on its writer, so nothing re-renders it as a second, vaguer line.
// 2 = the invocation was wrong, decided before any file is read or locked;
// 3 = it could not be answered at all (no refs file, no op, no 1Password
// session), never conflated with a bad answer; 1 = answered, and it failed.
func exitCode(code int) error { return cli.SilentError{Code: code} }

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
		case r.Placeholder:
			fmt.Fprintf(out, "    ✗ %s = placeholder (fill in the op:// ref)\n", r.Key)
		case r.IsRef:
			fmt.Fprintf(out, "    ✓ %s = op:// ref\n", r.Key)
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
// both require one and reject a percent-encoded one), seeds the file via the
// ONE seeder (config.SeedOpRefs) if absent, upserts preserving every other
// line, and prints the REF it stored (never a resolved secret).
//
// CLI-ARGUMENT failures (a bad env-var name, a control character, a non-ref
// value for a secret key) reject the invocation itself: the reason is printed
// on out and exitCode(2) returned, before anything is read or locked. The rest
// is one transaction under the provider-refs lock (WithProviderRefsLock), so a
// concurrent `secret set`/`secret rm`/setup cannot interleave; failures inside
// it return errors too, so the lock's deferred release always runs.
func RunSecretSet(env hostenv.Env, out io.Writer, key, value string) error {
	if !EnvVarNameRe.MatchString(key) {
		fmt.Fprintf(out, "pix secret set: %q does not look like an env var name (want %s)\n", key, EnvVarNameRe.String())
		return exitCode(2)
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
		return exitCode(2)
	}

	isRef := strings.HasPrefix(value, "op://")
	if !isRef && !config.NonSecretOpRefsKeys[key] {
		if config.LooksSecretShaped(key, value) {
			fmt.Fprintf(out, "pix secret set: %s looks like a pasted secret — this file is refs-only; pass op://vault/item/field, or your secret would land on disk\n", key)
		} else {
			fmt.Fprintf(out, "pix secret set: %s is not an op:// ref — this file is refs-only; pass op://vault/item/field, or your secret would land on disk\n", key)
		}
		return exitCode(2)
	}

	// Refs are stored with LITERAL spaces; the write chokepoint below does that
	// normalization, so the value passes through untouched here. A lock
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
// op-refs.env). Caller MUST hold the provider-refs lock; every failure returns
// an error, so the lock is always released.
func RunSecretSetLocked(env hostenv.Env, out io.Writer, key, value string) error {
	// %20 -> literal space BEFORE writing: op 2.35.0's `op read` AND `op run
	// --env-file` both require a literal space in a ref (a spaced 1Password
	// field name, e.g. "Anthropic API Key") and reject a percent-encoded one.
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

	fmt.Fprintf(out, "set %s = %s in %s\n", key, value, path)
	// A provider key is only half-wired at this point. `secret set` deliberately
	// does NOT reconcile: it is a file transaction over arbitrary keys, and making
	// it fire N live inference probes would mean a dead provider API could fail a
	// credential write. But saying nothing is what produced "I set the key and
	// nothing happened, and I could not find where to finish", so name the next
	// command here, where the user actually is.
	if p, known := providerKeyRefs[key]; known {
		if isModelProviderKey(key) {
			fmt.Fprintf(out, "%s is stored but not yet wired to any model. Finish with: pix models add %s\n", key, p)
		} else {
			// A TOOL key has no `models add` step: it is wired the moment it reaches
			// the sbx secret store, and the sandbox picks it up on its next launch.
			// Saying "pix models add parallel" here would send the user to a command
			// that rejects the name.
			fmt.Fprintf(out, "%s is stored. Mirror it with: pix secret sync   (then a fresh `pix run` picks it up)\n", key)
		}
	}
	return nil
}

// RunSecretRm removes ENV_VAR's line from op-refs.env, preserving every other
// line (comments, blanks, other refs). A missing file or a key that was never
// present is a clean, exit-0 no-op — `rm` is idempotent.
//
// The write goes through env.WriteFile, which for the real CLI
// (defaultShellEnv) is symlink-safe and atomic (a same-directory temp file +
// rename, so a symlinked leaf is replaced rather than followed/truncated —
// see atomicWriteInDir). A write failure is reported HONESTLY and returns a
// non-nil error so the dispatcher exits nonzero — it never claims a clean
// removal while the file still carries the key. Never prints a resolved
// secret value: rm takes no value at all, only a ref (refs are safe to echo).
//
// The removal runs under the provider-refs lock, mirroring RunSecretSet, so a
// concurrent set/setup cannot interleave. A lock acquisition failure fails the
// command honestly.
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
		// Missing is an idempotent no-op.
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

	switch {
	case !exists:
		fmt.Fprintf(out, "op-refs.env not found (%s) — nothing to remove\n", path)
	case !opRemoved:
		fmt.Fprintf(out, "no ref named %s in %s\n", key, path)
	default:
		fmt.Fprintf(out, "removed %s from %s\n", key, path)
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
// The three no-evidence arms (no refs file, no op, not signed in) return exit
// 3; a ref that fails to RESOLVE is a plain failure, exit 1.
func RunSecretCheck(env hostenv.Env, out io.Writer) error {
	path, content, exists := OpRefsContent(env)
	if !exists {
		fmt.Fprintf(out, "op-refs.env not found (%s) — create it with: pix secret set <ENV_VAR> op://vault/item/field\n", path)
		return exitCode(3)
	}
	if !OpInstalled(env) {
		fmt.Fprintln(out, "op (1Password CLI) not installed — https://developer.1password.com/docs/cli")
		return exitCode(3)
	}
	if !OpSignedIn(env) {
		fmt.Fprintln(out, "op is installed but no account configured — run: op signin, then retry")
		return exitCode(3)
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
		return nil
	}
	if failed > 0 {
		fmt.Fprintf(out, "%d of %d refs failed to resolve.\n", failed, checked)
		return exitCode(1)
	}
	fmt.Fprintf(out, "all %d refs resolve.\n", checked)
	return nil
}

// indent prefixes every line of s with two spaces (for nested help/status text).
func indent(s string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = "  " + ln
	}
	return strings.Join(lines, "\n")
}

// CurrentOpRef returns the current FILLED op:// ref for a provider env var
// from op-refs.env, the single refs file. Pure/read-only — it never writes.
func CurrentOpRef(env hostenv.Env, envVar string) (string, bool) {
	if _, content, exists := OpRefsContent(env); exists {
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
